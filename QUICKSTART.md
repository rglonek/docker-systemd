# Getting started

## Getting started with prebuilt images

Prebuild images are available from latest with just the binary entrypoint added.

```
docker run -itd robertglonek/ubuntu:24.04
docker run -itd robertglonek/ubuntu:22.04
docker run -itd robertglonek/ubuntu:20.04

docker run -itd robertglonek/debian:12
docker run -itd robertglonek/debian:11
docker run -itd robertglonek/debian:10

docker run -itd robertglonek/rockylinux:9
docker run -itd robertglonek/rockylinux:8

docker run -itd robertglonek/centos:stream9
```

## Show me a demo

`make conformance-quick` builds an image from `Dockerfile-ubuntu2404` and runs
[test/conformance/run.sh](test/conformance/run.sh) against it: it boots the
manager, installs real distro packages so their maintainer scripts run, and
asserts that each one enables, starts, reports a plausible main PID, stops
leaving zero surviving processes, survives ten restarts in a row, and shows up
in `journalctl -u`. `make conformance` does the same across all ten supported
base images.

This also covers the case where a package's own installation calls `systemctl`:
the control socket is bound before any unit starts, precisely so that
maintainer scripts running inside `docker build` work.

## Manual installation and usage

Head to the [releases page](/../../releases) and download the relevant binary (two are provided for `aarch64/arm64` and `amd64/x86_64` platforms).

### The most basic `Dockerfile`:

```dockerfile
FROM ubuntu:22.04
ADD systemd-amd64 /usr/sbin/init-docker-systemd
ENTRYPOINT ["/usr/sbin/init-docker-systemd"]
```

### The most basic usage:

```bash
docker build -t mytest .
docker run -itd --name bob mytest
docker logs -f bob
docker exec -it bob systemctl list
```

### Usage with behavioural parameters

Mirror unit logs into `docker logs`, skip the log files, and turn the manager's
own logging up:

```bash
docker run -itd --name bob mytest --log-to-stderr --no-logfile --log-level=debug
```

Mirror only one noisy unit rather than everything:

```bash
docker run -itd --name bob mytest --log-to-stderr=nginx.service
```

Check what the manager probed at boot — the selected process-tracking backend,
the runtime paths, and every unit directive it had to ignore:

```bash
docker exec bob systemctl show --property=Capabilities
```

Give shutdown room for a slow database. `docker stop -t <n>` should be at least
the sum of the critical units' `TimeoutStopSec`:

```bash
docker run -itd --name bob mytest --shutdown-timeout=120s
docker stop -t 120 bob
```

## Ubuntu/Debian using apt repository

### Dockerfile

```Dockerfile
FROM ubuntu:22.04
RUN apt-get update && \
  apt-get -y install ca-certificates curl gnupg && \
  install -m 0755 -d /etc/apt/keyrings && \
  curl -fsSL https://rglonek.github.io/docker-systemd/ubuntu/KEY.gpg | gpg --dearmor -o /etc/apt/keyrings/rglonek.gpg && \
  chmod a+r /etc/apt/keyrings/rglonek.gpg
RUN echo "deb [arch="$(dpkg --print-architecture)" signed-by=/etc/apt/keyrings/rglonek.gpg] https://rglonek.github.io/docker-systemd/ubuntu ./" | tee -a /etc/apt/sources.list.d/rglonek.list > /dev/null && \
  apt-get update && \
  apt-get -y install docker-systemd
ENTRYPOINT ["/usr/sbin/init-docker-systemd"]
```

### Script form

```bash
[ $UID -eq 0 ] && APT="apt-get" || APT="sudo apt-get"
$APT update
$APT -y install ca-certificates curl gnupg sudo
sudo install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://rglonek.github.io/docker-systemd/ubuntu/KEY.gpg | sudo gpg --dearmor -o /etc/apt/keyrings/rglonek.gpg
sudo chmod a+r /etc/apt/keyrings/rglonek.gpg

echo "deb [arch="$(dpkg --print-architecture)" signed-by=/etc/apt/keyrings/rglonek.gpg] https://rglonek.github.io/docker-systemd/ubuntu ./" | sudo tee /etc/apt/sources.list.d/rglonek.list > /dev/null
sudo apt-get update
sudo apt-get -y install docker-systemd
sudo /usr/sbin/init-docker-systemd
```

## RPM repository

```Dockerfile
FROM centos:7
RUN rpm --import https://rglonek.github.io/docker-systemd/ubuntu/KEY.gpg && \
  yum -y install yum-utils && \
  yum-config-manager --add-repo https://rglonek.github.io/docker-systemd/rh && \
  yum -y install docker-systemd
ENTRYPOINT ["/usr/sbin/init-docker-systemd"]
```

Did you know: centos-stream official repository is: `quay.io/centos/centos:streamX`. Use `FROM:quay.io/centos/centos:stream8` to use centos 8 as base and `FROM:quay.io/centos/centos:stream9` to use centos 9 as base.

## Getting started inside the container

Just use `systemctl/journalctl/service` commands inside the container as one normally would for the most part.

Installing a package makes its units visible straight away — unlike on a real systemd host, there is no need to run `systemctl daemon-reload` first, because the unit directories are watched. See [Automatic `daemon-reload`](README.md#automatic-daemon-reload).

## Example apache2 web server install

Dockerfile:

```dockerfile
FROM ubuntu:22.04
ADD systemd-amd64 /usr/sbin/init-docker-systemd
RUN apt update && apt -y install apache2
ENTRYPOINT ["/usr/sbin/init-docker-systemd"]
```

Install:

```bash
docker build -t apache2 .
docker run -itd --name apache2 apache2
docker exec -it apache2 systemctl enable apache2
docker exec -it apache2 systemctl start apache2
```
