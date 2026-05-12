# TLA+ Model — DBInstance Controller

Companion to [`../001-patching-and-minor-upgrades.md`](../001-patching-and-minor-upgrades.md). Models the full DBInstance lifecycle (provisioning, stop/start, patch, delete) so TLC can verify safety and liveness across all interleavings of user actions, controller phases, and crashes.

## Files

| File | Purpose |
|------|---------|
| `DBInstance.tla` | The specification |
| `DBInstance.cfg` | TLC model parameters (`Images`, `MaxAttempts`) and the invariants/properties to check |

## What is verified

**Safety invariants** (must hold in every reachable state):

| Invariant | Why it matters |
|-----------|----------------|
| `PgDataAlwaysPresentWhileResourceExists` | The encrypted pgdata volume is never deleted while the resource is alive. The single most important data-loss invariant. |
| `NoDestructiveStepWithoutSnapshot` | OS DataVolume is only replaced after a successful snapshot. Catches phase reorderings or skip-snapshot retries. |
| `OSReplaceOnlyWhileVMStopped` | OS DataVolume is only mutated while the VM is stopped. Catches races where a stop completes late or a user start arrives mid-patch. |
| `CurrentImageConsistency` | In `available`, `status.currentImage` matches the image the OS DV was actually built from. Catches status/reality drift after patch. |
| `RollbackTargetSet` | Whenever the controller is in `patching`, the rollback target is recorded. Catches a restart losing `previousImage`. |
| `VMRunningImpliesOSDV` | A running VM always has an OS DataVolume to boot from. Catches missing recreate-after-delete in the patch flow. |

**Liveness properties** (every transient phase eventually terminates):

- `PatchTerminates`, `CreateTerminates`, `StopTerminates`, `StartTerminates`, `DeleteTerminates`

## How crashes are modeled

The destructive OS DataVolume replacement is split into two atomic actions: `PatchOSDelete` and `PatchOSCreate`. A crash between them is a reachable state in the model — `subPhase` is still `PatchOSReplaced` and `osDV` is `absent`. On restart, `PatchOSCreate` is enabled, so the controller resumes correctly. If a future refactor breaks this idempotency (for example by gating recreate on a status field that was already cleared), TLC finds it as a `CurrentImageConsistency` or `VMRunningImpliesOSDV` violation, or as a liveness failure.

All other phase transitions are atomic (effect + status update modeled as one TLA+ step). This is the standard simplification for controller specs and is sound as long as each phase action is itself idempotent on re-entry — which is what the preconditions on the underlying Harvester state (e.g. `osDV = "absent"` for `PatchOSCreate`) enforce.

## Running TLC

The spec is written for TLC 1.8+ (the model checker bundled with the TLA+ Toolbox and the VS Code extension).

**Command line:**

```bash
# Install: https://github.com/tlaplus/tlaplus/releases  (tla2tools.jar)
java -cp tla2tools.jar tlc2.TLC -workers auto -config DBInstance.cfg DBInstance.tla
```

**VS Code:** install the [TLA+ extension](https://marketplace.visualstudio.com/items?itemName=alygin.vscode-tlaplus), open `DBInstance.tla`, run "TLA+: Check model with TLC" using `DBInstance.cfg`.

**Expected outcome:** `Model checking completed. No error has been found.` State space with `MaxAttempts = 2` and three images is a few hundred thousand states, runs in seconds on a laptop.

## How to break it (to confirm TLC catches violations)

Try these one-line changes and re-run — each should produce a counterexample trace:

1. Remove `vmRunning = FALSE` from `PatchOSDelete` → expect `OSReplaceOnlyWhileVMStopped` violation.
2. Remove `snapshot = "available"` from `PatchOSDelete` → expect `NoDestructiveStepWithoutSnapshot` violation.
3. Add `pgDataDV' = "absent"` somewhere in the patch flow → expect `PgDataAlwaysPresentWhileResourceExists` violation.
4. Remove `osDV = "absent"` precondition from `PatchOSCreate` → spec still passes but is now non-idempotent; remove the split entirely and replace with a single atomic `PatchOSReplace` that doesn't gate on `osDV` state, then introduce a partial-completion flag, to see how missing crash handling breaks liveness.

## What is NOT modeled (deliberate scope cuts)

- **Multiple concurrent controllers.** Single-writer is enforced in production by leader election; modeling two writers would require explicit etcd resourceVersion semantics.
- **The full provisioning state machine.** Real code has Network, Storage, OS, VM, CloudInit, DB-Ready, Monitoring, VPC-Peering — collapsed here to Storage / OS / VM / Ready. The patch flow (the new code) is modeled in full detail.
- **Resize / class change.** Orthogonal to the patching invariants; would add states without testing anything new.
- **`engineVersion` as a separate dimension.** Modeled jointly with `osImage` under `specImage` since they have the same effect on the patch flow.
- **In-VM cloud-init behavior.** The "is pgdata empty?" gate that protects against re-formatting on a re-patched OS disk is a code-level concern, not a state-machine concern; covered in the proposal's test plan.
