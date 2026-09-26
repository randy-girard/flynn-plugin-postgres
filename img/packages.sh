#!/bin/bash
set -e

export DEBIAN_FRONTEND=noninteractive

# ---- Update base system ----
apt-get update -o Acquire::Retries=5
apt-get install -y --no-install-recommends \
  ca-certificates \
  curl \
  gnupg \
  openssl

# PostgreSQL 16 plus the extensions the smoke checks are available
# (postgis, pgrouting, timescaledb). Ubuntu's postgresql metapackage
# does not ship those.
curl -fsSL --retry 5 --retry-delay 3 https://www.postgresql.org/media/keys/ACCC4CF8.asc \
  | gpg --dearmor -o /usr/share/keyrings/postgresql.gpg
echo "deb [signed-by=/usr/share/keyrings/postgresql.gpg] \
https://apt.postgresql.org/pub/repos/apt noble-pgdg main" \
  > /etc/apt/sources.list.d/postgresql.list
curl -fsSL --retry 5 --retry-delay 3 https://packagecloud.io/timescale/timescaledb/gpgkey \
  | gpg --dearmor -o /usr/share/keyrings/timescaledb.gpg
echo "deb [signed-by=/usr/share/keyrings/timescaledb.gpg] \
https://packagecloud.io/timescale/timescaledb/ubuntu/ noble main" \
  > /etc/apt/sources.list.d/timescaledb.list

apt-get update -o Acquire::Retries=5
apt-get install -y --no-install-recommends \
  postgresql-16 \
  postgresql-contrib-16 \
  postgresql-16-postgis-3 \
  postgresql-16-pgrouting \
  timescaledb-2-postgresql-16 \
  timescaledb-tools

# ---- Data directory ----
mkdir -p /data

# shellcheck source=img/apt-slim-finish.sh
source "$(dirname "$0")/apt-slim-finish.sh"
