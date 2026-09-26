#!/bin/bash
set -euo pipefail

if [[ "${1:-}" == "api" ]]; then
  exec /bin/flynn-postgres-api
fi

exec /bin/flynn-postgres "$@"
