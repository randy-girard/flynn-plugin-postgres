#!/bin/bash
set -euo pipefail

if [[ "${1:-}" == "api" ]]; then
  exec /bin/flynn-postgres-api
fi

if [[ "${1:-}" != "postgres" ]]; then
  exec /bin/flynn-postgres "$@"
fi

# One data directory, one process. Credentials come from the release env.
echo "flynn-plugin-postgres engine version ${ENGINE_VERSION:-16} PG_VERSION=$(cat /data/PG_VERSION 2>/dev/null || echo new)"
PG_BIN="$(ls -d /usr/lib/postgresql/*/bin | sort -V | tail -1)"
as_postgres() {
  setpriv --reuid=postgres --regid=postgres --init-groups --inh-caps=-all "$@"
}

install -d -o postgres -g postgres -m 0700 /data
if [[ ! -s /data/PG_VERSION ]]; then
  if [[ -n "${POSTGRES_PRIMARY_URL:-}" ]]; then
    # --checkpoint=fast: spread (the default) waits for the next scheduled
    # checkpoint, which on an idle/empty primary can be checkpoint_timeout
    # (5 minutes) because nothing fills WAL.
    as_postgres "${PG_BIN}/pg_basebackup" -d "${POSTGRES_PRIMARY_URL}" -D /data -Fp -Xs -R --checkpoint=fast --no-password
    chown -R postgres:postgres /data
  else
    as_postgres "${PG_BIN}/initdb" -D /data --auth-local=trust --auth-host=md5
  fi
fi

conf=/data/postgresql.conf
sed -i "s/^#listen_addresses.*/listen_addresses = '*'/" "${conf}"
sed -i "s/^listen_addresses.*/listen_addresses = '*'/" "${conf}"
sed -i "s/^#port .*/port = 5432/" "${conf}"
sed -i "s/^port =.*/port = 5432/" "${conf}"
grep -q "^listen_addresses" "${conf}" || echo "listen_addresses = '*'" >> "${conf}"
grep -q "^port " "${conf}" || echo "port = 5432" >> "${conf}"
grep -q "^unix_socket_directories" "${conf}" || echo "unix_socket_directories = '/tmp'" >> "${conf}"
grep -q "^wal_level" "${conf}" || echo "wal_level = replica" >> "${conf}"
grep -q "^max_wal_senders" "${conf}" || echo "max_wal_senders = 10" >> "${conf}"
grep -q "^max_replication_slots" "${conf}" || echo "max_replication_slots = 10" >> "${conf}"
if ! grep -q "0.0.0.0/0" /data/pg_hba.conf; then
  printf '%s\n' "host all all 0.0.0.0/0 md5" "host all all ::/0 md5" >> /data/pg_hba.conf
fi
if ! grep -q "host replication" /data/pg_hba.conf; then
  printf '%s\n' "host replication all 0.0.0.0/0 md5" "host replication all ::/0 md5" >> /data/pg_hba.conf
fi
# sslmode=require encrypts but does not verify the CA. A local cert is enough.
if [[ ! -s /data/server.crt || ! -s /data/server.key ]]; then
  openssl req -new -x509 -days 3650 -nodes -text \
    -out /data/server.crt -keyout /data/server.key \
    -subj "/CN=postgres"
  chown postgres:postgres /data/server.crt /data/server.key
  chmod 600 /data/server.key
fi
grep -q "^ssl " "${conf}" || echo "ssl = on" >> "${conf}"
grep -q "^ssl_cert_file" "${conf}" || echo "ssl_cert_file = '/data/server.crt'" >> "${conf}"
grep -q "^ssl_key_file" "${conf}" || echo "ssl_key_file = '/data/server.key'" >> "${conf}"
# timescaledb must stay first. pg_stat_statements feeds slow-query metrics.
if grep -q "^shared_preload_libraries" "${conf}"; then
  if ! grep -q "pg_stat_statements" "${conf}"; then
    sed -i "s/^shared_preload_libraries = '\\([^']*\\)'/shared_preload_libraries = '\\1,pg_stat_statements'/" "${conf}"
  fi
else
  echo "shared_preload_libraries = 'timescaledb,pg_stat_statements'" >> "${conf}"
fi
grep -q "^pg_stat_statements.track" "${conf}" || echo "pg_stat_statements.track = all" >> "${conf}"
grep -q "^timescaledb.max_background_workers" "${conf}" || echo "timescaledb.max_background_workers = 8" >> "${conf}"

if [[ ! -f /data/standby.signal && ! -f /data/.flynn-bootstrapped ]]; then
  user="${POSTGRES_USER:?POSTGRES_USER is required}"
  pass="${POSTGRES_PASSWORD:?POSTGRES_PASSWORD is required}"
  db="${POSTGRES_DB:?POSTGRES_DB is required}"
  if ! [[ "${user}" =~ ^[a-zA-Z0-9_]+$ && "${db}" =~ ^[a-zA-Z0-9_]+$ && "${pass}" =~ ^[a-zA-Z0-9_]+$ ]]; then
    echo "postgres credentials must be alphanumeric" >&2
    exit 1
  fi
  as_postgres "${PG_BIN}/pg_ctl" -D /data -w start
  as_postgres "${PG_BIN}/psql" -h /tmp -v ON_ERROR_STOP=1 -d postgres <<SQL
CREATE ROLE ${user} LOGIN PASSWORD '${pass}' CONNECTION LIMIT 20 NOSUPERUSER CREATEDB CREATEROLE REPLICATION;
GRANT pg_monitor TO ${user};
CREATE DATABASE ${db} OWNER ${user};
REVOKE CONNECT ON DATABASE postgres FROM PUBLIC;
REVOKE CONNECT ON DATABASE template1 FROM PUBLIC;
REVOKE CONNECT ON DATABASE ${db} FROM PUBLIC;
GRANT CONNECT ON DATABASE ${db} TO ${user};
SQL
  as_postgres "${PG_BIN}/psql" -h /tmp -v ON_ERROR_STOP=1 -d "${db}" <<'SQL'
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
SQL
  as_postgres "${PG_BIN}/pg_ctl" -D /data -m fast -w stop
  touch /data/.flynn-bootstrapped
  chown postgres:postgres /data/.flynn-bootstrapped
fi

# The Go process stays up so discoverd keeps the registration. exec cannot
# call a shell function, and a bare postgres process never registers.
export POSTGRES_BIN="${PG_BIN}/postgres"
exec /bin/flynn-postgres serve
