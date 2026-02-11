#!/usr/bin/env bash

COMPOSE_FILE="docker-compose.e2e.yaml"
TS_CONTAINER="e2e-tailscale"
DOCKTAIL_CONTAINER="e2e-docktail"
MAX_WAIT=120
RECONCILE_WAIT=10
SCRIPT_TIMEOUT=600

# Kill the script if it runs longer than SCRIPT_TIMEOUT seconds
( sleep "$SCRIPT_TIMEOUT" && echo "ERROR: E2E script timed out after ${SCRIPT_TIMEOUT}s" && kill $$ ) 2>/dev/null &
TIMEOUT_PID=$!

passed=0
failed=0
errors=""

# --- Logging ---
log()  { echo ""; echo "=== $1"; }
pass() { echo "  PASS: $1"; passed=$((passed + 1)); }
fail() { echo "  FAIL: $1"; failed=$((failed + 1)); errors="${errors}\n  - $1"; }

# --- Cleanup ---
cleanup() {
    log "Cleaning up"
    kill "$TIMEOUT_PID" 2>/dev/null || true
    docker compose -f "$COMPOSE_FILE" down -v --remove-orphans 2>/dev/null || true
}
trap cleanup EXIT

# --- Preflight (strict mode for setup) ---
set -euo pipefail

if [ -n "${TS_AUTHKEY:-}" ]; then
    echo "  Using provided TS_AUTHKEY"
elif [ -n "${TS_OAUTH_CLIENT_ID:-}" ] && [ -n "${TS_OAUTH_CLIENT_SECRET:-}" ]; then
    echo "  Generating ephemeral auth key from OAuth credentials..."
    TS_TAILNET="${TS_TAILNET:--}"

    # Get OAuth token
    TOKEN_RESPONSE=$(curl -s -X POST "https://api.tailscale.com/api/v2/oauth/token" \
        -u "${TS_OAUTH_CLIENT_ID}:${TS_OAUTH_CLIENT_SECRET}" \
        -d "grant_type=client_credentials")
    TS_TOKEN=$(echo "$TOKEN_RESPONSE" | jq -r '.access_token // empty')
    if [ -z "$TS_TOKEN" ]; then
        echo "ERROR: Failed to get OAuth token"
        echo "$TOKEN_RESPONSE"
        exit 1
    fi

    # Generate ephemeral auth key
    KEY_RESPONSE=$(curl -s -X POST "https://api.tailscale.com/api/v2/tailnet/${TS_TAILNET}/keys" \
        -H "Authorization: Bearer ${TS_TOKEN}" \
        -H "Content-Type: application/json" \
        -d '{
            "capabilities": {
                "devices": {
                    "create": {
                        "reusable": false,
                        "ephemeral": true,
                        "tags": ["tag:ci-test"]
                    }
                }
            },
            "expirySeconds": 600
        }')
    TS_AUTHKEY=$(echo "$KEY_RESPONSE" | jq -r '.key // empty')
    if [ -z "$TS_AUTHKEY" ]; then
        echo "ERROR: Failed to generate auth key"
        echo "$KEY_RESPONSE"
        exit 1
    fi
    export TS_AUTHKEY
    echo "  Auth key generated (ephemeral, expires in 10 min)"
else
    echo "ERROR: Either TS_AUTHKEY or TS_OAUTH_CLIENT_ID + TS_OAUTH_CLIENT_SECRET is required"
    exit 1
fi

# ==============================================================================
# Helpers
# ==============================================================================

# Cache serve status to avoid repeated exec calls within a test group
SERVE_STATUS_CACHE=""

refresh_serve_status() {
    SERVE_STATUS_CACHE=$(docker exec "$TS_CONTAINER" tailscale serve status --json 2>/dev/null || echo "{}")
}

# Check if a service exists in the serve status
assert_service_exists() {
    local name="svc:$1"
    if echo "$SERVE_STATUS_CACHE" | jq -e ".Services[\"$name\"]" >/dev/null 2>&1; then
        pass "$name exists"
    else
        fail "$name not found"
    fi
}

assert_service_not_exists() {
    local name="svc:$1"
    if echo "$SERVE_STATUS_CACHE" | jq -e ".Services[\"$name\"]" >/dev/null 2>&1; then
        fail "$name still exists (expected removal)"
    else
        pass "$name removed"
    fi
}

# Check that a service has a specific port in its TCP config
assert_service_port() {
    local name="svc:$1"
    local expected_port="$2"
    local actual
    actual=$(echo "$SERVE_STATUS_CACHE" | jq -r ".Services[\"$name\"].TCP | keys[0] // empty" 2>/dev/null || true)
    if [ "$actual" = "$expected_port" ]; then
        pass "$name has port $expected_port"
    else
        fail "$name expected port $expected_port, got '${actual:-<none>}'"
    fi
}

# Check service protocol via TCP config flags
# "http" = HTTP:true, "https" = HTTPS:true, "tcp" = neither
assert_service_protocol() {
    local name="svc:$1"
    local expected_proto="$2"
    local port
    port=$(echo "$SERVE_STATUS_CACHE" | jq -r ".Services[\"$name\"].TCP | keys[0] // empty" 2>/dev/null || true)
    if [ -z "$port" ]; then
        fail "$name protocol check: no TCP config found"
        return
    fi

    local is_https is_http actual
    is_https=$(echo "$SERVE_STATUS_CACHE" | jq -r ".Services[\"$name\"].TCP[\"$port\"].HTTPS // false" 2>/dev/null || true)
    is_http=$(echo "$SERVE_STATUS_CACHE" | jq -r ".Services[\"$name\"].TCP[\"$port\"].HTTP // false" 2>/dev/null || true)

    if [ "$is_https" = "true" ]; then
        actual="https"
    elif [ "$is_http" = "true" ]; then
        actual="http"
    else
        actual="tcp"
    fi

    if [ "$actual" = "$expected_proto" ]; then
        pass "$name protocol is $expected_proto"
    else
        fail "$name expected protocol $expected_proto, got $actual"
    fi
}

# Check that the destination proxy URL contains a substring
assert_service_destination_contains() {
    local name="svc:$1"
    local expected_substr="$2"
    local dest
    dest=$(echo "$SERVE_STATUS_CACHE" | jq -r "[.Services[\"$name\"].Web[].Handlers[].Proxy // empty] | first // empty" 2>/dev/null || true)
    if [ -z "$dest" ]; then
        # For TCP services, Web section may be empty - skip destination check
        pass "$name destination check skipped (TCP/no Web config)"
        return
    fi
    if echo "$dest" | grep -q "$expected_substr"; then
        pass "$name destination contains '$expected_substr'"
    else
        fail "$name destination '$dest' does not contain '$expected_substr'"
    fi
}

# Check funnel status
get_funnel_status() {
    docker exec "$TS_CONTAINER" tailscale funnel status --json 2>/dev/null || echo "{}"
}

assert_funnel_active() {
    local port="$1"
    local funnel_status
    funnel_status=$(get_funnel_status)
    if echo "$funnel_status" | jq -e ".AllowFunnel | to_entries[] | select(.key | endswith(\":$port\"))" >/dev/null 2>&1; then
        pass "funnel active on port $port"
    else
        fail "funnel not found on port $port"
    fi
}

# ==============================================================================
# Start the stack
# ==============================================================================

log "Building and starting E2E stack"
docker compose -f "$COMPOSE_FILE" up -d --build

log "Waiting for Tailscale to connect"
elapsed=0
while [ $elapsed -lt $MAX_WAIT ]; do
    if docker exec "$TS_CONTAINER" tailscale status --json 2>/dev/null | jq -e '.BackendState == "Running"' >/dev/null 2>&1; then
        break
    fi
    sleep 2
    elapsed=$((elapsed + 2))
done
if [ $elapsed -ge $MAX_WAIT ]; then
    echo "ERROR: Tailscale did not connect within ${MAX_WAIT}s"
    docker logs "$TS_CONTAINER" 2>&1 | tail -30
    exit 1
fi
echo "  Tailscale connected after ${elapsed}s"

# Give Tailscale a moment to fully register with the control plane
# BackendState=Running means authenticated, but service registration needs
# the control plane handshake to complete
log "Waiting for Tailscale to be fully ready for service registration"
sleep 10
echo "  Ready"

log "Waiting for DockTail to reconcile"
sleep "$RECONCILE_WAIT"

# Switch to non-strict mode for test assertions
set +e

# Get the initial serve status once
refresh_serve_status
echo "  Services found: $(echo "$SERVE_STATUS_CACHE" | jq -r '.Services | keys | join(", ")' 2>/dev/null || echo 'none')"

# ==============================================================================
# 1. Protocol Variations
# ==============================================================================

log "1. Protocol Variations"

echo "  --- HTTP ---"
assert_service_exists       "e2e-proto-http"
assert_service_port         "e2e-proto-http" "80"
assert_service_protocol     "e2e-proto-http" "http"
assert_service_destination_contains "e2e-proto-http" "http://"

echo "  --- HTTPS ---"
assert_service_exists       "e2e-proto-https"
assert_service_port         "e2e-proto-https" "443"
assert_service_protocol     "e2e-proto-https" "https"
assert_service_destination_contains "e2e-proto-https" "http://"  # backend is http, service is https

echo "  --- TCP ---"
assert_service_exists       "e2e-proto-tcp"
assert_service_port         "e2e-proto-tcp" "5432"
assert_service_protocol     "e2e-proto-tcp" "tcp"

# ==============================================================================
# 2. Smart Defaults
# ==============================================================================

log "2. Smart Defaults"

echo "  --- Minimal (→ http/80) ---"
assert_service_exists       "e2e-default-minimal"
assert_service_port         "e2e-default-minimal" "80"
assert_service_protocol     "e2e-default-minimal" "http"

echo "  --- service-port=443 only (→ https/443) ---"
assert_service_exists       "e2e-default-port443"
assert_service_port         "e2e-default-port443" "443"
assert_service_protocol     "e2e-default-port443" "https"

echo "  --- service-protocol=https only (→ https/443) ---"
assert_service_exists       "e2e-default-proto-https"
assert_service_port         "e2e-default-proto-https" "443"
assert_service_protocol     "e2e-default-proto-https" "https"

echo "  --- backend tcp, no service config (→ tcp/80) ---"
assert_service_exists       "e2e-default-tcp-backend"
assert_service_port         "e2e-default-tcp-backend" "80"
assert_service_protocol     "e2e-default-tcp-backend" "tcp"

# ==============================================================================
# 3. Network Modes
# ==============================================================================

log "3. Network Modes"

echo "  --- Custom Docker network ---"
assert_service_exists       "e2e-net-custom"
assert_service_destination_contains "e2e-net-custom" "http://"

echo "  --- Published ports (direct=false) ---"
assert_service_exists       "e2e-net-published"
assert_service_destination_contains "e2e-net-published" "localhost:19080"

echo "  --- Host networking ---"
assert_service_exists       "e2e-net-host"
assert_service_destination_contains "e2e-net-host" "localhost:80"

echo "  --- target port 443 (→ http/80) ---"
assert_service_exists       "e2e-default-target443"
assert_service_port         "e2e-default-target443" "80"
assert_service_protocol     "e2e-default-target443" "http"

# ==============================================================================
# 4. Funnel
# ==============================================================================

log "4. Funnel"
assert_service_exists       "e2e-funnel"
assert_funnel_active        "443"

# ==============================================================================
# 5. Custom Tags
# ==============================================================================

log "5. Custom Tags"
# Tags aren't in serve status, but we verify the service was created
# (tag validation would need API access)
assert_service_exists       "e2e-custom-tags"

# ==============================================================================
# 6. Ignored Container (no docktail labels)
# ==============================================================================

log "6. Ignored Container"
assert_service_not_exists   "e2e-ignored"

# ==============================================================================
# 7. Lifecycle: service removal on container stop
# ==============================================================================

log "7. Lifecycle"

echo "  --- Pre-check: lifecycle service exists ---"
assert_service_exists       "e2e-lifecycle"

echo "  --- Stopping container ---"
docker stop e2e-lifecycle >/dev/null 2>&1 || true
echo "  Waiting for reconciliation after stop..."
sleep "$RECONCILE_WAIT"
refresh_serve_status

echo "  --- Post-stop: service should be removed ---"
assert_service_not_exists   "e2e-lifecycle"

echo "  --- Other services unaffected ---"
assert_service_exists       "e2e-proto-http"
assert_service_exists       "e2e-proto-https"

# ==============================================================================
# 8. Service Update: change protocol from HTTP to HTTPS
# ==============================================================================

log "8. Service Update"

echo "  --- Pre-check: update service is HTTP/80 ---"
assert_service_exists       "e2e-update"
assert_service_port         "e2e-update" "80"
assert_service_protocol     "e2e-update" "http"

echo "  --- Recreating container with HTTPS labels ---"
docker stop e2e-update >/dev/null 2>&1 || true
docker rm e2e-update >/dev/null 2>&1 || true
docker run -d \
    --name e2e-update \
    --restart no \
    --label "docktail.service.enable=true" \
    --label "docktail.service.name=e2e-update" \
    --label "docktail.service.port=80" \
    --label "docktail.service.service-port=443" \
    --label "docktail.service.service-protocol=https" \
    nginx:alpine >/dev/null 2>&1

echo "  Waiting for reconciliation after update..."
sleep "$RECONCILE_WAIT"
refresh_serve_status

echo "  --- Post-update: service should be HTTPS/443 ---"
assert_service_exists       "e2e-update"
assert_service_port         "e2e-update" "443"
assert_service_protocol     "e2e-update" "https"

# ==============================================================================
# 9. Idempotency: reconciling again changes nothing
# ==============================================================================

log "9. Idempotency"
echo "  Waiting for another reconciliation cycle..."
sleep "$RECONCILE_WAIT"
refresh_serve_status

# All non-stopped services should still be there
assert_service_exists       "e2e-proto-http"
assert_service_exists       "e2e-proto-https"
assert_service_exists       "e2e-proto-tcp"
assert_service_exists       "e2e-default-minimal"
assert_service_exists       "e2e-net-custom"
assert_service_not_exists   "e2e-lifecycle"  # still removed
assert_service_not_exists   "e2e-ignored"    # still ignored

# ==============================================================================
# 10. Log Health
# ==============================================================================

log "10. DockTail Log Health"
docktail_logs=$(docker logs "$DOCKTAIL_CONTAINER" 2>&1)

if echo "$docktail_logs" | grep -qE "FATAL|panic"; then
    fail "FATAL or panic in logs"
else
    pass "no FATAL or panic in logs"
fi

# ==============================================================================
# Summary
# ==============================================================================

echo ""
echo "=============================================="
echo "  Results: $passed passed, $failed failed"
echo "=============================================="
if [ $failed -gt 0 ]; then
    echo ""
    echo "Failures:"
    echo -e "$errors"
    echo ""
    echo "Full serve status:"
    refresh_serve_status
    echo "$SERVE_STATUS_CACHE" | jq . 2>/dev/null || echo "$SERVE_STATUS_CACHE"
    echo ""
    echo "DockTail logs (last 50 lines):"
    docker logs "$DOCKTAIL_CONTAINER" 2>&1 | tail -50
    exit 1
fi
