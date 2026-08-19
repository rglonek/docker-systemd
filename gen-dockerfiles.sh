#!/usr/bin/env bash
#
# Regenerates the per-base-image Dockerfiles used by the conformance matrix
# (designs/docs/next/11-testing.md §4).
#
# alpine:3 is included as an expected-degraded case: it has no glibc, so the
# preload-based tracking this project used up to 0.5.x silently could not handle
# it at all. The subreaper backend handles it unchanged.

sources=(
    ubuntu:24.04
    ubuntu:22.04
    ubuntu:20.04
    debian:12
    debian:11
    debian:10
    rockylinux:9
    rockylinux:8
    quay.io/centos/centos:stream9
    alpine:3
)

for i in ${sources[@]}; do
targetName="${i/quay.io\/centos\/centos:stream/centos}"
targetName="${targetName/:/}"
targetName="Dockerfile-${targetName/./}"
cat <<EOF > $targetName
FROM golang:1.22 AS build
WORKDIR /src
COPY . /src/
RUN env CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /usr/sbin/init-docker-systemd ./cmd/docker-systemd

FROM $i
COPY --from=build /usr/sbin/init-docker-systemd /usr/sbin/init-docker-systemd
ENTRYPOINT ["/usr/sbin/init-docker-systemd"]
EOF
done
