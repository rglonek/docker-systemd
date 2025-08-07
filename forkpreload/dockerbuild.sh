set -e
# $1 == amd64 || arm64
docker run -it --rm -v .:/mnt ubuntu:24.04 bash -c "apt update && apt -y install make gcc && cd /mnt && make"
mv fork.so fork_$1.so
mv fakefork.so fakefork_$1.so
