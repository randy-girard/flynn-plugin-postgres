#!/usr/bin/env bash
# Runs on a cluster host during `flynn-host plugin uninstall`, before the
# plugin app is deleted. Flynn already refuses uninstall while other apps
# still use this provider (unless --force), then removes plugin webhooks
# and DeleteApp (routes and exclusive resources). This hook is idempotent.
set -euo pipefail

: "${FLYNN_PLUGIN_NAME:?}"
: "${FLYNN_PLUGIN_KIND:?}"

echo "${FLYNN_PLUGIN_NAME} plugin uninstall hook: app=${FLYNN_PLUGIN_APP:-${FLYNN_PLUGIN_NAME}} kind=${FLYNN_PLUGIN_KIND}"
exit 0
