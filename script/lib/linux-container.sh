#!/usr/bin/env bash
# Shared Docker Desktop wrapper (same idea as Flynn script/run-unit-tests).
# On macOS/Windows, run Linux for the host architecture so plugin-build can
# chroot + mksquashfs against Flynn's ubuntu-noble layer (amd64 or arm64).
# On Linux, callers run natively.
#
# Environment:
#   PLUGIN_BUILD_DOCKER=1   Force Docker even on Linux
#   PLUGIN_BUILD_DOCKER=0   Force native (fails on macOS without Linux deps)
#   PLUGIN_LINUX_IMAGE      Image tag [default: <repo>-dev:24.04-<arch>]
#   PLUGIN_LINUX_PLATFORM   Docker platform [default: linux/<host arch>]
#   PLUGIN_GOARCH           Override Go/OS-layer arch (amd64 or arm64)
#
# shellcheck disable=SC2034

plugin_default_linux_platform() {
  case "$(uname -m)" in
    arm64|aarch64) echo linux/arm64 ;;
    *) echo linux/amd64 ;;
  esac
}

PLUGIN_LINUX_PLATFORM="${PLUGIN_LINUX_PLATFORM:-$(plugin_default_linux_platform)}"

plugin_platform_arch() {
  case "${PLUGIN_LINUX_PLATFORM}" in
    *arm64*|*aarch64*) echo arm64 ;;
    *) echo amd64 ;;
  esac
}

PLUGIN_LINUX_IMAGE="${PLUGIN_LINUX_IMAGE:-$(basename "${ROOT:-flynn-plugin-postgres}")-dev:24.04}"
case "${PLUGIN_LINUX_IMAGE}" in
  *-amd64|*-arm64) ;;
  *) PLUGIN_LINUX_IMAGE="${PLUGIN_LINUX_IMAGE}-$(plugin_platform_arch)" ;;
esac
PLUGIN_LINUX_DOCKERFILE_DIR="${PLUGIN_LINUX_DOCKERFILE_DIR:-${ROOT}/script/docker/dev}"

plugin_host_os() {
  uname -s | tr '[:upper:]' '[:lower:]'
}

plugin_should_use_container() {
  if [[ -n "${FLYNN_PLUGIN_IN_CONTAINER:-}" ]]; then
    return 1
  fi
  case "${PLUGIN_BUILD_DOCKER:-}" in
    1|true|TRUE|yes|YES) return 0 ;;
    0|false|FALSE|no|NO) return 1 ;;
  esac
  [[ "$(plugin_host_os)" != "linux" ]]
}

plugin_ensure_docker() {
  local os
  os="$(plugin_host_os)"
  if ! command -v docker >/dev/null 2>&1; then
    echo >&2 "Docker is required to run this on ${os}."
    echo >&2 "Install Docker Desktop, or set PLUGIN_BUILD_DOCKER=0 to force native."
    exit 1
  fi
  if ! docker info >/dev/null 2>&1; then
    echo >&2 "Docker daemon is not running. Start Docker Desktop and retry."
    exit 1
  fi
}

plugin_image_stale() {
  if ! docker image inspect "${PLUGIN_LINUX_IMAGE}" >/dev/null 2>&1; then
    return 0
  fi
  local marker="${PLUGIN_LINUX_DOCKERFILE_DIR}/.image-built"
  if [[ ! -f "${marker}" ]]; then
    return 0
  fi
  if [[ "${PLUGIN_LINUX_DOCKERFILE_DIR}/Dockerfile" -nt "${marker}" ]] \
    || [[ "${PLUGIN_LINUX_DOCKERFILE_DIR}/entrypoint.sh" -nt "${marker}" ]]; then
    return 0
  fi
  return 1
}

plugin_run_in_linux() {
  plugin_ensure_docker

  if plugin_image_stale; then
    echo "==> Building Linux container ${PLUGIN_LINUX_IMAGE} (${PLUGIN_LINUX_PLATFORM})"
    docker build \
      --platform "${PLUGIN_LINUX_PLATFORM}" \
      -t "${PLUGIN_LINUX_IMAGE}" \
      -f "${PLUGIN_LINUX_DOCKERFILE_DIR}/Dockerfile" \
      "${PLUGIN_LINUX_DOCKERFILE_DIR}"
    touch "${PLUGIN_LINUX_DOCKERFILE_DIR}/.image-built"
  else
    echo "==> Using existing image ${PLUGIN_LINUX_IMAGE}"
  fi

  echo "==> Running in Docker (${PLUGIN_LINUX_IMAGE})"
  local docker_args=(
    --rm
    --privileged
    --platform "${PLUGIN_LINUX_PLATFORM}"
    -v "${ROOT}:/src:rw"
    -w /src
    -e FLYNN_PLUGIN_IN_CONTAINER=1
    -e CGO_ENABLED=0
    -e "GOFLAGS=-mod=mod -buildvcs=false"
    -e GOPRIVATE=github.com/randy-girard/*
    -e PLUGIN_VERSION
    -e VERSION
    -e GITHUB_REPOSITORY
    -e GITHUB_TOKEN
    -e GOPROXY
    -e FLYNN_VERSION
    -e FLYNN_GITHUB_REPO
    -e FLYNN_BASE_IMAGE
    -e FLYNN_IMAGES_JSON
    -e FLYNN_LAYERS_DIR
    -e PLUGIN_FLYNN_VERSION
    -e PLUGIN_GOARCH
    -e FLYNN_PLUGIN_BUILD_CACHE=/src/.plugin-build-cache
    -e GOCACHE=/src/.plugin-build-cache/go-build
    -e PLUGIN_BUILD_NO_PACKAGES_CACHE
    -e FLYNN_PLUGIN_BUILD_FORCE_WEB
  )

  if [[ -t 1 ]]; then
    docker_args+=(-t)
  fi
  if [[ -t 0 ]]; then
    docker_args+=(-i)
  fi

  mkdir -p "${ROOT}/.plugin-build-cache/go-build" "${ROOT}/.plugin-build-cache/packages"
  docker run "${docker_args[@]}" "${PLUGIN_LINUX_IMAGE}" "$@"
}
