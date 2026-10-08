#!/bin/bash
# Maintainer helper: download pinned PostgreSQL + extension sources into img/.
# packages.sh uses those local files; plugin-build does not fetch
# ftp.postgresql.org / osgeo / GitHub. Re-run when bumping versions, then
# commit. Source tarballs are arch-independent (amd64 and arm64).
# Pins: postgresql-16.9.tar.gz postgis-3.5.2.tar.gz pgrouting-3.7.3.tar.gz timescaledb-2.17.2.tar.gz
set -euo pipefail

PG_VERSION="16.9"
POSTGIS_VERSION="3.5.2"
PGROUTING_VERSION="3.7.3"
TIMESCALEDB_VERSION="2.17.2"

HERE="$(cd "$(dirname "$0")" && pwd)"
GITHUB_BLOB_MAX=$((90 * 1024 * 1024))

check_digest() {
  local algo="$1" digest="$2" file="$3"
  if command -v "${algo}sum" >/dev/null 2>&1; then
    echo "${digest}  ${file}" | "${algo}sum" -c -
    return
  fi
  case "${algo}" in
    sha256) echo "${digest}  ${file}" | shasum -a 256 -c - ;;
    sha512) echo "${digest}  ${file}" | shasum -a 512 -c - ;;
    *)
      echo "need ${algo}sum or shasum to verify ${file}" >&2
      return 1
      ;;
  esac
}

split_if_needed() {
  local file="$1"
  local size
  size=$(wc -c < "${file}" | tr -d ' ')
  if [[ "${size}" -le "${GITHUB_BLOB_MAX}" ]]; then
    return
  fi
  python3 - "${file}" "${GITHUB_BLOB_MAX}" <<'PY'
import sys
src, maxsz = sys.argv[1], int(sys.argv[2])
with open(src, "rb") as f:
    i = 0
    while True:
        chunk = f.read(maxsz)
        if not chunk:
            break
        with open("%s.%d" % (src, i), "wb") as out:
            out.write(chunk)
        i += 1
    if i < 2:
        raise SystemExit("split produced %d part(s); expected at least 2" % i)
    print("split %s into %d GitHub-safe parts" % (src, i))
PY
  rm -f "${file}"
}

assemble_existing() {
  local name="$1"
  if [[ -s "${HERE}/${name}" ]]; then
    printf '%s\n' "${HERE}/${name}"
    return
  fi
  shopt -s nullglob
  local parts=( "${HERE}/${name}".[0-9] )
  shopt -u nullglob
  if [[ ${#parts[@]} -eq 0 ]]; then
    return 1
  fi
  local tmp
  tmp="$(mktemp "${HERE}/${name}.assemble.XXXXXX")"
  cat "${parts[@]}" > "${tmp}"
  printf '%s\n' "${tmp}"
}

fetch() {
  local algo="$1" digest="$2" url="$3" name="$4"
  local dest="${HERE}/${name}"
  local assembled=""
  if assembled="$(assemble_existing "${name}")"; then
    check_digest "${algo}" "${digest}" "${assembled}"
    if [[ "${assembled}" != "${dest}" ]]; then
      rm -f "${assembled}"
    fi
    echo "already vendored ${name}"
    return
  fi
  echo "fetch ${url}"
  curl -fSL --retry 5 --retry-delay 3 -o "${dest}.partial" "${url}"
  check_digest "${algo}" "${digest}" "${dest}.partial"
  mv "${dest}.partial" "${dest}"
  split_if_needed "${dest}"
}

# https://ftp.postgresql.org/pub/source/v16.9/postgresql-16.9.tar.gz.sha256
fetch sha256 \
  "df5aaddcf7841457cbf9d31b7d79124a2057cdc7cdae47e8e0271c973546618e" \
  "https://ftp.postgresql.org/pub/source/v${PG_VERSION}/postgresql-${PG_VERSION}.tar.gz" \
  "postgresql-${PG_VERSION}.tar.gz"

# Gentoo Manifest SHA512 for download.osgeo.org postgis-3.5.2.tar.gz
fetch sha512 \
  "fcad74289d03bfe9d6eb6f0f5682c9cb03a90ba9c377e39727df85f5d66fa8f51ddb9a9bc7302612d13576cca92bff3717ed965d513ededfe8cf1627c1e2f92d" \
  "https://download.osgeo.org/postgis/source/postgis-${POSTGIS_VERSION}.tar.gz" \
  "postgis-${POSTGIS_VERSION}.tar.gz"

# Debian orig / GitHub release asset pgrouting-3.7.3.tar.gz
fetch sha256 \
  "949ebe7acd60565a5a1c206d8918caa371f836015f6a721bdc29482ca23b8298" \
  "https://github.com/pgRouting/pgrouting/releases/download/v${PGROUTING_VERSION}/pgrouting-${PGROUTING_VERSION}.tar.gz" \
  "pgrouting-${PGROUTING_VERSION}.tar.gz"

# Gentoo Manifest SHA512 for github.com/timescale/timescaledb/archive/2.17.2.tar.gz
fetch sha512 \
  "fc74d41062735bd4136ac71b0e10aecf34a64cdb29fd2ff31eae3f406beb8db00866e8ed7e142d47852e1586a6ad2ffa1652fbd792a45e995174b62464963d1e" \
  "https://github.com/timescale/timescaledb/archive/${TIMESCALEDB_VERSION}.tar.gz" \
  "timescaledb-${TIMESCALEDB_VERSION}.tar.gz"

echo "postgres ${PG_VERSION} postgis ${POSTGIS_VERSION} pgrouting ${PGROUTING_VERSION} timescaledb ${TIMESCALEDB_VERSION} vendored in img/"
