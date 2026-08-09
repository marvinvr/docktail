#!/usr/bin/env bash
#
# Repro harness for https://github.com/marvinvr/docktail/issues/72
# ("Service offline after container auto update").
#
# The reported symptom is that a Tailscale Service goes Offline / "0 hosts" in
# the admin console while the local serve config is perfectly healthy, and that
# it recovers only on a DockTail restart, when an unrelated service is touched,
# or after hours. Local `tailscale serve status` therefore proves nothing; the
# only ground truth is the Control Plane's view of who hosts the Service:
#
#   GET /api/v2/tailnet/<tailnet>/services/<svc>/devices  ->  .hosts[]
#
# A node's hosting state is derived from two independent pieces of local state:
#   1. ServeConfig.Services[svc]   (handlers/ports; `tailscale serve --service`)
#   2. prefs.AdvertiseServices     (Active; cleared by `serve drain`/`serve clear`)
# tailscaled merges them into []tailcfg.VIPService{Name,Ports,Active}, hashes
# that, and sends only the hash in Hostinfo.ServicesHash. Control pulls the real
# list back over c2n GET /vip-services, and the client re-pushes only when the
# hash changes. So any teardown/re-add cycle that lands back on its original
# hash gives control exactly one chance to observe the truth.
#
# The experiments below drive that cycle and check the Control Plane afterwards:
#   EXP0  baseline: how long a freshly created service takes to reach >=1 host
#   EXP1  manual flap: drain+clear+re-serve by hand (no DockTail involvement)
#   EXP2  DockTail flap: container replacement, i.e. the actual issue #72 path
#   EXP3  revival probe: does touching an unrelated service un-stick a dead one
#
# Exits 1 when the bug is provoked (TDD: red while the bug exists, green after
# the fix), and 2 on harness/infra failure.

COMPOSE_FILE="docker-compose.e2e-flap.yaml"
E2E_SECRETS_DIR=".e2e-secrets"
TS_CONTAINER="e2e-flap-tailscale"
DOCKTAIL_CONTAINER="e2e-flap-docktail"
API_BASE="https://api.tailscale.com/api/v2"
API_TAILNET="${TS_TAILNET:--}"

MAX_WAIT=120           # tailscaled backend Running
BASELINE_BUDGET=150    # first-ever host registration may include approval
FLAP_BUDGET=75         # a healthy re-advertise is expected well inside this
FLAP_ITERATIONS="${FLAP_ITERATIONS:-3}"
SCRIPT_TIMEOUT="${SCRIPT_TIMEOUT:-1500}"

SVC_A="svc:e2e-flap-a"        # DockTail-managed, target of EXP2
SVC_B="svc:e2e-flap-b"        # DockTail-managed, second service
SVC_MANUAL="svc:e2e-flap-manual"   # hand-driven, target of EXP1
SVC_PROBE="svc:e2e-flap-probe"     # hand-driven, hash poke for EXP3

repro_hits=0
harness_errors=0
findings=""

( sleep "$SCRIPT_TIMEOUT" && echo "ERROR: harness timed out after ${SCRIPT_TIMEOUT}s" && kill $$ ) 2>/dev/null &
TIMEOUT_PID=$!

log()     { echo ""; echo "=== $1"; }
step()    { echo "  -- $1"; }
ok()      { echo "  OK: $1"; }
repro()   { echo "  REPRO: $1"; repro_hits=$((repro_hits + 1)); findings="${findings}\n  - $1"; }
harness() { echo "  HARNESS-ERROR: $1"; harness_errors=$((harness_errors + 1)); }

cleanup() {
    log "Cleanup"
    kill "$TIMEOUT_PID" 2>/dev/null || true
    docker rm -f e2e-flap-a e2e-flap-b >/dev/null 2>&1 || true
    docker compose -f "$COMPOSE_FILE" down -v --remove-orphans 2>/dev/null || true
    # Defined further down; a preflight failure can trip this trap before then.
    command -v sweep_flap_services >/dev/null 2>&1 && sweep_flap_services
    rm -rf "$E2E_SECRETS_DIR"
}
trap cleanup EXIT

set -euo pipefail

# ==============================================================================
# Preflight
# ==============================================================================

if [ -z "${TS_OAUTH_CLIENT_ID:-}" ] || [ -z "${TS_OAUTH_CLIENT_SECRET:-}" ]; then
    echo "ERROR: TS_OAUTH_CLIENT_ID + TS_OAUTH_CLIENT_SECRET are required (the"
    echo "       Control Plane API is the only ground truth for this harness)"
    exit 2
fi

mint_api_token() {
    curl -s -X POST "${API_BASE}/oauth/token" \
        -u "${TS_OAUTH_CLIENT_ID}:${TS_OAUTH_CLIENT_SECRET}" \
        -d "grant_type=client_credentials" 2>/dev/null \
        | jq -r '.access_token // empty' 2>/dev/null || echo ""
}

API_TOKEN=$(mint_api_token)
[ -z "$API_TOKEN" ] && { echo "ERROR: could not mint an API token"; exit 2; }
echo "  API token minted for tailnet ${API_TAILNET}"

TOKEN_RESPONSE=$(curl -s -X POST "${API_BASE}/oauth/token" \
    -u "${TS_OAUTH_CLIENT_ID}:${TS_OAUTH_CLIENT_SECRET}" \
    -d "grant_type=client_credentials")
TS_TOKEN=$(echo "$TOKEN_RESPONSE" | jq -r '.access_token // empty')
KEY_RESPONSE=$(curl -s -X POST "${API_BASE}/tailnet/${API_TAILNET}/keys" \
    -H "Authorization: Bearer ${TS_TOKEN}" \
    -H "Content-Type: application/json" \
    -d '{"capabilities":{"devices":{"create":{"reusable":false,"ephemeral":true,"tags":["tag:ci-test"]}}},"expirySeconds":1800}')
TS_AUTHKEY=$(echo "$KEY_RESPONSE" | jq -r '.key // empty')
[ -z "$TS_AUTHKEY" ] && { echo "ERROR: failed to generate auth key"; echo "$KEY_RESPONSE"; exit 2; }
export TS_AUTHKEY
echo "  Ephemeral auth key generated"

mkdir -p "$E2E_SECRETS_DIR"
chmod 700 "$E2E_SECRETS_DIR"
printf '%s\n' "${TS_OAUTH_CLIENT_ID}" > "$E2E_SECRETS_DIR/tailscale_oauth_client_id"
printf '%s\n' "${TS_OAUTH_CLIENT_SECRET}" > "$E2E_SECRETS_DIR/tailscale_oauth_client_secret"
chmod 600 "$E2E_SECRETS_DIR"/*

# ==============================================================================
# Helpers
# ==============================================================================

# Stderr is deliberately left alone: callers that parse JSON redirect it away,
# callers that want to see CLI errors keep it.
ts() { docker exec "$TS_CONTAINER" tailscale "$@"; }

api_get() {
    curl -s -H "Authorization: Bearer ${API_TOKEN}" "${API_BASE}/tailnet/${API_TAILNET}/$1" 2>/dev/null || echo ""
}

# Raw .hosts array for a Service, or "null" when the call fails.
service_hosts_json() {
    local body
    body=$(api_get "services/$1/devices")
    echo "$body" | jq -c '.hosts // null' 2>/dev/null || echo "null"
}

service_host_count() {
    local hosts
    hosts=$(service_hosts_json "$1")
    if [ "$hosts" = "null" ] || [ -z "$hosts" ]; then
        echo "-1"
    else
        echo "$hosts" | jq 'length' 2>/dev/null || echo "-1"
    fi
}

api_create_service() {
    local name="$1" port="$2"
    curl -s -o /dev/null -w "%{http_code}" -X PUT \
        -H "Authorization: Bearer ${API_TOKEN}" -H "Content-Type: application/json" \
        -d "{\"name\":\"${name}\",\"tags\":[\"tag:ci-test-container\"],\"ports\":[\"tcp:${port}\"]}" \
        "${API_BASE}/tailnet/${API_TAILNET}/services/${name}" 2>/dev/null || echo "000"
}

sweep_flap_services() {
    local token leftovers name
    token=$(mint_api_token)
    [ -z "$token" ] && return 0
    leftovers=$(curl -s -H "Authorization: Bearer ${token}" \
        "${API_BASE}/tailnet/${API_TAILNET}/services" 2>/dev/null \
        | jq -r '.vipServices[]?.name // empty' 2>/dev/null | grep '^svc:e2e-flap-' || true)
    for name in $leftovers; do
        curl -s -o /dev/null -X DELETE -H "Authorization: Bearer ${token}" \
            "${API_BASE}/tailnet/${API_TAILNET}/services/${name}" 2>/dev/null || true
        echo "  Swept $name"
    done
}

# Poll the Control Plane until a Service has at least one host. Echoes the
# elapsed seconds on success, or -1 on timeout.
wait_for_hosts() {
    local svc="$1" budget="$2" elapsed=0 count
    while [ "$elapsed" -lt "$budget" ]; do
        count=$(service_host_count "$svc")
        if [ "$count" -ge 1 ] 2>/dev/null; then
            echo "$elapsed"
            return 0
        fi
        sleep 3
        elapsed=$((elapsed + 3))
    done
    echo "-1"
    return 1
}

# Local serve config presence (what DockTail and the user both see as "healthy").
local_serve_ok() {
    ts serve status --json 2>/dev/null | jq -e ".Services[\"$1\"].TCP | length > 0" >/dev/null 2>&1
}

local_advertised() {
    ts debug prefs 2>/dev/null | jq -r '.AdvertiseServices // [] | join(",")' 2>/dev/null || echo "<unreadable>"
}

diag() {
    local label="$1"
    echo ""
    echo "  ---------- DIAGNOSTICS: $label ----------"
    echo "  [local] serve status --json:"
    ts serve status --json 2>/dev/null | jq -c '.Services | to_entries | map({svc: .key, tcp: (.value.TCP | keys)})' 2>/dev/null | sed 's/^/    /' || echo "    <unparseable>"
    echo "  [local] prefs.AdvertiseServices: $(local_advertised)"
    echo "  [local] serve get-config --all:"
    ts serve get-config --all 2>&1 | sed 's/^/    /'
    echo "  [control] service definitions + hosts:"
    for svc in "$SVC_A" "$SVC_B" "$SVC_MANUAL" "$SVC_PROBE"; do
        echo "    ${svc}: def=$(api_get "services/${svc}" | jq -c '{ports,tags}' 2>/dev/null) hosts=$(service_hosts_json "$svc")"
    done
    echo "  [tailscaled] recent c2n / vip-service log lines:"
    docker logs "$TS_CONTAINER" 2>&1 | grep -iE "vip-service|c2n|advertis" | tail -25 | sed 's/^/    /' || echo "    <none>"
    echo "  ----------------------------------------"
    echo ""
}

# High-resolution sampler of the *local* two-part state, run inside the
# Tailscale container so a sample costs no docker-exec round trip. This is what
# shows the intermediate windows (advertised-with-no-ports, config-without-
# advertisement) that a 5s-resolution poll from outside would never catch.
install_sampler() {
    cat > /tmp/flap-sampler.sh <<'SAMPLER'
#!/bin/sh
# Samples the two independent pieces of local hosting state as fast as the CLI
# allows (~5-7 Hz). No sleep: the windows being hunted are sub-second.
rm -f /tmp/flap.stop
i=0
while [ ! -f /tmp/flap.stop ]; do
    i=$((i + 1))
    {
        printf '%s|%s|cfg=' "$i" "$(date -u +%H:%M:%S)"
        tailscale serve status --json 2>/dev/null | tr -d '\n '
        printf '|adv='
        tailscale debug prefs 2>/dev/null | tr -d '\n ' | grep -o '"AdvertiseServices":\[[^]]*\]' || true
        printf '\n'
    } >> /tmp/flap.samples
done
SAMPLER
    docker cp /tmp/flap-sampler.sh "$TS_CONTAINER":/tmp/flap-sampler.sh >/dev/null 2>&1
    docker exec "$TS_CONTAINER" sh -c ': > /tmp/flap.samples' >/dev/null 2>&1
}

start_sampler() {
    docker exec "$TS_CONTAINER" sh -c "echo '--- ROUND $1 ---' >> /tmp/flap.samples" >/dev/null 2>&1
    docker exec -d "$TS_CONTAINER" sh /tmp/flap-sampler.sh
}

stop_sampler() {
    docker exec "$TS_CONTAINER" touch /tmp/flap.stop >/dev/null 2>&1 || true
    sleep 1
}

# Print the sampled state transitions for one service: only lines where the
# (ports, advertised) pair changed, so the flap sequence is readable.
dump_sampler_transitions() {
    local svc="$1"
    docker exec "$TS_CONTAINER" cat /tmp/flap.samples 2>/dev/null | python3 -c "
import sys, json, re
svc = sys.argv[1]
last = None
print('    seq      time      serve-config-ports   advertised')
for line in sys.stdin:
    line = line.rstrip('\n')
    if line.startswith('--- ROUND'):
        print('   ', line)
        last = None
        continue
    m = re.match(r'^(\d+)\|([\d:]+)\|cfg=(.*)\|adv=(.*)$', line)
    if not m:
        continue
    seq, tstamp, cfg, adv = m.groups()
    try:
        parsed = json.loads(cfg) if cfg else {}
    except Exception:
        continue
    svccfg = (parsed.get('Services') or {}).get(svc)
    if svccfg is None:
        ports = 'ABSENT'
    else:
        ports = ','.join(sorted((svccfg.get('TCP') or {}).keys())) or 'NO-PORTS'
    advertised = 'yes' if svc in (adv or '') else 'no'
    state = (ports, advertised)
    if state != last:
        flag = ''
        if advertised == 'yes' and ports in ('ABSENT', 'NO-PORTS'):
            flag = '   <-- advertised with no ports'
        elif advertised == 'no' and ports not in ('ABSENT',):
            flag = '   <-- config present but NOT advertised'
        print(f'    {seq:<8} {tstamp}  {ports:<20} {advertised}{flag}')
        last = state
" "$svc" 2>/dev/null || echo "    <sampler output unavailable>"
}

# ==============================================================================
# Bring up the stack
# ==============================================================================

log "Building and starting the flap stack"
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
    docker logs "$TS_CONTAINER" 2>&1 | tail -40
    exit 2
fi
echo "  Connected after ${elapsed}s"
sleep 10

NODE_ID=$(ts status --json 2>/dev/null | jq -r '.Self.ID // empty')
echo "  Service host node ID: ${NODE_ID:-<unknown>}"

set +e

# ==============================================================================
# EXP0 - Baseline: how long does a fresh DockTail service take to reach 1 host?
# ==============================================================================

log "EXP0  Baseline: time to first host registration"

baseline_a=$(wait_for_hosts "$SVC_A" "$BASELINE_BUDGET")
if [ "$baseline_a" = "-1" ]; then
    echo "  $SVC_A never reached >=1 host within ${BASELINE_BUDGET}s"
    diag "EXP0 baseline failure"
    # This is itself issue-#72-shaped (a brand new service that never comes
    # online), so record it rather than bailing out.
    repro "EXP0: freshly created $SVC_A never registered a host (${BASELINE_BUDGET}s budget) while local serve config was $( local_serve_ok "$SVC_A" && echo healthy || echo missing )"
else
    ok "$SVC_A reached >=1 host after ${baseline_a}s"
fi

baseline_b=$(wait_for_hosts "$SVC_B" "$BASELINE_BUDGET")
if [ "$baseline_b" = "-1" ]; then
    repro "EXP0: freshly created $SVC_B never registered a host (${BASELINE_BUDGET}s budget)"
else
    ok "$SVC_B reached >=1 host after ${baseline_b}s"
fi

diag "EXP0 steady state"

# ==============================================================================
# EXP1 - Manual flap: drain + clear + immediate re-serve, no DockTail involved
# ==============================================================================
#
# Isolates the mechanism from DockTail entirely: it performs exactly the CLI
# sequence DockTail runs when a container disappears and comes straight back.
# If the Control Plane loses the host here, the flap alone is sufficient to
# cause the reported symptom.

log "EXP1  Manual flap (drain + clear + re-serve), ${FLAP_ITERATIONS} iterations"

step "Creating $SVC_MANUAL definition and advertising it by hand"
api_create_service "$SVC_MANUAL" "80" >/dev/null
ts serve --service="$SVC_MANUAL" --http=80 "http://127.0.0.1:80" >/dev/null 2>&1

manual_initial=$(wait_for_hosts "$SVC_MANUAL" "$BASELINE_BUDGET")
if [ "$manual_initial" = "-1" ]; then
    harness "$SVC_MANUAL never came online at all; EXP1 cannot distinguish flap damage from setup failure"
    diag "EXP1 setup failure"
else
    ok "$SVC_MANUAL online after ${manual_initial}s"

    for i in $(seq 1 "$FLAP_ITERATIONS"); do
        step "Iteration $i: drain -> clear -> re-serve"
        ts serve drain "$SVC_MANUAL" >/dev/null 2>&1
        ts serve clear "$SVC_MANUAL" >/dev/null 2>&1
        ts serve --service="$SVC_MANUAL" --http=80 "http://127.0.0.1:80" >/dev/null 2>&1

        if ! local_serve_ok "$SVC_MANUAL"; then
            harness "iteration $i: local serve config missing right after re-serve"
            continue
        fi

        recovered=$(wait_for_hosts "$SVC_MANUAL" "$FLAP_BUDGET")
        if [ "$recovered" = "-1" ]; then
            repro "EXP1 iteration $i: $SVC_MANUAL has 0 hosts ${FLAP_BUDGET}s after a drain/clear/re-serve cycle, while local serve config and prefs are correct"
            diag "EXP1 iteration $i stuck"
            break
        fi
        ok "iteration $i: recovered after ${recovered}s"
    done
fi

# ==============================================================================
# EXP2 - DockTail flap: container replacement (the actual issue #72 path)
# ==============================================================================

log "EXP2  DockTail flap: container replacement, ${FLAP_ITERATIONS} iterations"

APP_A_NETWORK=$(docker inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}}{{end}}' e2e-flap-a 2>/dev/null | head -1)
echo "  e2e-flap-a network: ${APP_A_NETWORK:-<default>}"

recreate_app_a() {
    docker rm -f e2e-flap-a >/dev/null 2>&1
    docker run -d --name e2e-flap-a --restart no \
        ${APP_A_NETWORK:+--network "$APP_A_NETWORK"} \
        --label "docktail.service.enable=true" \
        --label "docktail.service.name=e2e-flap-a" \
        --label "docktail.service.port=80" \
        nginx:alpine >/dev/null 2>&1
}

install_sampler
for i in $(seq 1 "$FLAP_ITERATIONS"); do
    step "Iteration $i: docker rm -f + immediate recreate of e2e-flap-a"
    before_ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' e2e-flap-a 2>/dev/null)

    # Sample only around the flap itself so the tight polling loop cannot
    # perturb the (much longer) Control Plane wait that follows.
    start_sampler "$i"
    recreate_app_a
    after_ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' e2e-flap-a 2>/dev/null)

    # Give DockTail its event-driven reconcile plus one periodic cycle.
    converged=0
    for _ in $(seq 1 20); do
        if local_serve_ok "$SVC_A"; then converged=1; break; fi
        sleep 1
    done
    sleep 2
    stop_sampler

    echo "    backend IP ${before_ip:-?} -> ${after_ip:-?}"
    if [ "$converged" -ne 1 ]; then
        harness "iteration $i: DockTail never restored the local serve config for $SVC_A"
        continue
    fi
    echo "    local serve config restored"

    recovered=$(wait_for_hosts "$SVC_A" "$FLAP_BUDGET")
    if [ "$recovered" = "-1" ]; then
        repro "EXP2 iteration $i: $SVC_A has 0 hosts ${FLAP_BUDGET}s after container replacement, while its local serve config is healthy (this is issue #72)"
        diag "EXP2 iteration $i stuck"
        break
    fi
    ok "iteration $i: recovered after ${recovered}s"
done

echo ""
echo "  Sampled local state transitions for $SVC_A (one block per iteration):"
dump_sampler_transitions "$SVC_A"

echo ""
echo "  DockTail's view of the replacement (drain/clear/add sequence):"
docker logs "$DOCKTAIL_CONTAINER" 2>&1 | grep -iE "e2e-flap-a" | grep -iE "removing|drain|clear|adding|added|changed" | tail -30 | sed 's/^/    /'

# ==============================================================================
# EXP3 - Revival probe: does touching an unrelated service un-stick a dead one?
# ==============================================================================
#
# Multiple reporters describe exactly this: a service stuck at "0 hosts" comes
# back the moment another service is added. That is the signature of a stale
# Control Plane view that only refreshes when the node's ServicesHash changes
# again. Only meaningful if something is actually stuck.

log "EXP3  Revival probe"

stuck=""
for svc in "$SVC_A" "$SVC_B" "$SVC_MANUAL"; do
    count=$(service_host_count "$svc")
    if [ "$count" -le 0 ] 2>/dev/null; then
        if local_serve_ok "$svc"; then
            stuck="$svc"
            break
        fi
    fi
done

if [ -z "$stuck" ]; then
    echo "  SKIP: nothing is stuck (no service with healthy local config and 0 hosts)"
else
    echo "  Stuck service: $stuck (local config healthy, control plane reports 0 hosts)"
    step "Advertising an unrelated service ($SVC_PROBE) to force a ServicesHash change"
    api_create_service "$SVC_PROBE" "9443" >/dev/null
    ts serve --service="$SVC_PROBE" --https=9443 "http://127.0.0.1:80" >/dev/null 2>&1

    revived=$(wait_for_hosts "$stuck" 60)
    if [ "$revived" = "-1" ]; then
        echo "  $stuck did NOT revive within 60s of the unrelated advertisement"
    else
        repro "EXP3: $stuck came back (${revived}s) purely because an unrelated service was advertised - no change to $stuck itself. This is the reported 'adding another service fixes the previous one' behaviour and confirms a stale Control Plane view."
    fi
    diag "EXP3 after revival probe"
fi

# ==============================================================================
# Verdict
# ==============================================================================

log "VERDICT"
echo ""
if [ "$repro_hits" -gt 0 ]; then
    echo "  ############################################"
    echo "  #  REPRODUCED: $repro_hits finding(s)"
    echo "  ############################################"
    echo -e "$findings"
    echo ""
    [ "$harness_errors" -gt 0 ] && echo "  (plus $harness_errors harness error(s))"
    exit 1
fi

echo "  NOT REPRODUCED - every service kept at least one host through all flaps"
[ "$harness_errors" -gt 0 ] && { echo "  WARNING: $harness_errors harness error(s); the run may not be conclusive"; exit 2; }
exit 0
