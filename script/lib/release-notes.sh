#!/bin/bash
# GitHub release-note body for Flynn plugins. Groups conventional commits
# (feat/fix/docs/…) the same way Flynn's script/lib/release-notes.sh does.
#
# shellcheck shell=bash

_plugin_repo_root() {
  (cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
}

plugin_manifest_name() {
  local json
  json="$(_plugin_repo_root)/flynn-plugin.json"
  if [[ -f "${json}" ]]; then
    python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("name",""))' "${json}" 2>/dev/null || true
  fi
}

# Previous published v* tag, independent of the current branch. git describe
# from the parent of HEAD follows ancestry and can pick the wrong tag on a topic or
# release branch. We take the newest CalVer tag strictly older than VERSION
# (or the newest tag when VERSION is empty / not yet tagged).
plugin_previous_release_tag() {
  local version=${1:-}
  local tag
  while IFS= read -r tag; do
    [[ -z "${tag}" ]] && continue
    if [[ -n "${version}" && "${tag}" == "${version}" ]]; then
      continue
    fi
    if [[ -n "${version}" ]]; then
      if [[ "$(printf '%s\n%s\n' "${tag}" "${version}" | sort -V | tail -n1)" != "${version}" ]]; then
        continue
      fi
    fi
    printf '%s' "${tag}"
    return 0
  done < <(git tag -l 'v*' 2>/dev/null | sort -V -r)
}

plugin_commit_range_since_previous() {
  local version=${1:-}
  local prev head
  prev="$(plugin_previous_release_tag "${version}")"
  head="${version}"
  if [[ -z "${head}" ]] || ! git rev-parse --verify --quiet "${head}^{commit}" >/dev/null 2>&1; then
    head="HEAD"
  fi
  if [[ -n "${prev}" ]]; then
    printf '%s' "${prev}..${head}"
  else
    printf '%s' "${head}"
  fi
}

# Drop automated coverage-badge commits from GitHub release bodies.
plugin_omit_coverage_badge_notes() {
  grep -viF -- 'ci: update coverage badge' || true
}

# Print categorized markdown for COMMIT_RANGE (e.g. v20260714.0..HEAD).
plugin_categorized_release_notes() {
  local range=$1
  local feat fix chore docs refactor perf testc build ci other notes=""

  feat="$(git log --no-merges --pretty=format:"- %s (%h)" --grep="^feat" "${range}" 2>/dev/null | plugin_omit_coverage_badge_notes || true)"
  fix="$(git log --no-merges --pretty=format:"- %s (%h)" --grep="^fix" "${range}" 2>/dev/null | plugin_omit_coverage_badge_notes || true)"
  chore="$(git log --no-merges --pretty=format:"- %s (%h)" --grep="^chore" "${range}" 2>/dev/null | plugin_omit_coverage_badge_notes || true)"
  docs="$(git log --no-merges --pretty=format:"- %s (%h)" --grep="^docs" "${range}" 2>/dev/null | plugin_omit_coverage_badge_notes || true)"
  refactor="$(git log --no-merges --pretty=format:"- %s (%h)" --grep="^refactor" "${range}" 2>/dev/null | plugin_omit_coverage_badge_notes || true)"
  perf="$(git log --no-merges --pretty=format:"- %s (%h)" --grep="^perf" "${range}" 2>/dev/null | plugin_omit_coverage_badge_notes || true)"
  testc="$(git log --no-merges --pretty=format:"- %s (%h)" --grep="^test" "${range}" 2>/dev/null | plugin_omit_coverage_badge_notes || true)"
  build="$(git log --no-merges --pretty=format:"- %s (%h)" --grep="^build" "${range}" 2>/dev/null | plugin_omit_coverage_badge_notes || true)"
  ci="$(git log --no-merges --pretty=format:"- %s (%h)" --grep="^ci" "${range}" 2>/dev/null | plugin_omit_coverage_badge_notes || true)"
  other="$(git log --no-merges --pretty=format:"- %s (%h)" "${range}" 2>/dev/null | grep -v -E "^- (feat|fix|chore|docs|refactor|perf|test|build|ci)" | plugin_omit_coverage_badge_notes || true)"

  if [[ -n "${feat}" ]]; then
    notes+="### ✨ Features

${feat}

"
  fi
  if [[ -n "${fix}" ]]; then
    notes+="### 🐛 Bug Fixes

${fix}

"
  fi
  if [[ -n "${perf}" ]]; then
    notes+="### ⚡ Performance

${perf}

"
  fi
  if [[ -n "${refactor}" ]]; then
    notes+="### ♻️ Refactoring

${refactor}

"
  fi
  if [[ -n "${docs}" ]]; then
    notes+="### 📚 Documentation

${docs}

"
  fi
  if [[ -n "${testc}" ]]; then
    notes+="### 🧪 Tests

${testc}

"
  fi
  if [[ -n "${build}" ]]; then
    notes+="### 🏗️ Build

${build}

"
  fi
  if [[ -n "${ci}" ]]; then
    notes+="### 👷 CI

${ci}

"
  fi
  if [[ -n "${chore}" ]]; then
    notes+="### 🔧 Chores

${chore}

"
  fi
  if [[ -n "${other}" ]]; then
    notes+="### 📦 Other Changes

${other}

"
  fi

  if [[ -z "${notes}" ]]; then
    notes="No changes recorded."
  fi
  printf '%s' "${notes}"
}

# Full GitHub release body (changelog groups + plugin install).
plugin_github_release_notes() {
  local version=$1
  local repo=$2
  local name prev range compare changelog categorized
  name="$(plugin_manifest_name)"
  if [[ -z "${name}" ]]; then
    name="${repo##*/}"
    name="${name#flynn-plugin-}"
  fi
  prev="$(plugin_previous_release_tag "${version}")"
  range="$(plugin_commit_range_since_previous "${version}")"
  compare="${prev:-initial}...${version}"
  changelog="https://github.com/${repo}/compare/${compare}"
  categorized="$(plugin_categorized_release_notes "${range}")"

  cat <<EOF
## ${name} ${version}

**Full Changelog**: [${compare}](${changelog})

${categorized}
---

## Install

On a Flynn cluster host:

\`\`\`text
sudo flynn-host plugin install ${name} --ref ${version}
\`\`\`

Or from this repository:

\`\`\`text
sudo flynn-host plugin install https://github.com/${repo}.git --ref ${version}
\`\`\`

\`flynn-host plugin install\` pulls \`image.json\` from this release:

https://github.com/${repo}/releases/download/${version}/image.json

Layers are Flynn ubuntu-noble plus this plugin's delta (\`{id}.squashfs\` next to that Artifact).
EOF
}
