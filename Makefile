.NOTPARALLEL:

ver:=$(shell cat VERSION)
commit:=$(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS:=-s -w -X main.version=$(ver)

# Where `make release` collects the artifact set that is attached to a GitHub
# release. `ver` can be overridden on the command line (`make release
# ver=1.0.1`) for a build that is not the one the VERSION file describes.
DIST:=dist

# alien is the only step that insists on being root.
SUDO:=$(shell [ "$$(id -u)" = 0 ] || echo sudo)

# The conformance matrix of designs/docs/next/11-testing.md §4. alpine:3 is the
# expected-degraded case: it has no glibc, so the preload-based tracking this
# project used up to 0.5.x could not handle it at all.
IMAGES:=ubuntu2404 ubuntu2204 ubuntu2004 debian12 debian11 debian10 \
        rockylinux9 rockylinux8 centos9 alpine3

.PHONY: cleanbuild
cleanbuild: clean build

.PHONY: clean
clean:
	rm -rf ./bin ./$(DIST)

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
	packaging/build-deb.sh amd64 $(ver) bin/systemd-amd64 repo/ubuntu

.PHONY: pkg-deb-arm64
pkg-deb-arm64:
	packaging/build-deb.sh arm64 $(ver) bin/systemd-arm64 repo/ubuntu

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

# The release artifact set, built into ./dist and named exactly as the
# published releases are:
#
#   systemd-amd64                       systemd-arm64
#   docker-systemd_$(ver)_amd64.deb     docker-systemd_$(ver)_arm64.deb
#   docker-systemd-$(ver)-2.x86_64.rpm  docker-systemd-$(ver)-2.aarch64.rpm
#   SHA256SUMS
#
# This is what .github/workflows/release.yml attaches to the GitHub release;
# `make release` on a Debian-family machine with dpkg-dev and alien installed
# produces the same set.
.PHONY: release
release: cleanbuild dist-bin dist-deb dist-rpm dist-checksums dist-verify
	@ls -l $(DIST)

.PHONY: dist-bin
dist-bin:
	mkdir -p $(DIST)
	cp bin/systemd-amd64 bin/systemd-arm64 $(DIST)/

.PHONY: dist-deb
dist-deb:
	packaging/build-deb.sh amd64 $(ver) bin/systemd-amd64 $(DIST)
	packaging/build-deb.sh arm64 $(ver) bin/systemd-arm64 $(DIST)

.PHONY: dist-rpm
dist-rpm:
	$(SUDO) packaging/build-rpm.sh x86_64  $(DIST)/docker-systemd_$(ver)_amd64.deb $(DIST)
	$(SUDO) packaging/build-rpm.sh aarch64 $(DIST)/docker-systemd_$(ver)_arm64.deb $(DIST)

.PHONY: dist-checksums
dist-checksums:
	cd $(DIST) && rm -f SHA256SUMS && sha256sum * > SHA256SUMS

# A release whose file names drifted is a broken release: every install
# document and download script pins these names.
.PHONY: dist-verify
dist-verify:
	@set -e; for f in systemd-amd64 systemd-arm64 \
	    docker-systemd_$(ver)_amd64.deb docker-systemd_$(ver)_arm64.deb \
	    docker-systemd-$(ver)-2.x86_64.rpm docker-systemd-$(ver)-2.aarch64.rpm \
	    SHA256SUMS; do \
		test -s $(DIST)/$$f || { echo "missing release artifact: $(DIST)/$$f" >&2; exit 1; }; \
	done; echo "release artifacts for $(ver) are complete"
