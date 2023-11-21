FROM ubuntu:22.04

ADD init /sbin/

ADD installer.sh /tmp/
WORKDIR /tmp/
RUN /bin/bash /tmp/installer.sh
WORKDIR /

ENTRYPOINT ["/sbin/init"]
