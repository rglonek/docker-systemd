#!/usr/bin/env bash
#
# Builds one installer .deb from an already-built binary.
#
#   build-deb.sh <deb-arch> <version> <binary> <outdir>
#
# The output is named to the convention the published releases use:
#
#   docker-systemd_<version>_<deb-arch>.deb
#
# dpkg-deb --root-owner-group makes the payload root:root without the build
# running as root, so this needs no sudo on a developer's machine or in CI.

set -euo pipefail

if [ "$#" -ne 4 ]; then
    echo "usage: $0 <amd64|arm64> <version> <binary> <outdir>" >&2
    exit 2
fi

arch="$1"
version="$2"
binary="$3"
outdir="$4"

case "$arch" in
    amd64 | arm64) ;;
    *)
        echo "$0: unsupported architecture '$arch'" >&2
        exit 2
        ;;
esac

if [ ! -f "$binary" ]; then
    echo "$0: no such binary: $binary" >&2
    exit 1
fi

staging="$(mktemp -d)"
trap 'rm -rf "$staging"' EXIT

# mktemp gives 0700; the package's own root directory has to be 0755 like any
# other directory dpkg unpacks.
chmod 0755 "$staging"
mkdir -p "$staging/DEBIAN" "$staging/usr/sbin"
install -m 0755 "$binary" "$staging/usr/sbin/init-docker-systemd"

cat <<EOF > "$staging/DEBIAN/control"
Website: www.glonek.io
Maintainer: Robert Glonek <robert@glonek.uk>
Name: docker-systemd
Package: docker-systemd
Section: docker-systemd
Version: ${version}
Architecture: ${arch}
Description: Systemd-service-file-compatible dropin for docker containers, docker as VM
EOF

mkdir -p "$outdir"
out="${outdir}/docker-systemd_${version}_${arch}.deb"
dpkg-deb --root-owner-group -Zxz -b "$staging" "$out" >/dev/null

echo "$out"
