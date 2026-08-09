#!/usr/bin/env bash
#
# Repro harness for https://github.com/marvinvr/docktail/issues/72
# ("Service offline after container auto update").
#
# The reported symptom is that a Tailscale Service goes Offline / "0 hosts" in
# the admin console while the local serve config is perfectly healthy, and that
# it recovers only on a DockTail restart, when an unrelated service is touched,
# or after several hours. Local `tailscale serve status` therefore proves
# nothing.
#
# Neither does the Control Plane API, as run 2 of this harness established:
# GET /services/<svc>/devices still reported hosts=1, ready=1 a full 90s after
# the service had been drained, so it is a configuration/approval registry
# rather than a liveness signal, and GET /services/<svc> carries no status field
# at all. The only trustworthy ground truth is whether a *different* device on
# the tailnet can actually reach the Service VIP, so this harness runs a second
# Tailscale node as an observer and probes the VIP from there.
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
# EXPM  measurement validation: drain a service and require the observer to lose
#       reachability. Without this a green run proves nothing.
# EXP0  baseline reachability of an HTTP and an HTTPS service
# EXP1  manual flap: drain+clear+re-serve by hand, no DockTail involved
# EXP2  DockTail flap: container replacement (the actual issue #72 path)
# EXP2B burst: several replacements back to back with no settling in between
# EXP2C slow replacement: a long gap between teardown and re-add
# EXP3  revival probe: does touching an unrelated service un-stick a dead one
#
# Exits 1 when the bug is provoked (TDD: red while the bug exists, green after
# the fix), and 2 on harness/infra failure or an unvalidated measurement.

COMPOSE_FILE="docker-compose.e2e-flap.yaml"
E2E_SECRETS_DIR=".e2e-secrets"
TS_CONTAINER="e2e-flap-tailscale"
CLIENT_CONTAINER="e2e-flap-client"
DOCKTAIL_CONTAINER="e2e-flap-docktail"
API_BASE="https://api.tailscale.com/api/v2"
API_TAILNET="${TS_TAILNET:--}"

MAX_WAIT=120
BASELINE_BUDGET=150
FLAP_BUDGET=90
DRAIN_OBSERVE=90
FLAP_ITERATIONS="${FLAP_ITERATIONS:-3}"
BURST_SIZE="${BURST_SIZE:-5}"
SLOW_GAP="${SLOW_GAP:-20}"
SCRIPT_TIMEOUT="${SCRIPT_TIMEOUT:-1700}"

SVC_A="svc:e2e-flap-a"             # http/80,  DockTail-managed, primary probe
SVC_B="svc:e2e-flap-b"             # https/443, DockTail-managed
SVC_MANUAL="svc:e2e-flap-manual"   # hand-driven, EXPM + EXP1
SVC_PROBE="svc:e2e-flap-probe"     # hand-driven, hash poke for EXP3

VIP_A=""; VIP_B=""; VIP_MANUAL=""

repro_hits=0
harness_errors=0
findings=""
metric_valid="unknown"

( sleep "$SCRIPT_TIMEOUT" && echo "ERROR: harness timed out after ${SCRIPT_TIMEOUT}s" && kill $$ ) 2>/dev/null &
TIMEOUT_PID=$!

log()     { echo ""; echo "=== $1"; }
step()    { echo "  -- $1"; }
ok()      { echo "  OK: $1"; }
note()    { echo "  ..  $1"; }
repro()   { echo "  REPRO: $1"; repro_hits=$((repro_hits + 1)); findings="${findings}\n  - $1"; }
harness() { echo "  HARNESS-ERROR: $1"; harness_errors=$((harness_errors + 1)); }

cleanup() {
    log "Cleanup"
    kill "$TIMEOUT_PID" 2>/dev/null || true
    docker rm -f e2e-flap-a e2e-flap-b >/dev/null 2>&1 || true
    docker compose -f "$COMPOSE_FILE" down -v --remove-orphans 2>/dev/null || true
    command -v sweep_flap_services >/dev/null 2>&1 && sweep_flap_services
    rm -rf "$E2E_SECRETS_DIR"
}
trap cleanup EXIT

set -euo pipefail

# ==============================================================================
# Preflight
# ==============================================================================

if [ -z "${TS_OAUTH_CLIENT_ID:-}" ] || [ -z "${TS_OAUTH_CLIENT_SECRET:-}" ]; then
    echo "ERROR: TS_OAUTH_CLIENT_ID + TS_OAUTH_CLIENT_SECRET are required"
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

mint_authkey() {
    curl -s -X POST "${API_BASE}/tailnet/${API_TAILNET}/keys" \
        -H "Authorization: Bearer ${API_TOKEN}" \
        -H "Content-Type: application/json" \
        -d '{"capabilities":{"devices":{"create":{"reusable":false,"ephemeral":true,"tags":["tag:ci-test"]}}},"expirySeconds":3600}' \
        | jq -r '.key // empty'
}

TS_AUTHKEY=$(mint_authkey)
TS_AUTHKEY_CLIENT=$(mint_authkey)
if [ -z "$TS_AUTHKEY" ] || [ -z "$TS_AUTHKEY_CLIENT" ]; then
    echo "ERROR: failed to generate auth keys"
    exit 2
fi
export TS_AUTHKEY TS_AUTHKEY_CLIENT
echo "  Ephemeral auth keys generated (service host + observer)"

mkdir -p "$E2E_SECRETS_DIR"
chmod 700 "$E2E_SECRETS_DIR"
printf '%s\n' "${TS_OAUTH_CLIENT_ID}" > "$E2E_SECRETS_DIR/tailscale_oauth_client_id"
printf '%s\n' "${TS_OAUTH_CLIENT_SECRET}" > "$E2E_SECRETS_DIR/tailscale_oauth_client_secret"
chmod 600 "$E2E_SECRETS_DIR"/*

# ==============================================================================
# Helpers
# ==============================================================================

ts()     { docker exec "$TS_CONTAINER" tailscale "$@"; }
client() { docker exec "$CLIENT_CONTAINER" "$@"; }

api_get() {
    curl -s -H "Authorization: Bearer ${API_TOKEN}" "${API_BASE}/tailnet/${API_TAILNET}/$1" 2>/dev/null || echo ""
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

# The Service's IPv4 VIP, straight from its definition.
service_vip() {
    api_get "services/$1" | jq -r '(.addrs // []) | map(select(test("^100\\."))) | .[0] // empty' 2>/dev/null || echo ""
}

resolve_vips() {
    [ -z "$VIP_A" ] && VIP_A=$(service_vip "$SVC_A")
    [ -z "$VIP_B" ] && VIP_B=$(service_vip "$SVC_B")
    [ -z "$VIP_MANUAL" ] && VIP_MANUAL=$(service_vip "$SVC_MANUAL")
    return 0
}

# GROUND TRUTH: can another tailnet device actually use the Service right now?
# HTTP services get a real request/response; the HTTPS one gets a TCP connect,
# because busybox wget in the Tailscale image has no usable TLS.
probe_service() {
    local svc="$1" vip port kind
    case "$svc" in
        "$SVC_A")      vip="$VIP_A";      port=80;  kind=http ;;
        "$SVC_MANUAL") vip="$VIP_MANUAL"; port=80;  kind=http ;;
        "$SVC_B")      vip="$VIP_B";      port=443; kind=tcp  ;;
        *) return 1 ;;
    esac
    [ -z "$vip" ] && return 1
    if [ "$kind" = "http" ]; then
        client wget -q -T 5 -O /dev/null "http://${vip}:${port}/" >/dev/null 2>&1
    else
        client timeout 5 nc -z "$vip" "$port" >/dev/null 2>&1
    fi
}

# Poll until the observer can use the Service. Echoes elapsed seconds, or -1.
wait_for_reachable() {
    local svc="$1" budget="$2" elapsed=0
    while [ "$elapsed" -lt "$budget" ]; do
        if probe_service "$svc"; then
            echo "$elapsed"
            return 0
        fi
        sleep 3
        elapsed=$((elapsed + 3))
    done
    echo "-1"
    return 1
}

# Poll until the observer can NO LONGER use the Service. Echoes elapsed, or -1.
wait_for_unreachable() {
    local svc="$1" budget="$2" elapsed=0
    while [ "$elapsed" -lt "$budget" ]; do
        if ! probe_service "$svc"; then
            echo "$elapsed"
            return 0
        fi
        sleep 3
        elapsed=$((elapsed + 3))
    done
    echo "-1"
    return 1
}

local_serve_ok() {
    ts serve status --json 2>/dev/null | jq -e ".Services[\"$1\"].TCP | length > 0" >/dev/null 2>&1
}

local_advertised_list() {
    ts debug prefs 2>/dev/null | jq -r '.AdvertiseServices // [] | join(",")' 2>/dev/null || echo "<unreadable>"
}

local_is_advertised() {
    ts debug prefs 2>/dev/null | jq -e --arg s "$1" '(.AdvertiseServices // []) | index($s) != null' >/dev/null 2>&1
}

diag() {
    local label="$1"
    echo ""
    echo "  ---------- DIAGNOSTICS: $label ----------"
    echo "  [local] serve status --json:"
    ts serve status --json 2>/dev/null | jq -c '.Services | to_entries | map({svc: .key, tcp: (.value.TCP | keys)})' 2>/dev/null | sed 's/^/    /' || echo "    <unparseable>"
    echo "  [local] prefs.AdvertiseServices: $(local_advertised_list)"
    echo "  [local] serve get-config --all:"
    ts serve get-config --all 2>&1 | sed 's/^/    /'
    echo "  [observer] reachability:"
    for svc in "$SVC_A" "$SVC_B" "$SVC_MANUAL"; do
        echo "    ${svc}: $( probe_service "$svc" && echo REACHABLE || echo unreachable )"
    done
    echo "  [control] service objects and hosts:"
    for svc in "$SVC_A" "$SVC_B" "$SVC_MANUAL" "$SVC_PROBE"; do
        echo "    ${svc}"
        echo "      def   = $(api_get "services/${svc}")"
        echo "      hosts = $(api_get "services/${svc}/devices")"
    done
    echo "  [tailscaled/host] recent c2n / advertise lines:"
    docker logs "$TS_CONTAINER" 2>&1 | grep -iE "vip-service|c2n|advertis|cert" | tail -25 | sed 's/^/    /' || echo "    <none>"
    echo "  ----------------------------------------"
    echo ""
}

install_sampler() {
    cat > /tmp/flap-sampler.sh <<'SAMPLER'
#!/bin/sh
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
        elif advertised == 'no' and ports != 'ABSENT':
            flag = '   <-- config present but NOT advertised'
        print(f'    {seq:<8} {tstamp}  {ports:<20} {advertised}{flag}')
        last = state
" "$svc" 2>/dev/null || echo "    <sampler output unavailable>"
}

# ==============================================================================
# Bring up the stack
# ==============================================================================

log "Building and starting the flap stack (service host + observer)"
docker compose -f "$COMPOSE_FILE" up -d --build

wait_backend_running() {
    local cname="$1" elapsed=0
    while [ $elapsed -lt $MAX_WAIT ]; do
        if docker exec "$cname" tailscale status --json 2>/dev/null | jq -e '.BackendState == "Running"' >/dev/null 2>&1; then
            echo "$elapsed"; return 0
        fi
        sleep 2
        elapsed=$((elapsed + 2))
    done
    echo "-1"; return 1
}

log "Waiting for both Tailscale nodes to connect"
host_t=$(wait_backend_running "$TS_CONTAINER")
client_t=$(wait_backend_running "$CLIENT_CONTAINER")
if [ "$host_t" = "-1" ] || [ "$client_t" = "-1" ]; then
    echo "ERROR: a Tailscale node did not connect (host=${host_t}s client=${client_t}s)"
    docker logs "$TS_CONTAINER" 2>&1 | tail -25
    docker logs "$CLIENT_CONTAINER" 2>&1 | tail -25
    exit 2
fi
echo "  service host connected after ${host_t}s, observer after ${client_t}s"
sleep 10

echo "  service host node: $(ts status --json 2>/dev/null | jq -r '.Self.ID // "?"')"
echo "  observer node:     $(client tailscale status --json 2>/dev/null | jq -r '.Self.ID // "?"')"

set +e

# ==============================================================================
# EXP0 - Baseline reachability
# ==============================================================================

log "EXP0  Baseline: can the observer reach the services at all?"

resolve_vips
echo "  VIPs: A=${VIP_A:-<none>} B=${VIP_B:-<none>}"
if [ -z "$VIP_A" ]; then
    harness "could not resolve a VIP for $SVC_A; the service definition may not exist yet"
fi

t_a=$(wait_for_reachable "$SVC_A" "$BASELINE_BUDGET")
if [ "$t_a" = "-1" ]; then
    harness "observer cannot reach $SVC_A at all (${BASELINE_BUDGET}s). If the local serve config is healthy this is most likely a tailnet ACL that does not grant tag:ci-test access to the service, in which case reachability cannot be used as ground truth."
    diag "EXP0 baseline unreachable"
else
    ok "$SVC_A reachable from the observer after ${t_a}s"
fi

t_b=$(wait_for_reachable "$SVC_B" 60)
[ "$t_b" = "-1" ] && note "$SVC_B (https/443) not TCP-reachable within 60s; treating it as a secondary signal only" \
                  || ok "$SVC_B TCP-reachable after ${t_b}s"

diag "EXP0 steady state"

# ==============================================================================
# EXPM - Measurement validation
# ==============================================================================

log "EXPM  Measurement validation: does a drained service become unreachable?"

step "Creating $SVC_MANUAL and advertising it by hand"
api_create_service "$SVC_MANUAL" "80" >/dev/null
ts serve --service="$SVC_MANUAL" --http=80 "http://127.0.0.1:80" >/dev/null 2>&1
sleep 3
VIP_MANUAL=$(service_vip "$SVC_MANUAL")
echo "  $SVC_MANUAL VIP: ${VIP_MANUAL:-<none>}"

manual_initial=$(wait_for_reachable "$SVC_MANUAL" "$BASELINE_BUDGET")
if [ "$manual_initial" = "-1" ]; then
    harness "$SVC_MANUAL never became reachable; measurement validation inconclusive"
    diag "EXPM setup failure"
else
    ok "$SVC_MANUAL reachable after ${manual_initial}s"

    step "Draining $SVC_MANUAL; the observer must lose it"
    ts serve drain "$SVC_MANUAL" >/dev/null 2>&1
    sleep 2
    local_is_advertised "$SVC_MANUAL" && harness "drain did not remove $SVC_MANUAL from prefs.AdvertiseServices"

    gone=$(wait_for_unreachable "$SVC_MANUAL" "$DRAIN_OBSERVE")
    if [ "$gone" = "-1" ]; then
        metric_valid="no"
        harness "METRIC INVALID: $SVC_MANUAL is still reachable ${DRAIN_OBSERVE}s after being drained, so reachability cannot distinguish an online service from an offline one. Every other result this run is inconclusive."
        diag "EXPM drained but still reachable"
    else
        metric_valid="yes"
        ok "METRIC VALID: drained service became unreachable after ${gone}s"

        step "Re-advertising $SVC_MANUAL"
        ts serve advertise "$SVC_MANUAL" >/dev/null 2>&1
        back=$(wait_for_reachable "$SVC_MANUAL" 90)
        [ "$back" = "-1" ] && harness "$SVC_MANUAL did not come back after 'serve advertise'" \
                           || ok "$SVC_MANUAL back after ${back}s"
    fi
fi

# ==============================================================================
# EXP1 - Manual flap, no DockTail involved
# ==============================================================================

log "EXP1  Manual flap (drain + clear + re-serve), ${FLAP_ITERATIONS} iterations"

for i in $(seq 1 "$FLAP_ITERATIONS"); do
    step "Iteration $i: drain -> clear -> re-serve"
    ts serve drain "$SVC_MANUAL" >/dev/null 2>&1
    ts serve clear "$SVC_MANUAL" >/dev/null 2>&1
    ts serve --service="$SVC_MANUAL" --http=80 "http://127.0.0.1:80" >/dev/null 2>&1

    if ! local_serve_ok "$SVC_MANUAL"; then
        harness "iteration $i: local serve config missing right after re-serve"
        continue
    fi

    recovered=$(wait_for_reachable "$SVC_MANUAL" "$FLAP_BUDGET")
    if [ "$recovered" = "-1" ]; then
        repro "EXP1 iteration $i: $SVC_MANUAL unreachable from another tailnet device ${FLAP_BUDGET}s after a drain/clear/re-serve cycle, while its local serve config and prefs are correct"
        diag "EXP1 iteration $i stuck"
        break
    fi
    ok "iteration $i: recovered after ${recovered}s"
done

# ==============================================================================
# EXP2 - DockTail flap: container replacement (issue #72 path)
# ==============================================================================

log "EXP2  DockTail flap: container replacement, ${FLAP_ITERATIONS} iterations"

APP_NETWORK=$(docker inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}}{{end}}' e2e-flap-a 2>/dev/null | head -1)
echo "  app network: ${APP_NETWORK:-<default>}"

recreate_app_a() {
    docker rm -f e2e-flap-a >/dev/null 2>&1
    docker run -d --name e2e-flap-a --restart no \
        ${APP_NETWORK:+--network "$APP_NETWORK"} \
        --label "docktail.service.enable=true" \
        --label "docktail.service.name=e2e-flap-a" \
        --label "docktail.service.port=80" \
        nginx:alpine >/dev/null 2>&1
}

recreate_app_b() {
    docker rm -f e2e-flap-b >/dev/null 2>&1
    docker run -d --name e2e-flap-b --restart no \
        ${APP_NETWORK:+--network "$APP_NETWORK"} \
        --label "docktail.service.enable=true" \
        --label "docktail.service.name=e2e-flap-b" \
        --label "docktail.service.port=80" \
        --label "docktail.service.service-port=443" \
        --label "docktail.service.service-protocol=https" \
        nginx:alpine >/dev/null 2>&1
}

wait_local_restored() {
    local svc="$1" n
    for n in $(seq 1 25); do
        if local_serve_ok "$svc"; then return 0; fi
        sleep 1
    done
    return 1
}

install_sampler
for i in $(seq 1 "$FLAP_ITERATIONS"); do
    step "Iteration $i: docker rm -f + immediate recreate of both app containers"
    before_ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' e2e-flap-a 2>/dev/null)

    start_sampler "exp2-$i"
    recreate_app_a
    recreate_app_b
    after_ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' e2e-flap-a 2>/dev/null)
    wait_local_restored "$SVC_A"; converged=$?
    sleep 2
    stop_sampler

    echo "    backend IP ${before_ip:-?} -> ${after_ip:-?}"
    if [ "$converged" -ne 0 ]; then
        harness "iteration $i: DockTail never restored the local serve config for $SVC_A"
        continue
    fi
    echo "    local serve config restored"

    recovered=$(wait_for_reachable "$SVC_A" "$FLAP_BUDGET")
    if [ "$recovered" = "-1" ]; then
        repro "EXP2 iteration $i: $SVC_A unreachable from another tailnet device ${FLAP_BUDGET}s after container replacement, while its local serve config is healthy (this is issue #72)"
        diag "EXP2 iteration $i stuck"
        break
    fi
    ok "iteration $i: recovered after ${recovered}s"
done

# ==============================================================================
# EXP2B - Burst
# ==============================================================================

log "EXP2B  Burst: ${BURST_SIZE} back-to-back replacements"

start_sampler "exp2b"
for i in $(seq 1 "$BURST_SIZE"); do
    recreate_app_a
    recreate_app_b
    sleep 2
done
wait_local_restored "$SVC_A"; converged=$?
sleep 3
stop_sampler

if [ "$converged" -ne 0 ]; then
    harness "burst: DockTail never restored the local serve config for $SVC_A"
else
    recovered=$(wait_for_reachable "$SVC_A" "$FLAP_BUDGET")
    if [ "$recovered" = "-1" ]; then
        repro "EXP2B: $SVC_A unreachable ${FLAP_BUDGET}s after ${BURST_SIZE} back-to-back replacements, while its local serve config is healthy"
        diag "EXP2B stuck"
    else
        ok "burst: recovered after ${recovered}s"
    fi
fi

# ==============================================================================
# EXP2C - Slow replacement
# ==============================================================================

log "EXP2C  Slow replacement: ${SLOW_GAP}s gap between teardown and re-add"

start_sampler "exp2c"
docker rm -f e2e-flap-a >/dev/null 2>&1
note "container removed, waiting ${SLOW_GAP}s before recreating"
sleep "$SLOW_GAP"
note "observer view while the container is gone: $( probe_service "$SVC_A" && echo REACHABLE || echo unreachable )"
recreate_app_a
wait_local_restored "$SVC_A"; converged=$?
sleep 2
stop_sampler

if [ "$converged" -ne 0 ]; then
    harness "slow replacement: DockTail never restored the local serve config for $SVC_A"
else
    recovered=$(wait_for_reachable "$SVC_A" "$FLAP_BUDGET")
    if [ "$recovered" = "-1" ]; then
        repro "EXP2C: $SVC_A unreachable ${FLAP_BUDGET}s after a slow replacement, while its local serve config is healthy"
        diag "EXP2C stuck"
    else
        ok "slow replacement: recovered after ${recovered}s"
    fi
fi

echo ""
echo "  Sampled local state transitions for $SVC_A (one block per round):"
dump_sampler_transitions "$SVC_A"

echo ""
echo "  DockTail's view of the replacements:"
docker logs "$DOCKTAIL_CONTAINER" 2>&1 | grep -iE "e2e-flap-a" | grep -iE "removing|drain|clear|adding|added|changed|fail" | tail -40 | sed 's/^/    /'

# ==============================================================================
# EXP3 - Revival probe
# ==============================================================================

log "EXP3  Revival probe"

stuck=""
for svc in "$SVC_A" "$SVC_MANUAL" "$SVC_B"; do
    if ! probe_service "$svc" && local_serve_ok "$svc"; then
        stuck="$svc"
        break
    fi
done

if [ -z "$stuck" ]; then
    echo "  SKIP: nothing is stuck (no service with healthy local config that the observer cannot reach)"
else
    echo "  Stuck service: $stuck (local config healthy, observer cannot reach it)"
    step "Advertising an unrelated service ($SVC_PROBE) to force a ServicesHash change"
    api_create_service "$SVC_PROBE" "9443" >/dev/null
    ts serve --service="$SVC_PROBE" --https=9443 "http://127.0.0.1:80" >/dev/null 2>&1

    revived=$(wait_for_reachable "$stuck" 90)
    if [ "$revived" = "-1" ]; then
        echo "  $stuck did NOT revive within 90s of the unrelated advertisement"
    else
        repro "EXP3: $stuck came back (${revived}s) purely because an unrelated service was advertised - nothing about $stuck itself changed. This is the reported 'adding another service fixes the previous one' behaviour."
    fi
    diag "EXP3 after revival probe"
fi

# ==============================================================================
# Verdict
# ==============================================================================

log "VERDICT"
echo ""
echo "  measurement validity (does a drained service become unreachable?): $metric_valid"
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

if [ "$metric_valid" != "yes" ]; then
    echo "  INCONCLUSIVE - the ground-truth signal could not be validated, so"
    echo "  'no findings' does not mean the bug is absent."
    exit 2
fi

echo "  NOT REPRODUCED - the observer kept reaching every service through every flap"
[ "$harness_errors" -gt 0 ] && { echo "  WARNING: $harness_errors harness error(s); the run may not be conclusive"; exit 2; }
exit 0
