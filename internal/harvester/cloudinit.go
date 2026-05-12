package harvester

import (
	"encoding/base64"
	"fmt"
)

func buildCloudInit(p VMCreateParams, adminPw, replPw, exporterPw, luksKey string, tls *TLSBundle) string {
	backupConfig := "# backups disabled"
	if p.BackupEnabled && p.S3Config != nil {
		backupConfig = fmt.Sprintf(
			"S3_ENDPOINT=%s\n      S3_BUCKET=%s\n      S3_REGION=%s\n      S3_SECRET_REF=%s",
			p.S3Config.Endpoint,
			p.S3Config.Bucket,
			p.S3Config.Region,
			p.S3Config.SecretRef,
		)
	}

	consumerNetplan := ""
	if p.ConsumerNetwork != "" {
		consumerNetplan = `
          enp3s0:
            dhcp4: true
            dhcp4-overrides:
              use-routes: true
              route-metric: 300`
	}

	vmUserBlock := ""
	if p.VMPassword != "" {
		vmUserBlock = fmt.Sprintf(`password: %s
chpasswd:
  expire: false
ssh_pwauth: true
`, p.VMPassword)
	}

	caCertB64 := base64.StdEncoding.EncodeToString([]byte(tls.CACertPEM))
	serverCertB64 := base64.StdEncoding.EncodeToString([]byte(tls.ServerCertPEM))
	serverKeyB64 := base64.StdEncoding.EncodeToString([]byte(tls.ServerKeyPEM))

	return fmt.Sprintf(`#cloud-config
%spackage_update: true
packages:
  - postgresql
  - postgresql-contrib
  - cryptsetup-bin
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
      LUKS_DEV="/dev/vdb"
      LUKS_NAME="pgdata"
      PG_DATA_MOUNT="/mnt/pgdata"

      # Stop the auto-created cluster apt installed; we use a different data dir.
      systemctl stop postgresql || true
      systemctl disable postgresql || true

      # Format LUKS on first boot. Subsequent boots (after a patch that replaced
      # the OS disk) see an existing LUKS container and skip the format.
      if ! cryptsetup isLuks "${LUKS_DEV}" >/dev/null 2>&1; then
        printf '%%s' "${LUKS_KEY}" | cryptsetup -q luksFormat "${LUKS_DEV}" -
      fi
      printf '%%s' "${LUKS_KEY}" | cryptsetup -q luksOpen "${LUKS_DEV}" "${LUKS_NAME}" -

      # Create filesystem only if the volume is blank (first boot).
      if ! blkid "/dev/mapper/${LUKS_NAME}" >/dev/null 2>&1; then
        mkfs.ext4 -F "/dev/mapper/${LUKS_NAME}"
      fi

      mkdir -p "${PG_DATA_MOUNT}"
      mount "/dev/mapper/${LUKS_NAME}" "${PG_DATA_MOUNT}"

      # First-boot data init: seed the encrypted volume with the cluster files
      # apt-install created at /var/lib/postgresql/<ver>/main. On re-patched
      # boots the PG_VERSION file already exists and we skip this step.
      FIRST_BOOT=0
      if [ ! -f "${PG_DATA_MOUNT}/PG_VERSION" ]; then
        FIRST_BOOT=1
        cp -a "/var/lib/postgresql/${PG_VER}/main/." "${PG_DATA_MOUNT}/"
        chown -R postgres:postgres "${PG_DATA_MOUNT}"
        chmod 0700 "${PG_DATA_MOUNT}"
      fi

      chown postgres:postgres /etc/ssl/private/pg-server.key

      # Point the cluster at the mounted encrypted volume.
      sed -i "s|^#\?data_directory.*|data_directory = '${PG_DATA_MOUNT}'|" "${PG_CONF}/postgresql.conf"
      sed -i "s/^#\?listen_addresses.*/listen_addresses = '*'/" "${PG_CONF}/postgresql.conf"
      sed -i "s/^#\?port.*/port = ${DB_PORT}/" "${PG_CONF}/postgresql.conf"
      sed -i "s/^#\?max_connections.*/max_connections = ${MAX_CONNECTIONS}/" "${PG_CONF}/postgresql.conf"

      sed -i "s/^#\?ssl\b.*/ssl = on/" "${PG_CONF}/postgresql.conf"
      sed -i "s|^#\?ssl_cert_file.*|ssl_cert_file = '/etc/ssl/certs/pg-server.crt'|" "${PG_CONF}/postgresql.conf"
      sed -i "s|^#\?ssl_key_file.*|ssl_key_file = '/etc/ssl/private/pg-server.key'|" "${PG_CONF}/postgresql.conf"
      sed -i "s|^#\?ssl_ca_file.*|ssl_ca_file = '/etc/ssl/certs/pg-ca.crt'|" "${PG_CONF}/postgresql.conf"

      echo "hostssl all all 0.0.0.0/0 scram-sha-256" >> "${PG_CONF}/pg_hba.conf"
      echo "hostssl replication all 0.0.0.0/0 scram-sha-256" >> "${PG_CONF}/pg_hba.conf"

      systemctl enable postgresql
      systemctl start postgresql

      # Role/database creation only on first boot. On re-patched boots the
      # role and database already live in the encrypted volume.
      if [ "${FIRST_BOOT}" = "1" ]; then
        sudo -u postgres psql -p "${DB_PORT}" <<EOSQL
      DO \$\$
      BEGIN
        IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '${MASTER_USER}') THEN
          CREATE ROLE ${MASTER_USER} LOGIN SUPERUSER PASSWORD '${MASTER_PASSWORD}';
        END IF;
      END \$\$;
      SELECT 'CREATE DATABASE ${DB_NAME} OWNER ${MASTER_USER}'
        WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = '${DB_NAME}')\gexec
      EOSQL
      fi
runcmd:
  - systemctl enable --now qemu-guest-agent
  - netplan apply
  - /etc/dbaas/bootstrap.sh
final_message: "DBaaS bootstrap complete for %s"
`,
		vmUserBlock,
		p.ID,
		p.DBName,
		p.Port,
		p.MasterUser,
		adminPw,
		replPw,
		exporterPw,
		p.MaxConnections,
		luksKey,
		backupConfig,
		consumerNetplan,
		caCertB64,
		serverCertB64,
		serverKeyB64,
		p.ID,
	)
}
