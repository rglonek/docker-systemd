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
)

for i in ${sources[@]}; do
targetName="${i/quay.io\/centos\/centos:stream/centos}"
targetName="${targetName/:/}"
targetName="Dockerfile-${targetName/./}"
cat <<EOF > $targetName
FROM golang:1.22 AS build
WORKDIR /src
COPY . /src/
RUN cp forkpreload/*.so systemd/
RUN env CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /usr/sbin/init-docker-systemd .

FROM $i
COPY --from=build /usr/sbin/init-docker-systemd /usr/sbin/init-docker-systemd
ENTRYPOINT ["/usr/sbin/init-docker-systemd"]
EOF
done
