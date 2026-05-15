package harvester

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// resolvedS3Config carries the actual S3 credentials resolved from the K8s
// Secret at provisioning time. The controller reads the Secret before calling
// buildCloudInit so credentials are embedded directly into pgbackrest.conf
// on the VM — the VM never talks to Kubernetes.
type resolvedS3Config struct {
	Endpoint  string
	Bucket    string
	Region    string
	Path      string // per-instance prefix inside the bucket, e.g. "/ssl-test-01"
	AccessKey string
	SecretKey string
}

func buildCloudInit(p VMCreateParams, adminPw, replPw, exporterPw, luksKey string, tls *TLSBundle, sshKey *SSHKeyPair, s3 *resolvedS3Config) string {
	pgbrConf := ""

	// --- Cron file write_files entry ---
	// Runs as the postgres OS user. Uses the real instance ID directly (not a
	// shell variable) because cron does not source bootstrap.env.
	// Schedule is derived from spec.preferredBackupWindow, e.g. "02:00-03:00"
	// → incremental at 02:00, full at 01:00.
	pgbrCron := ""
	if s3 != nil {
		incrHour, fullHour := backupWindowToCron(p.BackupWindow)
		pgbrCron = fmt.Sprintf(`
  - path: /etc/cron.d/pgbackrest
    permissions: "0644"
    content: |
      SHELL=/bin/bash
      PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
      0 %d * * * postgres pgbackrest --stanza=%s backup --type=incr 2>&1 | logger -t pgbackrest
      0 %d * * 0 postgres pgbackrest --stanza=%s backup --type=full 2>&1 | logger -t pgbackrest`,
			incrHour, p.ID,
			fullHour, p.ID,
		)
	}

	// --- pgBackRest section inside bootstrap.sh ---
	// pgbackrest.conf is base64-encoded at provisioning time and written with a
	// single echo|base64 -d line — avoids heredoc body at column 0 breaking the
	// YAML literal block. pg1-path uses PGVERPLACEHOLDER replaced by sed at
	// runtime once ${PG_VER} is known. %%p in fmt.Sprintf → %p in the output
	// (the pgBackRest WAL file path placeholder in archive_command).
	pgbrBootstrap := ""
	if s3 != nil {
		confContent := fmt.Sprintf(
			"[global]\nrepo1-type=s3\nrepo1-s3-endpoint=%s\nrepo1-s3-bucket=%s\nrepo1-s3-region=%s\nrepo1-s3-key=%s\nrepo1-s3-key-secret=%s\nrepo1-s3-uri-style=path\nrepo1-path=%s\nrepo1-retention-full=2\nrepo1-retention-diff=7\nlog-level-console=info\nlog-level-file=detail\n\n[%s]\npg1-path=PGVERPLACEHOLDER\npg1-port=%d\n",
			s3.Endpoint, s3.Bucket, s3.Region,
			s3.AccessKey, s3.SecretKey,
			s3.Path,
			p.ID, p.Port,
		)
		confB64 := base64.StdEncoding.EncodeToString([]byte(confContent))
		pgbrBootstrap = fmt.Sprintf(`
      # --- pgBackRest: write config now that postgres user exists ---
      mkdir -p /etc/pgbackrest
      echo %s | base64 -d > /etc/pgbackrest/pgbackrest.conf
      sed -i "s|PGVERPLACEHOLDER|/var/lib/postgresql/${PG_VER}/main|" /etc/pgbackrest/pgbackrest.conf
      chmod 0640 /etc/pgbackrest/pgbackrest.conf
      chown root:postgres /etc/pgbackrest/pgbackrest.conf

      # --- pgBackRest WAL archiving ---
      sed -i "s/^#\?archive_mode.*/archive_mode = on/" "${PG_CONF}/postgresql.conf"
      sed -i "s/^#\?wal_level.*/wal_level = replica/" "${PG_CONF}/postgresql.conf"
      sed -i "s|^#\?archive_command.*|archive_command = 'pgbackrest --stanza=${INSTANCE_ID} archive-push %%p'|" "${PG_CONF}/postgresql.conf"
      systemctl restart postgresql

      # Register stanza and take initial full backup
      sudo -u postgres pgbackrest --stanza=${INSTANCE_ID} stanza-create
      sudo -u postgres pgbackrest --stanza=${INSTANCE_ID} backup --type=full`,
			confB64,
		)
	}

	// --- SSH authorized_keys ---
	// cloud-init's ssh_authorized_keys directive is the canonical way to inject
	// keys. It handles .ssh directory creation and permissions automatically.
	sshAuthKeys := ""
	if sshKey != nil {
		sshAuthKeys = fmt.Sprintf("ssh_authorized_keys:\n  - %s\n", strings.TrimSpace(sshKey.AuthorizedKeyLine))
	}

	// --- Optional VM console password (dev/debug only) ---
	vmUserBlock := ""
	if p.VMPassword != "" {
		vmUserBlock = fmt.Sprintf("password: %s\nchpasswd:\n  expire: false\nssh_pwauth: true\n", p.VMPassword)
	}

	// --- TLS certs (base64 for cloud-init write_files b64 encoding) ---
	caCertB64 := base64.StdEncoding.EncodeToString([]byte(tls.CACertPEM))
	serverCertB64 := base64.StdEncoding.EncodeToString([]byte(tls.ServerCertPEM))
	serverKeyB64 := base64.StdEncoding.EncodeToString([]byte(tls.ServerKeyPEM))

	// --- Consumer network netplan stanza (optional third NIC) ---
	consumerNetplan := ""
	if p.ConsumerNetwork != "" {
		consumerNetplan = `
          enp3s0:
            dhcp4: true
            dhcp4-overrides:
              use-routes: true
              route-metric: 300`
	}

	// --- Backup block in bootstrap.env ---
	backupEnv := "# backups disabled"
	if s3 != nil {
		backupEnv = fmt.Sprintf("BACKUP_ENABLED=true\n      S3_BUCKET=%s\n      S3_PATH=%s", s3.Bucket, s3.Path)
	}

	return fmt.Sprintf(`#cloud-config
%s%spackage_update: true
packages:
  - postgresql
  - postgresql-contrib
  - pgbackrest
  - jq
  - qemu-guest-agent
write_files:
  - path: /etc/dbaas/bootstrap.env
    permissions: "0600"
    content: |
      INSTANCE_ID=%s
      DB_NAME=%s
      DB_PORT=%d
      MASTER_USER=%s
      MASTER_PASSWORD=%s
      REPL_PASSWORD=%s
      EXPORTER_PASSWORD=%s
      MAX_CONNECTIONS=%d
      LUKS_KEY=%s
      %s
  - path: /etc/netplan/60-vpc-net.yaml
    permissions: "0600"
    content: |
      network:
        version: 2
        ethernets:
          enp2s0:
            dhcp4: true
            dhcp4-overrides:
              use-routes: true
              route-metric: 200%s
  - path: /etc/ssl/certs/pg-ca.crt
    encoding: b64
    permissions: "0644"
    content: %s
  - path: /etc/ssl/certs/pg-server.crt
    encoding: b64
    permissions: "0644"
    content: %s
  - path: /etc/ssl/private/pg-server.key
    encoding: b64
    permissions: "0600"
    content: %s
  - path: /etc/dbaas/bootstrap.sh
    permissions: "0700"
    content: |
      #!/bin/bash
      set -euo pipefail
      source /etc/dbaas/bootstrap.env

      PG_VER=$(pg_lsclusters -h | awk '{print $1}' | head -1)
      PG_CONF="/etc/postgresql/${PG_VER}/main"

      # Fix server key ownership now that postgres user exists
      chown postgres:postgres /etc/ssl/private/pg-server.key

      # Listen on all interfaces and set port / max_connections
      sed -i "s/^#\?listen_addresses.*/listen_addresses = '*'/" "${PG_CONF}/postgresql.conf"
      sed -i "s/^#\?port.*/port = ${DB_PORT}/" "${PG_CONF}/postgresql.conf"
      sed -i "s/^#\?max_connections.*/max_connections = ${MAX_CONNECTIONS}/" "${PG_CONF}/postgresql.conf"

      # Enable SSL
      sed -i "s/^#\?ssl\b.*/ssl = on/" "${PG_CONF}/postgresql.conf"
      sed -i "s|^#\?ssl_cert_file.*|ssl_cert_file = '/etc/ssl/certs/pg-server.crt'|" "${PG_CONF}/postgresql.conf"
      sed -i "s|^#\?ssl_key_file.*|ssl_key_file = '/etc/ssl/private/pg-server.key'|" "${PG_CONF}/postgresql.conf"
      sed -i "s|^#\?ssl_ca_file.*|ssl_ca_file = '/etc/ssl/certs/pg-ca.crt'|" "${PG_CONF}/postgresql.conf"

      # SSL-only remote connections — plain-text is rejected
      echo "hostssl all all 0.0.0.0/0 scram-sha-256" >> "${PG_CONF}/pg_hba.conf"
      echo "hostssl replication all 0.0.0.0/0 scram-sha-256" >> "${PG_CONF}/pg_hba.conf"

      systemctl restart postgresql

      # Create admin user (CREATEDB CREATEROLE — not SUPERUSER) and initial database
      sudo -u postgres psql -p "${DB_PORT}" <<EOSQL
      DO \$\$
      BEGIN
        IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '${MASTER_USER}') THEN
          CREATE ROLE ${MASTER_USER} LOGIN CREATEDB CREATEROLE PASSWORD '${MASTER_PASSWORD}';
        END IF;
      END \$\$;
      SELECT 'CREATE DATABASE ${DB_NAME} OWNER ${MASTER_USER}'
        WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = '${DB_NAME}')\gexec
      EOSQL
%s%s%s
runcmd:
  - systemctl enable --now qemu-guest-agent
  - netplan apply
  - mkdir -p /var/lib/dbaas
  - chown postgres:postgres /var/lib/dbaas
  - /etc/dbaas/bootstrap.sh
final_message: "DBaaS bootstrap complete for %s"
`,
		vmUserBlock,
		sshAuthKeys,
		p.ID,
		p.DBName,
		p.Port,
		p.MasterUser,
		adminPw,
		replPw,
		exporterPw,
		p.MaxConnections,
		luksKey,
		backupEnv,
		consumerNetplan,
		caCertB64,
		serverCertB64,
		serverKeyB64,
		pgbrBootstrap,
		pgbrConf,
		pgbrCron,
		p.ID,
	)
}

// backupWindowToCron converts "02:00-03:00" → incremental hour 2, full hour 1.
// Falls back to 02:00 / 01:00 if empty or unparseable.
func backupWindowToCron(window string) (incrHour, fullHour int) {
	incrHour, fullHour = 2, 1
	if window == "" {
		return
	}
	var h, m int
	if _, err := fmt.Sscanf(strings.SplitN(window, "-", 2)[0], "%d:%d", &h, &m); err == nil {
		incrHour = h
		fullHour = (h + 23) % 24
	}
	return
}
