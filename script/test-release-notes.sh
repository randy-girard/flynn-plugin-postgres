#!/bin/bash
# Regression: GitHub Releases must group conventional commits like Flynn,
# not a one-line "plugin image for VERSION" blurb.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "${ROOT}"
notes_lib="${ROOT}/script/lib/release-notes.sh"
workflow="${ROOT}/.github/workflows/release.yml"

need_file() {
  local path=$1 msg=$2
  if [[ ! -f "${path}" ]]; then
    echo "${msg}" >&2
    echo "  missing ${path}" >&2
    exit 1
  fi
}

need_file "${notes_lib}" "grouped notes must live in script/lib/release-notes.sh"
need_file "${workflow}" "release workflow must exist"

if ! grep -q 'plugin_categorized_release_notes' "${notes_lib}"; then
  echo "release-notes lib must group commits by conventional type" >&2
  exit 1
fi
if ! grep -q '### ✨ Features' "${notes_lib}"; then
  echo "release-notes lib must keep the Features heading" >&2
  exit 1
fi
if ! grep -q '### 🐛 Bug Fixes' "${notes_lib}"; then
  echo "release-notes lib must keep the Bug Fixes heading" >&2
  exit 1
fi
if grep -q "git log --pretty=format:'- %s (%h)'" "${workflow}"; then
  echo "GitHub Actions must not write a flat commit list for the release body" >&2
  exit 1
fi
if ! grep -q 'plugin_github_release_notes' "${workflow}"; then
  echo "release workflow must generate notes via plugin_github_release_notes" >&2
  exit 1
fi
if ! grep -q -- '--notes-file' "${workflow}"; then
  echo "release workflow must pass grouped notes with --notes-file" >&2
  exit 1
fi
if grep -B8 'PRERELEASE=true' "${workflow}" | grep -q 'inputs.draft'; then
  echo "full releases must not force prerelease when draft is set" >&2
  exit 1
fi
if grep -A8 '^[[:space:]]*draft:' "${workflow}" | grep -q 'default: true'; then
  echo "release workflow draft input must default to false (published release)" >&2
  exit 1
fi

# shellcheck source=/dev/null
source "${notes_lib}"

sample="$(mktemp)"
trap 'rm -f "${sample}"' EXIT
plugin_github_release_notes "v20990101.0" "example/flynn-plugin" > "${sample}"
# Any conventional-commit group counts (docs-only or chore-only ranges emit
# only their own heading), or the empty-range fallback.
if ! grep -qE '^### |No changes recorded' "${sample}"; then
  echo "generated notes must include a grouped section or an empty-range fallback" >&2
  head -n 40 "${sample}" >&2
  exit 1
fi
if ! grep -q 'flynn-host plugin install' "${sample}"; then
  echo "generated notes must include plugin install instructions" >&2
  exit 1
fi
if ! grep -q 'Full Changelog' "${sample}"; then
  echo "generated notes must include a Full Changelog compare link" >&2
  exit 1
fi
if grep -q 'Flynn plugin image for' "${sample}"; then
  echo "notes must not replace grouped changes with a one-line image blurb" >&2
  exit 1
fi
if ! grep -q 'plugin_omit_coverage_badge_notes' "${notes_lib}"; then
  echo "release-notes lib must filter coverage-badge commits" >&2
  exit 1
fi
filtered="$(printf '%s\n' '- ci: add dispatch_plugins (abc123)' '- ci: update coverage badge [skip ci] (def456)' | plugin_omit_coverage_badge_notes)"
if ! printf '%s\n' "${filtered}" | grep -q 'dispatch_plugins'; then
  echo "coverage-badge filter must keep other ci commits" >&2
  exit 1
fi
if printf '%s\n' "${filtered}" | grep -qi 'ci: update coverage badge'; then
  echo "coverage-badge filter must drop update coverage badge commits" >&2
  exit 1
fi
if grep -qi 'ci: update coverage badge' "${sample}"; then
  echo "generated notes must omit coverage badge commits" >&2
  grep -i 'ci: update coverage badge' "${sample}" >&2
  exit 1
fi

"${ROOT}/script/test-calver.sh"

if ! grep -q 'sort -V' "${notes_lib}"; then
  echo "release-notes lib must pick the previous tag by version, not git describe HEAD^" >&2
  exit 1
fi
if grep -q 'describe --tags --abbrev=0 --match' "${notes_lib}"; then
  echo "release-notes lib must not use git describe for the previous tag (wrong tag on topic branches)" >&2
  exit 1
fi
if ! grep -q 'plugin_commit_range_since_previous' "${notes_lib}"; then
  echo "release-notes lib must range previous tag..this tag" >&2
  exit 1
fi

echo "ok GitHub release notes are grouped like Flynn"
