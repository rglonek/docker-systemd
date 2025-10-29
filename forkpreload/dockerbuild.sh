#!/bin/bash
set -e
arch=$1
if [ -z "$arch" ]; then
    echo "Usage: $0 <arch>"
    echo "arch: amd64 or arm64"
    exit 1
fi

IMG="rockylinux:8"
PLATFORM="linux/amd64"
if [ "$arch" == "arm64" ]; then
    PLATFORM="linux/arm64"
fi
docker run --platform $PLATFORM -it --rm -v .:/mnt $IMG bash -c "dnf install -y make gcc && cd /mnt && make"
mv fork.so fork_$arch.so
mv fakefork.so fakefork_$arch.so
