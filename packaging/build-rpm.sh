#!/usr/bin/env bash
#
# Converts one installer .deb into the matching .rpm.
#
#   build-rpm.sh <rpm-arch> <deb> <outdir>
#
# alien is what produced every published rpm so far, so it stays: it is the
# only way to keep the package identity the same as the rpms already in the
# wild. The "-2" release suffix is alien's default version bump (the deb's
# implicit revision 1, plus one) and is part of the published naming, so the
# conversion deliberately does not pass --keep-version.
#
#   docker-systemd-<version>-2.<rpm-arch>.rpm
#
# alien insists on running as root, so callers outside a container need sudo.

set -euo pipefail

if [ "$#" -ne 3 ]; then
    echo "usage: $0 <x86_64|aarch64> <deb> <outdir>" >&2
    exit 2
fi

arch="$1"
deb="$2"
outdir="$3"

case "$arch" in
    x86_64 | aarch64) ;;
    *)
        echo "$0: unsupported architecture '$arch'" >&2
        exit 2
        ;;
esac

if [ ! -f "$deb" ]; then
    echo "$0: no such package: $deb" >&2
    exit 1
fi

deb="$(cd "$(dirname "$deb")" && pwd)/$(basename "$deb")"
mkdir -p "$outdir"
outdir="$(cd "$outdir" && pwd)"

# alien writes into the current directory and offers no way to redirect it.
workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT
(cd "$workdir" && alien --to-rpm --target "$arch" "$deb" >/dev/null)

built="$(find "$workdir" -maxdepth 1 -name '*.rpm' -print -quit)"
if [ -z "$built" ]; then
    echo "$0: alien produced no rpm for $deb" >&2
    exit 1
fi

out="${outdir}/$(basename "$built")"
mv "$built" "$out"

# Running under sudo means the rpm lands root-owned, and the release pipeline
# signs the packages afterwards as the unprivileged user that ran make:
# rpmsign opens the package read-write, so a root-owned file fails with
# "open failed: Permission denied". Hand it back to the caller.
if [ "$(id -u)" = 0 ] && [ -n "${SUDO_UID:-}" ]; then
    chown "${SUDO_UID}:${SUDO_GID:-$SUDO_UID}" "$out"
fi

echo "$out"
