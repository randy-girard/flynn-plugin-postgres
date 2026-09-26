#!/bin/bash
# Runs inside the flynn-plugin-dev container (macOS Docker Desktop wrapper).
set -euo pipefail

export PATH="/usr/local/go/bin:${PATH}"
export CGO_ENABLED="${CGO_ENABLED:-0}"
export GOFLAGS="${GOFLAGS:--mod=mod -buildvcs=false}"
export FLYNN_PLUGIN_IN_CONTAINER=1

cd /src

if [[ $# -eq 0 ]]; then
  echo "usage: docker run ... <command>" >&2
  echo "  ./script/run-unit-tests" >&2
  echo "  ./script/plugin-build [args...]" >&2
  exit 2
fi

exec "$@"
