#!/bin/bash
set -euo pipefail

if [[ "${1:-}" == "api" ]]; then
  exec /bin/flynn-postgres-api
fi

if [[ "${1:-}" != "postgres" ]]; then
  exec /bin/flynn-postgres "$@"
fi

# One data directory, one process. Credentials come from the release env.
PG_BIN="$(ls -d /usr/lib/postgresql/*/bin | sort -V | tail -1)"
as_postgres() {
  setpriv --reuid=postgres --regid=postgres --init-groups --inh-caps=-all "$@"
}

install -d -o postgres -g postgres -m 0700 /data
if [[ ! -s /data/PG_VERSION ]]; then
  as_postgres "${PG_BIN}/initdb" -D /data --auth-local=trust --auth-host=md5
fi

conf=/data/postgresql.conf
sed -i "s/^#listen_addresses.*/listen_addresses = '*'/" "${conf}"
sed -i "s/^listen_addresses.*/listen_addresses = '*'/" "${conf}"
sed -i "s/^#port .*/port = 5432/" "${conf}"
sed -i "s/^port =.*/port = 5432/" "${conf}"
grep -q "^listen_addresses" "${conf}" || echo "listen_addresses = '*'" >> "${conf}"
grep -q "^port " "${conf}" || echo "port = 5432" >> "${conf}"
grep -q "^unix_socket_directories" "${conf}" || echo "unix_socket_directories = '/tmp'" >> "${conf}"
if ! grep -q "0.0.0.0/0" /data/pg_hba.conf; then
  printf '%s\n' "host all all 0.0.0.0/0 md5" "host all all ::/0 md5" >> /data/pg_hba.conf
fi

if [[ ! -f /data/.flynn-bootstrapped ]]; then
  user="${POSTGRES_USER:?POSTGRES_USER is required}"
  pass="${POSTGRES_PASSWORD:?POSTGRES_PASSWORD is required}"
  db="${POSTGRES_DB:?POSTGRES_DB is required}"
  if ! [[ "${user}" =~ ^[a-zA-Z0-9_]+$ && "${db}" =~ ^[a-zA-Z0-9_]+$ && "${pass}" =~ ^[a-zA-Z0-9_]+$ ]]; then
    echo "postgres credentials must be alphanumeric" >&2
    exit 1
  fi
  as_postgres "${PG_BIN}/pg_ctl" -D /data -w start
  as_postgres "${PG_BIN}/psql" -h /tmp -v ON_ERROR_STOP=1 -d postgres <<SQL
CREATE ROLE ${user} LOGIN PASSWORD '${pass}';
CREATE DATABASE ${db} OWNER ${user};
SQL
  as_postgres "${PG_BIN}/psql" -h /tmp -v ON_ERROR_STOP=1 -d "${db}" <<'SQL'
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS pgcrypto;
SQL
  as_postgres "${PG_BIN}/pg_ctl" -D /data -m fast -w stop
  touch /data/.flynn-bootstrapped
  chown postgres:postgres /data/.flynn-bootstrapped
fi

# exec cannot invoke a shell function.
exec setpriv --reuid=postgres --regid=postgres --init-groups --inh-caps=-all "${PG_BIN}/postgres" -D /data
