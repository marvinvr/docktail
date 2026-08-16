#!/usr/bin/env bash

COMPOSE_FILE="docker-compose.e2e.yaml"
E2E_SECRETS_DIR=".e2e-secrets"
TS_CONTAINER="e2e-tailscale"
DOCKTAIL_CONTAINER="e2e-docktail"
MAX_WAIT=120
RECONCILE_WAIT=10
SCRIPT_TIMEOUT=600
MANUAL_PROTECTED_SERVICE_NAME="svc:e2e-manual-protected"
MANUAL_PROTECTED_SERVICE_PORT="80"
# Control-plane cleanup test fixtures (created directly via the Tailscale API)
ORPHAN_SERVICE_NAME="svc:e2e-orphan"           # never advertised -> DockTail should delete it
ORPHAN_SERVICE_PORT="80"
API_TAILNET="${TS_TAILNET:--}"
API_BASE="https://api.tailscale.com/api/v2"

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
    sweep_e2e_services
    rm -rf "$E2E_SECRETS_DIR"
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

mkdir -p "$E2E_SECRETS_DIR"
chmod 700 "$E2E_SECRETS_DIR"
printf '%s\n' "${TS_OAUTH_CLIENT_ID:-}" > "$E2E_SECRETS_DIR/tailscale_oauth_client_id"
printf '%s\n' "${TS_OAUTH_CLIENT_SECRET:-}" > "$E2E_SECRETS_DIR/tailscale_oauth_client_secret"
chmod 600 "$E2E_SECRETS_DIR/tailscale_oauth_client_id" "$E2E_SECRETS_DIR/tailscale_oauth_client_secret"
echo "  DockTail OAuth credentials prepared as file-backed secrets"

# ==============================================================================
# Helpers
# ==============================================================================

# Cache serve status to avoid repeated exec calls within a test group
SERVE_STATUS_CACHE=""

refresh_serve_status() {
    SERVE_STATUS_CACHE=$(docker exec "$TS_CONTAINER" tailscale serve status --json 2>/dev/null || echo "{}")
}

# Poll until a service reaches an exact port/protocol state. Container
# replacement legitimately produces a brief remove-then-add window while the
# stop/die/start events are reconciled, so update checks must wait for the final
# state instead of sampling that transition at a fixed instant.
wait_for_service_state() {
    local name="svc:$1"
    local expected_port="$2"
    local expected_proto="$3"
    local timeout="${4:-30}"
    local elapsed=0
    local actual_port terminate_tls is_https is_http actual_proto

    while [ "$elapsed" -lt "$timeout" ]; do
        refresh_serve_status
        actual_port=$(echo "$SERVE_STATUS_CACHE" | jq -r ".Services[\"$name\"].TCP | keys[0] // empty" 2>/dev/null || true)
        if [ "$actual_port" = "$expected_port" ]; then
            terminate_tls=$(echo "$SERVE_STATUS_CACHE" | jq -r ".Services[\"$name\"].TCP[\"$actual_port\"].TerminateTLS // empty" 2>/dev/null || true)
            is_https=$(echo "$SERVE_STATUS_CACHE" | jq -r ".Services[\"$name\"].TCP[\"$actual_port\"].HTTPS // false" 2>/dev/null || true)
            is_http=$(echo "$SERVE_STATUS_CACHE" | jq -r ".Services[\"$name\"].TCP[\"$actual_port\"].HTTP // false" 2>/dev/null || true)
            if [ -n "$terminate_tls" ]; then
                actual_proto="tls-terminated-tcp"
            elif [ "$is_https" = "true" ]; then
                actual_proto="https"
            elif [ "$is_http" = "true" ]; then
                actual_proto="http"
            else
                actual_proto="tcp"
            fi
            if [ "$actual_proto" = "$expected_proto" ]; then
                return 0
            fi
        fi
        sleep 1
        elapsed=$((elapsed + 1))
    done

    fail "$name did not converge to $expected_proto/$expected_port within ${timeout}s"
    return 1
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

# Check that a service has a specific port anywhere in its TCP config (multi-port aware)
assert_service_has_port() {
    local name="svc:$1"
    local expected_port="$2"
    if ! echo "$SERVE_STATUS_CACHE" | jq -e ".Services[\"$name\"]" >/dev/null 2>&1; then
        fail "$name not found (checking for port $expected_port)"
        return
    fi
    if echo "$SERVE_STATUS_CACHE" | jq -e ".Services[\"$name\"].TCP[\"$expected_port\"]" >/dev/null 2>&1; then
        pass "$name has port $expected_port"
    else
        local actual_ports
        actual_ports=$(echo "$SERVE_STATUS_CACHE" | jq -r ".Services[\"$name\"].TCP | keys | join(\", \")" 2>/dev/null || echo "<none>")
        fail "$name expected port $expected_port, available ports: $actual_ports"
    fi
}

# Check the total number of TCP ports on a service
assert_service_port_count() {
    local name="svc:$1"
    local expected_count="$2"
    local actual_count
    actual_count=$(echo "$SERVE_STATUS_CACHE" | jq -r ".Services[\"$name\"].TCP | keys | length" 2>/dev/null || echo "0")
    if [ "$actual_count" = "$expected_count" ]; then
        pass "$name has $expected_count port(s)"
    else
        fail "$name expected $expected_count port(s), got $actual_count"
    fi
}

# Check service protocol via TCP config flags
# "tls-terminated-tcp" = TerminateTLS set, "http" = HTTP:true,
# "https" = HTTPS:true, "tcp" = none of those fields set.
assert_service_protocol() {
    local name="svc:$1"
    local expected_proto="$2"
    local port
    port=$(echo "$SERVE_STATUS_CACHE" | jq -r ".Services[\"$name\"].TCP | keys[0] // empty" 2>/dev/null || true)
    if [ -z "$port" ]; then
        fail "$name protocol check: no TCP config found"
        return
    fi

    local terminate_tls is_https is_http actual
    terminate_tls=$(echo "$SERVE_STATUS_CACHE" | jq -r ".Services[\"$name\"].TCP[\"$port\"].TerminateTLS // empty" 2>/dev/null || true)
    is_https=$(echo "$SERVE_STATUS_CACHE" | jq -r ".Services[\"$name\"].TCP[\"$port\"].HTTPS // false" 2>/dev/null || true)
    is_http=$(echo "$SERVE_STATUS_CACHE" | jq -r ".Services[\"$name\"].TCP[\"$port\"].HTTP // false" 2>/dev/null || true)

    if [ -n "$terminate_tls" ]; then
        actual="tls-terminated-tcp"
    elif [ "$is_https" = "true" ]; then
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

# Check PROXY protocol version on the TCP handler (0 / omitted means unset).
assert_service_proxy_protocol() {
    local name="svc:$1"
    local expected="$2"
    local port
    port=$(echo "$SERVE_STATUS_CACHE" | jq -r ".Services[\"$name\"].TCP | keys[0] // empty" 2>/dev/null || true)
    if [ -z "$port" ]; then
        fail "$name proxy-protocol check: no TCP config found"
        return
    fi

    local actual
    actual=$(echo "$SERVE_STATUS_CACHE" | jq -r ".Services[\"$name\"].TCP[\"$port\"].ProxyProtocol // 0" 2>/dev/null || echo "0")
    if [ "$actual" = "$expected" ]; then
        pass "$name PROXY protocol is $expected"
    else
        fail "$name expected PROXY protocol $expected, got $actual"
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

create_manual_protected_service() {
    docker exec "$TS_CONTAINER" tailscale serve \
        --service="$MANUAL_PROTECTED_SERVICE_NAME" \
        --http="$MANUAL_PROTECTED_SERVICE_PORT" \
        http://127.0.0.1:19080 >/dev/null 2>&1
}

clear_manual_protected_service() {
    docker exec "$TS_CONTAINER" tailscale serve clear "$MANUAL_PROTECTED_SERVICE_NAME" >/dev/null 2>&1 || true
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

assert_funnel_path() {
    local port="$1"
    local path="$2"
    local funnel_status
    funnel_status=$(get_funnel_status)
    if echo "$funnel_status" | jq -e --arg port "$port" --arg path "$path" '.Web | to_entries[] | select(.key | endswith(":" + $port)) | .value.Handlers[$path].Proxy? | strings | select(length > 0)' >/dev/null 2>&1; then
        pass "funnel path $path active on port $port"
    else
        fail "funnel path $path not found on port $port"
    fi
}

wait_for_docktail_log() {
    local pattern="$1"
    local timeout="${2:-$RECONCILE_WAIT}"
    local elapsed=0
    local logs

    while [ "$elapsed" -lt "$timeout" ]; do
        logs=$(docker logs "$DOCKTAIL_CONTAINER" 2>&1 || true)
        if grep -q -- "$pattern" <<<"$logs"; then
            pass "docktail log contains '$pattern'"
            return 0
        fi
        sleep 1
        elapsed=$((elapsed + 1))
    done

    fail "docktail log missing '$pattern'"
    return 1
}

# --- Tailscale Control Plane API helpers (for unused-service cleanup tests) ---

# Mint a fresh OAuth access token. Echoes the token, or empty if no creds.
mint_api_token() {
    if [ -z "${TS_OAUTH_CLIENT_ID:-}" ] || [ -z "${TS_OAUTH_CLIENT_SECRET:-}" ]; then
        echo ""
        return
    fi
    curl -s -X POST "${API_BASE}/oauth/token" \
        -u "${TS_OAUTH_CLIENT_ID}:${TS_OAUTH_CLIENT_SECRET}" \
        -d "grant_type=client_credentials" 2>/dev/null \
        | jq -r '.access_token // empty' 2>/dev/null || echo ""
}

# Print the HTTP status code of GET on a service definition (200 exists, 404 gone).
api_service_status() {
    local token="$1" name="$2"
    curl -s -o /dev/null -w "%{http_code}" \
        -H "Authorization: Bearer ${token}" \
        "${API_BASE}/tailnet/${API_TAILNET}/services/${name}" 2>/dev/null || echo "000"
}

# Create/update a service definition via PUT. Echoes HTTP status code.
api_create_service() {
    local token="$1" name="$2"
    curl -s -o /dev/null -w "%{http_code}" -X PUT \
        -H "Authorization: Bearer ${token}" \
        -H "Content-Type: application/json" \
        -d "{\"name\":\"${name}\",\"tags\":[\"tag:ci-test-container\"],\"ports\":[\"tcp:${ORPHAN_SERVICE_PORT}\"]}" \
        "${API_BASE}/tailnet/${API_TAILNET}/services/${name}" 2>/dev/null || echo "000"
}

# Print the "comment" (description) of a service definition, empty if unset/gone.
api_service_comment() {
    local token="$1" name="$2"
    curl -s -H "Authorization: Bearer ${token}" \
        "${API_BASE}/tailnet/${API_TAILNET}/services/${name}" 2>/dev/null \
        | jq -r '.comment // empty' 2>/dev/null || echo ""
}

# Assert that a Control Plane service definition carries exactly the expected
# tags (order-independent). The tags a service carries are NOT visible in
# `tailscale serve status`; they only live in the service definition on the
# control plane, so this is the only way to actually verify tag parsing.
# Polls until the tags converge on the expected set (the definition is created
# asynchronously during reconciliation), so the check reflects the reconciled
# result rather than the first partial/empty response. Usage:
# assert_service_tags <token> <service-name> <tag>...
assert_service_tags() {
    local token="$1" name="svc:$2"
    shift 2
    local expected actual=""
    expected=$(jq -rn --args '$ARGS.positional | sort | join(",")' "$@")

    local attempt
    for attempt in $(seq 1 20); do
        actual=$(curl -s -H "Authorization: Bearer ${token}" \
            "${API_BASE}/tailnet/${API_TAILNET}/services/${name}" 2>/dev/null \
            | jq -r '(.tags // []) | sort | join(",")' 2>/dev/null || true)
        if [ "$actual" = "$expected" ]; then
            break
        fi
        sleep 2
    done

    if [ "$actual" = "$expected" ]; then
        pass "$name has tags [$expected]"
    else
        fail "$name expected tags [$expected], got [${actual:-<none>}]"
    fi
}

# Simulate a manual admin-console edit on an existing service definition:
# fetch the current definition, overwrite its tags and comment, and PUT it
# back with name/addrs/ports preserved. Echoes the PUT HTTP status code.
# Usage: api_tamper_service_tags <token> <svc:name> <comment> <tag>...
api_tamper_service_tags() {
    local token="$1" name="$2" comment="$3"
    shift 3
    local current payload
    current=$(curl -s -H "Authorization: Bearer ${token}" \
        "${API_BASE}/tailnet/${API_TAILNET}/services/${name}" 2>/dev/null)
    payload=$(echo "$current" | jq -c --arg comment "$comment" --args \
        '.tags = $ARGS.positional | .comment = $comment' "$@" 2>/dev/null)
    if [ -z "$payload" ]; then
        echo "000"
        return
    fi
    curl -s -o /dev/null -w "%{http_code}" -X PUT \
        -H "Authorization: Bearer ${token}" \
        -H "Content-Type: application/json" \
        -d "$payload" \
        "${API_BASE}/tailnet/${API_TAILNET}/services/${name}" 2>/dev/null || echo "000"
}

# Delete a service definition (best effort).
api_delete_service() {
    local token="$1" name="$2"
    curl -s -o /dev/null -X DELETE \
        -H "Authorization: Bearer ${token}" \
        "${API_BASE}/tailnet/${API_TAILNET}/services/${name}" 2>/dev/null || true
}

# Best-effort removal of any leftover e2e-* service definitions so repeated CI
# runs against the shared tailnet don't accumulate orphaned services.
sweep_e2e_services() {
    local token leftovers name
    token=$(mint_api_token)
    [ -z "$token" ] && return 0
    leftovers=$(curl -s -H "Authorization: Bearer ${token}" \
        "${API_BASE}/tailnet/${API_TAILNET}/services" 2>/dev/null \
        | jq -r '.vipServices[]?.name // empty' 2>/dev/null | grep '^svc:e2e-' || true)
    for name in $leftovers; do
        api_delete_service "$token" "$name"
        echo "  Swept leftover service $name"
    done
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

echo "  --- TLS-terminated TCP ---"
assert_service_exists       "e2e-proto-tls-terminated-tcp"
assert_service_port         "e2e-proto-tls-terminated-tcp" "6697"
assert_service_protocol     "e2e-proto-tls-terminated-tcp" "tls-terminated-tcp"

echo "  --- TCP with PROXY protocol v2 ---"
assert_service_exists       "e2e-proxy-protocol"
assert_service_port         "e2e-proxy-protocol" "8443"
assert_service_protocol     "e2e-proxy-protocol" "tcp"
assert_service_proxy_protocol "e2e-proxy-protocol" "2"
assert_service_proxy_protocol "e2e-proto-tcp" "0"

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
assert_service_destination_contains "e2e-net-published" "127.0.0.1:19080"

echo "  --- Host networking ---"
assert_service_exists       "e2e-net-host"
assert_service_destination_contains "e2e-net-host" "127.0.0.1:80"

echo "  --- target port 443 (→ http/80) ---"
assert_service_exists       "e2e-default-target443"
assert_service_port         "e2e-default-target443" "80"
assert_service_protocol     "e2e-default-target443" "http"

# ==============================================================================
# 4. Funnel
# ==============================================================================

log "4. Funnel"
echo "  --- service + funnel ---"
assert_service_exists       "e2e-funnel"
assert_funnel_active        "443"
assert_funnel_path          "443" "/"
assert_funnel_path          "443" "/e2e-path"

echo "  --- funnel only ---"
assert_service_not_exists   "e2e-funnel-only"
assert_funnel_active        "8443"

# ==============================================================================
# 5. Custom Tags
# ==============================================================================

log "5. Custom Tags"
assert_service_exists       "e2e-custom-tags"

# The container sets `docktail.tags=tag:web,tag:production` (comma-separated).
# Verify BOTH tags actually landed on the service definition in the control
# plane. This guards the comma-splitting tag parser: a regression that dropped
# the second tag, mis-split the value, or ignored the label would be caught
# here (an exact, order-independent match), whereas the existence check above
# would still pass. The tags are only visible via the Control Plane API, not
# `tailscale serve status`, so this needs an OAuth token — without one we fail
# loudly rather than silently skip the verification.
CUSTOM_TAGS_TOKEN=$(mint_api_token)
if [ -z "$CUSTOM_TAGS_TOKEN" ]; then
    fail "no OAuth credentials available to verify custom tags via Control Plane API"
else
    assert_service_tags "$CUSTOM_TAGS_TOKEN" "e2e-custom-tags" "tag:web" "tag:production"
fi

# ==============================================================================
# 6. Multiple Ports
# ==============================================================================

log "6. Multiple Services from One Container"

echo "  --- Primary service + one indexed service ---"
assert_service_exists       "e2e-multiport"
assert_service_has_port     "e2e-multiport" "443"
assert_service_exists       "e2e-multiport-secondary"
assert_service_has_port     "e2e-multiport-secondary" "8080"

echo "  --- Primary service + two indexed services (non-contiguous) ---"
assert_service_exists       "e2e-multiport-three"
assert_service_has_port     "e2e-multiport-three" "443"
assert_service_exists       "e2e-multiport-three-b"
assert_service_has_port     "e2e-multiport-three-b" "3000"
assert_service_exists       "e2e-multiport-three-c"
assert_service_has_port     "e2e-multiport-three-c" "5000"

# ==============================================================================
# 7. Ignored Container (no docktail labels)
# ==============================================================================

log "7. Ignored Container"
assert_service_not_exists   "e2e-ignored"

# ==============================================================================
# 8. Ignore Service Names: keep manual svc:* entries
# ==============================================================================

log "8. Ignore Service Names"

echo "  --- Creating manual protected service ---"
clear_manual_protected_service
create_manual_protected_service
refresh_serve_status
assert_service_exists       "e2e-manual-protected"
assert_service_port         "e2e-manual-protected" "$MANUAL_PROTECTED_SERVICE_PORT"
assert_service_protocol     "e2e-manual-protected" "http"
assert_service_destination_contains "e2e-manual-protected" "127.0.0.1:19080"

echo "  Waiting for reconciliation while protected service exists..."
sleep "$RECONCILE_WAIT"
refresh_serve_status

echo "  --- Post-reconcile: manual protected service should still exist ---"
assert_service_exists       "e2e-manual-protected"
assert_service_port         "e2e-manual-protected" "$MANUAL_PROTECTED_SERVICE_PORT"
wait_for_docktail_log "Skipping removal for ignored service"

echo "  --- Cleaning up manual protected service ---"
clear_manual_protected_service
refresh_serve_status
assert_service_not_exists   "e2e-manual-protected"

# ==============================================================================
# 9. Lifecycle: service removal on container stop
# ==============================================================================

log "9. Lifecycle"

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
# 10. Service Update: change protocol from HTTP to HTTPS
# ==============================================================================

log "10. Service Update"

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
wait_for_service_state "e2e-update" "443" "https" "$((RECONCILE_WAIT * 3))"

echo "  --- Post-update: service should be HTTPS/443 ---"
assert_service_exists       "e2e-update"
assert_service_port         "e2e-update" "443"
assert_service_protocol     "e2e-update" "https"

# ==============================================================================
# 11. Idempotency: reconciling again changes nothing
# ==============================================================================

log "11. Idempotency"
echo "  Waiting for another reconciliation cycle..."
sleep "$RECONCILE_WAIT"
refresh_serve_status

# All non-stopped services should still be there
assert_service_exists       "e2e-proto-http"
assert_service_exists       "e2e-proto-https"
assert_service_exists       "e2e-proto-tcp"
assert_service_exists       "e2e-proto-tls-terminated-tcp"
assert_service_exists       "e2e-proxy-protocol"
assert_service_exists       "e2e-default-minimal"
assert_service_exists       "e2e-net-custom"
assert_service_exists       "e2e-multiport"
assert_service_exists       "e2e-multiport-three"
assert_service_not_exists   "e2e-lifecycle"  # still removed
assert_service_not_exists   "e2e-ignored"    # still ignored
assert_service_not_exists   "e2e-manual-protected"  # cleaned up explicitly

# ==============================================================================
# 12. TCP Reconciliation Stability (issue #56)
# ==============================================================================
#
# Regression test for https://github.com/marvinvr/docktail/issues/56:
# TCP services were detected as "changed" on every reconciliation cycle when
# DockTail could not parse their full Tailscale serve status. Plain TCP stores
# its destination on the TCP handler (issue #56), while TLS-terminated TCP also
# uses the TerminateTLS field to distinguish it from plain TCP (issue #71).
#
# A correctly reconciling TCP service is flagged as "changed" zero times once it
# has been established. We measure the number of "Service configuration changed"
# log lines for the TCP service over several reconciliation cycles; with the bug
# present the count grows by one per cycle, with the fix it stays flat. The
# PROXY-protocol service is included so a missing ProxyProtocol field in serve
# status cannot flap the endpoint every cycle (issue #77).

log "12. TCP Reconciliation Stability (issue #56)"

count_service_changed() {
    local changed_key="$1"
    docker logs "$DOCKTAIL_CONTAINER" 2>&1 \
        | grep "Service configuration changed, will update" \
        | grep -c -- "$changed_key" || true
}

echo "  --- Pre-check: TCP service exists ---"
refresh_serve_status
assert_service_exists       "e2e-proto-tcp"
assert_service_protocol     "e2e-proto-tcp" "tcp"
assert_service_exists       "e2e-proto-tls-terminated-tcp"
assert_service_protocol     "e2e-proto-tls-terminated-tcp" "tls-terminated-tcp"
assert_service_exists       "e2e-proxy-protocol"
assert_service_protocol     "e2e-proxy-protocol" "tcp"
assert_service_proxy_protocol "e2e-proxy-protocol" "2"

echo "  Measuring TCP reconciliation stability across multiple cycles..."
before_tcp_changed=$(count_service_changed "key=svc:e2e-proto-tcp:5432")
before_tls_tcp_changed=$(count_service_changed "key=svc:e2e-proto-tls-terminated-tcp:6697")
before_proxy_changed=$(count_service_changed "key=svc:e2e-proxy-protocol:8443")
sleep 16   # >= 3 reconcile cycles at RECONCILE_INTERVAL=5s
after_tcp_changed=$(count_service_changed "key=svc:e2e-proto-tcp:5432")
after_tls_tcp_changed=$(count_service_changed "key=svc:e2e-proto-tls-terminated-tcp:6697")
after_proxy_changed=$(count_service_changed "key=svc:e2e-proxy-protocol:8443")
tcp_changed_delta=$((after_tcp_changed - before_tcp_changed))
tls_tcp_changed_delta=$((after_tls_tcp_changed - before_tls_tcp_changed))
proxy_changed_delta=$((after_proxy_changed - before_proxy_changed))

if [ "$tcp_changed_delta" -le 0 ]; then
    pass "TCP service stable: not re-detected as changed across cycles (delta=$tcp_changed_delta)"
else
    fail "TCP service re-detected as changed $tcp_changed_delta time(s) across reconcile cycles (issue #56: TCP services re-added every reconciliation)"
fi

if [ "$tls_tcp_changed_delta" -le 0 ]; then
    pass "TLS-terminated TCP service stable: not re-detected as changed across cycles (delta=$tls_tcp_changed_delta)"
else
    fail "TLS-terminated TCP service re-detected as changed $tls_tcp_changed_delta time(s) across reconcile cycles (issue #71)"
fi

if [ "$proxy_changed_delta" -le 0 ]; then
    pass "PROXY-protocol TCP service stable: not re-detected as changed across cycles (delta=$proxy_changed_delta)"
else
    fail "PROXY-protocol TCP service re-detected as changed $proxy_changed_delta time(s) across reconcile cycles (issue #77: ProxyProtocol not parsed from serve status)"
fi

# ==============================================================================
# 13. Control Plane Cleanup of Unused Services (DELETE_UNUSED_SERVICES=true)
# ==============================================================================
#
# DockTail runs with DELETE_UNUSED_SERVICES=true in this stack. The live test
# verifies that it deletes a zero-host definition while preserving a Service in
# its desired set. Preservation of a Service advertised by another host is
# covered deterministically in tailscale/client_test.go; creating that topology
# here would make CI depend on the shared tailnet's host-approval policy.

log "13. Control Plane Cleanup of Unused Services"

API_TOKEN=$(mint_api_token)

if [ -z "$API_TOKEN" ]; then
    echo "  SKIP: no OAuth credentials available for Control Plane API assertions"
else
    # Capture whether DockTail created a definition for a running service, so the
    # "preserved" assertion is decoupled from Control Plane creation timing.
    proto_http_before=$(api_service_status "$API_TOKEN" "svc:e2e-proto-http")

    echo "  --- Creating an unused (zero-host) service via API ---"
    create_status=$(api_create_service "$API_TOKEN" "$ORPHAN_SERVICE_NAME")
    if [ "$create_status" = "200" ]; then
        pass "created $ORPHAN_SERVICE_NAME via API"
    else
        fail "failed to create $ORPHAN_SERVICE_NAME via API (status $create_status)"
    fi

    echo "  --- DockTail should delete the unused service ---"
    orphan_deleted=0
    for _ in $(seq 1 20); do
        if [ "$(api_service_status "$API_TOKEN" "$ORPHAN_SERVICE_NAME")" = "404" ]; then
            orphan_deleted=1
            break
        fi
        sleep 2
    done
    if [ "$orphan_deleted" = "1" ]; then
        pass "$ORPHAN_SERVICE_NAME deleted by DockTail (no advertising hosts)"
    else
        fail "$ORPHAN_SERVICE_NAME still exists; DockTail did not delete the unused service"
    fi

    echo "  --- DockTail must keep the running container's service ---"
    if [ "$proto_http_before" = "200" ]; then
        if [ "$(api_service_status "$API_TOKEN" "svc:e2e-proto-http")" = "200" ]; then
            pass "svc:e2e-proto-http preserved (DockTail is advertising it)"
        else
            fail "svc:e2e-proto-http was deleted despite being advertised by DockTail"
        fi
    else
        echo "  SKIP: svc:e2e-proto-http has no Control Plane definition to check"
    fi

    wait_for_docktail_log "Deleting unused service definition"

    echo "  --- Cleaning up cleanup-test fixture ---"
    api_delete_service "$API_TOKEN" "$ORPHAN_SERVICE_NAME"
fi

# ==============================================================================
# 14. Service Description (synced to Control Plane "comment", issue #60)
# ==============================================================================
#
# https://github.com/marvinvr/docktail/issues/60
# docktail.service.description sets the human-readable description shown per
# service in the Tailscale admin panel. DockTail syncs it to the Service
# definition's "comment" field via the Control Plane API, so it is only
# verifiable when API credentials are configured. Indexed services carry their
# own description independently of the primary service. Covers both the initial
# sync and a later description change being reflected in the comment.

log "14. Service Description (issue #60)"

echo "  --- Pre-check: described services exist locally ---"
refresh_serve_status
assert_service_exists       "e2e-description"
assert_service_exists       "e2e-description-secondary"

if [ -z "${API_TOKEN:-}" ]; then
    echo "  SKIP: no OAuth credentials available for Control Plane API assertions"
else
    echo "  --- Primary service description synced to Control Plane comment ---"
    desc_synced=0
    for _ in $(seq 1 20); do
        if [ "$(api_service_comment "$API_TOKEN" "svc:e2e-description")" = "E2E Bookmark Manager" ]; then
            desc_synced=1
            break
        fi
        sleep 2
    done
    if [ "$desc_synced" = "1" ]; then
        pass "svc:e2e-description comment set to 'E2E Bookmark Manager'"
    else
        fail "svc:e2e-description comment not synced (got '$(api_service_comment "$API_TOKEN" "svc:e2e-description")')"
    fi

    echo "  --- Indexed service description synced independently ---"
    idx_desc_synced=0
    for _ in $(seq 1 20); do
        if [ "$(api_service_comment "$API_TOKEN" "svc:e2e-description-secondary")" = "E2E Secondary Service" ]; then
            idx_desc_synced=1
            break
        fi
        sleep 2
    done
    if [ "$idx_desc_synced" = "1" ]; then
        pass "svc:e2e-description-secondary comment set to 'E2E Secondary Service'"
    else
        fail "svc:e2e-description-secondary comment not synced (got '$(api_service_comment "$API_TOKEN" "svc:e2e-description-secondary")')"
    fi

    echo "  --- Changing the description is reflected in the Control Plane comment ---"
    docker stop e2e-description >/dev/null 2>&1 || true
    docker rm e2e-description >/dev/null 2>&1 || true
    docker run -d \
        --name e2e-description \
        --restart no \
        --label "docktail.service.enable=true" \
        --label "docktail.service.name=e2e-description" \
        --label "docktail.service.port=80" \
        --label "docktail.service.description=E2E Updated Description" \
        nginx:alpine >/dev/null 2>&1

    desc_updated=0
    for _ in $(seq 1 20); do
        if [ "$(api_service_comment "$API_TOKEN" "svc:e2e-description")" = "E2E Updated Description" ]; then
            desc_updated=1
            break
        fi
        sleep 2
    done
    if [ "$desc_updated" = "1" ]; then
        pass "svc:e2e-description comment changed to 'E2E Updated Description'"
    else
        fail "svc:e2e-description comment not updated (got '$(api_service_comment "$API_TOKEN" "svc:e2e-description")')"
    fi
fi

# ==============================================================================
# 15. Authoritative Tag Reconciliation (issue #63)
# ==============================================================================
#
# https://github.com/marvinvr/docktail/issues/63
# Labels are the source of truth for service tags: manual edits to a service
# definition's tags in the admin console must be overwritten on the next
# reconcile. Previously tags were only applied at creation time, so a tag label
# added after the service already existed never took effect.
#
# Simulate a manual console edit by rewriting svc:e2e-custom-tags with a stale
# tag set plus a manual comment, then assert DockTail converges the tags back
# to the label-declared set. The container declares no description label, so
# the manual comment must survive the tag update (an empty declared value
# means DockTail leaves the existing one alone).

log "15. Authoritative Tag Reconciliation (issue #63)"

TAG_SYNC_TOKEN=$(mint_api_token)
if [ -z "$TAG_SYNC_TOKEN" ]; then
    fail "no OAuth credentials available to verify tag reconciliation via Control Plane API"
else
    echo "  --- Simulating a manual tag edit in the admin console ---"
    tamper_status=$(api_tamper_service_tags "$TAG_SYNC_TOKEN" "svc:e2e-custom-tags" "e2e manual comment" "tag:ci-test-container")
    if [ "$tamper_status" = "200" ]; then
        pass "rewrote svc:e2e-custom-tags with stale tags via API"
    else
        fail "failed to rewrite svc:e2e-custom-tags via API (status $tamper_status)"
    fi

    echo "  --- DockTail must revert the tags to the label-declared set ---"
    assert_service_tags "$TAG_SYNC_TOKEN" "e2e-custom-tags" "tag:web" "tag:production"
    wait_for_docktail_log "Updating service tags in Control Plane"

    echo "  --- Manual comment survives (no description label declared) ---"
    tampered_comment=$(api_service_comment "$TAG_SYNC_TOKEN" "svc:e2e-custom-tags")
    if [ "$tampered_comment" = "e2e manual comment" ]; then
        pass "manual comment preserved on svc:e2e-custom-tags"
    else
        fail "manual comment lost on svc:e2e-custom-tags (got '${tampered_comment:-<none>}')"
    fi
fi

# ==============================================================================
# 16. Log Health
# ==============================================================================

log "16. DockTail Log Health"
docktail_logs=$(docker logs "$DOCKTAIL_CONTAINER" 2>&1)

if grep -qE "FATAL|panic" <<<"$docktail_logs"; then
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
