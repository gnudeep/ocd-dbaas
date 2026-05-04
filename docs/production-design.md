# WSO2 Cloud DBaaS — Production Design

> **Scope:** This document describes the **target production architecture** for the
> ocd-dbaas service. It is forward-looking: most of what is described here is not
> yet implemented. The currently-shipped implementation is in
> [`/DESIGN.md`](../DESIGN.md) and [`/API.md`](../API.md).
>
> When a phase from section 10 ships, update DESIGN.md and API.md to match the
> reality of what is built; this document remains the north-star spec.

---

## 1. Goals and Non-Goals

### 1.1 Goals

- **AWS/Azure-equivalent UX** for downstream cluster engineers. They issue
  CLI/API calls and get a working PostgreSQL endpoint. No interaction with
  Harvester admins, Kubernetes manifests, or VM/network plumbing.
- **Multi-environment, multi-tenant.** One DBaaS service per WSO2 Cloud
  environment (`lk`, `lk-dev`, `us`, `eu`, ...); tenants are isolated within
  each environment.
- **Self-service.** Once a tenant is onboarded, engineers create, modify,
  snapshot, restore, and delete databases through API or CLI alone.
- **Production-grade from v1.** TLS to the database, scheduled backups, stable
  endpoints, audit log, HA control plane. Not "POC-quality, fix later."
- **Asgardeo-integrated** identity and authorization. No separate user store.
- **API-first.** A documented HTTP API is the contract. CLI and (future) UI are
  thin clients on top of it.

### 1.2 Non-Goals (v1)

- Cross-region replication.
- Read replicas.
- Per-DB network isolation via dynamic OVN provisioning (defer to v2).
- A graphical web UI (post-v1).
- Engines other than PostgreSQL.
- Per-DB cost reporting.

---

## 2. Target User Experience

A downstream cluster engineer in tenant `payments` of the `lk` environment:

```bash
# One-time login (browser-based Asgardeo OAuth)
$ dcctl login --env lk
Opening browser for authentication...
Logged in as alice@wso2.com (tenant: payments)

# Create a database
$ dcctl create-db \
    --name orders \
    --class db.t3.medium \
    --engine-version 16 \
    --storage 50 \
    --master-username orders_admin \
    --backup-retention 7

DBInstance "orders" created. Status: creating.

# Watch it come up
$ dcctl describe-db --name orders --watch
Name:               orders
Class:              db.t3.medium
Engine:             postgres 16
Status:             available
Endpoint:           orders.payments.lk.dbaas.wso2.cloud:5432
Master Username:    orders_admin
Master Secret:      orders-master-credentials  (kubectl get secret in your namespace)
TLS CA:             arn:dbaas:lk:payments:ca/orders   (dcctl get-ca --name orders)
Storage:            50 GiB (longhorn)
Backups:            daily, retention 7d, S3 bucket dbaas-lk-backups
Created:            2026-05-03T08:14:22Z

# Connect
$ psql "host=orders.payments.lk.dbaas.wso2.cloud \
        port=5432 dbname=postgres user=orders_admin \
        sslmode=verify-full sslrootcert=$(dcctl get-ca --name orders -o file)"
```

Engineers never see VM IPs, namespaces, or Harvester resources. The database
behaves like AWS RDS: stable DNS endpoint, TLS-secured, automated backups,
modify-in-place via subsequent `dcctl modify-db` calls.

---

## 3. System Architecture

### 3.1 Per-Environment Topology

Each WSO2 Cloud environment is a separate Harvester cluster running its own
DBaaS control plane. Tenants in `lk` are unrelated to tenants in `eu`.

```
┌──────────────────────────────────────────────────────────────────────────────┐
│  WSO2 Cloud — environment "lk"                                               │
│                                                                              │
│  ┌────────────────────────────────────────────────────────────────────────┐ │
│  │  Asgardeo (lk tenant)                                                  │ │
│  │  - Users, groups, OIDC                                                 │ │
│  │  - Group "payments-dbs" → tenant namespace "payments"                  │ │
│  │  - Group "orders-dbs"   → tenant namespace "orders"                    │ │
│  └────────────────────────────────────────────────────────────────────────┘ │
│                          │ OIDC tokens                                       │
│                          ▼                                                   │
│  ┌────────────────────────────────────────────────────────────────────────┐ │
│  │  DBaaS Gateway  (HA: 2+ replicas behind a Service, leader-elected      │ │
│  │  controller process is in the same pods)                                │ │
│  │  - HTTPS (TLS terminated by ingress or by gateway directly)             │ │
│  │  - Validates Asgardeo token, extracts groups → tenant                   │ │
│  │  - Translates AWS-RDS-style API → DBInstance CRD writes                 │ │
│  │  - Audit log → S3                                                       │ │
│  └────────────────────────────────────────────────────────────────────────┘ │
│                          │ DBInstance CRDs (tenant-namespaced)               │
│                          ▼                                                   │
│  ┌────────────────────────────────────────────────────────────────────────┐ │
│  │  Harvester / KubeVirt control plane                                    │ │
│  │  - Tenant namespace "payments" → bound to VLAN NAD iaas/vm-network-101 │ │
│  │  - Tenant namespace "orders"   → bound to VLAN NAD iaas/vm-network-102 │ │
│  │  - DBaaS controller reconciles each DBInstance into VMs, storage, ...  │ │
│  └────────────────────────────────────────────────────────────────────────┘ │
└──────────────────────────────────────────────────────────────────────────────┘

(Identical structure exists in lk-dev, us, eu, ...)
```

### 3.2 Request Flow (Create DB)

```
[ engineer ]
    │
    │  dcctl create-db --name orders --class db.t3.medium ...
    ▼
[ dcctl CLI ]
    │
    │  HTTPS POST https://dbaas.lk.wso2.cloud/v1/dbs
    │  Authorization: Bearer <asgardeo-id-token>
    │  Body: { "name": "orders", "class": "db.t3.medium", ... }
    ▼
[ DBaaS Gateway (HA, ingress-fronted) ]
    │
    │  1. Validate token signature against Asgardeo JWKS
    │  2. Parse claims → {sub: alice, groups: [payments-dbs]}
    │  3. Look up tenant: payments-dbs → namespace "payments"
    │  4. Authorize: "create db" allowed for this group? (RBAC)
    │  5. Validate request: class is real, name is k8s-legal, etc.
    │  6. Convert payload → DBInstance CRD (namespace=payments)
    │  7. Create CRD via controller-runtime client
    │  8. Audit-log: { actor, action, tenant, resource, at, request-id }
    │  9. Return 202 Accepted with the API representation
    ▼
[ K8s API Server → etcd ]
    │
    │  watch event
    ▼
[ DBaaS Controller (one of the HA replicas, leader holds the work) ]
    │
    │  Reconcile state machine:
    │   Pending → CertGenerated → NetworkAttached → StorageProvisioned →
    │   VMCreated → CloudInitDone → BackupConfigured → ServiceCreated →
    │   Available
    ▼
[ Harvester resources created in tenant namespace ]
   - cert-manager: Certificate "orders-tls" (CA + server cert)
   - CDI DataVolume: pg-orders-data
   - KubeVirt VM: pg-orders (NIC bound to iaas/vm-network-101)
   - Secret: orders-master-credentials
   - Service: orders.payments.svc.cluster.local (External-DNS → public DNS)
   - CronJob: orders-backup (pgBackRest → S3)
   - ServiceMonitor: orders-monitor
```

---

## 4. Tenancy Model

### 4.1 Identifiers

```
Full identity of a database = (environment, tenant, db-name)

  environment   : lk | lk-dev | us | eu | ...      (one Harvester cluster each)
  tenant        : k8s namespace name within the    (e.g. payments, orders)
                  environment's "dbaas" project
  db-name       : unique within (env, tenant)      (e.g. main, analytics)
```

### 4.2 Mapping Table (lives in DBaaS gateway config)

```yaml
# /etc/dbaas/tenants.yaml on each environment
tenants:
  - asgardeoGroup: payments-dbs
    namespace: payments
    networkRef: iaas/vm-network-101
    quotas:
      maxInstances: 20
      maxStoragePerInstanceGB: 1000
      maxTotalStorageGB: 5000
      allowedClasses: [db.t3.small, db.t3.medium, db.t3.large, db.m5.large]

  - asgardeoGroup: orders-dbs
    namespace: orders
    networkRef: iaas/vm-network-102
    quotas:
      maxInstances: 50
      maxStoragePerInstanceGB: 2000
      maxTotalStorageGB: 20000
      allowedClasses: [db.t3.medium, db.m5.large, db.m5.xlarge]
```

The gateway loads this on startup (and on SIGHUP). It is the only place that
knows the group→namespace→VLAN mapping; the controller just sees `DBInstance`
objects in tenant namespaces.

### 4.3 Why Namespace = Tenant

- Native Kubernetes RBAC isolation: a tenant's controller permissions, secrets,
  and resources are namespace-scoped — no chance of cross-tenant leakage.
- DBInstance becomes a **namespaced** resource (changing from today's
  cluster-scoped). This is the single biggest API change in v1.
- Backups and audit logs naturally segment by namespace.
- A tenant can be deleted by deleting one namespace.

---

## 5. Authentication and Authorization

### 5.1 Identity Provider

Each environment integrates with Asgardeo:

| Environment | Asgardeo Tenant       | OIDC Issuer URL                                |
|-------------|-----------------------|------------------------------------------------|
| lk          | wso2cloud-lk          | https://api.asgardeo.io/t/wso2cloud-lk/oauth2  |
| lk-dev      | wso2cloud-lkdev       | https://api.asgardeo.io/t/wso2cloud-lkdev/...  |
| us          | wso2cloud-us          | ...                                            |

A user is granted access to a tenant by being added to the corresponding
Asgardeo group (`payments-dbs`, etc.).

### 5.2 dcctl Login Flow (PKCE)

```
[ user ]                  [ dcctl ]                  [ browser ]            [ Asgardeo ]
   │                          │                          │                       │
   │  dcctl login --env lk    │                          │                       │
   ├─────────────────────────►│                          │                       │
   │                          │ open browser to          │                       │
   │                          │ authorize URL with PKCE  │                       │
   │                          │  + redirect to localhost │                       │
   │                          ├─────────────────────────►│                       │
   │                          │                          │ login + consent       │
   │                          │                          ├──────────────────────►│
   │                          │                          │◄──────────────────────┤
   │                          │                          │ redirect to localhost │
   │                          │                          │ with auth code        │
   │                          │◄─────────────────────────┤                       │
   │                          │ exchange code+verifier   │                       │
   │                          │ for tokens               │                       │
   │                          ├──────────────────────────────────────────────────►
   │                          │◄──────────────────────────────────────────────────
   │                          │  id_token + access_token + refresh_token         │
   │                          │                                                  │
   │                          │ store in ~/.dcctl/credentials/lk                 │
   │  "Logged in as alice"    │                                                  │
   │◄─────────────────────────┤                                                  │
```

Tokens are stored in an OS-native keyring (macOS Keychain, Linux Secret
Service, Windows Credential Manager) when available, with a file-based fallback
at `~/.dcctl/credentials/<env>` (mode 0600).

### 5.3 Gateway Token Validation

For each request:

1. Extract `Authorization: Bearer <token>`.
2. Verify signature against Asgardeo JWKS (cached for 5 minutes).
3. Verify `iss`, `aud`, `exp`, `nbf`.
4. Extract `groups` claim.
5. Find the first group that maps to a tenant in `tenants.yaml`. (One group
   per tenant for v1 — multi-group users get the first match deterministically.)
6. Set request context: `{user, tenant, namespace, networkRef, quotas}`.
7. Apply per-action authorization (see 5.4).

### 5.4 Authorization Model

For v1, two roles per tenant — declared in `tenants.yaml`:

```yaml
tenants:
  - asgardeoGroup: payments-dbs
    namespace: payments
    networkRef: iaas/vm-network-101
    roles:
      admin:
        - payments-admins             # Asgardeo group
      readWrite:
        - payments-dbs                # default group → can CRUD DBs
```

| Role       | Permissions                                                           |
|------------|-----------------------------------------------------------------------|
| readWrite  | Create, list, describe, modify, delete, snapshot, restore own tenant  |
| admin      | All readWrite + manage tenant quotas (future), view audit log         |

If a token has none of the configured groups, the gateway returns
`403 Forbidden` with `error: NotAuthorized`.

---

## 6. Tenant Onboarding (Admin Workflow)

This is the only workflow that requires a Harvester / DBaaS admin. Done **once
per tenant** when a new downstream cluster comes online.

```
1. Provision the VLAN (one-time per tenant)
   - Network team allocates a VLAN ID (e.g., 101) and CIDR (10.50.101.0/24)
   - Harvester admin creates a NetworkAttachmentDefinition:
       kubectl apply -f - <<EOF
       apiVersion: k8s.cni.cncf.io/v1
       kind: NetworkAttachmentDefinition
       metadata:
         name: vm-network-101
         namespace: iaas
       spec:
         config: |
           { "cniVersion": "0.3.1", "type": "bridge", "bridge": "vlan101", ... }
       EOF

2. Create the tenant namespace
       kubectl create namespace payments
       kubectl label namespace payments dbaas.wso2.com/tenant=payments

3. Apply tenant RBAC (template)
       envsubst < hack/tenant-rbac.yaml | kubectl apply -f -
   This creates a Role binding the controller's ServiceAccount to read/write
   DBInstance/Secret/Service/etc. in the payments namespace.

4. Create the Asgardeo group (in the Asgardeo console)
   - Group name: payments-dbs
   - (Optional) admin sub-group: payments-admins

5. Update DBaaS gateway config
       # On the dbaas-gateway pods, either via ConfigMap update + rollout,
       # or via the admin API (future):
       tenants:
         - asgardeoGroup: payments-dbs
           namespace: payments
           networkRef: iaas/vm-network-101
           quotas: {...}

6. (Optional) Set DNS for the tenant
   - External-DNS automatically creates *.payments.lk.dbaas.wso2.cloud
     A-records for each DB Service.

7. Invite users to the Asgardeo group
   - Once added, users can run `dcctl login --env lk` and immediately
     create DBs in the payments tenant.
```

After step 7, tenant users are fully self-service.

---

## 7. API Specification

### 7.1 Conventions

- **Base URL:** `https://dbaas.<env>.wso2.cloud/v1`
- **Auth:** `Authorization: Bearer <asgardeo-token>` on every request.
- **Content type:** `application/json`. Field names are lowerCamelCase.
- **Idempotency:** `POST` accepts `Idempotency-Key` header. Repeats with the
  same key within 24h return the original response.
- **Pagination:** List endpoints accept `?pageSize=N&pageToken=...` and return
  `{ items, nextPageToken }`.
- **Status codes:** `2xx` success, `4xx` client error, `5xx` server error.
- **Error body:**
  ```json
  {
    "error": "InvalidParameterValue",
    "message": "class must be one of: db.t3.small, db.t3.medium, ...",
    "requestId": "01J2Z6KDNXE0R2T8R6ARNB1V8X"
  }
  ```

### 7.2 Endpoints

```
DB Instances
  POST    /v1/dbs                         Create
  GET     /v1/dbs                         List (in tenant)
  GET     /v1/dbs/{name}                  Describe
  PATCH   /v1/dbs/{name}                  Modify (resize, params, retention, ...)
  DELETE  /v1/dbs/{name}?finalSnapshot=Y  Delete (optionally take final snapshot)
  POST    /v1/dbs/{name}/start            Start a stopped DB
  POST    /v1/dbs/{name}/stop             Stop (preserve storage)
  GET     /v1/dbs/{name}/credentials      Get master secret reference + connection info
  GET     /v1/dbs/{name}/ca               Download TLS CA cert (PEM)

Snapshots
  POST    /v1/dbs/{name}/snapshots        Create a manual snapshot
  GET     /v1/dbs/{name}/snapshots        List snapshots for one DB
  GET     /v1/snapshots                   List all snapshots (in tenant)
  GET     /v1/snapshots/{name}            Describe
  DELETE  /v1/snapshots/{name}            Delete
  POST    /v1/snapshots/{name}/restore    Restore as a new DB instance

Reference data
  GET     /v1/instance-classes            List available classes (env-wide)
  GET     /v1/engine-versions             List supported PostgreSQL versions
  GET     /v1/networks                    List networks the tenant can use

Tenant
  GET     /v1/tenant                      Describe current tenant + quotas + usage

System
  GET     /healthz                        Public health
  GET     /readyz                         Public readiness
```

### 7.3 Sample: Create DB

**Request**
```http
POST /v1/dbs HTTP/1.1
Host: dbaas.lk.wso2.cloud
Authorization: Bearer eyJ...
Idempotency-Key: 01J2Z6...
Content-Type: application/json

{
  "name": "orders",
  "class": "db.t3.medium",
  "engineVersion": "16",
  "allocatedStorageGB": 50,
  "masterUsername": "orders_admin",
  "manageMasterUserPassword": true,
  "backup": {
    "retentionDays": 7,
    "preferredWindow": "02:00-03:00"
  },
  "tags": { "owner": "alice@wso2.com", "service": "orders-api" }
}
```

**Response**
```http
HTTP/1.1 202 Accepted
Content-Type: application/json

{
  "name": "orders",
  "tenant": "payments",
  "environment": "lk",
  "status": "creating",
  "class": "db.t3.medium",
  "engine": "postgres",
  "engineVersion": "16",
  "allocatedStorageGB": 50,
  "endpoint": null,
  "masterUsername": "orders_admin",
  "tls": { "caFingerprint": null },
  "backup": { "retentionDays": 7, "preferredWindow": "02:00-03:00" },
  "createdAt": "2026-05-03T08:14:22Z",
  "tags": { "owner": "alice@wso2.com", "service": "orders-api" }
}
```

### 7.4 Sample: Describe DB (after Available)

```json
{
  "name": "orders",
  "tenant": "payments",
  "environment": "lk",
  "status": "available",
  "class": "db.t3.medium",
  "engine": "postgres",
  "engineVersion": "16",
  "allocatedStorageGB": 50,
  "endpoint": {
    "host": "orders.payments.lk.dbaas.wso2.cloud",
    "port": 5432
  },
  "masterUsername": "orders_admin",
  "masterSecretRef": {
    "namespace": "payments",
    "name": "orders-master-credentials"
  },
  "tls": {
    "caFingerprint": "sha256:abc123...",
    "caDownloadUrl": "/v1/dbs/orders/ca"
  },
  "backup": {
    "retentionDays": 7,
    "preferredWindow": "02:00-03:00",
    "lastBackupAt": "2026-05-03T02:14:11Z",
    "lastBackupSize": 8388608
  },
  "metrics": {
    "prometheusUrl": "https://prom.lk.wso2.cloud/d/dbaas-orders",
    "grafanaUrl": "https://grafana.lk.wso2.cloud/d/dbaas-orders"
  },
  "createdAt": "2026-05-03T08:14:22Z",
  "modifiedAt": "2026-05-03T08:19:51Z",
  "tags": { "owner": "alice@wso2.com", "service": "orders-api" }
}
```

### 7.5 Status Values (RDS-compatible)

`creating`, `available`, `modifying`, `stopping`, `stopped`, `starting`,
`backing-up`, `restoring`, `deleting`, `failed`.

### 7.6 Standard Error Codes

| Code                       | HTTP | Meaning                                           |
|----------------------------|------|---------------------------------------------------|
| `Unauthenticated`          | 401  | Missing or invalid token                          |
| `NotAuthorized`            | 403  | Token valid but action not permitted              |
| `NotFound`                 | 404  | DB / snapshot / etc. not found in tenant          |
| `AlreadyExists`            | 409  | DB with that name already exists in tenant        |
| `InvalidParameterValue`    | 400  | Field value rejected (wrong class, bad name, ...) |
| `MissingParameter`         | 400  | Required field absent                             |
| `QuotaExceeded`            | 429  | Tenant quota would be exceeded                    |
| `DeletionProtectionActive` | 409  | DB has deletionProtection=true                    |
| `BackendUnavailable`       | 503  | Harvester / etcd / etc. transiently down          |
| `Internal`                 | 500  | Unexpected server error (always with requestId)   |

---

## 8. Network Architecture

### 8.1 v1 — Per-Tenant VLAN

```
┌─────────────────────────────────────────────────────────────────────────┐
│  Harvester cluster (env = lk)                                           │
│                                                                         │
│  Namespace iaas (admin-managed NADs):                                   │
│    - vm-network-101 (VLAN 101, 10.50.101.0/24)  ← bound to "payments"   │
│    - vm-network-102 (VLAN 102, 10.50.102.0/24)  ← bound to "orders"     │
│                                                                         │
│  Namespace payments:                                                    │
│    - DBInstance "orders"                                                │
│      → VM with NIC on iaas/vm-network-101 (e.g., 10.50.101.42)          │
│    - DBInstance "main"                                                  │
│      → VM with NIC on iaas/vm-network-101 (e.g., 10.50.101.43)          │
│  (DBs within a tenant share an L2 broadcast domain)                     │
│                                                                         │
│  Namespace orders:                                                      │
│    - DBInstance "main"                                                  │
│      → VM with NIC on iaas/vm-network-102 (e.g., 10.50.102.7)           │
│                                                                         │
│  Inter-VLAN routing controlled by FortiGate:                            │
│    - Default deny.                                                      │
│    - Per-tenant rules add allow-from-app-cluster-VLAN to tenant-VLAN.   │
└─────────────────────────────────────────────────────────────────────────┘
```

**Trade-offs accepted in v1:**
- DBs within a tenant are *not* mutually isolated (acceptable: same tenant).
- New tenant onboarding requires a one-time admin VLAN allocation.
- VLAN exhaustion is a real limit (~4000 usable IDs per fabric).

### 8.2 v2 — Per-DB Native OVN (deferred)

Once the per-tenant model has shipped, we add per-DB isolation by integrating
directly with OVN's northbound DB:

- Controller creates a logical switch per DBInstance.
- Logical router connects the switch to the tenant gateway.
- ACLs enforce that only the tenant's app-cluster CIDR can reach the DB.
- NAD generated dynamically and attached to the VM.

This replaces the static per-tenant VLAN with per-DB isolation while keeping
the same API contract — `networkRef` becomes optional and admin-controlled
per-tenant defaults take over.

---

## 9. Production Pillars

Each pillar maps to one or more delivery phases (section 10).

### 9.1 TLS to the database
- cert-manager generates per-DB CA + server cert as Kubernetes Secrets.
- Cloud-init mounts certs into PostgreSQL config (`ssl=on`,
  `ssl_cert_file`, `ssl_key_file`, `ssl_ca_file`).
- `pg_hba.conf` requires `hostssl ... scram-sha-256` for non-local connections.
- Gateway exposes `GET /v1/dbs/{name}/ca` to download the CA in PEM.

### 9.2 Backups
- pgBackRest installed in the VM via cloud-init.
- Daily full backup at `spec.preferredBackupWindow` UTC.
- WAL archive shipped continuously to S3 (PITR window = retention period).
- Manual snapshots via `POST /v1/dbs/{name}/snapshots` trigger a pgBackRest
  diff or full as configured.
- Restore creates a new DBInstance with `restoreFromSnapshot: <snapshot-name>`;
  cloud-init runs `pgbackrest restore` before starting Postgres.
- Backup S3 bucket and credentials are environment-wide; backup objects are
  prefixed `tenant=<ns>/db=<name>/...` and tenant access is enforced by
  bucket policy or signed-URL generation in the gateway.

### 9.3 Stable endpoints
- Each DBInstance gets a Kubernetes Service in the tenant namespace.
- External-DNS publishes `<dbname>.<tenant>.<env>.dbaas.wso2.cloud` as an
  A-record pointing to the VM's VLAN IP.
- VM IP changes on rebuild are masked by the Service / DNS.

### 9.4 HA Control Plane
- Gateway + controller deployed as 2+ replicas.
- Controller uses `coordination.k8s.io/Lease` leader election (already
  scaffolded — confirm enabled in main.go).
- Gateway is stateless; any replica can serve any request.
- Behind a Kubernetes Service or ingress with health checks.

### 9.5 Audit log
- Every state-changing API call is appended to a structured log
  (`{ timestamp, requestId, actor, tenant, action, resource, statusCode, ... }`).
- Logs ship to S3 (one file per hour per environment) with object lock for
  tamper resistance.
- Read endpoint for tenant admins: `GET /v1/audit?from=...&to=...`.

### 9.6 Observability
- Per-DB metrics via postgres_exporter sidecar (already partly built).
- ServiceMonitor annotation; works inside the existing Choreo-observability
  stack on downstream clusters (no kube-prometheus-stack on top — see
  observability constraint memory).
- Per-DB Grafana dashboard auto-provisioned from a template at create time.
- Controller exposes its own Prometheus metrics on `:8081`.

### 9.7 Lifecycle operations
- `PATCH /v1/dbs/{name}` accepts diffable fields:
  - `class` → resize VM (CPU/memory) without storage churn.
  - `allocatedStorageGB` → expand DataVolume (no shrink).
  - `engineVersion` → planned major-version upgrade (later).
  - `backup`, `tags`, `deletionProtection` → metadata-only updates.
- The controller reconciles `spec` changes by walking a separate
  `modifyPhase` state machine.

### 9.8 Quotas
- Enforced by the gateway against `tenants.yaml`:
  - `maxInstances` per tenant.
  - `maxStoragePerInstanceGB`.
  - `maxTotalStorageGB`.
  - `allowedClasses` (optional whitelist).
- Quota check happens before CRD creation; over-quota returns
  `429 QuotaExceeded`.

### 9.9 dcctl CLI
- Single Go binary, multi-platform builds.
- Subcommands: `login`, `logout`, `whoami`, `create-db`, `describe-db`,
  `modify-db`, `delete-db`, `list-dbs`, `start-db`, `stop-db`,
  `create-snapshot`, `list-snapshots`, `restore-snapshot`, `get-ca`,
  `get-credentials`, `tenant`, `instance-classes`.
- `--env` flag (or `DCCTL_ENV`) selects environment; per-env credentials
  cached separately.
- Generated from an OpenAPI spec (single source of truth shared with the
  gateway implementation).

---

## 10. Delivery Roadmap

Each phase is **demoable end-to-end at production quality**. No phase ships
"POC quality, harden later."

### Phase 1 — Multi-tenancy + Auth (foundation)
- Convert `DBInstance` from cluster-scoped to namespaced.
- Per-namespace RBAC template.
- Asgardeo OIDC validation in the gateway.
- `tenants.yaml` config + group→namespace→networkRef mapping.
- Quota enforcement.
- Audit log skeleton (writes to stdout, S3 wiring in Phase 4).
- Migration plan for existing dev DBs (will require destroy+recreate; tested
  on `lk-dev` first).

**Done when:** A non-admin Asgardeo user can `kubectl auth can-i create
dbinstance -n payments` succeed for their tenant and fail for others; the
gateway accepts a token and creates a DBInstance in the right namespace.

### Phase 2 — TLS + stable endpoints
- cert-manager Issuer per environment.
- Per-DB Certificate resource → Secret with CA, server cert, server key.
- Cloud-init mounts and configures Postgres for `ssl=on`.
- Service per DBInstance in the tenant namespace.
- External-DNS configured for `*.<tenant>.<env>.dbaas.wso2.cloud`.
- Endpoint in API responses uses DNS name, not IP.

**Done when:** `psql sslmode=verify-full sslrootcert=...` connects via the
DNS endpoint and verifies.

### Phase 3 — Backups + snapshot/restore
- pgBackRest in the VM image (or installed by cloud-init).
- Daily backup CronJob configured per DB.
- WAL archiving to S3.
- DBSnapshot reconciler (manual snapshots).
- Restore creates a new DBInstance with `restoreFromSnapshot` field.
- Final-snapshot-on-delete option.

**Done when:** Snapshot a populated DB → delete it → restore it → row counts
match.

### Phase 4 — Production control plane + dcctl
- Gateway HA: 2+ replicas, Service-fronted, leader-elected controller.
- Audit log → S3 with object lock.
- Versioned API (`/v1`) — make this the only supported version.
- OpenAPI spec written and versioned in repo.
- `dcctl` CLI generated from OpenAPI; published to internal release.
- Migrate from `manageMasterUserPassword`-only to also supporting
  user-supplied secrets (closes IMPROVEMENTS.md item 3).

**Done when:** A new engineer can `dcctl login → dcctl create-db → psql` in
under 10 minutes from a fresh laptop.

### Phase 5 — Lifecycle operations
- `PATCH /v1/dbs/{name}` for resize, retention changes, tag updates.
- DBParameterGroup reconciler.
- Major-version upgrade workflow.
- Engine version field actually drives Postgres install (closes
  IMPROVEMENTS.md item 4).

### Phase 6 — Native OVN (per-DB isolation)
- OVN northbound client.
- Per-DB logical switch + ACL provisioning.
- Migration path from per-tenant VLAN → per-DB OVN networks.

### Phase 7 — Web UI
- Thin React app on top of the same OpenAPI client.
- Tenant dashboard, DB list, modify forms, snapshot list, audit log viewer.

### Cross-cutting (parallel with all phases)
- Document onboarding for each new tenant in `docs/onboarding-runbook.md`.
- Disaster recovery runbook (env-wide).
- Capacity planning (VLAN allocation, S3 budgets, VM density).
- Helm chart for deploying the DBaaS stack to a new environment.

---

## 11. Decisions

| # | Topic | Decision |
|---|-------|----------|
| 1 | Asgardeo group naming convention | `<tenant>-dbs` (e.g., `asgardeo-dbs`, `wso2cloud-dbs`, `choreo-dbs`). |
| 2 | Master password rotation | Required, but **not v1**. Add in a post-Phase-5 phase using Asgardeo + a rotation reconciler. |
| 3 | Backup encryption | **Per-tenant KMS key.** Each tenant onboarding allocates a KMS key; pgBackRest config encrypts both backups and WAL using the tenant key. Audit logs use the env-wide key. |
| 4 | Cross-env DR | **Out of scope.** No cross-region replication, no cross-env restore. v1 API does not assume DR; revisit in v2 if business requires it. |
| 5 | Per-DB observability access | **Per-tenant Grafana** is sufficient. One Grafana org / folder per tenant; per-DB dashboards live inside it. No per-DB user separation. |
| 6 | API versioning policy | `/v1` ships at the end of Phase 4 (when dcctl is GA). Until then everything is `/v1alpha1` with no compatibility guarantees. |
| 7 | Custom domain endpoints | **Default v1: no custom domains.** Canonical hostname is `<dbname>.<tenant>.<env>.dbaas.wso2.cloud`. Custom domains revisited later if customer demand arises (likely post-UI). |
| 8 | Migration of existing test DBs | **No migration.** Existing `dev-test-01` and similar test DBs will be destroyed and recreated on the new namespaced API. |

---

## 12. Glossary

| Term            | Meaning                                                            |
|-----------------|--------------------------------------------------------------------|
| Environment     | One Harvester cluster + DBaaS control plane (`lk`, `eu`, ...)      |
| Tenant          | A K8s namespace within an environment, owned by an Asgardeo group  |
| DBInstance      | One PostgreSQL database (one VM today, one logical service)        |
| Snapshot        | A pgBackRest backup of a DBInstance, stored in S3                  |
| DBaaS Gateway   | The HTTP API service in front of the controller                    |
| dcctl           | The CLI built on top of the gateway's OpenAPI                      |
| Network ref     | A NAD reference (`<ns>/<name>`) the VM attaches to                 |
| Asgardeo group  | Asgardeo's identity grouping — maps to one tenant namespace        |
