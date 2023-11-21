# Getting started

## Show me a demo

Check out the [test/](/test) directory in this repo for a working basic demo. This installs and is capable of running: `aerospike`, `aerospike-prometheus-exporter`, `apache2`, `mariadb-server` and `postfix`. Some of these are already enabled by default upon installation. Just copy the `systemd-xxx` to `test/init` and go for it.

The demo also covers a specific scenario where a particular package requires `systemctl` commands to be available, or otherwise it won't successfully install. In this case, the `init` system is started during installation to allow said package to be installed.

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

### Usage with special behavioural parameters (log services to stderr - `docker logs`, do not log to `/var/log/services/`, disable PID tracking via `LD_PRELOD`):

```bash
docker run -itd --name bob mytest --log-to-stderr --no-logfile --no-pidtrack
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
