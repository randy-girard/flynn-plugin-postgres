#!/usr/bin/env bash
# Live-cluster e2e for the postgres plugin. Uses the Flynn cluster that `flynn`
# would use (flynnrc default, or FLYNN_CLUSTER / --cluster). The plugin must
# already be installed there. Does not run in CI.
#
#   cluster/run.sh
#   cluster/run.sh --cli-only
#   cluster/run.sh --web-only --headed
#   cluster/run.sh --cluster local --web-only --headed
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "${ROOT}"

run_cli=1
run_web=1
headed=0
cluster_flag=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --cli-only) run_web=0 ;;
    --web-only) run_cli=0 ;;
    --headed) headed=1 ;;
    --cluster)
      if [[ $# -lt 2 || "$2" == -* ]]; then
        echo "--cluster requires a cluster name from \`flynn cluster\`" >&2
        exit 1
      fi
      cluster_flag="$2"
      shift
      ;;
    --cluster=*)
      cluster_flag="${1#--cluster=}"
      if [[ -z "${cluster_flag}" ]]; then
        echo "--cluster requires a cluster name from \`flynn cluster\`" >&2
        exit 1
      fi
      ;;
    -h|--help)
      cat <<'EOF'
Live-cluster e2e for the postgres plugin. Uses the same Flynn cluster `flynn`
uses (flynnrc default, or FLYNN_CLUSTER / --cluster). Plugin must already be
installed there.

  cluster/run.sh
  cluster/run.sh --cli-only
  cluster/run.sh --web-only --headed
  cluster/run.sh --cluster local --web-only --headed
EOF
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      echo "usage: cluster/run.sh [--cluster NAME] [--cli-only|--web-only] [--headed]" >&2
      exit 1
      ;;
  esac
  shift
done

FLYNN="${FLYNN:-flynn}"
export FLYNN
export FLYNN_CLUSTER_E2E=1
export CGO_ENABLED="${CGO_ENABLED:-0}"
export GOFLAGS="${GOFLAGS:--mod=mod -buildvcs=false}"

if [[ -n "${cluster_flag}" ]]; then
  export FLYNN_CLUSTER="${cluster_flag}"
fi

need_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "need $1 on PATH" >&2
    exit 1
  fi
}

# NAME and CONTROLLER URL for the cluster flynn will actually call.
cluster_identity() {
  local want="${FLYNN_CLUSTER:-}"
  "${FLYNN}" cluster 2>/dev/null | awk -v want="${want}" '
    NR == 1 { next }
    {
      name = $1
      url = $2
      is_default = ($0 ~ /\(default\)/)
      if (want != "") {
        if (name == want) { print name "\t" url; exit }
      } else if (is_default) {
        print name "\t" url; exit
      }
    }
  '
}

dashboard_from_flynn() {
  local want="${FLYNN_CLUSTER:-}"
  "${FLYNN}" cluster 2>/dev/null | awk -v want="${want}" '
    NR == 1 { next }
    {
      name = $1
      is_default = ($0 ~ /\(default\)/)
      if (want != "" && name != want) next
      if (want == "" && !is_default) next
      for (i = 1; i <= NF; i++) {
        if ($i ~ /^https?:\/\/dashboard\./) { print $i; exit }
      }
    }
  '
}

password_from_dashboard_app() {
  local app line
  for app in dashboard-plugin dashboard; do
    line="$("${FLYNN}" -a "${app}" env 2>/dev/null | awk -F= '$1=="BOOTSTRAP_ADMIN_PASSWORD"{print substr($0,index($0,$2)); exit}')" || true
    if [[ -n "${line}" ]]; then
      printf '%s' "${line}"
      return 0
    fi
  done
  return 1
}

need_cmd "${FLYNN}"
need_cmd go

ident="$(cluster_identity || true)"
cluster_name="${ident%%$'\t'*}"
cluster_url="${ident#*$'\t'}"
if [[ -z "${cluster_name}" || "${cluster_name}" == "${ident}" ]]; then
  cluster_name="${FLYNN_CLUSTER:-}"
  cluster_url=""
fi

echo "==> cluster e2e"
echo "    flynn=${FLYNN}"
if [[ -n "${cluster_name}" ]]; then
  echo "    cluster=${cluster_name}${cluster_url:+  ${cluster_url}}"
else
  echo "    cluster=(flynn default; run: flynn cluster)"
fi

# CLI commands (plugin, env, create) need a controller session. Bootstrap
# admin is email+password (vagrant default flynn-dev).
ensure_cluster_login() {
  if "${FLYNN}" whoami >/dev/null 2>&1; then
    local session
    session="$("${FLYNN}" whoami 2>/dev/null | awk -F': ' '/^email:/{print $2; exit}')"
    echo "    session=${session:-ok}"
    return 0
  fi
  local domain="1.localflynn.com"
  local url="${cluster_url:-${DASHBOARD_URL:-}}"
  if [[ -n "${url}" ]]; then
    local host="${url#https://}"
    host="${host#http://}"
    host="${host%%/*}"
    host="${host#controller.}"
    host="${host#dashboard.}"
    host="${host#auth.}"
    domain="${host}"
  fi
  local email="${FLYNN_EMAIL:-${DASHBOARD_EMAIL:-admin@${domain}}}"
  local password="${FLYNN_PASSWORD:-${FLYNN_ADMIN_PASSWORD:-${DASHBOARD_PASSWORD:-${BOOTSTRAP_ADMIN_PASSWORD:-flynn-dev}}}}"
  echo "    login=${email}"
  if ! "${FLYNN}" login --email "${email}" --password "${password}"; then
    echo "flynn login failed for cluster ${cluster_name:-default}." >&2
    echo "Set FLYNN_EMAIL and FLYNN_PASSWORD to the bootstrap admin on this cluster." >&2
    exit 1
  fi
}
ensure_cluster_login

if ! plugins="$("${FLYNN}" plugin 2>/dev/null)"; then
  echo "flynn cannot list plugins on cluster ${cluster_name:-default}." >&2
  echo "Configured clusters:" >&2
  "${FLYNN}" cluster >&2 || true
  exit 1
fi
if ! printf '%s\n' "${plugins}" | grep -Eqi '(^|[[:space:]])postgres(-plugin)?([[:space:]]|$)'; then
  echo "postgres plugin is not installed on cluster ${cluster_name:-default}${cluster_url:+ (${cluster_url})}." >&2
  echo "This script uses that Flynn cluster (flynnrc default, FLYNN_CLUSTER, or --cluster), not a hardcoded localflynn URL." >&2
  echo "Installed plugins:" >&2
  printf '%s\n' "${plugins}" >&2
  echo "Configured clusters:" >&2
  "${FLYNN}" cluster >&2 || true
  echo "Retry with: cluster/run.sh --cluster local --web-only --headed" >&2
  exit 1
fi

if [[ -z "${DASHBOARD_URL:-}" ]]; then
  DASHBOARD_URL="$(dashboard_from_flynn || true)"
fi
DASHBOARD_URL="${DASHBOARD_URL:-https://dashboard.1.localflynn.com}"
export DASHBOARD_URL

if [[ -z "${DASHBOARD_EMAIL:-}" ]]; then
  host="${DASHBOARD_URL#https://}"
  host="${host#http://}"
  host="${host%%/*}"
  domain="${host#dashboard.}"
  DASHBOARD_EMAIL="admin@${domain}"
fi
export DASHBOARD_EMAIL

if [[ -z "${DASHBOARD_PASSWORD:-}" ]]; then
  DASHBOARD_PASSWORD="$(password_from_dashboard_app || true)"
fi
if [[ -z "${DASHBOARD_PASSWORD:-}" ]]; then
  DASHBOARD_PASSWORD="${FLYNN_PASSWORD:-${FLYNN_ADMIN_PASSWORD:-${BOOTSTRAP_ADMIN_PASSWORD:-flynn-dev}}}"
fi
export DASHBOARD_PASSWORD

if [[ "${headed}" -eq 1 ]]; then
  export HEADLESS=0
fi
export HEADLESS="${HEADLESS:-1}"

echo "    dashboard=${DASHBOARD_URL}"
echo "    email=${DASHBOARD_EMAIL}"
echo "    cli=${run_cli} web=${run_web} headless=${HEADLESS}"

if [[ "${run_cli}" -eq 1 ]]; then
  echo "==> CLI (go test -tags=cluster)"
  go test -tags=cluster -count=1 -timeout 15m -v ./cluster/cli
fi

if [[ "${run_web}" -eq 1 ]]; then
  need_cmd npm
  echo "==> web (Playwright Chromium)"
  (
    cd "${ROOT}/cluster/web"
    if [[ ! -d node_modules ]]; then
      npm install
    fi
    npx playwright install chromium
    npx playwright test
  )
fi
