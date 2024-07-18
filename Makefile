.NOTPARALLEL:

ver:=$(shell cat VERSION)
define _amddebscript
ver=$(cat VERSION)
cat <<EOF > bin/deb/DEBIAN/control
Website: www.glonek.io
Maintainer: Robert Glonek <robert@glonek.uk>
Name: docker-systemd
Package: docker-systemd
Section: docker-systemd
Version: ${ver}
Architecture: amd64
Description: Systemd-service-file-compatible dropin for docker containers, docker as VM
EOF
endef
export amddebscript = $(value _amddebscript)
define _armdebscript
ver=$(cat VERSION)
cat <<EOF > bin/deb/DEBIAN/control
Website: www.glonek.io
Maintainer: Robert Glonek <robert@glonek.uk>
Name: docker-systemd
Package: docker-systemd
Section: docker-systemd
Version: ${ver}
Architecture: arm64
Description: Systemd-service-file-compatible dropin for docker containers, docker as VM
EOF
endef
export armdebscript = $(value _armdebscript)

.PHONY: cleanbuild
cleanbuild: clean build shrink

.PHONY: clean
clean:
	rm -rf ./bin

.PHONY: build
build:
	mkdir bin
	cp forkpreload/*.so systemd/
	env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o bin/systemd-amd64
	env CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o bin/systemd-arm64

.PHONY: shrink
shrink:
	cd bin && upx systemd-amd64
	cd bin && upx systemd-arm64

.PHONY: test
test:
	cp bin/systemd-amd64 test/init
	cd test && docker build -t init:test .
	docker run -itd --name bob init:test --debug-reaper
	echo "docker exec -it bob bash"
	docker logs -f bob

.PHONY: stoptest
stoptest:
	docker stop -t 1 bob
	docker rm bob

.PHONY: buildtest
buildtest: cleanbuild test

.PHONY: updatetest
updatetest:
	docker stop -t 1 bob || echo
	make cleanbuild
	docker cp bin/systemd-amd64 bob:/sbin/init
	docker start bob
	docker logs -f bob

.PHONY: pkg-deb-amd64
pkg-deb-amd64:
	cp bin/systemd-amd64 bin/init-docker-systemd
	rm -rf bin/deb
	mkdir -p bin/deb/DEBIAN
	mkdir -p bin/deb/usr/sbin
	@ eval "$$amddebscript"
	mv bin/init-docker-systemd bin/deb/usr/sbin/init-docker-systemd
	sudo dpkg-deb -Zxz -b bin/deb
	mv bin/deb.deb repo/ubuntu/docker-systemd_${ver}_amd64.deb
	rm -rf bin/deb

.PHONY: pkg-deb-arm64
pkg-deb-arm64:
	cp bin/systemd-arm64 bin/init-docker-systemd
	rm -rf bin/deb
	mkdir -p bin/deb/DEBIAN
	mkdir -p bin/deb/usr/sbin
	@ eval "$$armdebscript"
	mv bin/init-docker-systemd bin/deb/usr/sbin/init-docker-systemd
	sudo dpkg-deb -Zxz -b bin/deb
	mv bin/deb.deb repo/ubuntu/docker-systemd_${ver}_arm64.deb
	rm -rf bin/deb

.PHONY: pkg-deb
pkg-deb: pkg-deb-amd64 pkg-deb-arm64
	echo "${gpgprivate}" |base64 -d > /tmp/private.asc
	gpg --import /tmp/private.asc
	rm -f repo/ubuntu/Packages* repo/ubuntu/Release*
	cd repo/ubuntu && dpkg-scanpackages --multiversion . > Packages
	cd repo/ubuntu && gzip -k -f Packages
	cd repo/ubuntu && apt-ftparchive release . > Release
	cd repo/ubuntu && gpg --default-key "${email}" -abs -o - Release > Release.gpg
	cd repo/ubuntu && gpg --default-key "${email}" --clearsign -o - Release > InRelease

# pre-req:
#   apt update && apt -y install gnupg gzip upx wget make apt-utils sudo dpkg-dev
#   wget -q https://go.dev/dl/go1.21.4.linux-amd64.tar.gz && rm -rf /usr/local/go && tar -C /usr/local -xzf go1.21.4.linux-amd64.tar.gz && echo "export PATH=$PATH:/usr/local/go/bin" >> ~/.$(cat /proc/$$/comm)rc
#   source ~/.$(cat /proc/$$/comm)rc
