#!/bin/bash
# Build PostgreSQL 16 plus PostGIS, pgRouting, and TimescaleDB from vendored
# source tarballs. Ubuntu/apt is only used for GIS/runtime libs and compilers.
# Same script on amd64 and arm64. start.sh still finds /usr/lib/postgresql/*/bin.
set -euo pipefail

export DEBIAN_FRONTEND=noninteractive

PG_VERSION="16.9"
PG_MAJOR="16"
POSTGIS_VERSION="3.5.2"
PGROUTING_VERSION="3.7.3"
TIMESCALEDB_VERSION="2.17.2"

HERE="$(cd "$(dirname "$0")" && pwd)"
PREFIX="/usr/lib/postgresql/${PG_MAJOR}"
PG_BIN="${PREFIX}/bin"

arch="$(uname -m)"
case "${arch}" in
  x86_64|amd64|aarch64|arm64) ;;
  *)
    echo "unsupported arch ${arch} (need x86_64/amd64 or aarch64/arm64)" >&2
    exit 1
    ;;
esac

# Official source tarballs, stored next to this script. GitHub rejects git
# blobs over 100MB, so a future bump may be split (name.tar.gz.0 + .1) and
# concatenated here. plugin-build copies img/ into the overlay chroot
# (refresh with img/fetch-engine.sh when bumping versions).
assemble_dist() {
  local name="$1" dest="$2"
  if [[ -s "${HERE}/${name}" ]]; then
    cp -f "${HERE}/${name}" "${dest}"
    return
  fi
  shopt -s nullglob
  local parts=( "${HERE}/${name}".[0-9] )
  shopt -u nullglob
  if [[ ${#parts[@]} -eq 0 ]]; then
    echo "missing vendored ${HERE}/${name} (or ${name}.0 + .1)" >&2
    exit 1
  fi
  echo "assembling ${name} from ${#parts[@]} part(s)"
  cat "${parts[@]}" > "${dest}"
}

PG_TAR=/tmp/postgresql.tar.gz
POSTGIS_TAR=/tmp/postgis.tar.gz
PGROUTING_TAR=/tmp/pgrouting.tar.gz
TIMESCALEDB_TAR=/tmp/timescaledb.tar.gz
assemble_dist "postgresql-${PG_VERSION}.tar.gz" "${PG_TAR}"
assemble_dist "postgis-${POSTGIS_VERSION}.tar.gz" "${POSTGIS_TAR}"
assemble_dist "pgrouting-${PGROUTING_VERSION}.tar.gz" "${PGROUTING_TAR}"
assemble_dist "timescaledb-${TIMESCALEDB_VERSION}.tar.gz" "${TIMESCALEDB_TAR}"
echo "df5aaddcf7841457cbf9d31b7d79124a2057cdc7cdae47e8e0271c973546618e  ${PG_TAR}" | sha256sum -c -
echo "fcad74289d03bfe9d6eb6f0f5682c9cb03a90ba9c377e39727df85f5d66fa8f51ddb9a9bc7302612d13576cca92bff3717ed965d513ededfe8cf1627c1e2f92d  ${POSTGIS_TAR}" | sha512sum -c -
echo "949ebe7acd60565a5a1c206d8918caa371f836015f6a721bdc29482ca23b8298  ${PGROUTING_TAR}" | sha256sum -c -
echo "fc74d41062735bd4136ac71b0e10aecf34a64cdb29fd2ff31eae3f406beb8db00866e8ed7e142d47852e1586a6ad2ffa1652fbd792a45e995174b62464963d1e  ${TIMESCALEDB_TAR}" | sha512sum -c -

apt-get update -o Acquire::Retries=5
# Runtime libs first so purge of *-dev leaves them installed.
apt-get install -y --no-install-recommends \
  ca-certificates \
  curl \
  openssl \
  libssl3t64 \
  libxml2 \
  libicu74 \
  libreadline8t64 \
  zlib1g \
  liblz4-1 \
  libuuid1 \
  libgeos-c1t64 \
  libproj25 \
  libgdal34t64 \
  libjson-c5 \
  libprotobuf-c1 \
  libboost-graph1.83.0

apt-get install -y --no-install-recommends \
  build-essential \
  bison \
  flex \
  pkg-config \
  cmake \
  libssl-dev \
  libxml2-dev \
  libicu-dev \
  libreadline-dev \
  zlib1g-dev \
  liblz4-dev \
  uuid-dev \
  libgeos-dev \
  libproj-dev \
  libgdal-dev \
  libjson-c-dev \
  libprotobuf-c-dev \
  protobuf-c-compiler \
  libboost-graph-dev

JOBS="$(nproc)"
workdir=/tmp/pg-src
rm -rf "${workdir}"
mkdir -p "${workdir}"

tar -xzf "${PG_TAR}" -C "${workdir}"
rm -f "${PG_TAR}"
pushd "${workdir}/postgresql-${PG_VERSION}" >/dev/null
./configure \
  --prefix="${PREFIX}" \
  --with-openssl \
  --with-libxml \
  --with-uuid=e2fs \
  --with-icu \
  --with-lz4 \
  --enable-nls=no
make -j"${JOBS}" world
make install-world
popd >/dev/null

echo "${PREFIX}/lib" > /etc/ld.so.conf.d/postgresql-${PG_MAJOR}.conf
ldconfig
export PATH="${PG_BIN}:${PATH}"
export PG_CONFIG="${PG_BIN}/pg_config"

ln -sfn "${PG_BIN}/psql" /usr/bin/psql
ln -sfn "${PG_BIN}/pg_dump" /usr/bin/pg_dump
ln -sfn "${PG_BIN}/pg_restore" /usr/bin/pg_restore
ln -sfn "${PG_BIN}/pg_isready" /usr/bin/pg_isready
ln -sfn "${PG_BIN}/pg_basebackup" /usr/bin/pg_basebackup

# postgresql-16-postgis-3 (from source)
tar -xzf "${POSTGIS_TAR}" -C "${workdir}"
rm -f "${POSTGIS_TAR}"
pushd "${workdir}/postgis-${POSTGIS_VERSION}" >/dev/null
./configure --with-pgconfig="${PG_CONFIG}"
make -j"${JOBS}"
make install
popd >/dev/null

# postgresql-16-pgrouting (from source)
tar -xzf "${PGROUTING_TAR}" -C "${workdir}"
rm -f "${PGROUTING_TAR}"
pushd "${workdir}/pgrouting-${PGROUTING_VERSION}" >/dev/null
cmake -S . -B build \
  -DCMAKE_BUILD_TYPE=Release \
  -DWITH_DOC=OFF \
  -DPOSTGRESQL_BIN="${PG_BIN}" \
  -DPOSTGRESQL_LIBRARY_DIRS="$("${PG_CONFIG}" --libdir)" \
  -DPOSTGRESQL_INCLUDE_DIRS="$("${PG_CONFIG}" --includedir-server)"
cmake --build build -j"${JOBS}"
cmake --install build
popd >/dev/null

# timescaledb-2-postgresql-16 (from source; TSL/community, matching the old apt package)
tar -xzf "${TIMESCALEDB_TAR}" -C "${workdir}"
rm -f "${TIMESCALEDB_TAR}"
pushd "${workdir}/timescaledb-${TIMESCALEDB_VERSION}" >/dev/null
./bootstrap \
  -DCMAKE_BUILD_TYPE=RelWithDebInfo \
  -DREGRESS_CHECKS=OFF \
  -DWARNINGS_AS_ERRORS=OFF \
  -DSEND_TELEMETRY_DEFAULT=OFF \
  -DAPACHE_ONLY=OFF \
  -DPG_CONFIG="${PG_CONFIG}"
cmake --build build -j"${JOBS}"
cmake --install build
popd >/dev/null

rm -rf "${workdir}"

getent group postgres >/dev/null || groupadd -r postgres
getent passwd postgres >/dev/null || useradd -r -g postgres -d /var/lib/postgresql -s /bin/bash postgres
mkdir -p /var/lib/postgresql /data
chown postgres:postgres /var/lib/postgresql

test -x "${PG_BIN}/postgres"
test -f "${PREFIX}/share/extension/postgis.control"
test -f "${PREFIX}/share/extension/pgrouting.control"
test -f "${PREFIX}/share/extension/timescaledb.control"
test -f "${PREFIX}/share/extension/pg_stat_statements.control"

apt-get purge -y --auto-remove \
  build-essential \
  bison \
  flex \
  pkg-config \
  cmake \
  libssl-dev \
  libxml2-dev \
  libicu-dev \
  libreadline-dev \
  zlib1g-dev \
  liblz4-dev \
  uuid-dev \
  libgeos-dev \
  libproj-dev \
  libgdal-dev \
  libjson-c-dev \
  libprotobuf-c-dev \
  protobuf-c-compiler \
  libboost-graph-dev \
  gcc \
  g++ \
  make \
  dpkg-dev \
  libc6-dev || true
apt-get install -y --no-install-recommends \
  ca-certificates \
  curl \
  openssl \
  libssl3t64 \
  libxml2 \
  libicu74 \
  libreadline8t64 \
  zlib1g \
  liblz4-1 \
  libuuid1 \
  libgeos-c1t64 \
  libproj25 \
  libgdal34t64 \
  libjson-c5 \
  libprotobuf-c1 \
  libboost-graph1.83.0

# Do not run timescaledb-tune; Flynn writes postgresql.conf (shared_preload_libraries).

# shellcheck source=img/apt-slim-finish.sh
source "$(dirname "$0")/apt-slim-finish.sh"
