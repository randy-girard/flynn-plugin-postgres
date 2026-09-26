#!/bin/bash
# Contract: an empty Build and Release version input picks vYYYYMMDD.N.P from tags.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "${ROOT}"
calver_lib="${ROOT}/script/lib/calver.sh"
workflow="${ROOT}/.github/workflows/release.yml"

if [[ ! -f "${calver_lib}" ]]; then
  echo "missing ${calver_lib}" >&2
  exit 1
fi
if [[ ! -f "${workflow}" ]]; then
  echo "missing ${workflow}" >&2
  exit 1
fi

if grep -A8 '^[[:space:]]*version:' "${workflow}" | grep -q 'required: true'; then
  echo "release workflow version must be optional so empty means next vYYYYMMDD.N.P" >&2
  exit 1
fi
if ! grep -q 'next_release_version_from_tags' "${workflow}"; then
  echo "release workflow must auto-pick the next calver tag when version is empty" >&2
  exit 1
fi
if ! grep -q 'script/lib/calver.sh' "${workflow}"; then
  echo "release workflow must source script/lib/calver.sh" >&2
  exit 1
fi
if ! grep -q 'vYYYYMMDD.N.P' "${workflow}"; then
  echo "release workflow must document vYYYYMMDD.N.P plugin tags" >&2
  exit 1
fi

# shellcheck source=script/lib/calver.sh
source "${calver_lib}"

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
git init -q "${tmp}"
git -C "${tmp}" config user.email "calver@example.com"
git -C "${tmp}" config user.name "calver"
git -C "${tmp}" commit --allow-empty -q -m init

pushd "${tmp}" >/dev/null
today="$(date +%Y%m%d)"
got="$(next_release_version_from_tags)"
if [[ "${got}" != "v${today}.0.0" ]]; then
  echo "expected v${today}.0.0 with no tags, got ${got}" >&2
  exit 1
fi
git tag "v${today}.0.0"
git tag "v${today}.0.0-smoke"
got="$(next_release_version_from_tags)"
if [[ "${got}" != "v${today}.0.1" ]]; then
  echo "expected v${today}.0.1 after .0.0 (ignoring -smoke), got ${got}" >&2
  exit 1
fi
git tag "v${today}.0.9"
got="$(next_release_version_from_tags)"
if [[ "${got}" != "v${today}.0.10" ]]; then
  echo "expected v${today}.0.10 after .0.9, got ${got}" >&2
  exit 1
fi
git tag "v${today}.2"
got="$(next_release_version_from_tags)"
if [[ "${got}" != "v${today}.2.1" ]]; then
  echo "expected v${today}.2.1 after legacy two-part v${today}.2, got ${got}" >&2
  exit 1
fi
if [[ "$(plugin_calver_normalize "v${today}.3")" != "v${today}.3.0" ]]; then
  echo "plugin_calver_normalize must append .0 to Flynn-style tags" >&2
  exit 1
fi
if [[ "$(plugin_calver_normalize "v${today}.3.1")" != "v${today}.3.1" ]]; then
  echo "plugin_calver_normalize must leave three-part tags alone" >&2
  exit 1
fi
popd >/dev/null

echo "ok next plugin version is vYYYYMMDD.N.P from git tags"
