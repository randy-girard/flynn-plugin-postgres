#!/usr/bin/env bash
# Runs on a cluster host during `flynn-host plugin uninstall`, before the
# plugin app is deleted. Drops this engine's database runtimes (including
# builtins). Flynn already refuses uninstall while other apps still use this
# provider (unless --force). This hook is idempotent.
set -euo pipefail

: "${FLYNN_PLUGIN_NAME:?}"
: "${FLYNN_PLUGIN_KIND:?}"

engine="${FLYNN_PLUGIN_NAME}"
echo "removing database runtimes for ${engine}"
flynn-host db-runtime:drop-engine "${engine}" || true

echo "${FLYNN_PLUGIN_NAME} plugin uninstall hook: app=${FLYNN_PLUGIN_APP:-${FLYNN_PLUGIN_NAME}} kind=${FLYNN_PLUGIN_KIND}"
exit 0
