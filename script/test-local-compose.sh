#!/bin/bash
# Local docker compose is for add-on UI development only. Plugin install must
# not ship mock data or set DASHBOARD_DEV.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
manifest="${ROOT}/flynn-plugin.json"
compose="${ROOT}/compose.yaml"
readme="${ROOT}/README.md"

need() {
  local file=$1 needle=$2 msg=$3
  if ! grep -qE -- "${needle}" "${file}"; then
    echo "${msg}" >&2
    echo "  missing /${needle}/ in ${file}" >&2
    exit 1
  fi
}

need_file() {
  local path=$1 msg=$2
  if [[ ! -f "${path}" ]]; then
    echo "${msg}" >&2
    echo "  missing ${path}" >&2
    exit 1
  fi
}

need_file "${compose}" "local development must have compose.yaml"
need_file "${ROOT}/mock/Dockerfile" "compose must build the dashboard-dev image from mock/"
need_file "${ROOT}/mock/air.toml" "compose must rebuild the API with Air when Go files change"
need_file "${ROOT}/cmd/flynn-postgres-api/dashboard_dev.go" "compose must serve dashboard pages from in-memory mock data"

need "${compose}" 'DASHBOARD_DEV' \
  "compose must enable the in-process mock dashboard (DASHBOARD_DEV)"
need "${compose}" 'DASHBOARD_SSO_OPTIONAL' \
  "compose must allow opening /dashboard/?app_id=demo without an SSO JWT"
need "${compose}" '8091:8091' \
  "compose must publish the Redis UI on localhost:8091"
need "${readme}" 'docker compose up' \
  "README must document docker compose for local dashboard development"
need "${readme}" 'localhost:8091' \
  "README must document the local compose URL"

if grep -q 'DASHBOARD_DEV' "${manifest}"; then
  echo "flynn-plugin.json must not enable DASHBOARD_DEV on a real cluster" >&2
  exit 1
fi
if grep -q 'compose.yaml' "${manifest}"; then
  echo "compose.yaml is local-dev only and must not be part of the plugin install contract" >&2
  exit 1
fi
if grep -qE '"./mock|"mock/' "${manifest}"; then
  echo "flynn-plugin.json must not compile mock/ into the plugin image" >&2
  exit 1
fi

echo "ok local compose develops the Redis dashboard against mock data without changing plugin install"
