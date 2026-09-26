#!/bin/bash
# Shared finish for Flynn image layers that run apt-get.
# Drops documentation and apt indexes from the layer. Never wipe a bind-mounted
# host cache (ubuntu_ports_cache). Sourced from */img/packages.sh with cwd=repo.
set -e
export DEBIAN_FRONTEND=noninteractive
apt-get autoremove --purge -y >/dev/null 2>&1 || true
# Drop documentation *files* but keep the directory tree. dpkg, man-db, and
# update-alternatives fail if /usr/share/man/man1 or /usr/share/info is missing.
find /usr/share/doc /usr/share/man /usr/share/info /usr/share/lintian /usr/share/linda \
  -type f -delete 2>/dev/null || true
mkdir -p /usr/share/man/man1 /usr/share/info /usr/share/doc
rm -rf /var/cache/debconf/*-old /root/.cache || true
if ! mountpoint -q /var/cache/apt/archives 2>/dev/null; then
  rm -rf /var/cache/apt/archives/* "/var/cache/apt/archives/partial"/* || true
fi
if ! mountpoint -q /var/lib/apt/lists 2>/dev/null; then
  rm -rf /var/lib/apt/lists/* || true
fi
