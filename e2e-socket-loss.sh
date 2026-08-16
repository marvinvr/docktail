#!/usr/bin/env bash
#
# Regression test for the root cause of
# https://github.com/marvinvr/docktail/issues/72 ("Service offline after
# container auto update") and https://github.com/marvinvr/docktail/issues/78
# ("Directory mount becomes stale when tailscaled upgrade recreates
# RuntimeDirectory").
#
# tailscaled.service declares `RuntimeDirectory=tailscale` and leaves
# `RuntimeDirectoryPreserve` at the default `no`, so systemd removes
# /run/tailscale when the daemon stops and creates a *new* directory when it
# starts. Any container that bind-mounts that directory stays attached to the
# old, now-unlinked inode: the socket never reappears inside the container,
# however long it waits. Measured on Debian 12 / systemd 252 / tailscale 1.102.2,
# every ordinary `systemctl restart tailscaled` swaps the inode, so an upgrade is
# only the most common trigger, not the only one.
#
# Before the fix DockTail sat in that state indefinitely — process alive, every
# CLI call failing, every managed service drifting out of date with nothing able
# to reconcile it. Field diagnostics caught it doing exactly that for 34 hours
# straight, from the moment the host's tailscaled was upgraded.
#
# Nothing inside the container can repair a stale bind mount; only a container
# restart re-resolves it. So the fix is for DockTail to notice and exit, and the
# behaviour worth testing is the whole loop.
#
# The test runs the same scenario twice:
#
#   PHASE 1 (control, EXIT_ON_SOCKET_LOSS=false)
#     Reproduces the bug and requires it to be observable: DockTail must still be
#     running, still blind, and still reporting clean reconciliations. If this
#     phase cannot produce the failure, the run is INCONCLUSIVE rather than
#     green — a passing phase 2 proves nothing if the bug is not reproducible in
#     the first place.
#
#   PHASE 2 (fixed, EXIT_ON_SOCKET_LOSS=true)
#     The same break must now be detected: DockTail exits, Docker restarts it,
#     the mount is re-resolved, and it settles instead of restart-looping.
#
# Two ways of breaking the mount, picked automatically:
#
#   systemd    A real tailscaled runs under a real systemd unit with
#              RuntimeDirectory=, and the mount is broken with
#              `systemctl restart`. This is the field mechanism itself, so it
#              also fails if the systemd behaviour the docs describe ever stops
#              being true. Needs systemd and root/passwordless sudo.
#   simulated  tailscaled runs as a sidecar and the socket directory is replaced
#              by a helper container. Runs anywhere Docker does, including macOS.
#
# Override with SOCKET_LOSS_MODE=systemd|simulated|auto (default auto).
#
# Hermetic in both modes: no tailnet, no credentials, no TUN.
# Exit codes: 0 pass, 1 regression, 2 harness failure or inconclusive.

set -uo pipefail

COMPOSE_FILE="docker-compose.e2e-socket-loss.yaml"
PROJECT="docktail-e2e-socket-loss"
DOCKTAIL_CONTAINER="e2e-sl-docktail"
TS_CONTAINER="e2e-sl-tailscale"
HELPER_IMAGE="alpine:3.20"

# Grace period under test. Shortened from the 90s default so the run is quick;
# exported so the compose file and the harness can never disagree about it.
GRACE_SECONDS=20
export SOCKET_LOSS_GRACE_PERIOD="${GRACE_SECONDS}s"

# simulated mode: the socket directory is a child of a parent the helper
# container mounts, so replacing it needs no privileges on the host.
SOCKET_ROOT="$PWD/.e2e-socket-root"
SIM_SOCKET_DIR="$SOCKET_ROOT/tailscale"

# systemd mode: a real unit, whose RuntimeDirectory= is the directory under test.
SYSTEMD_UNIT="docktail-e2e-tailscaled"
SYSTEMD_RUNTIME_NAME="docktail-e2e-tailscale"
SYSTEMD_SOCKET_DIR="/run/$SYSTEMD_RUNTIME_NAME"
SYSTEMD_BIN_DIR="/usr/local/lib/docktail-e2e"

MODE="${SOCKET_LOSS_MODE:-auto}"

RED=$'\033[0;31m'; GREEN=$'\033[0;32m'; YELLOW=$'\033[1;33m'; BLUE=$'\033[0;34m'; NC=$'\033[0m'

FAILURES=0

log()  { echo "${BLUE}[$(date -u +%H:%M:%S)]${NC} $*"; }
pass() { echo "${GREEN}  PASS${NC} $*"; }
fail() { echo "${RED}  FAIL${NC} $*"; FAILURES=$((FAILURES + 1)); }
warn() { echo "${YELLOW}  WARN${NC} $*"; }

# harness_fail reports a problem with the test setup itself rather than with
# DockTail, and exits 2 so it is never mistaken for a passing or failing test.
harness_fail() {
	echo >&2
	echo "${RED}HARNESS FAILURE:${NC} $*" >&2
	dump_diagnostics
	cleanup
	exit 2
}

# inconclusive is for the one outcome that must never be reported as success:
# the control phase could not reproduce the bug, so the fix was never actually
# put to the test.
inconclusive() {
	echo >&2
	echo "${YELLOW}INCONCLUSIVE:${NC} $*" >&2
	dump_diagnostics
	cleanup
	exit 2
}

compose() { docker compose -p "$PROJECT" -f "$COMPOSE_FILE" "$@"; }

as_root() {
	if [ "$(id -u)" -eq 0 ]; then "$@"; else sudo -n "$@"; fi
}

# ---------------------------------------------------------------------------
# Mode selection
# ---------------------------------------------------------------------------

systemd_usable() {
	command -v systemctl >/dev/null 2>&1 || return 1
	# A booted systemd, not just the client binary (containers often have both).
	[ -d /run/systemd/system ] || return 1
	if [ "$(id -u)" -ne 0 ]; then
		sudo -n true >/dev/null 2>&1 || return 1
	fi
	return 0
}

select_mode() {
	case "$MODE" in
		systemd)
			systemd_usable || harness_fail "SOCKET_LOSS_MODE=systemd but systemd is not usable here (needs a booted systemd and root or passwordless sudo)"
			;;
		simulated) ;;
		auto)
			if systemd_usable; then MODE=systemd; else MODE=simulated; fi
			;;
		*) harness_fail "unknown SOCKET_LOSS_MODE '$MODE' (expected systemd, simulated or auto)" ;;
	esac

	if [ "$MODE" = systemd ]; then
		SOCKET_DIR="$SYSTEMD_SOCKET_DIR"
	else
		SOCKET_DIR="$SIM_SOCKET_DIR"
	fi
	export SOCKET_DIR
}

# ---------------------------------------------------------------------------
# systemd mode: a real unit whose RuntimeDirectory is the directory under test
# ---------------------------------------------------------------------------

# systemd_install_unit puts a real tailscaled behind a real systemd unit. The
# binary is taken from the host when present and otherwise lifted out of the
# official image, so the test never depends on a tailnet or a package install.
systemd_install_unit() {
	local bin
	if command -v tailscaled >/dev/null 2>&1; then
		bin="$(command -v tailscaled)"
	else
		log "  extracting tailscaled from the official image"
		local cid
		cid=$(docker create tailscale/tailscale:latest 2>/dev/null) || harness_fail "could not create a container to extract tailscaled from"
		as_root mkdir -p "$SYSTEMD_BIN_DIR" || harness_fail "could not create $SYSTEMD_BIN_DIR"
		docker cp "$cid:/usr/local/bin/tailscaled" "/tmp/e2e-tailscaled" >/dev/null 2>&1 || harness_fail "could not copy tailscaled out of the image"
		docker rm "$cid" >/dev/null 2>&1
		as_root install -m 0755 /tmp/e2e-tailscaled "$SYSTEMD_BIN_DIR/tailscaled" || harness_fail "could not install the extracted tailscaled"
		rm -f /tmp/e2e-tailscaled
		bin="$SYSTEMD_BIN_DIR/tailscaled"
	fi
	log "  tailscaled binary: $bin"

	# The unit mirrors the parts of the real tailscaled.service that matter here:
	# RuntimeDirectory= plus the default RuntimeDirectoryPreserve=no.
	as_root tee "/etc/systemd/system/$SYSTEMD_UNIT.service" >/dev/null <<EOF
[Unit]
Description=DockTail e2e tailscaled (issue #72 socket-loss harness)

[Service]
Type=simple
RuntimeDirectory=$SYSTEMD_RUNTIME_NAME
RuntimeDirectoryMode=0755
ExecStart=$bin --tun=userspace-networking --socket=$SYSTEMD_SOCKET_DIR/tailscaled.sock --state=mem: --port=0
Restart=no
EOF

	as_root systemctl daemon-reload || harness_fail "systemctl daemon-reload failed"

	local preserve
	preserve=$(as_root systemctl show "$SYSTEMD_UNIT" -p RuntimeDirectoryPreserve --value 2>/dev/null)
	if [ "$preserve" != "no" ]; then
		harness_fail "the test unit reports RuntimeDirectoryPreserve=$preserve; the harness needs the default 'no' to reproduce the bug"
	fi
}

systemd_remove_unit() {
	as_root systemctl stop "$SYSTEMD_UNIT" >/dev/null 2>&1
	as_root rm -f "/etc/systemd/system/$SYSTEMD_UNIT.service" >/dev/null 2>&1
	as_root systemctl daemon-reload >/dev/null 2>&1
	as_root rm -rf "$SYSTEMD_BIN_DIR" >/dev/null 2>&1
	as_root rm -rf "$SYSTEMD_SOCKET_DIR" >/dev/null 2>&1
}

# ---------------------------------------------------------------------------
# Filesystem probes, phrased so they work the same in both modes and on macOS
# ---------------------------------------------------------------------------

# host_dir_inode reads the inode of the socket directory as the host sees it.
# In simulated mode it is read through a helper container so the result comes
# from the same filesystem view the containers get, and so the harness does not
# need a `stat` with GNU flags on the host.
host_dir_inode() {
	if [ "$MODE" = systemd ]; then
		as_root stat -c '%i' "$SOCKET_DIR" 2>/dev/null || echo "GONE"
	else
		docker run --rm -v "$SOCKET_ROOT:/parent" "$HELPER_IMAGE" \
			stat -c '%i' /parent/tailscale 2>/dev/null || echo "GONE"
	fi
}

# container_dir_inode reports the inode DockTail's mount currently resolves to,
# or GONE when the mount no longer resolves at all. Both are stale-mount
# outcomes: on a Linux host the container stays pinned to the old, unlinked
# directory and sees it empty, while on a macOS/virtiofs Docker host the mount
# simply stops resolving. What matters either way is that it is not the new one.
container_dir_inode() {
	docker exec "$DOCKTAIL_CONTAINER" stat -c '%i' /var/run/tailscale 2>/dev/null || echo "GONE"
}

container_sees_socket() {
	docker exec "$DOCKTAIL_CONTAINER" test -S /var/run/tailscale/tailscaled.sock
}

host_has_socket() {
	if [ "$MODE" = systemd ]; then
		as_root test -S "$SOCKET_DIR/tailscaled.sock"
	else
		docker run --rm -v "$SOCKET_ROOT:/parent" "$HELPER_IMAGE" \
			test -S /parent/tailscale/tailscaled.sock
	fi
}

restart_count() { docker inspect -f '{{.RestartCount}}' "$DOCKTAIL_CONTAINER" 2>/dev/null || echo "-1"; }
started_at()    { docker inspect -f '{{.State.StartedAt}}' "$DOCKTAIL_CONTAINER" 2>/dev/null || echo ""; }
is_running()    { [ "$(docker inspect -f '{{.State.Running}}' "$DOCKTAIL_CONTAINER" 2>/dev/null)" = "true" ]; }

# `docker logs` keeps the output of earlier runs of a restarted container, so an
# unanchored grep would happily match a line written before the break and report
# a recovery that never happened. Every log assertion is therefore phrased as
# "did this happen *again*": the number of occurrences is recorded before the
# event under test and compared afterwards.
log_occurrences() { docker logs "$DOCKTAIL_CONTAINER" 2>&1 | grep -cF "$1" | tr -d ' '; }
logged_again()    { [ "$(log_occurrences "$1")" -gt "$2" ]; }

wait_for() {
	local timeout=$1 desc=$2; shift 2
	local deadline=$((SECONDS + timeout))
	while [ $SECONDS -lt $deadline ]; do
		if "$@" >/dev/null 2>&1; then return 0; fi
		sleep 1
	done
	echo "    timed out after ${timeout}s waiting for: $desc" >&2
	return 1
}

dump_diagnostics() {
	echo
	echo "===== mode: ${MODE:-unset} ====="
	echo "===== docktail logs (last 60) ====="
	docker logs --tail 60 "$DOCKTAIL_CONTAINER" 2>&1 || true
	if [ "$MODE" = systemd ]; then
		echo "===== $SYSTEMD_UNIT (last 20) ====="
		as_root journalctl -u "$SYSTEMD_UNIT" -n 20 --no-pager 2>&1 || true
		echo "===== host socket dir ====="
		as_root ls -lai "$SOCKET_DIR" 2>&1 || true
	else
		echo "===== tailscaled logs (last 20) ====="
		docker logs --tail 20 "$TS_CONTAINER" 2>&1 || true
		echo "===== host socket dir ====="
		docker run --rm -v "$SOCKET_ROOT:/parent" "$HELPER_IMAGE" ls -lai /parent/tailscale 2>&1 || true
	fi
	echo "===== container view ====="
	docker exec "$DOCKTAIL_CONTAINER" ls -lai /var/run/tailscale 2>&1 || true
	echo "==================================="
}

teardown_stack() {
	compose --profile sidecar down -v --remove-orphans >/dev/null 2>&1 || true
}

cleanup() {
	teardown_stack
	if [ "${MODE:-}" = systemd ]; then
		systemd_remove_unit
	fi
	if [ -d "$SOCKET_ROOT" ]; then
		docker run --rm -v "$SOCKET_ROOT:/parent" "$HELPER_IMAGE" sh -c 'rm -rf /parent/tailscale' >/dev/null 2>&1 || true
		rm -rf "$SOCKET_ROOT" 2>/dev/null || true
	fi
}

trap cleanup EXIT

# ---------------------------------------------------------------------------
# Bringing the daemon up and breaking the mount, per mode
# ---------------------------------------------------------------------------

start_daemon() {
	if [ "$MODE" = systemd ]; then
		as_root systemctl start "$SYSTEMD_UNIT" || harness_fail "could not start $SYSTEMD_UNIT"
	else
		mkdir -p "$SOCKET_ROOT" || harness_fail "could not create $SOCKET_ROOT"
		docker run --rm -v "$SOCKET_ROOT:/parent" "$HELPER_IMAGE" mkdir -p /parent/tailscale \
			|| harness_fail "could not create the socket directory"
		compose --profile sidecar up -d tailscale >/dev/null 2>&1 || harness_fail "could not start the tailscaled sidecar"
	fi
}

# break_the_mount replaces the socket directory the way systemd does: the daemon
# stops, the directory is removed and a new one is created, the daemon starts
# again and puts its socket in the new one. In systemd mode this is literally
# `systemctl restart`; in simulated mode the same sequence is performed by hand.
break_the_mount() {
	if [ "$MODE" = systemd ]; then
		as_root systemctl restart "$SYSTEMD_UNIT" || harness_fail "could not restart $SYSTEMD_UNIT"
		return
	fi

	compose stop tailscale >/dev/null 2>&1 || harness_fail "could not stop the tailscaled sidecar"
	docker run --rm -v "$SOCKET_ROOT:/parent" "$HELPER_IMAGE" \
		sh -c 'rm -rf /parent/tailscale && mkdir -m 0755 /parent/tailscale' \
		|| harness_fail "could not replace the socket directory"
	compose --profile sidecar start tailscale >/dev/null 2>&1 || harness_fail "could not restart the tailscaled sidecar"
}

# ---------------------------------------------------------------------------
# One phase: bring the stack up healthy, break the mount, judge the outcome.
# ---------------------------------------------------------------------------

# setup_healthy_baseline leaves DockTail running and talking to the daemon, and
# records the state the assertions after the break are compared against.
setup_healthy_baseline() {
	local expect_watchdog=$1

	start_daemon

	if ! wait_for 60 "tailscaled to create its socket on the host" host_has_socket; then
		harness_fail "tailscaled never created a socket"
	fi

	compose up -d docktail >/dev/null 2>&1 || harness_fail "could not start DockTail"

	if ! wait_for 60 "DockTail to see the socket" container_sees_socket; then
		harness_fail "DockTail never saw the socket, so the baseline is not healthy"
	fi
	pass "DockTail can reach the tailscaled socket"

	BASE_HOST_INODE=$(host_dir_inode)
	BASE_CTR_INODE=$(container_dir_inode)
	if [ "$BASE_HOST_INODE" != "$BASE_CTR_INODE" ]; then
		harness_fail "the bind mount does not share an inode with the host directory ($BASE_HOST_INODE vs $BASE_CTR_INODE); the harness is not testing what it thinks it is"
	fi
	log "  socket directory inode: $BASE_HOST_INODE (shared by host and container)"

	if [ "$expect_watchdog" = yes ]; then
		if wait_for 60 "the watchdog to arm" bash -c "docker logs '$DOCKTAIL_CONTAINER' 2>&1 | grep -qF 'Tailscale socket watchdog armed'"; then
			pass "socket watchdog armed"
		else
			fail "the watchdog never armed; a lost socket would go undetected"
			dump_diagnostics
		fi
	fi

	# Settle before breaking anything: the assertions after the break are all
	# "did this happen again", so the baseline has to have happened once first.
	if ! wait_for 60 "DockTail to complete a reconciliation" \
		bash -c "docker logs '$DOCKTAIL_CONTAINER' 2>&1 | grep -qF 'Service reconciliation completed'"; then
		harness_fail "DockTail never completed a reconciliation against a working socket"
	fi
	pass "DockTail reconciled successfully against the live socket"

	BASE_RESTARTS=$(restart_count)
	BASE_STARTED=$(started_at)

	# Baselines for the "did this happen again after the break" assertions.
	N_CALL_FAILED=$(log_occurrences "Failed to get current services")
	N_RECONCILED=$(log_occurrences "Service reconciliation completed")
	N_WATCHDOG=$(log_occurrences "unreachable for longer than the grace period")
	N_LOOP_STARTED=$(log_occurrences "Starting reconciliation loop")
}

# assert_mount_is_stale is the reproduction itself. Everything after it is only
# meaningful if this holds, so a failure here is a harness failure: it means the
# environment no longer produces the bug and the run proves nothing either way.
assert_mount_is_stale() {
	if ! wait_for 60 "tailscaled to recreate its socket" host_has_socket; then
		harness_fail "tailscaled did not come back after the directory was replaced"
	fi

	local new_host new_ctr
	new_host=$(host_dir_inode)
	new_ctr=$(container_dir_inode)

	if [ "$new_host" = "$BASE_HOST_INODE" ]; then
		harness_fail "the socket directory kept inode $new_host; the stale-mount condition was never created"
	fi
	log "  host directory inode $BASE_HOST_INODE -> $new_host"

	# The one outcome that would mean there is no bug here: the container's mount
	# tracked the replacement and now points at the live directory.
	if [ "$new_ctr" = "$new_host" ]; then
		harness_fail "DockTail's mount followed the new directory (inode $new_ctr); this environment does not reproduce the bug, so the result would be meaningless"
	fi

	if container_sees_socket; then
		harness_fail "DockTail still sees a socket after the directory was replaced; the bug did not reproduce, so this test proves nothing"
	fi

	if [ "$new_ctr" = GONE ]; then
		pass "the mount is stale: the host has a working socket at inode $new_host, DockTail's mount no longer resolves at all"
	else
		pass "the mount is stale: the host has a working socket at inode $new_host, DockTail is still pinned to $new_ctr and sees nothing"
	fi
}

# phase_control runs the scenario with the watchdog switched off. It must
# reproduce the pre-fix behaviour: DockTail stays up, stays blind, and keeps
# reporting successful reconciliations.
phase_control() {
	echo
	log "${YELLOW}PHASE 1 (control)${NC} — with EXIT_ON_SOCKET_LOSS=false the bug must still be reproducible"

	export EXIT_ON_SOCKET_LOSS=false
	setup_healthy_baseline no

	log "  breaking the mount"
	break_the_mount
	assert_mount_is_stale

	local settle=$((GRACE_SECONDS + 25))
	log "  waiting ${settle}s — longer than the grace period a watchdog would use"
	sleep "$settle"

	if ! is_running; then
		inconclusive "DockTail exited even with EXIT_ON_SOCKET_LOSS=false. The control did not reproduce the pre-fix behaviour, so phase 2 could not have proved anything."
	fi
	if [ "$(restart_count)" != "$BASE_RESTARTS" ] || [ "$(started_at)" != "$BASE_STARTED" ]; then
		inconclusive "DockTail restarted with the watchdog disabled; something other than the watchdog is restarting it, so phase 2 would not attribute a restart correctly."
	fi
	pass "reproduced: DockTail is still running ${settle}s after losing its socket"

	if container_sees_socket; then
		inconclusive "DockTail's mount repaired itself, so there is no lasting failure to fix"
	fi
	pass "reproduced: it is still blind — the socket never came back inside the container"

	# The reason the issue reports say "the logs don't show anything special":
	# reconciliation keeps reporting success while every call underneath it fails.
	# Both of these lines are DockTail's own, so they are stable enough to assert
	# on, and together they are the whole "clean logs" symptom.
	if logged_again "Failed to get current services" "$N_CALL_FAILED"; then
		pass "reproduced: every Tailscale call is failing on the socket it can no longer reach"
	else
		inconclusive "DockTail is not reporting failing Tailscale calls after the break, so it is not in the state issue #72 describes"
	fi
	if logged_again "Service reconciliation completed" "$N_RECONCILED"; then
		pass "reproduced: it still reports completed reconciliations while blind (this is why the reported logs look clean)"
	else
		warn "no completed-reconciliation line after the break; the 'clean logs' half of the symptom was not observed"
	fi

	teardown_stack
}

# phase_fixed runs the same scenario with the watchdog on. The break is now
# expected to be detected and recovered from.
phase_fixed() {
	echo
	log "${GREEN}PHASE 2 (fixed)${NC} — with EXIT_ON_SOCKET_LOSS=true the same break must be recovered"

	export EXIT_ON_SOCKET_LOSS=true
	setup_healthy_baseline yes

	log "  breaking the mount"
	break_the_mount
	assert_mount_is_stale

	local detect_timeout=$((GRACE_SECONDS + 45))
	restarted() {
		[ "$(restart_count)" != "$BASE_RESTARTS" ] || [ "$(started_at)" != "$BASE_STARTED" ]
	}

	if ! wait_for "$detect_timeout" "DockTail to exit and be restarted" restarted; then
		fail "DockTail did not exit within ${detect_timeout}s of losing its socket (this is the issue #72 regression: it stays up forever, unable to reconcile anything)"
		dump_diagnostics
		cleanup
		exit 1
	fi
	pass "DockTail exited after the grace period and Docker restarted it"

	if logged_again "unreachable for longer than the grace period" "$N_WATCHDOG"; then
		pass "the reason was logged explicitly"
	else
		fail "DockTail restarted but did not log why; an operator would have no idea what happened"
	fi

	if ! wait_for 60 "the restarted DockTail to reach the socket" container_sees_socket; then
		fail "DockTail restarted but still cannot reach the socket; restarting is not sufficient to recover"
		dump_diagnostics
		cleanup
		exit 1
	fi
	pass "the restarted container sees the new socket"

	if wait_for 60 "DockTail to resume reconciling" logged_again "Starting reconciliation loop" "$N_LOOP_STARTED"; then
		pass "DockTail resumed reconciling"
	else
		fail "DockTail never resumed its reconciliation loop after the restart"
		dump_diagnostics
	fi

	local settle_count final_count
	settle_count=$(restart_count)
	log "  watching for 45s to confirm it stays up"
	sleep 45
	final_count=$(restart_count)

	if [ "$final_count" != "$settle_count" ]; then
		fail "DockTail kept restarting after recovery (restart count $settle_count -> $final_count); the watchdog is too aggressive"
		dump_diagnostics
	else
		pass "DockTail stayed up after recovery"
	fi

	if ! is_running; then
		fail "DockTail is not running at the end of the test"
		dump_diagnostics
	fi

	teardown_stack
}

# ---------------------------------------------------------------------------
log "Preparing environment"
# ---------------------------------------------------------------------------
select_mode
log "  mode: $MODE  (socket directory: $SOCKET_DIR)"

cleanup

docker pull -q "$HELPER_IMAGE" >/dev/null 2>&1 || harness_fail "could not pull $HELPER_IMAGE"

log "Building the DockTail image"
if ! compose build docktail; then
	harness_fail "could not build the DockTail image"
fi

if [ "$MODE" = systemd ]; then
	log "Installing the test systemd unit"
	systemd_install_unit
fi

phase_control
phase_fixed

# ---------------------------------------------------------------------------
echo
if [ "$FAILURES" -eq 0 ]; then
	echo "${GREEN}PASS${NC} the bug reproduces without the watchdog ($MODE mode), and DockTail recovers with it"
	exit 0
fi
echo "${RED}FAIL${NC} $FAILURES check(s) failed"
exit 1
