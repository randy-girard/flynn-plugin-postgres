# Upload Flynn GitHub Release assets one file at a time.
#
# `gh release create TAG file1 file2 ...` creates the release immediately, then
# uploads every asset with no progress. A Flynn release is ~80–90 files and
# several hundred MiB of squashfs, so that step looks hung, cannot resume after
# a cancelled run, and leaves extra untagged drafts when CI is stopped mid-upload.
#
# This helper:
# - always creates (or reuses) a draft first
# - uploads each file with retries, a size-based timeout, and a log line
# - skips assets already uploaded at the same size (resume)
# - publishes only after every file is on the release (unless --draft)
# - disables HTTP/2 (GODEBUG=http2client=0); large GitHub uploads can stall on it
#
# shellcheck shell=bash

_discord_release_lib_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=script/lib/discord-release.sh
source "${_discord_release_lib_dir}/discord-release.sh"

GITHUB_RELEASE_ASSET_LIMIT=2147483648
GITHUB_RELEASE_UPLOAD_ATTEMPTS="${GITHUB_RELEASE_UPLOAD_ATTEMPTS:-4}"
GITHUB_RELEASE_RETRY_SLEEP="${GITHUB_RELEASE_RETRY_SLEEP:-5}"

github_release_file_size() {
  stat -c%s "$1" 2>/dev/null || stat -f%z "$1"
}

github_release_human_size() {
  awk -v n="$1" 'BEGIN {
    if (n >= 1073741824) printf "%.1fGiB", n/1073741824
    else if (n >= 1048576) printf "%.1fMiB", n/1048576
    else if (n >= 1024) printf "%.1fKiB", n/1024
    else printf "%dB", n
  }'
}

# Seconds for one upload: 2 minutes plus 4s/MiB, capped at 20 minutes.
github_release_upload_timeout() {
  local bytes=$1
  local mb=$(( (bytes + 1048575) / 1048576 ))
  local secs=$((120 + mb * 4))
  if (( secs > 1200 )); then
    secs=1200
  fi
  printf '%s' "${secs}"
}

github_release_cmd() {
  local gh=${GH:-gh}
  env \
    GH_NO_UPDATE_NOTIFIER=1 \
    GODEBUG="${GODEBUG:+${GODEBUG},}http2client=0" \
    "${gh}" "$@"
}

github_release_cmd_timeout() {
  local secs=$1
  shift
  local gh=${GH:-gh}
  if command -v timeout >/dev/null 2>&1; then
    timeout --signal=TERM --kill-after=20s "${secs}" env \
      GH_NO_UPDATE_NOTIFIER=1 \
      GODEBUG="${GODEBUG:+${GODEBUG},}http2client=0" \
      "${gh}" "$@"
  else
    github_release_cmd "$@"
  fi
}

# Print absolute/relative file paths, one per line, smallest first.
# Skips mega tarballs. Fails if any file is >= 2GiB.
github_release_list_files() {
  local dir=$1
  local f base size
  local -a files=()
  shopt -s nullglob
  for f in "${dir}"/*; do
    [[ -f "${f}" ]] || continue
    base="$(basename "${f}")"
    if [[ "${base}" == flynn-*.tar.gz ]]; then
      echo "Skipping ${base} (mega-tarball; use individual assets)" >&2
      continue
    fi
    size="$(github_release_file_size "${f}")"
    if [[ "${size}" -ge "${GITHUB_RELEASE_ASSET_LIMIT}" ]]; then
      echo "ERROR: ${base} is ${size} bytes (>= 2GiB GitHub release asset limit)" >&2
      return 1
    fi
    files+=("${size}:${f}")
  done
  if [[ ${#files[@]} -eq 0 ]]; then
    echo "ERROR: no release files found in ${dir}" >&2
    return 1
  fi
  local line
  while IFS= read -r line; do
    printf '%s\n' "${line#*:}"
  done < <(printf '%s\n' "${files[@]}" | sort -n)
}

github_release_existing_tsv() {
  local version=$1 repo=$2
  github_release_cmd release view "${version}" --repo "${repo}" --json assets \
    --jq '.assets[] | select(.state=="uploaded") | [.name,(.size|tostring)] | @tsv' \
    2>/dev/null || true
}

github_release_asset_present() {
  local tsv=$1 name=$2 size=$3
  local line
  while IFS= read -r line; do
    [[ -z "${line}" ]] && continue
    if [[ "${line}" == "${name}"$'\t'"${size}" ]]; then
      return 0
    fi
  done <<EOF
${tsv}
EOF
  return 1
}

github_release_upload_one() {
  local version=$1 repo=$2 file=$3 index=$4 total=$5
  local name size human secs attempt
  name="$(basename "${file}")"
  size="$(github_release_file_size "${file}")"
  human="$(github_release_human_size "${size}")"
  secs="$(github_release_upload_timeout "${size}")"

  echo "Uploading ${index}/${total} ${name} (${human}, timeout ${secs}s)"
  attempt=1
  while (( attempt <= GITHUB_RELEASE_UPLOAD_ATTEMPTS )); do
    if github_release_cmd_timeout "${secs}" release upload "${version}" "${file}" \
      --repo "${repo}" --clobber; then
      echo "Uploaded ${name}"
      return 0
    fi
    echo "upload ${name} attempt ${attempt}/${GITHUB_RELEASE_UPLOAD_ATTEMPTS} failed" >&2
    if (( attempt == GITHUB_RELEASE_UPLOAD_ATTEMPTS )); then
      echo "ERROR: giving up on ${name}" >&2
      return 1
    fi
    sleep $((attempt * GITHUB_RELEASE_RETRY_SLEEP))
    attempt=$((attempt + 1))
  done
}

github_release_id() {
  local version=$1 repo=$2
  github_release_cmd release view "${version}" --repo "${repo}" --json id --jq .id 2>/dev/null || true
}

# Delete leftover untagged drafts that share this tag (cancelled CI runs).
github_release_delete_stale_drafts() {
  local repo=$1 tag=$2 keep_id=$3
  local ids id
  keep_id="${keep_id:-0}"
  ids="$(github_release_cmd api --paginate "repos/${repo}/releases" --jq \
    ".[] | select(.tag_name==\"${tag}\" and .draft==true and .id != ${keep_id}) | .id" \
    2>/dev/null || true)"
  while IFS= read -r id; do
    [[ -z "${id}" ]] && continue
    echo "Deleting leftover draft release ${id} for ${tag}"
    github_release_cmd api -X DELETE "repos/${repo}/releases/${id}" >/dev/null || \
      echo "warning: could not delete draft ${id}" >&2
  done <<EOF
${ids}
EOF
}

# github_release_publish --version TAG --repo OWNER/REPO --title TITLE \
#   --notes-file FILE --dir DIR --draft true|false --prerelease true|false
github_release_publish() {
  local version="" repo="" title="" notes="" dir="" draft="false" prerelease="false"
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --version) version=$2; shift 2 ;;
      --repo) repo=$2; shift 2 ;;
      --title) title=$2; shift 2 ;;
      --notes-file) notes=$2; shift 2 ;;
      --dir) dir=$2; shift 2 ;;
      --draft) draft=$2; shift 2 ;;
      --prerelease) prerelease=$2; shift 2 ;;
      *)
        echo "unknown github_release_publish argument: $1" >&2
        return 1
        ;;
    esac
  done
  if [[ -z "${version}" || -z "${repo}" || -z "${title}" || -z "${notes}" || -z "${dir}" ]]; then
    echo "github_release_publish requires --version --repo --title --notes-file --dir" >&2
    return 1
  fi

  local -a files=()
  local f list
  list="$(github_release_list_files "${dir}")" || return 1
  while IFS= read -r f; do
    [[ -n "${f}" ]] && files+=("${f}")
  done <<< "${list}"

  local total=${#files[@]}
  local bytes=0
  for f in "${files[@]}"; do
    bytes=$((bytes + $(github_release_file_size "${f}")))
  done
  echo "Publishing ${total} assets (${bytes} bytes, $(github_release_human_size "${bytes}")) to ${repo} ${version}"

  local create_args=(--repo "${repo}" --title "${title}" --notes-file "${notes}" --draft)
  if [[ "${prerelease}" == "true" ]]; then
    create_args+=(--prerelease)
  fi

  if github_release_cmd release view "${version}" --repo "${repo}" >/dev/null 2>&1; then
    echo "Reusing existing GitHub release ${version}"
    github_release_cmd release edit "${version}" --repo "${repo}" --title "${title}" \
      --notes-file "${notes}" >/dev/null || true
  else
    echo "Creating draft GitHub release ${version}"
    github_release_cmd release create "${version}" "${create_args[@]}"
  fi

  local keep_id
  keep_id="$(github_release_id "${version}" "${repo}")"
  github_release_delete_stale_drafts "${repo}" "${version}" "${keep_id}"

  local existing
  existing="$(github_release_existing_tsv "${version}" "${repo}")"

  local i=0 name size
  for f in "${files[@]}"; do
    i=$((i + 1))
    name="$(basename "${f}")"
    size="$(github_release_file_size "${f}")"
    if github_release_asset_present "${existing}" "${name}" "${size}"; then
      echo "Skipping ${i}/${total} ${name} (already uploaded)"
      continue
    fi
    github_release_upload_one "${version}" "${repo}" "${f}" "${i}" "${total}"
    existing="${existing}"$'\n'"${name}"$'\t'"${size}"
  done

  if [[ "${draft}" == "true" ]]; then
    echo "Leaving ${version} as a draft"
  else
    echo "Publishing ${version}"
    local edit_args=(--repo "${repo}" --draft=false)
    if [[ "${prerelease}" == "true" ]]; then
      edit_args+=(--prerelease)
    else
      edit_args+=(--prerelease=false)
    fi
    github_release_cmd release edit "${version}" "${edit_args[@]}"
  fi

  echo "Created release ${version}"
  github_release_cmd release view "${version}" --repo "${repo}"
  local html_url=""
  html_url="$(github_release_cmd release view "${version}" --repo "${repo}" --json url --jq .url || true)"
  if ! discord_notify_github_release \
    --repo "${repo}" \
    --version "${version}" \
    --title "${title}" \
    --notes-file "${notes}" \
    --draft "${draft}" \
    --prerelease "${prerelease}" \
    --url "${html_url}"; then
    echo "ERROR: Discord release notify failed for ${version}" >&2
    return 1
  fi
}
