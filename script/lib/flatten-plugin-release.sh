#!/usr/bin/env bash
# Copy only the plugin overlay delta to dist/ for GitHub Release upload.
#
# plugin-build keeps Flynn ubuntu-noble under dist/layers/ so local DistReady
# and overlay still work. That OS layer is already a Flynn GitHub Release asset
# (same id). Re-uploading it from every plugin (~200MiB × N parallel jobs)
# 502s uploads.github.com. Flynn hosts fetch it from flynn.plugin.base.
#
# shellcheck shell=bash
set -euo pipefail

flatten_plugin_release_assets() {
  local dist="${1:-dist}"
  if [[ ! -f "${dist}/image.json" ]]; then
    echo "ERROR: ${dist}/image.json missing (run script/plugin-build)" >&2
    return 1
  fi
  python3 - "${dist}" <<'PY'
import json, os, shutil, sys

dist = sys.argv[1]
with open(os.path.join(dist, "image.json"), encoding="utf-8") as f:
    art = json.load(f)
man = art.get("manifest")
if isinstance(man, str):
    man = json.loads(man)
if not isinstance(man, dict):
    sys.exit("image.json missing manifest")
rootfs = man.get("rootfs") or []
if not rootfs or not isinstance(rootfs[0], dict):
    sys.exit("image.json missing rootfs")
layers = rootfs[0].get("layers") or []
ids = [layer.get("id") for layer in layers if isinstance(layer, dict) and layer.get("id")]
if len(ids) < 2:
    sys.exit("need Flynn ubuntu-noble plus a plugin delta; got %d layer(s)" % len(ids))
delta = ids[-1]
src = os.path.join(dist, "layers", delta + ".squashfs")
if not os.path.isfile(src):
    sys.exit("missing plugin delta " + src)
dst = os.path.join(dist, delta + ".squashfs")
shutil.copy2(src, dst)
print("publishing plugin delta %s (Flynn ubuntu-noble stays on the Flynn GitHub Release)" % delta)
for name in os.listdir(dist):
    if not name.endswith(".squashfs"):
        continue
    if name != delta + ".squashfs":
        os.remove(os.path.join(dist, name))
        print("removed leftover %s from release dir" % name)
PY
}

if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  flatten_plugin_release_assets "${1:-dist}"
fi
