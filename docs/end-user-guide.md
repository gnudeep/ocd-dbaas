# DBaaS End User Guide

This guide covers everything a tenant user needs to provision a PostgreSQL database, connect to it, retrieve credentials and certificates, configure backups, and take on-demand snapshots.

---

## 1. What Your Admin Gives You

Before you can create any databases, the platform team provisions a **project** for your team. A project is a quota-bounded, network-isolated space that contains your namespace and the VM network your databases will attach to. You will receive:

| Item | Example | What it is |
|---|---|---|
| **Rancher URL** | `https://rancher-lk-dev.wso2.com` | Web UI and kubectl API entry point |
| **SSO login** | Your corporate/Asgardeo account | How you authenticate — no separate password |
| **Project name** | `db-test` | Your Rancher project — the RBAC boundary for your team |
| **Namespace** | `db-test` | K8s namespace inside the project where you create DBs |
| **Network reference** | `iaas/vm-network-002` | The VLAN pre-configured for your project — use exactly this value as `networkRef` in your DBInstance spec |
| **S3 secret name** *(if backups enabled)* | `pg-s3-backup-creds` | Name of the pre-created Secret in your namespace — required if you want S3 backups. The admin creates this; you reference it by name. |

You can only create and manage resources inside your project's namespace. Other teams' projects and namespaces are not visible to you.

---

## 2. Set Up kubectl Access

You access the platform through Rancher using your SSO account. Rancher acts as the authentication proxy — no static tokens or service accounts needed.

**Step 1 — Log in to Rancher**

Open the Rancher URL in your browser and sign in with your SSO account. You will see the cluster your project lives in on the home page.

**Step 2 — Download your kubeconfig**

1. Click on the cluster name on the home page
2. In the top-right of the cluster dashboard click **Download KubeConfig**
3. Save the file — e.g. `~/dbaas-kubeconfig.yaml`

The kubeconfig uses a token tied to your SSO session. Download a fresh one if it expires.

**Step 3 — Point kubectl at it**

```bash
export KUBECONFIG=~/dbaas-kubeconfig.yaml

# Verify your identity
kubectl auth whoami
# Should show your SSO email under Extra: username

# Confirm access to your namespace
kubectl get dbinstances -n <your-namespace>
```

> Trying to access any namespace outside your project returns `Forbidden` — this is enforced by Kubernetes RBAC, not just the UI.

---

## 3. Provision a Database

Create a `DBInstance` resource in your namespace. The controller provisions a PostgreSQL VM on Harvester, configures SSL, creates your admin user, and makes the endpoint available — all automatically.

### Minimal example (no backups)

```yaml
apiVersion: dbaas.wso2.com/v1alpha1
kind: DBInstance
metadata:
  name: my-db
  namespace: <your-namespace>
spec:
  dbInstanceClass: db.t3.small
  dbName: myapp
  masterUsername: dbadmin
  manageMasterUserPassword: true
  allocatedStorage: 20
  networkRef: iaas/vm-network-001
  deletionProtection: true
  running: true
```

```bash
kubectl apply -f my-db.yaml
```

### Full example (with backups)

```yaml
apiVersion: dbaas.wso2.com/v1alpha1
kind: DBInstance
metadata:
  name: my-db
  namespace: <your-namespace>
spec:
  dbInstanceClass: db.t3.medium
  dbName: myapp
  port: 5432
  masterUsername: dbadmin
  manageMasterUserPassword: true
  allocatedStorage: 50
  storageType: longhorn
  networkRef: iaas/vm-network-001
  deletionProtection: true
  running: true
  preferredBackupWindow: "02:00-03:00"
  s3BackupConfig:
    endpoint: s3.ap-southeast-1.amazonaws.com
    bucket: <your-s3-bucket>
    region: ap-southeast-1
    secretRef: pg-s3-backup-creds   # created by your admin
```

### Spec field reference

| Field | Default | Description |
|---|---|---|
| `dbInstanceClass` | required | VM size — see class table below |
| `dbName` | instance name | Initial database to create |
| `port` | `5432` | PostgreSQL port |
| `masterUsername` | `dbadmin` | Admin user name |
| `manageMasterUserPassword` | `false` | Auto-generate password and store in a Secret |
| `allocatedStorage` | required | Disk size in GiB |
| `storageType` | `longhorn` | Longhorn storage class |
| `networkRef` | required | Harvester NAD to attach the VM to |
| `deletionProtection` | `false` | Prevent accidental deletion |
| `running` | `true` | Power state — set to `false` to stop without deleting |
| `preferredBackupWindow` | none | UTC window for scheduled backups e.g. `"02:00-03:00"` |
| `s3BackupConfig` | none | S3 target for pgBackRest backups |

### Instance class reference

| Class | vCPU | RAM | Max Connections |
|---|---|---|---|
| `db.t3.micro` | 1 | 1 GB | 50 |
| `db.t3.small` | 1 | 2 GB | 100 |
| `db.t3.medium` | 2 | 4 GB | 150 |
| `db.t3.large` | 2 | 8 GB | 200 |
| `db.m5.large` | 2 | 8 GB | 200 |
| `db.m5.xlarge` | 4 | 16 GB | 400 |
| `db.r5.large` | 2 | 16 GB | 300 |

---

## 4. Watch Provisioning Progress

```bash
kubectl get dbinstance -n <your-namespace> my-db -w
```

Provisioning takes 5–10 minutes. The `PHASE` column progresses through:

```
creating → available
```

For detailed step-by-step progress:

```bash
kubectl get dbinstance -n <your-namespace> my-db \
  -o jsonpath='{.status.provisioningPhase}'
```

Provisioning phases in order:
1. `Pending` — CR created, reconciler starting
2. `StorageProvisioned` — DataVolume created
3. `VMCreated` — VM is booting
4. `WaitingForCloudInit` — cloud-init running inside the VM (PostgreSQL install, SSL config, user creation)
5. `DatabaseReady` — PostgreSQL is accepting connections
6. `MonitoringDeployed` — Grafana/Prometheus registered
7. `Available` — fully ready

---

## 5. Get the Connection Endpoint

```bash
kubectl get dbinstance -n <your-namespace> my-db \
  -o jsonpath='{.status.endpoint}'
```

Example output:
```json
{
  "address": "172.22.100.8",
  "port": 5432,
  "jdbcUrl": "jdbc:postgresql://172.22.100.8:5432/myapp?ssl=true&sslmode=verify-ca"
}
```

---

## 6. Retrieve Credentials

The admin password is stored in a Kubernetes Secret created automatically when `manageMasterUserPassword: true`.

```bash
# Find the secret name
kubectl get dbinstance -n <your-namespace> my-db \
  -o jsonpath='{.status.masterUserSecret.name}'

# Retrieve the password
kubectl get secret -n <your-namespace> <secret-name> \
  -o jsonpath='{.data.password}' | base64 -d
```

The Secret contains:

| Key | Description |
|---|---|
| `password` | Admin user password |
| `replication_password` | Replication user password (internal) |
| `exporter_password` | Prometheus exporter password (internal) |
| `ca_cert` | CA certificate PEM |
| `ca_key` | CA private key PEM |
| `ssh_private_key` | Ed25519 private key for controller SSH access (internal) |

---

## 7. Retrieve the CA Certificate for SSL Verification

All connections are SSL-only. The DB VM uses a self-signed CA. Extract the CA cert to verify the server certificate on the client side.

```bash
# Save to a file
kubectl get dbinstance -n <your-namespace> my-db \
  -o jsonpath='{.status.caCertPem}' > pg-ca.crt
```

Or from the credentials Secret:

```bash
kubectl get secret -n <your-namespace> <secret-name> \
  -o jsonpath='{.data.ca_cert}' | base64 -d > pg-ca.crt
```

---

## 8. Connect to the Database

### psql (with SSL verification)

```bash
DB_HOST=$(kubectl get dbinstance -n <your-namespace> my-db \
  -o jsonpath='{.status.endpoint.address}')
DB_PASS=$(kubectl get secret -n <your-namespace> <secret-name> \
  -o jsonpath='{.data.password}' | base64 -d)

psql "host=$DB_HOST port=5432 dbname=myapp user=dbadmin \
  sslmode=verify-ca sslrootcert=pg-ca.crt" \
  -W
```

Enter the password when prompted.

### Verify SSL is active

```sql
SELECT ssl_is_used();
-- Returns: t
```

### JDBC connection string

Use the pre-built JDBC URL from the status:

```bash
kubectl get dbinstance -n <your-namespace> my-db \
  -o jsonpath='{.status.endpoint.jdbcUrl}'
```

Example: `jdbc:postgresql://172.22.100.8:5432/myapp?ssl=true&sslmode=verify-ca`

For full certificate verification in JDBC, add `sslrootcert=/path/to/pg-ca.crt`.

---

## 9. Connect from Application Pods

To let pods in a different namespace reach the DB without embedding the VM IP directly, create a Kubernetes Service with a manual Endpoints entry. This gives you a stable DNS name inside the cluster.

Create `db-access.yaml`:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: my-db
spec:
  type: ClusterIP
  ports:
  - name: postgres
    port: 5432
    targetPort: 5432
---
apiVersion: v1
kind: Endpoints
metadata:
  name: my-db
subsets:
- addresses:
  - ip: 172.22.100.8   # replace with your DB endpoint address
  ports:
  - name: postgres
    port: 5432
---
apiVersion: v1
kind: Secret
metadata:
  name: my-db-credentials
type: Opaque
stringData:
  host: "my-db"
  port: "5432"
  dbname: "myapp"
  username: "dbadmin"
  password: "<password from step 4>"
  jdbc-url: "jdbc:postgresql://my-db:5432/myapp?sslmode=require"
```

```bash
kubectl apply -n <app-namespace> -f db-access.yaml
```

Pods in `<app-namespace>` can now connect to `my-db:5432` using normal DNS.

---

## 10. Configure Automated Backups

Backups are **optional**. If you do not include `s3BackupConfig` in your spec, the database is provisioned without any backup configuration and WAL archiving is not enabled. You can add a minimal DBInstance with no backups:

```yaml
spec:
  dbInstanceClass: db.t3.small
  dbName: myapp
  allocatedStorage: 20
  networkRef: iaas/vm-network-001
  manageMasterUserPassword: true
```

To enable backups, two things must be in place **before** you apply the DBInstance:

1. **Your admin must create an S3 credentials Secret in your namespace.** Ask your admin to create it — you cannot create it yourself as it contains IAM keys. The Secret name must match what you put in `secretRef`.
2. **The Secret must exist at provisioning time.** The controller reads it when creating the VM. If it is missing, provisioning fails at the VM creation phase with an error visible in `status.message`. The instance will be stuck in `failed` phase and must be deleted and recreated after the secret is added.

```yaml
spec:
  preferredBackupWindow: "02:00-03:00"
  s3BackupConfig:
    endpoint: s3.ap-southeast-1.amazonaws.com
    bucket: <your-s3-bucket>
    region: ap-southeast-1
    secretRef: pg-s3-backup-creds   # must exist in your namespace before apply
```

Check whether the secret exists before applying:

```bash
kubectl get secret pg-s3-backup-creds -n <your-namespace>
```

If it is missing, contact your admin. Do not apply the DBInstance until it exists.

When `s3BackupConfig` is present and the secret exists, the controller:
- Reads the S3 credentials from the secret at provisioning time and bakes them into the VM — the VM never contacts Kubernetes
- Configures pgBackRest with the S3 target inside the VM
- Enables WAL archiving so every committed transaction is continuously shipped to S3
- Takes an initial full backup as part of provisioning
- Schedules automated backups via cron:
  - **Incremental** — daily at the start of your `preferredBackupWindow`
  - **Full** — weekly (Sundays), one hour before the window start

### Verify backup configuration on the VM

If you have VM console access, verify backups are working:

```bash
# Check pgBackRest sees all backups
sudo -u postgres pgbackrest --stanza=<instance-name> info

# Verify WAL archiving and S3 connectivity are healthy
sudo -u postgres pgbackrest --stanza=<instance-name> check

# View the backup schedule
cat /etc/cron.d/pgbackrest
```

### S3 path layout

All backups for an instance are stored under:
```
s3://<bucket>/<instance-name>/
  archive/   ← continuous WAL stream (enables PITR)
  backup/    ← base backups (full, incremental)
```

---

## 11. Take an On-Demand Backup (DBSnapshot)

Create a `DBSnapshot` to trigger an immediate backup outside the scheduled window.

```yaml
apiVersion: dbaas.wso2.com/v1alpha1
kind: DBSnapshot
metadata:
  name: my-db-snap-before-migration
  namespace: <your-namespace>
spec:
  dbInstanceRef: my-db
  snapshotType: full   # full, diff, or incr
```

```bash
kubectl apply -f snapshot.yaml
kubectl get dbsnapshot -n <your-namespace> my-db-snap-before-migration -w
```

Watch the `PHASE` column:
- `creating` — backup is running on the VM
- `available` — backup completed and is in S3
- `failed` — check `.status.message` for the pgBackRest error output

Check the result:

```bash
kubectl describe dbsnapshot -n <your-namespace> my-db-snap-before-migration
```

The status shows:
- `phase` — current state
- `s3Path` — path prefix in S3 where the backup was written
- `done` — timestamp when the backup completed
- `message` — pgBackRest output (useful for debugging failures)

---

## 12. Stop and Start the Database

Stop (preserves storage, shuts down VM):

```bash
kubectl patch dbinstance -n <your-namespace> my-db \
  --type=merge -p '{"spec":{"running":false}}'
```

Start:

```bash
kubectl patch dbinstance -n <your-namespace> my-db \
  --type=merge -p '{"spec":{"running":true}}'
```

---

## 13. Delete a Database

Deletion is blocked if `deletionProtection: true`. To delete:

```bash
# Disable protection first
kubectl patch dbinstance -n <your-namespace> my-db \
  --type=merge -p '{"spec":{"deletionProtection":false}}'

# Delete — this removes the VM, volume, and all Harvester resources
kubectl delete dbinstance -n <your-namespace> my-db
```

Backups in S3 are **not** deleted automatically. Manage S3 lifecycle policies separately.

---

## 14. Monitoring

If monitoring is configured by the admin, the status contains a Grafana URL:

```bash
kubectl get dbinstance -n <your-namespace> my-db \
  -o jsonpath='{.status.grafanaUrl}'
```

Open the URL in your browser to see per-instance dashboards showing CPU, memory, connections, WAL activity, and replication lag.

---

## 15. Troubleshooting

### DB not becoming available after 10 minutes

```bash
kubectl describe dbinstance -n <your-namespace> my-db
```

Check `.status.message` and `.status.provisioningPhase` for where it stalled.

### Connection refused / SSL error

- Confirm the endpoint is reachable from your client network
- Ensure you are using `sslmode=verify-ca` or `sslmode=require` — plain-text connections are rejected
- Confirm you are using the CA cert from `.status.caCertPem` for `verify-ca` mode

### Snapshot failed

```bash
kubectl describe dbsnapshot -n <your-namespace> <snapshot-name>
```

The `.status.message` field contains the full pgBackRest error output. Common causes:
- S3 credentials expired or incorrect
- VM not reachable (check if `running: true`)
- Stanza not yet initialised (instance still provisioning)
