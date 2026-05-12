# Proposal: Patching & Minor Version Upgrades for DBInstance

| | |
|---|---|
| **Status** | Draft |
| **Author** | Deependra Ariyadewa |
| **Created** | 2026-05-12 |
| **Component** | `dbaas-controller` |
| **Tracks** | DBInstance lifecycle |

## Summary

Add a controlled, crash-safe mechanism for applying OS-level security patches (kernel, openssl, glibc) and PostgreSQL minor-version updates (e.g. 14.10 → 14.11) to running `DBInstance` resources, without losing the encrypted pgdata volume and without manual intervention inside the VM.

The mechanism is an *immutable rebuild* of the OS DataVolume from a newer Harvester VM image, orchestrated by a new patch-phase machine in the controller. The encrypted pgdata DataVolume is left in place and reattached to the rebuilt VM.

## Motivation

Today, once a `DBInstance` reaches `Available`, there is no controller-aware way to apply security patches:

- The VM is provisioned once from a fixed `spec.osImage` (default `ubuntu-22.04-server-cloudimg-amd64.img`) and never changes.
- `spec.engineVersion` (PostgreSQL major version) is consumed at provisioning time and ignored afterwards.
- `reconcileModify` handles CPU/RAM/storage resize but not image or engine changes.
- The only options for an operator today are SSH-and-`apt upgrade` (untracked, drifts between instances) or destroy-and-recreate (loses data).

This blocks the platform from meeting routine security-compliance obligations and means CVE response is a manual, per-instance toil.

## Goals

- Apply OS patches and PostgreSQL minor-version updates to a running instance with bounded downtime (~minutes).
- Preserve the LUKS-encrypted pgdata volume across the operation.
- Survive controller restarts mid-patch (same crash-safe model as provisioning).
- Provide an explicit rollback path if the new image is broken.
- Expose the operation through the existing declarative spec — no new API surface, no out-of-band scripts.

## Non-Goals

- **PostgreSQL major version upgrades** (14 → 15). These need `pg_upgrade` or dump/restore and are tracked separately.
- **Auto-discovery of new patch images.** A future controller could watch an `ImagePolicy` and bump `spec.osImage` automatically; this proposal handles only the apply mechanism.
- **Zero-downtime patching.** Achievable only with replica failover (MultiAZ), which is a separate workstream.
- **In-place `apt upgrade`** inside the VM. Explicitly rejected — see Alternatives.

## Design

### Trigger

The user (or an upstream automation) edits `spec.osImage` and/or `spec.engineVersion` on an `Available` instance:

```yaml
spec:
  osImage: ubuntu-22.04-server-cloudimg-amd64-20260501.img  # was -20260301
  engineVersion: "16"
```

The reconciler classifies the spec diff:

| Fields changed | Path |
|----------------|------|
| `dbInstanceClass`, `allocatedStorage` only | existing `reconcileModify` (resize) |
| `osImage` or `engineVersion` | new `reconcilePatch` (this proposal) |
| Both | resize first, then patch |

### Patch phase machine

`Status.Phase` becomes `patching`. `Status.ProvisioningPhase` walks through six new phases:

| Phase | Action | Idempotent via |
|-------|--------|----------------|
| `PatchPending` | Validate target image exists in Harvester; seed `Status.PatchState`; record `Status.PreviousOSImage` for rollback | spec validation + status field check |
| `PatchSnapshotting` | Create a `DBSnapshot` named after the patch attempt; wait for `available` | snapshot name deterministic per attempt |
| `PatchStopping` | Set VM `spec.running=false`; wait for VMI to disappear | `setVMRunning(false)` already idempotent |
| `PatchOSReplaced` | Delete `pg-{id}-os` DataVolume; recreate it from the new image via `EnsureOSDataVolume` | delete tolerates NotFound; create tolerates AlreadyExists |
| `PatchStarting` | Set VM `spec.running=true`; cloud-init re-runs against existing pgdata | cloud-init must detect already-formatted pgdata |
| `PatchVerifying` | Wait for VMI Running + Postgres ready; run `SELECT version()` and assert match | reuses `GetVMIReadiness` |
| → `Available` | Update `Status.CurrentOSImage`, `Status.CurrentEngineVersion`, `Status.LastPatchTime`; clear `Status.PatchState` | terminal |

If any phase fails twice (tracked via `Status.PatchState.AttemptCount`), the controller enters `reconcileRollback`: it re-runs the same flow with `previousOSImage` as the target. If rollback itself fails, the instance moves to `Failed` and pages the operator.

### Crash safety

The same model as creation phases. On restart, the controller reads `Status.ProvisioningPhase` and `Status.PatchState` and resumes from the next step. `PatchState` is a deliberate field — without it, the controller would not know what target the patch was aiming for after a restart.

### Cloud-init behavior on a re-patched OS disk

The pgdata volume is already LUKS-formatted and populated. Cloud-init must:

- Skip `cryptsetup luksFormat` (the volume is already a LUKS container).
- Skip `initdb` (PGDATA already exists).
- Skip master-user creation (the role already exists in the catalog).
- Still: `luksOpen` with the stored key, mount, `pg_ctlcluster start`, ensure pgBackRest/exporter services come up.

The current cloud-init has a "is pgdata empty?" check that gates `initdb`. That check must be the gate for *all* init steps, not just `initdb`. This is a hardening task tracked as part of this proposal.

### Data model changes

```go
// types.go
const (
    StatusPatching          = "patching"
    PhasePatchPending       = "PatchPending"
    PhasePatchSnapshotting  = "PatchSnapshotting"
    PhasePatchStopping      = "PatchStopping"
    PhasePatchOSReplaced    = "PatchOSReplaced"
    PhasePatchStarting      = "PatchStarting"
    PhasePatchVerifying     = "PatchVerifying"
)

type PatchState struct {
    TargetOSImage        string       `json:"targetOSImage"`
    TargetEngineVersion  string       `json:"targetEngineVersion,omitempty"`
    SnapshotName         string       `json:"snapshotName,omitempty"`
    StartedAt            *metav1.Time `json:"startedAt,omitempty"`
    AttemptCount         int          `json:"attemptCount"`
}

type DBInstanceStatus struct {
    // ... existing fields ...
    CurrentOSImage        string       `json:"currentOSImage,omitempty"`
    CurrentEngineVersion  string       `json:"currentEngineVersion,omitempty"`
    PreviousOSImage       string       `json:"previousOSImage,omitempty"`
    LastPatchTime         *metav1.Time `json:"lastPatchTime,omitempty"`
    PatchState            *PatchState  `json:"patchState,omitempty"`
}
```

### Code touchpoints

- `api/v1alpha1/types.go` — constants, status fields, `PatchState` struct, CRD printer columns.
- `internal/controller/dbinstance_reconciler.go` — diff classifier in `Reconcile`, `reconcilePatch` entry, six `phasePatch*` functions, `reconcileRollback`.
- `internal/harvester/client.go` — `EnsureOSDataVolume` (extracted from `CreateVM`), `DeleteDataVolume`, `WaitForVMIGone`.
- `internal/harvester/cloudinit.go` — gate all init steps on "pgdata empty"; add an idempotent restart path.
- `Makefile` — convenience target `make patch INSTANCE=<name> IMAGE=<new-image>`.
- `README.md` — Operations → Patching section.

## Alternatives Considered

**In-VM `apt upgrade` driven by the controller (SSH/exec).** Faster, often zero downtime for non-kernel patches. Rejected because: every instance ends up slightly different over time (state drift), there is no atomic rollback, and the controller acquires a new failure mode (SSH key management). The platform's existing guarantee — "VMs are disposable, state lives in pgdata" — is the reason we *can* do immutable rebuild cheaply.

**Unattended-upgrades inside the VM.** Zero controller work but uncontrolled reboot timing, no central visibility, no tie-in to maintenance windows. Acceptable as a defense-in-depth layer, not as the primary mechanism.

**Live migration to a new VM.** KubeVirt supports live migration but not across different OS images. Not applicable.

## Risks

| Risk | Mitigation |
|------|------------|
| Cloud-init reformats pgdata on the new OS disk | Audit + harden the empty-check; add an integration test that boots with pre-populated pgdata |
| New image has a broken kernel module (e.g. virtio-blk) — VM never boots | Snapshot-before-patch + `PatchVerifying` timeout triggers rollback |
| Many instances patched concurrently saturate Harvester | Global semaphore (`--max-concurrent-patches`), default 3 |
| User patches during peak load | Optional `spec.preferredMaintenanceWindow` to queue; immediate-apply remains default |
| LUKS key rotation drift after re-patch | Key is stored in the credentials Secret, not regenerated by cloud-init — confirm in audit |

## Open Questions

1. **Snapshot before patch:** mandatory or opt-in (`spec.skipPatchSnapshot`)? Recommended: mandatory by default.
2. **Maintenance windows:** apply-on-spec-change or queue-and-apply? Recommended: apply-on-spec-change in v1; window support in v2.
3. **Major-version upgrade UX:** separate `DBInstanceUpgrade` action or a `spec.engineVersionTarget` field? Out of scope but worth aligning on.
4. **Concurrency cap default:** 3 seems safe; needs validation with a Harvester sizing test.

## Rollout

1. Land the data model + reconciler changes behind a feature flag (`--enable-patching`, default off).
2. Pilot on a single non-production instance: bump `osImage` end-to-end, validate pgdata survives, validate rollback.
3. Enable in staging; run for two weeks against scheduled patch images.
4. Flip the flag default to on in production.
5. (Later) Add `ImagePolicy` watcher for auto-bumping.

## Test Plan

- Unit tests for each `phasePatch*` function with a fake Harvester client.
- E2E: provision → seed table → bump `osImage` → assert row count + `SELECT version()` after patch.
- Crash test: kill controller during `PatchStopping`; verify resume completes the patch.
- Rollback test: point `osImage` at a non-existent image; verify `previousOSImage` is restored.
- Concurrency test: trigger patches on 10 instances; verify the semaphore holds and none corrupt.

## References

- `api/v1alpha1/types.go` — current `DBInstanceSpec`, `DBInstanceStatus`, phase constants
- `internal/controller/dbinstance_reconciler.go:73-76` — current spec-change branch
- `internal/controller/dbinstance_reconciler.go:385-419` — current `reconcileModify` (resize)
- `internal/harvester/client.go:171-211` — current `CreateDataVolume` / `ResizeDataVolume`
- `internal/harvester/cloudinit.go` — cloud-init template
- AWS RDS reference: [Maintaining a DB instance](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/USER_UpgradeDBInstance.Maintenance.html)
