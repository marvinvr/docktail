#!/usr/bin/env bash
#
# Regression test for the root cause of
# https://github.com/marvinvr/docktail/issues/72
# ("Service offline after container auto update").
#
# When tailscaled runs on the host under systemd, its unit declares
# `RuntimeDirectory=tailscale` and leaves `RuntimeDirectoryPreserve` at the
# default `no`. systemd therefore deletes /run/tailscale when the daemon stops
# and creates a *new* directory when it starts. Any container that bind-mounts
# that directory stays attached to the old, now-unlinked inode: the socket never
# reappears inside the container, however long it waits.
#
# Before the fix, DockTail sat in that state indefinitely — process alive, every
# CLI call failing, every managed service drifting out of date with nothing to
# reconcile it. Field diagnostics caught it doing exactly that for 34 hours
# straight, from the moment the host's tailscaled was upgraded.
#
# Nothing inside the container can repair a stale bind mount; only a container
# restart re-resolves it. So the fix is for DockTail to notice and exit, and the
# behaviour worth testing is the whole loop:
#
#   1. the socket is reachable and DockTail is running normally
#   2. the socket directory is replaced underneath it (the systemd behaviour)
#   3. the mount really is stale — the container sees an empty directory while
#      the host has a working socket (asserted, so a harness that stopped
#      reproducing the bug fails loudly instead of passing vacuously)
#   4. DockTail exits within the grace period
#   5. the restart policy re-creates it, the mount is re-resolved, and the socket
#      is reachable again
#   6. it then stays up, rather than falling into a restart loop
#
# Hermetic: no tailnet, no credentials, no TUN. Exits 1 on regression, 2 on
# harness failure.

set -uo pipefail

COMPOSE_FILE="docker-compose.e2e-socket-loss.yaml"
PROJECT="docktail-e2e-socket-loss"
SOCKET_DIR="./.e2e-socket"
DOCKTAIL_CONTAINER="e2e-sl-docktail"
TS_CONTAINER="e2e-sl-tailscale"
GRACE_SECONDS=20

RED=$'\033[0;31m'; GREEN=$'\033[0;32m'; YELLOW=$'\033[1;33m'; BLUE=$'\033[0;34m'; NC=$'\033[0m'

FAILURES=0

log()  { echo "${BLUE}[$(date -u +%H:%M:%S)]${NC} $*"; }
pass() { echo "${GREEN}  PASS${NC} $*"; }
fail() { echo "${RED}  FAIL${NC} $*"; FAILURES=$((FAILURES + 1)); }
warn() { echo "${YELLOW}  WARN${NC} $*"; }

# harness_fail reports a problem with the test setup itself rather than with
# DockTail, and exits 2 so it is never mistaken for a passing or failing test.
harness_fail() {
	echo "${RED}HARNESS FAILURE:${NC} $*" >&2
	dump_diagnostics
	cleanup
	exit 2
}

compose() { docker compose -p "$PROJECT" -f "$COMPOSE_FILE" "$@"; }

# The socket directory is created by a root process inside the container, so
# removing it from the runner needs elevation.
as_root() {
	if [ "$(id -u)" -eq 0 ]; then
		"$@"
	else
		sudo "$@"
	fi
}

dump_diagnostics() {
	echo
	echo "===== docktail logs (last 60) ====="
	docker logs --tail 60 "$DOCKTAIL_CONTAINER" 2>&1 || true
	echo "===== tailscaled logs (last 20) ====="
	docker logs --tail 20 "$TS_CONTAINER" 2>&1 || true
	echo "===== host socket dir ====="
	ls -lai "$SOCKET_DIR" 2>&1 || true
	echo "===== container view ====="
	docker exec "$DOCKTAIL_CONTAINER" ls -lai /var/run/tailscale 2>&1 || true
	echo "==================================="
}

cleanup() {
	log "Cleaning up"
	compose down -v --remove-orphans >/dev/null 2>&1 || true
	as_root rm -rf "$SOCKET_DIR" 2>/dev/null || true
}

# restart_count reads Docker's restart counter, which the restart policy bumps
# each time it re-creates the container.
restart_count() {
	docker inspect -f '{{.RestartCount}}' "$DOCKTAIL_CONTAINER" 2>/dev/null || echo "-1"
}

started_at() {
	docker inspect -f '{{.State.StartedAt}}' "$DOCKTAIL_CONTAINER" 2>/dev/null || echo ""
}

# wait_for polls a command until it succeeds or the timeout expires.
wait_for() {
	local timeout=$1 desc=$2; shift 2
	local deadline=$((SECONDS + timeout))
	while [ $SECONDS -lt $deadline ]; do
		if "$@" >/dev/null 2>&1; then
			return 0
		fi
		sleep 1
	done
	echo "    timed out after ${timeout}s waiting for: $desc" >&2
	return 1
}

container_sees_socket() {
	docker exec "$DOCKTAIL_CONTAINER" test -S /var/run/tailscale/tailscaled.sock
}

host_has_socket() {
	as_root test -S "$SOCKET_DIR/tailscaled.sock"
}

docktail_logged() {
	docker logs "$DOCKTAIL_CONTAINER" 2>&1 | grep -qF "$1"
}

trap cleanup EXIT

# ---------------------------------------------------------------------------
log "Preparing environment"
# ---------------------------------------------------------------------------
cleanup
mkdir -p "$SOCKET_DIR" || harness_fail "could not create $SOCKET_DIR"

log "Building and starting the stack"
if ! compose up -d --build; then
	harness_fail "compose up failed"
fi

# ---------------------------------------------------------------------------
log "Step 1: healthy baseline"
# ---------------------------------------------------------------------------
if ! wait_for 60 "tailscaled to create its socket on the host" host_has_socket; then
	harness_fail "tailscaled never created a socket"
fi

if ! wait_for 60 "DockTail to see the socket" container_sees_socket; then
	harness_fail "DockTail never saw the socket, so the baseline is not healthy"
fi
pass "DockTail can reach the tailscaled socket"

if ! wait_for 60 "the watchdog to arm" docktail_logged "Tailscale socket watchdog armed"; then
	fail "the watchdog never armed; a lost socket would go undetected"
	dump_diagnostics
fi
pass "socket watchdog armed"

BASELINE_RESTARTS=$(restart_count)
BASELINE_STARTED=$(started_at)
log "  baseline restart count: $BASELINE_RESTARTS"

# ---------------------------------------------------------------------------
log "Step 2: replacing the socket directory (what systemd does on daemon restart)"
# ---------------------------------------------------------------------------
compose stop tailscale >/dev/null 2>&1 || harness_fail "could not stop tailscaled"

OLD_INODE=$(as_root stat -c '%i' "$SOCKET_DIR" 2>/dev/null || echo "?")
as_root rm -rf "$SOCKET_DIR" || harness_fail "could not remove $SOCKET_DIR"
mkdir -p "$SOCKET_DIR" || harness_fail "could not recreate $SOCKET_DIR"
NEW_INODE=$(as_root stat -c '%i' "$SOCKET_DIR" 2>/dev/null || echo "?")

if [ "$OLD_INODE" = "$NEW_INODE" ]; then
	harness_fail "the socket directory kept inode $OLD_INODE; the stale-mount condition was not created"
fi
log "  directory inode $OLD_INODE -> $NEW_INODE"

compose start tailscale >/dev/null 2>&1 || harness_fail "could not restart tailscaled"

if ! wait_for 60 "tailscaled to recreate its socket" host_has_socket; then
	harness_fail "tailscaled did not come back"
fi
pass "tailscaled is running again with a working socket on the host"

# ---------------------------------------------------------------------------
log "Step 3: confirming the mount really is stale"
# ---------------------------------------------------------------------------
# Without this check the rest of the test could pass for the wrong reason: if
# Docker ever resolved the new directory for the running container, there would
# be no bug left to detect and no exit to observe.
if container_sees_socket; then
	harness_fail "DockTail still sees the socket after the directory was replaced; the bug did not reproduce, so this test proves nothing"
fi
pass "DockTail's mount is stale: the host has a socket, the container sees none"

# ---------------------------------------------------------------------------
log "Step 4: DockTail should give up and exit within the grace period"
# ---------------------------------------------------------------------------
# Allow the grace period plus the probe interval plus room for a slow runner.
DETECT_TIMEOUT=$((GRACE_SECONDS + 45))

restarted() {
	local now_count now_started
	now_count=$(restart_count)
	now_started=$(started_at)
	[ "$now_count" != "$BASELINE_RESTARTS" ] || [ "$now_started" != "$BASELINE_STARTED" ]
}

if ! wait_for "$DETECT_TIMEOUT" "DockTail to exit and be restarted" restarted; then
	fail "DockTail did not exit within ${DETECT_TIMEOUT}s of losing its socket (this is the issue #72 regression: it would stay up forever, unable to reconcile anything)"
	dump_diagnostics
	cleanup
	exit 1
fi
pass "DockTail exited after the grace period and Docker restarted it"

if docktail_logged "unreachable for longer than the grace period"; then
	pass "the reason was logged explicitly"
else
	warn "DockTail restarted but the watchdog message was not found in its logs"
fi

# ---------------------------------------------------------------------------
log "Step 5: the restart must re-resolve the mount"
# ---------------------------------------------------------------------------
if ! wait_for 60 "the restarted DockTail to reach the socket" container_sees_socket; then
	fail "DockTail restarted but still cannot reach the socket; restarting is not sufficient to recover"
	dump_diagnostics
	cleanup
	exit 1
fi
pass "the restarted container sees the new socket"

if ! wait_for 60 "DockTail to resume reconciling" docktail_logged "Starting reconciliation loop"; then
	fail "DockTail never resumed its reconciliation loop"
fi
pass "DockTail resumed reconciling"

# ---------------------------------------------------------------------------
log "Step 6: it must settle, not restart-loop"
# ---------------------------------------------------------------------------
SETTLE_COUNT=$(restart_count)
log "  watching for 45s to confirm it stays up"
sleep 45

FINAL_COUNT=$(restart_count)
if [ "$FINAL_COUNT" != "$SETTLE_COUNT" ]; then
	fail "DockTail kept restarting after recovery (restart count $SETTLE_COUNT -> $FINAL_COUNT); the watchdog is too aggressive"
	dump_diagnostics
else
	pass "DockTail stayed up after recovery"
fi

if ! docker ps --filter "name=$DOCKTAIL_CONTAINER" --filter "status=running" --format '{{.Names}}' | grep -q "$DOCKTAIL_CONTAINER"; then
	fail "DockTail is not running at the end of the test"
	dump_diagnostics
fi

# ---------------------------------------------------------------------------
echo
if [ "$FAILURES" -eq 0 ]; then
	echo "${GREEN}PASS${NC} DockTail recovers from a replaced tailscaled socket directory"
	exit 0
fi
echo "${RED}FAIL${NC} $FAILURES check(s) failed"
exit 1
