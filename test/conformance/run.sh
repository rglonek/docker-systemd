#!/usr/bin/env bash
#
# L3/L4 conformance runner (designs/docs/next/11-testing.md §3–§5).
#
# Boots the manager inside one base image, installs a set of real distro
# packages so their maintainer scripts run, and asserts the properties the
# design commits to. Everything it asserts is a defect from
# designs/docs/next/02-defect-register.md.
#
#   usage: test/conformance/run.sh <image-tag> [package-set]
#
# The image is expected to have been built from one of the repository's
# Dockerfile-* files, so /usr/sbin/init-docker-systemd is the entrypoint.

set -uo pipefail

IMAGE="${1:?usage: run.sh <image-tag> [package-set]}"
PKGSET="${2:-default}"
NAME="docker-systemd-conformance-$$"

fail=0
pass() { printf '  ok    %s\n' "$1"; }
fault() { printf '  FAIL  %s\n' "$1"; fail=$((fail + 1)); }

check() {
    local desc="$1"; shift
    if "$@" >/dev/null 2>&1; then pass "$desc"; else fault "$desc"; fi
}

check_out() {
    local desc="$1" want="$2"; shift 2
    local got
    got=$("$@" 2>&1)
    if printf '%s' "$got" | grep -q -- "$want"; then
        pass "$desc"
    else
        fault "$desc (got: ${got})"
    fi
}

in_container() { docker exec "$NAME" "$@"; }

cleanup() { docker rm -f "$NAME" >/dev/null 2>&1 || true; }
trap cleanup EXIT

printf '== %s (%s) ==\n' "$IMAGE" "$PKGSET"

docker run -d --name "$NAME" "$IMAGE" --log-to-stderr >/dev/null || {
    echo "cannot start the container"; exit 1
}
sleep 3

# --- the manager is up and reachable -----------------------------------------
check "the manager answers systemctl --version" in_container systemctl --version
check "daemon-reload returns 0" in_container systemctl daemon-reload

# --- B19: maintainer scripts gate on this directory --------------------------
check "/run/systemd/system exists" in_container test -d /run/systemd/system

# --- E1: the control socket is not world-accessible --------------------------
# 0711, not 0700: a Type=notify unit that dropped to User= needs the search bit
# to reach its own $NOTIFY_SOCKET. Neither directory is listable.
check_out "the runtime directory is 0711" "711" \
    in_container stat -c '%a' /run/docker-systemd
check_out "the notify directory is 0711" "711" \
    in_container stat -c '%a' /run/docker-systemd/notify
check_out "the control socket is 0600" "600" \
    in_container stat -c '%a' /run/docker-systemd/control.sock
check "the legacy /tmp socket is absent by default" \
    sh -c "! docker exec $NAME test -e /tmp/docker-systemd.sock"

# A non-root uid must be refused by the SO_PEERCRED check.
if docker exec -u 65534 "$NAME" systemctl is-active nonexistent.service >/dev/null 2>&1; then
    fault "a non-root uid was allowed to control the manager"
else
    pass "a non-root uid is denied on the control socket"
fi

# --- F6: no LD_PRELOAD machinery remains -------------------------------------
check "no /etc/ld.so.preload entry is installed" \
    sh -c "! docker exec $NAME test -e /etc/ld.so.preload"
check "no fork.so is installed" \
    sh -c "! docker exec $NAME test -e /usr/local/lib/fork.so"

# --- D6: an unknown verb must not look like success --------------------------
if in_container systemctl definitely-not-a-verb >/dev/null 2>&1; then
    fault "an unknown verb exited 0"
else
    pass "an unknown verb exits non-zero"
fi

# --- self-installed command aliases ------------------------------------------
for cmd in systemctl journalctl service poweroff systemd-notify systemd-detect-virt; do
    check "$cmd is installed" in_container sh -c "command -v $cmd"
done
check_out "systemd-detect-virt reports a container" "docker" \
    in_container systemd-detect-virt

# --- install real packages so their maintainer scripts run -------------------
install_packages() {
    if in_container sh -c 'command -v apt-get' >/dev/null 2>&1; then
        in_container env DEBIAN_FRONTEND=noninteractive apt-get update -qq
        in_container env DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "$@"
    elif in_container sh -c 'command -v dnf' >/dev/null 2>&1; then
        in_container dnf install -y -q "$@"
    elif in_container sh -c 'command -v yum' >/dev/null 2>&1; then
        in_container yum install -y -q "$@"
    elif in_container sh -c 'command -v apk' >/dev/null 2>&1; then
        in_container apk add --no-cache "$@"
    else
        return 1
    fi
}

case "$PKGSET" in
none) PACKAGES=() ;;
minimal) PACKAGES=(cron) ;;
*) PACKAGES=(cron nginx openssh-server) ;;
esac

if [ "${#PACKAGES[@]}" -gt 0 ]; then
    if install_packages "${PACKAGES[@]}"; then
        pass "packages installed: ${PACKAGES[*]}"
    else
        printf '  skip  package installation is unavailable on this image\n'
        PACKAGES=()
    fi
fi

# --- the unit directories are watched: no manual daemon-reload is needed -----
# Asserted before any explicit reload below, which would mask it.
for pkg in "${PACKAGES[@]}"; do
    unit="$pkg"
    case "$pkg" in
    openssh-server) unit=ssh ;;
    esac
    seen=0
    for _ in $(seq 1 10); do
        if in_container systemctl cat "$unit" >/dev/null 2>&1; then seen=1; break; fi
        sleep 1
    done
    [ "$seen" = 1 ] &&
        pass "$unit: visible without an explicit daemon-reload" ||
        fault "$unit: not visible until daemon-reload was run by hand"
done

for pkg in "${PACKAGES[@]}"; do
    unit="$pkg"
    case "$pkg" in
    openssh-server) unit=ssh ;;
    esac
    in_container systemctl daemon-reload >/dev/null 2>&1

    # B18: enable must create a real symlink in the right .wants directory.
    in_container systemctl enable "$unit" >/dev/null 2>&1
    check "$unit: is-enabled succeeds" in_container systemctl is-enabled "$unit"

    check "$unit: starts" in_container systemctl start "$unit"
    sleep 1
    check "$unit: is-active" in_container systemctl --quiet is-active "$unit"
    check_out "$unit: status shows a main PID" "Main PID" in_container systemctl status "$unit"

    # B1/B2: stopping must leave nothing behind.
    before=$(in_container sh -c 'ps -e -o pid= | wc -l')
    in_container systemctl stop "$unit" >/dev/null 2>&1
    sleep 1
    if in_container systemctl --quiet is-active "$unit"; then
        fault "$unit: still active after stop"
    else
        pass "$unit: stops"
    fi
    after=$(in_container sh -c 'ps -e -o pid= | wc -l')
    if [ "$after" -gt "$before" ]; then
        fault "$unit: $((after - before)) processes survived the stop"
    else
        pass "$unit: no processes survived the stop"
    fi

    # Ten restarts in a row must not leak.
    ok=1
    for _ in $(seq 1 10); do
        in_container systemctl restart "$unit" >/dev/null 2>&1 || ok=0
    done
    [ "$ok" = 1 ] && pass "$unit: ten restarts in a row" || fault "$unit: restart loop failed"
    in_container systemctl stop "$unit" >/dev/null 2>&1

    # B5/B6/B7: journalctl must find the unit's output.
    check "$unit: journalctl -u returns" in_container journalctl -u "$unit" -n 5
done

# --- C14: shutdown must fit inside docker's grace period ---------------------
start=$(date +%s)
docker stop -t 10 "$NAME" >/dev/null 2>&1
elapsed=$(( $(date +%s) - start ))
code=$(docker inspect -f '{{.State.ExitCode}}' "$NAME" 2>/dev/null || echo 1)
if [ "$elapsed" -le 11 ]; then
    pass "docker stop -t 10 completed in ${elapsed}s"
else
    fault "docker stop -t 10 took ${elapsed}s and was killed"
fi
if [ "$code" = "0" ]; then
    pass "the container exited 0"
else
    fault "the container exited $code"
fi

printf '\n%s: %d failure(s)\n' "$IMAGE" "$fail"
exit $((fail > 0))
