# CalVer helpers for Flynn plugin releases: vYYYYMMDD.N.P
#
# Flynn itself stays vYYYYMMDD.N. Plugins add a patch so a plugin-only
# change can ship while flynn_version (ubuntu-noble) stays the same.
# A two-part tag vYYYYMMDD.N is treated as vYYYYMMDD.N.0.

plugin_calver_ok() {
  [[ "${1:-}" =~ ^v[0-9]{8}\.[0-9]+\.[0-9]+$ ]]
}

plugin_calver_tag_ok() {
  [[ "${1:-}" =~ ^v[0-9]{8}\.[0-9]+(\.[0-9]+)?$ ]]
}

# Normalize a Flynn-style vYYYYMMDD.N tag to a plugin tag (append .0).
plugin_calver_normalize() {
  local v=$1
  if [[ "${v}" =~ ^v[0-9]{8}\.[0-9]+$ ]]; then
    echo "${v}.0"
    return
  fi
  echo "${v}"
}

# next_release_version takes the previous plugin tag and returns the next
# unused vYYYYMMDD.N.P for today (UTC via `date`). Same-day tags increment
# the last number; a two-part tag becomes .1 (the two-part tag is .0).
next_release_version() {
  local previous=$1
  local date
  date=$(date +%Y%m%d)
  local n p
  if [[ ${previous} =~ ^v${date}\.([0-9]+)\.([0-9]+)$ ]]; then
    n="${BASH_REMATCH[1]}"
    p="${BASH_REMATCH[2]}"
    echo "v${date}.${n}.$((p + 1))"
    return
  fi
  if [[ ${previous} =~ ^v${date}\.([0-9]+)$ ]]; then
    n="${BASH_REMATCH[1]}"
    echo "v${date}.${n}.1"
    return
  fi
  echo "v${date}.0.0"
}

# next_release_version_from_tags inspects local git tags for today and returns
# the next unused vYYYYMMDD.N.P (vYYYYMMDD.0.0 when none exist). Ignores
# suffixes like vYYYYMMDD.0.0-smoke so they cannot poison the integer increment.
next_release_version_from_tags() {
  local date
  date=$(date +%Y%m%d)
  local latest=""
  latest=$(git tag -l "v${date}.*" 2>/dev/null | { grep -E "^v[0-9]{8}\\.[0-9]+(\\.[0-9]+)?$" || true; } | sort -V | tail -n1)
  next_release_version "${latest}"
}
