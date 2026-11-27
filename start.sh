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

# follower_basebackup seeds a follower's data directory from the primary.
# The password goes in PGPASSWORD and the URL loses it: pg_basebackup's argv
# is readable by every process in the job (/proc/<pid>/cmdline). -R still
# writes the password into primary_conninfo in postgresql.auto.conf (0600).
follower_basebackup() {
  local url="$1" dir="$2" pw=""
  if [[ "${url}" =~ ^([A-Za-z][A-Za-z0-9+.-]*://)([^:@/]*):([^@/]*)@(.*)$ ]]; then
    url="${BASH_REMATCH[1]}${BASH_REMATCH[2]}@${BASH_REMATCH[4]}"
    pw="${BASH_REMATCH[3]}"
    pw="$(printf '%b' "${pw//%/\\x}")"
  fi
  PGPASSWORD="${pw}" as_postgres "${PG_BIN}/pg_basebackup" -d "${url}" -D "${dir}" -Fp -Xs -R --checkpoint=fast --no-password
}

install -d -o postgres -g postgres -m 0700 /data
# The API sets FLYNN_POSTGRES_PRIMARY_URL. POSTGRES_PRIMARY_URL is the older
# name. Gating on only the old name initdb's a follower, then standby.signal
# has no primary_conninfo and the replica polls pg_wal forever.
primary_url="${FLYNN_POSTGRES_PRIMARY_URL:-${POSTGRES_PRIMARY_URL:-}}"
if [[ ! -s /data/PG_VERSION ]]; then
  if [[ -n "${primary_url}" ]]; then
    # --checkpoint=fast: spread (the default) waits for the next scheduled
    # checkpoint, which on an idle/empty primary can be checkpoint_timeout
    # (5 minutes) because nothing fills WAL.
    follower_basebackup "${primary_url}" /data
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
# Image-refresh basebackup jumps WAL by tens of MB, then checkpoints recycle
# segments. Without a keep size, an existing follower (upland) requests WAL
# that is already gone: "requested WAL segment ... has already been removed".
grep -q "^wal_keep_size" "${conf}" || echo "wal_keep_size = '1GB'" >> "${conf}"
# The plugin derives shared_buffers and effective_cache_size from the job's
# memory limit (FLYNN_DB_* on the release, NEW-26). Rewrite them on every
# start: initdb wrote its own shared_buffers, and a follower copied the
# leader's file. Unset leaves the file as it is (instances made before).
apply_memory_settings() {
  local conf="$1" key val tmp
  for key in shared_buffers effective_cache_size; do
    case "${key}" in
      shared_buffers) val="${FLYNN_DB_SHARED_BUFFERS:-}" ;;
      effective_cache_size) val="${FLYNN_DB_EFFECTIVE_CACHE_SIZE:-}" ;;
    esac
    [[ -n "${val}" ]] || continue
    if ! [[ "${val}" =~ ^[0-9]+(kB|MB|GB)$ ]]; then
      echo "ignoring ${key} '${val}': not a size" >&2
      continue
    fi
    tmp="$(mktemp)"
    grep -Ev "^[[:space:]]*${key}[[:space:]]*=" "${conf}" > "${tmp}" || true
    echo "${key} = '${val}'" >> "${tmp}"
    # cat keeps the file's owner and mode (postgres, 0600).
    cat "${tmp}" > "${conf}"
    rm -f "${tmp}"
  done
}
apply_memory_settings "${conf}"
if ! grep -q "0.0.0.0/0" /data/pg_hba.conf; then
  printf '%s\n' "host all all 0.0.0.0/0 md5" "host all all ::/0 md5" >> /data/pg_hba.conf
fi
if ! grep -q "host replication" /data/pg_hba.conf; then
  printf '%s\n' "host replication all 0.0.0.0/0 md5" "host replication all ::/0 md5" >> /data/pg_hba.conf
fi
# The release carries this instance's own CA-signed certificate; the plugin
# verifies it (sslmode=verify-full). Install it on every start: a follower's
# pg_basebackup copied the primary's files. An instance whose release has no
# bundle keeps a local self-signed certificate (sslmode=require).
/bin/flynn-postgres install-tls /data
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
# none until bootstrap finishes. The block below turns tracking on and
# resets the view so CREATE ROLE / CREATE DATABASE never become slow queries.
grep -q "^pg_stat_statements.track" "${conf}" || echo "pg_stat_statements.track = none" >> "${conf}"
grep -q "^timescaledb.max_background_workers" "${conf}" || echo "timescaledb.max_background_workers = 8" >> "${conf}"

# Follower-swap image refresh stamps POSTGRES_ROLE=primary and bounces this
# job. pg_basebackup -R left standby.signal, which would keep us read-only.
# If that job is put back to follower, the signal is already gone and bootstrap
# would treat the copied data directory as a new primary.
role="${FLYNN_POSTGRES_ROLE:-${POSTGRES_ROLE:-}}"
# A fenced primary is deposed. It must not accept writes if the host starts
# the volume again. An empty role still bootstraps a new primary.
if [[ "${role}" == "deposed" ]]; then
  echo "refusing writable start: postgres role is deposed" >&2
  exit 1
fi
if [[ "${role}" == "primary" ]]; then
  rm -f /data/standby.signal /data/recovery.signal
elif [[ "${role}" == "follower" ]]; then
  touch /data/standby.signal
  chown postgres:postgres /data/standby.signal
fi

if [[ "${role}" != "follower" && ! -f /data/standby.signal && ! -f /data/.flynn-bootstrapped ]]; then
  user="${POSTGRES_USER:?POSTGRES_USER is required}"
  pass="${POSTGRES_PASSWORD:?POSTGRES_PASSWORD is required}"
  db="${POSTGRES_DB:?POSTGRES_DB is required}"
  if ! [[ "${user}" =~ ^[a-zA-Z0-9_]+$ && "${db}" =~ ^[a-zA-Z0-9_]+$ && "${pass}" =~ ^[a-zA-Z0-9_]+$ ]]; then
    echo "postgres credentials must be alphanumeric" >&2
    exit 1
  fi
  as_postgres "${PG_BIN}/pg_ctl" -D /data -w start
  # Older images finished bootstrap without .flynn-bootstrapped. CREATE ROLE
  # and CREATE DATABASE then fail, the marker is never written, and the job
  # crash-loops. Skip the ones that already exist.
  as_postgres "${PG_BIN}/psql" -h /tmp -v ON_ERROR_STOP=1 -d postgres <<SQL
SELECT format('CREATE ROLE %I LOGIN PASSWORD %L CONNECTION LIMIT 20 NOSUPERUSER CREATEDB CREATEROLE REPLICATION;', '${user}', '${pass}')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '${user}')
\gexec
GRANT pg_monitor TO ${user};
SELECT format('CREATE DATABASE %I OWNER %I', '${db}', '${user}')
WHERE NOT EXISTS (SELECT 1 FROM pg_database WHERE datname = '${db}')
\gexec
REVOKE CONNECT ON DATABASE postgres FROM PUBLIC;
REVOKE CONNECT ON DATABASE template1 FROM PUBLIC;
REVOKE CONNECT ON DATABASE ${db} FROM PUBLIC;
GRANT CONNECT ON DATABASE ${db} TO ${user};
SQL
  as_postgres "${PG_BIN}/psql" -h /tmp -v ON_ERROR_STOP=1 -d template1 <<'SQL'
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
SQL
  as_postgres "${PG_BIN}/psql" -h /tmp -v ON_ERROR_STOP=1 -d "${db}" <<'SQL'
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
SQL
  # The bootstrap session runs in the postgres database. Resetting stats there
  # before the extension exists makes ON_ERROR_STOP abort init, the marker is
  # never written, and the job crash-loops.
  as_postgres "${PG_BIN}/psql" -h /tmp -v ON_ERROR_STOP=1 -d postgres <<'SQL'
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
SQL
  # Tenant queries start here. Clear anything the bootstrap session recorded.
  sed -i "s/^pg_stat_statements.track.*/pg_stat_statements.track = all/" "${conf}"
  as_postgres "${PG_BIN}/psql" -h /tmp -v ON_ERROR_STOP=1 -d postgres -c "SELECT pg_stat_statements_reset();"
  as_postgres "${PG_BIN}/psql" -h /tmp -v ON_ERROR_STOP=1 -d "${db}" -c "SELECT pg_stat_statements_reset();"
  as_postgres "${PG_BIN}/pg_ctl" -D /data -m fast -w stop
  touch /data/.flynn-bootstrapped
  chown postgres:postgres /data/.flynn-bootstrapped
fi

# A restarted follower keeps its data directory, so pg_basebackup does not
# run again. primary_conninfo would still name the previous primary. Rewrite
# it from FLYNN_POSTGRES_PRIMARY_URL before postgres reads the file.
if [[ "${role}" == "follower" && -n "${primary_url}" && -s /data/PG_VERSION ]]; then
  auto=/data/postgresql.auto.conf
  touch "${auto}"
  grep -v '^primary_conninfo' "${auto}" > "${auto}.flynn" || true
  escaped=${primary_url//\'/\'\'}
  printf "primary_conninfo = '%s'\n" "${escaped}" >> "${auto}.flynn"
  mv "${auto}.flynn" "${auto}"
  chown postgres:postgres "${auto}"
fi

# The Go process stays up so discoverd keeps the registration. exec cannot
# call a shell function, and a bare postgres process never registers.
export POSTGRES_BIN="${PG_BIN}/postgres"
exec /bin/flynn-postgres serve
