#!/usr/bin/env bash
# Publish small/medium/large database runtimes for this engine after the
# plugin app is up. Idempotent. flynn-host plugin install also ensures them
# after this hook, so a missing or old db-runtime:ensure must not fail install.
set -euo pipefail

: "${FLYNN_PLUGIN_NAME:?}"
engine="${FLYNN_PLUGIN_NAME}"
echo "ensuring database runtimes for ${engine}"
if command -v flynn-host >/dev/null 2>&1; then
  flynn-host db-runtime:ensure "${engine}" || true
else
  echo "flynn-host not on PATH; plugin install will publish runtimes"
fi
