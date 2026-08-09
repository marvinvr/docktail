#!/usr/bin/env bash
#
# Repro harness for https://github.com/marvinvr/docktail/issues/72
# ("Service offline after container auto update").
#
# The reported symptom is that a Tailscale Service goes Offline / "0 hosts" in
# the admin console while the local serve config is perfectly healthy, and that
# it recovers only on a DockTail restart, when an unrelated service is touched,
# or after several hours. Local `tailscale serve status` therefore proves
# nothing; the ground truth has to come from the Control Plane:
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
# EXPM  measurement validation: does /devices actually drop a drained host?
#       Without this every other result is meaningless, because a registry that
#       never forgets a host cannot show the reported symptom.
# EXP0  baseline host registration for an HTTP and an HTTPS service
# EXP1  manual flap: drain+clear+re-serve by hand, no DockTail involved
# EXP2  DockTail flap: container replacement (the actual issue #72 path), run
#       against the HTTPS/443 service because reporters singled those out
# EXP2B burst: several replacements back to back with no settling in between
# EXP2C slow replacement: a long gap between teardown and re-add
# EXP3  revival probe: does touching an unrelated service un-stick a dead one
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
BASELINE_BUDGET=150    # first-ever registration can include approval + certs
FLAP_BUDGET=90         # a healthy re-advertise is expected well inside this
DRAIN_OBSERVE=90       # how long to watch a deliberately drained service
FLAP_ITERATIONS="${FLAP_ITERATIONS:-3}"
BURST_SIZE="${BURST_SIZE:-5}"
SLOW_GAP="${SLOW_GAP:-20}"
SCRIPT_TIMEOUT="${SCRIPT_TIMEOUT:-1700}"

SVC_A="svc:e2e-flap-a"             # DockTail-managed, http/80,  unrelated service
SVC_B="svc:e2e-flap-b"             # DockTail-managed, https/443, main flap target
SVC_MANUAL="svc:e2e-flap-manual"   # hand-driven, used by EXPM and EXP1
SVC_PROBE="svc:e2e-flap-probe"     # hand-driven, hash poke for EXP3

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

KEY_RESPONSE=$(curl -s -X POST "${API_BASE}/tailnet/${API_TAILNET}/keys" \
    -H "Authorization: Bearer ${API_TOKEN}" \
    -H "Content-Type: application/json" \
    -d '{"capabilities":{"devices":{"create":{"reusable":false,"ephemeral":true,"tags":["tag:ci-test"]}}},"expirySeconds":3600}')
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

ts() { docker exec "$TS_CONTAINER" tailscale "$@"; }

api_get() {
    curl -s -H "Authorization: Bearer ${API_TOKEN}" "${API_BASE}/tailnet/${API_TAILNET}/$1" 2>/dev/null || echo ""
}

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

# Hosts the Control Plane considers actually usable for the Service. A host that
# is listed but no longer "ready" is just as offline to a user as one that is
# missing, so both count as unhealthy.
service_ready_count() {
    local hosts
    hosts=$(service_hosts_json "$1")
    if [ "$hosts" = "null" ] || [ -z "$hosts" ]; then
        echo "-1"
    else
        echo "$hosts" | jq '[.[] | select(.configured == "ready")] | length' 2>/dev/null || echo "-1"
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

# Poll until a Service has at least one READY host. Echoes elapsed seconds, or
# -1 on timeout.
wait_for_ready_host() {
    local svc="$1" budget="$2" elapsed=0 count
    while [ "$elapsed" -lt "$budget" ]; do
        count=$(service_ready_count "$svc")
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
    echo "  [control] full service objects and hosts:"
    for svc in "$SVC_A" "$SVC_B" "$SVC_MANUAL" "$SVC_PROBE"; do
        echo "    ${svc}"
        echo "      def   = $(api_get "services/${svc}")"
        echo "      hosts = $(api_get "services/${svc}/devices")"
    done
    echo "  [tailscaled] recent c2n / advertise log lines:"
    docker logs "$TS_CONTAINER" 2>&1 | grep -iE "vip-service|c2n|advertis|cert" | tail -30 | sed 's/^/    /' || echo "    <none>"
    echo "  ----------------------------------------"
    echo ""
}

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

# Watch the Control Plane's view of a Service and print every distinct
# observation, so a transient dip is visible rather than averaged away.
observe_hosts() {
    local svc="$1" duration="$2" elapsed=0 last="" cur
    while [ "$elapsed" -lt "$duration" ]; do
        cur=$(service_hosts_json "$svc")
        if [ "$cur" != "$last" ]; then
            echo "    t+${elapsed}s: $cur"
            last="$cur"
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done
    echo "$last"
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
# EXP0 - Baseline
# ==============================================================================

log "EXP0  Baseline: time to first ready host"

for svc in "$SVC_A" "$SVC_B"; do
    t=$(wait_for_ready_host "$svc" "$BASELINE_BUDGET")
    if [ "$t" = "-1" ]; then
        repro "EXP0: freshly created $svc never reached a ready host within ${BASELINE_BUDGET}s (local serve config $( local_serve_ok "$svc" && echo healthy || echo missing ))"
        diag "EXP0 $svc never registered"
    else
        ok "$svc reached a ready host after ${t}s"
    fi
done

diag "EXP0 steady state"

# ==============================================================================
# EXPM - Measurement validation
# ==============================================================================
#
# Everything downstream assumes /devices reflects whether the node is CURRENTLY
# advertising. If it is really a configuration/approval registry that keeps a
# host listed after it stops advertising, then it cannot show the reported
# symptom and the whole harness is measuring the wrong thing. Find out by
# draining a service and leaving it drained.

log "EXPM  Measurement validation: does /devices drop a drained host?"

step "Creating $SVC_MANUAL and advertising it by hand"
api_create_service "$SVC_MANUAL" "80" >/dev/null
ts serve --service="$SVC_MANUAL" --http=80 "http://127.0.0.1:80" >/dev/null 2>&1

manual_initial=$(wait_for_ready_host "$SVC_MANUAL" "$BASELINE_BUDGET")
if [ "$manual_initial" = "-1" ]; then
    harness "$SVC_MANUAL never came online; measurement validation is inconclusive"
    diag "EXPM setup failure"
else
    ok "$SVC_MANUAL ready after ${manual_initial}s"

    step "Draining $SVC_MANUAL and leaving it drained for ${DRAIN_OBSERVE}s"
    ts serve drain "$SVC_MANUAL" >/dev/null 2>&1
    sleep 2
    if local_is_advertised "$SVC_MANUAL"; then
        harness "drain did not remove $SVC_MANUAL from prefs.AdvertiseServices"
    else
        note "local state after drain: advertised=no, serve config $( local_serve_ok "$SVC_MANUAL" && echo present || echo absent )"
    fi

    echo "    Control Plane observations while drained:"
    observe_hosts "$SVC_MANUAL" "$DRAIN_OBSERVE" >/dev/null
    drained_hosts=$(service_host_count "$SVC_MANUAL")
    drained_ready=$(service_ready_count "$SVC_MANUAL")
    note "after ${DRAIN_OBSERVE}s drained: hosts=${drained_hosts} ready=${drained_ready}"

    if [ "$drained_ready" -le 0 ] 2>/dev/null; then
        metric_valid="yes"
        ok "METRIC VALID: a drained host stops counting as ready, so /devices can detect the reported symptom"
    else
        metric_valid="no"
        harness "METRIC INVALID: $SVC_MANUAL still reports ${drained_ready} ready host(s) ${DRAIN_OBSERVE}s after being drained. /devices does not track live advertisement, so 'hosts >= 1' cannot prove a service is online and every other result in this run is inconclusive."
    fi

    step "Re-advertising $SVC_MANUAL"
    ts serve advertise "$SVC_MANUAL" >/dev/null 2>&1
    back=$(wait_for_ready_host "$SVC_MANUAL" 60)
    [ "$back" = "-1" ] && harness "$SVC_MANUAL did not come back after 'serve advertise'" || ok "$SVC_MANUAL back after ${back}s"
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

    recovered=$(wait_for_ready_host "$SVC_MANUAL" "$FLAP_BUDGET")
    if [ "$recovered" = "-1" ]; then
        repro "EXP1 iteration $i: $SVC_MANUAL has no ready host ${FLAP_BUDGET}s after a drain/clear/re-serve cycle, while local serve config and prefs are correct"
        diag "EXP1 iteration $i stuck"
        break
    fi
    ok "iteration $i: recovered after ${recovered}s"
done

# ==============================================================================
# EXP2 - DockTail flap: container replacement (issue #72 path), HTTPS/443
# ==============================================================================

log "EXP2  DockTail flap: container replacement of the HTTPS service, ${FLAP_ITERATIONS} iterations"

APP_B_NETWORK=$(docker inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}}{{end}}' e2e-flap-b 2>/dev/null | head -1)
echo "  e2e-flap-b network: ${APP_B_NETWORK:-<default>}"

recreate_app_b() {
    docker rm -f e2e-flap-b >/dev/null 2>&1
    docker run -d --name e2e-flap-b --restart no \
        ${APP_B_NETWORK:+--network "$APP_B_NETWORK"} \
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
    step "Iteration $i: docker rm -f + immediate recreate of e2e-flap-b"
    before_ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' e2e-flap-b 2>/dev/null)

    start_sampler "exp2-$i"
    recreate_app_b
    after_ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' e2e-flap-b 2>/dev/null)
    wait_local_restored "$SVC_B"; converged=$?
    sleep 2
    stop_sampler

    echo "    backend IP ${before_ip:-?} -> ${after_ip:-?}"
    if [ "$converged" -ne 0 ]; then
        harness "iteration $i: DockTail never restored the local serve config for $SVC_B"
        continue
    fi
    echo "    local serve config restored"

    recovered=$(wait_for_ready_host "$SVC_B" "$FLAP_BUDGET")
    if [ "$recovered" = "-1" ]; then
        repro "EXP2 iteration $i: $SVC_B has no ready host ${FLAP_BUDGET}s after container replacement, while its local serve config is healthy (this is issue #72)"
        diag "EXP2 iteration $i stuck"
        break
    fi
    ok "iteration $i: recovered after ${recovered}s"
done

# ==============================================================================
# EXP2B - Burst: several replacements with no settling in between
# ==============================================================================
#
# Maximises the number of ServicesHash transitions in flight at once, which is
# the state where a stale Control Plane view is most likely to survive.

log "EXP2B  Burst: ${BURST_SIZE} back-to-back replacements"

start_sampler "exp2b"
for i in $(seq 1 "$BURST_SIZE"); do
    recreate_app_b
    sleep 2
done
wait_local_restored "$SVC_B"; converged=$?
sleep 3
stop_sampler

if [ "$converged" -ne 0 ]; then
    harness "burst: DockTail never restored the local serve config for $SVC_B"
else
    recovered=$(wait_for_ready_host "$SVC_B" "$FLAP_BUDGET")
    if [ "$recovered" = "-1" ]; then
        repro "EXP2B: $SVC_B has no ready host ${FLAP_BUDGET}s after ${BURST_SIZE} back-to-back replacements, while its local serve config is healthy"
        diag "EXP2B stuck"
    else
        ok "burst: recovered after ${recovered}s"
    fi
fi

# ==============================================================================
# EXP2C - Slow replacement: long gap between teardown and re-add
# ==============================================================================
#
# A real image pull can leave the container gone for tens of seconds, which is
# long enough for control to fully process the un-advertise before the re-add
# arrives. Different race window from EXP2.

log "EXP2C  Slow replacement: ${SLOW_GAP}s gap between teardown and re-add"

start_sampler "exp2c"
docker rm -f e2e-flap-b >/dev/null 2>&1
note "container removed, waiting ${SLOW_GAP}s before recreating"
sleep "$SLOW_GAP"
mid_hosts=$(service_hosts_json "$SVC_B")
note "control plane view while the container is gone: $mid_hosts"
recreate_app_b
wait_local_restored "$SVC_B"; converged=$?
sleep 2
stop_sampler

if [ "$converged" -ne 0 ]; then
    harness "slow replacement: DockTail never restored the local serve config for $SVC_B"
else
    recovered=$(wait_for_ready_host "$SVC_B" "$FLAP_BUDGET")
    if [ "$recovered" = "-1" ]; then
        repro "EXP2C: $SVC_B has no ready host ${FLAP_BUDGET}s after a slow replacement, while its local serve config is healthy"
        diag "EXP2C stuck"
    else
        ok "slow replacement: recovered after ${recovered}s"
    fi
fi

echo ""
echo "  Sampled local state transitions for $SVC_B (one block per round):"
dump_sampler_transitions "$SVC_B"

echo ""
echo "  DockTail's view of the replacements:"
docker logs "$DOCKTAIL_CONTAINER" 2>&1 | grep -iE "e2e-flap-b" | grep -iE "removing|drain|clear|adding|added|changed|fail" | tail -40 | sed 's/^/    /'

# ==============================================================================
# EXP3 - Revival probe
# ==============================================================================

log "EXP3  Revival probe"

stuck=""
for svc in "$SVC_B" "$SVC_A" "$SVC_MANUAL"; do
    count=$(service_ready_count "$svc")
    if [ "$count" -le 0 ] 2>/dev/null && local_serve_ok "$svc"; then
        stuck="$svc"
        break
    fi
done

if [ -z "$stuck" ]; then
    echo "  SKIP: nothing is stuck (no service with healthy local config and no ready host)"
else
    echo "  Stuck service: $stuck (local config healthy, control plane reports no ready host)"
    step "Advertising an unrelated service ($SVC_PROBE) to force a ServicesHash change"
    api_create_service "$SVC_PROBE" "9443" >/dev/null
    ts serve --service="$SVC_PROBE" --https=9443 "http://127.0.0.1:80" >/dev/null 2>&1

    revived=$(wait_for_ready_host "$stuck" 60)
    if [ "$revived" = "-1" ]; then
        echo "  $stuck did NOT revive within 60s of the unrelated advertisement"
    else
        repro "EXP3: $stuck came back (${revived}s) purely because an unrelated service was advertised - nothing about $stuck itself changed. This is the reported 'adding another service fixes the previous one' behaviour and confirms a stale Control Plane view."
    fi
    diag "EXP3 after revival probe"
fi

# ==============================================================================
# Verdict
# ==============================================================================

log "VERDICT"
echo ""
echo "  measurement validity (does /devices drop a drained host?): $metric_valid"
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
    echo "  INCONCLUSIVE - the Control Plane signal could not be validated, so"
    echo "  'no findings' does not mean the bug is absent."
    exit 2
fi

echo "  NOT REPRODUCED - every service kept a ready host through every flap"
[ "$harness_errors" -gt 0 ] && { echo "  WARNING: $harness_errors harness error(s); the run may not be conclusive"; exit 2; }
exit 0
