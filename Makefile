.NOTPARALLEL:

ver:=$(shell cat VERSION)
commit:=$(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS:=-s -w -X main.version=$(ver)

# The conformance matrix of designs/docs/next/11-testing.md §4. alpine:3 is the
# expected-degraded case: it has no glibc, so the preload-based tracking this
# project used up to 0.5.x could not handle it at all.
IMAGES:=ubuntu2404 ubuntu2204 ubuntu2004 debian12 debian11 debian10 \
        rockylinux9 rockylinux8 centos9 alpine3

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
cleanbuild: clean build

.PHONY: clean
clean:
	rm -rf ./bin

# CGO_ENABLED=0 produces a genuinely static binary with no libc dependency, so
# one file runs unchanged on glibc and musl images (10 §2). There is
# deliberately no UPX step: every supervisor is a re-exec of this image and
# would decompress its own private copy into anonymous memory (F10).
.PHONY: build
build:
	mkdir -p bin
	env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/systemd-amd64 ./cmd/docker-systemd
	env CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/systemd-arm64 ./cmd/docker-systemd

.PHONY: lint
lint:
	gofmt -l . | tee /dev/stderr | (! read)
	go vet ./...
	go vet -tags=proc ./...

# L1: pure unit tests. The race detector is mandatory — the defect class it
# catches (A9, F3) is exactly what the old implementation shipped.
.PHONY: test
test:
	go test -race ./...

# L1 fuzzing: the unit-file parser, the command lexer, the duration parser and
# the protocol framer all take untrusted-ish input.
.PHONY: fuzz
fuzz:
	go test -run=XXX -fuzz=FuzzLex        -fuzztime=60s ./internal/unitfile/
	go test -run=XXX -fuzz=FuzzTokenize   -fuzztime=60s ./internal/unitfile/
	go test -run=XXX -fuzz=FuzzParseDuration -fuzztime=60s ./internal/unitfile/
	go test -run=XXX -fuzz=FuzzReadFrame  -fuzztime=60s ./internal/proto/
	go test -run=XXX -fuzz=FuzzDecodeRecord -fuzztime=60s ./internal/journal/

# L2: process-level tests against real daemonising services. Needs a real
# Linux kernel, not a mock.
.PHONY: test-proc
test-proc:
	go test -race -tags=proc -timeout 600s ./...

# L3/L4: the conformance matrix, one container per base image.
.PHONY: conformance
conformance:
	@set -e; for img in $(IMAGES); do \
		echo "== building $$img =="; \
		docker build -q -f Dockerfile-$$img -t docker-systemd-test:$$img . ; \
		test/conformance/run.sh docker-systemd-test:$$img ; \
	done

.PHONY: conformance-quick
conformance-quick:
	docker build -q -f Dockerfile-ubuntu2404 -t docker-systemd-test:ubuntu2404 .
	test/conformance/run.sh docker-systemd-test:ubuntu2404 minimal

.PHONY: dockerfiles
dockerfiles:
	bash gen-dockerfiles.sh

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
