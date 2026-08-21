## DockTail Cloud

DockTail Cloud is optional, opt-in monitoring for DockTail-managed services across one or more hosts. It is a hosted dashboard that watches what DockTail exposes and tells you *why* something is unreachable.

[Explore DockTail Cloud](https://docktail.org/cloud/) or [open the dashboard](https://cloud.docktail.org/login).

### What You Get

- **One view of every host and service.** The full catalog of DockTail-managed services, plus a read-only inventory of the host's other containers, with health history.
- **The useful distinction.** A plain uptime check stops at "down." Cloud already has the Docker and Tailscale context, so it separates a *container* problem (exit code, Docker OOM event, restart loop) from an *exposure* problem (the container answers locally, but the Tailscale control plane says the service is awaiting approval, not advertised by this host, or gone) from a *host* problem (the box stopped sending heartbeats).
- **Incidents with the evidence attached.** Docker failure events and a bounded log tail captured at the moment of failure, so you start from the reason instead of from a graph.
- **Alerts and recoveries.** Notification when a service breaks and when it comes back — including hosts that drop off entirely, since detection runs in Cloud rather than on the box.

Reporting rides along with the normal agent — there is no separate binary. The same DockTail container gains cloud reporting when a workspace key is present, and stays completely inert without one.

### How To Enable

1. Create a workspace in the DockTail Cloud dashboard and copy the workspace key (`dtc_...`).
2. Set `DOCKTAIL_CLOUD_KEY` on the DockTail agent.

That is the only configuration — the cloud endpoint is built into the agent.

```yaml
services:
  docktail:
    image: ghcr.io/marvinvr/docktail:latest
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - /var/run/tailscale:/var/run/tailscale
    environment:
      - TAILSCALE_OAUTH_CLIENT_ID=${TAILSCALE_OAUTH_CLIENT_ID}
      - TAILSCALE_OAUTH_CLIENT_SECRET=${TAILSCALE_OAUTH_CLIENT_SECRET}
      # Optional. Enables DockTail Cloud reporting.
      - DOCKTAIL_CLOUD_KEY=${DOCKTAIL_CLOUD_KEY}
```

With no key set, no connection is opened and DockTail runs exactly as before.

If Cloud marks a host as unmonitored (for example, the host sits past the
workspace's host cap), the agent keeps the connection open with heartbeats and
occasional catalog snapshots and pauses checks, Docker events, metrics, tailnet
state, tailnet-health answers, and log excerpts. Cloud pushes an updated configuration over the same
connection when that changes; the agent does not need a restart.

### Environment Variables

| Variable | Default | Description |
| --- | --- | --- |
| `DOCKTAIL_CLOUD_KEY` | - | Workspace key (`dtc_...`) from the cloud dashboard. Enables reporting. Inert when unset. |
| `DOCKTAIL_LOG_LEVEL` | `info` | Log level for the cloud module: `debug`, `info`, `warn`, or `error`. |
| `DOCKTAIL_CHECK_INTERVAL` | `30s` | How often local-vantage checks run (5s–5m). |

For local development, `DOCKTAIL_CLOUD_URL` overrides the built-in endpoint.
Plaintext `ws://` is accepted only for loopback endpoints unless
`DOCKTAIL_CLOUD_ALLOW_INSECURE=true` is also set. Do not use that escape hatch
in production; non-loopback production endpoints must use `wss://`.

### What It Sends

The hosted control plane (the dashboard and ingest service behind `wss://ingest.docktail.org`) is a separate, proprietary product. The reporting module in this repository sends operational metadata and bounded incident log tails to it over an outbound-only WebSocket Secure (WSS) connection.

When enabled, the agent reports the following operational data:

- Periodic snapshots of DockTail-managed services, including stopped containers, plus refreshes after successful reconciles.
- A read-only inventory of the host's **other** containers — the ones *not* published with `docktail.*` labels, including stopped ones — with name, image, state/health, ports, and live CPU/memory. These containers are not actively probed; they are listed on the dashboard so you can see the host's whole Docker footprint, and can be explicitly watched for Docker-event-driven incidents and alerts.
- Docker failure events, including container exit codes, out-of-memory (OOM) kills, health-status changes, and restart loops.
- Local-vantage check results. Checks default to TCP; cloud-managed config may select HTTP, a relative path, and expected status, but the destination always comes from the agent's local service discovery.
- Tailscale control-plane service state, when Cloud asks for it (see [Tailnet Health](#tailnet-health)).
- Bounded incident log excerpts when the workspace opts in. Before sending, the agent best-effort redacts common Authorization/Bearer credentials, passwords, tokens, API keys, credential URLs, JWTs, and private-key blocks, then applies the 40-line/8-KiB caps. Redaction cannot recognize every application-specific secret.

### What It Never Does

- No remote command execution, deployment, or shell access. The protocol is metadata-only and has no exec, deploy, or shell message types.
- It never executes anything on the host on behalf of the cloud.
- Cloud configuration cannot select an arbitrary network destination; checks are bound to services discovered locally from Docker. A tailnet-health request carries only service names the agent itself published, and carries no credentials.
- Tailscale credentials stay on the host. Cloud never receives an OAuth client, an API key, or a Tailscale token — only the metadata the agent reads with them.
- The connection is outbound only; the agent dials the ingest endpoint and nothing dials in.

### Tailnet Health

Cloud's tailnet vantage answers one question: does the Tailscale *control plane*
agree that this host is advertising the service and that the service is approved?
Answering it needs Tailscale API credentials, so it only works when
`TAILSCALE_OAUTH_CLIENT_ID`/`TAILSCALE_OAUTH_CLIENT_SECRET` or
`TAILSCALE_API_KEY` is already configured (see
[Tailscale Admin Setup](03-tailscale-admin.md#tailscale-admin-setup)). No new
credential, and none of them ever leaves the host: Cloud asks a question, the
agent reads the Tailscale API and answers with metadata.

It reads the control plane rather than the host's own `tailscale serve` config
because serve state only says "this host believes it is serving." It cannot see
that the service is waiting for approval in the Admin Console, that its
definition was deleted, or that the advertisement never reached the control
plane — cases where the host looks perfectly healthy and nobody can reach the
service.

This is an approval and advertisement oracle, not a reachability ping. Tailscale
exposes no health or liveness probe for a service, so a service Cloud reports as
`connected` is one the control plane considers advertised and approved — nothing
more. Actual reachability still comes from the local vantage and, for Funnel
services, the public one.

Cloud drives the polling: it asks exactly one credentialed host per tailnet on a
slow cadence (service state changes when a human approves something), and that
answer covers every host on the tailnet. Agents never poll on their own, and each
agent enforces its own minimum interval of 60 seconds between control-plane
reads, re-serving its previous answer if asked sooner — your Tailscale API quota
is yours, and nothing on the Cloud side can spend more of it than that.

Without credentials the agent still says so explicitly: its hello advertises that
it understands tailnet health but has no credentials for it, so Cloud reports "no
Tailscale credentials" and links to [Tailscale Admin
Setup](03-tailscale-admin.md#tailscale-admin-setup) rather than showing an outage —
or telling you to upgrade an agent that is already current. Local checks, Docker
events, metrics, and incidents are unaffected.

### Identity

Each host is identified by its Docker engine ID, used as a stable fingerprint.
A workspace key can enroll multiple hosts while its enrollment window is open
(one hour by default). After the window closes, the key continues to authenticate
the hosts it already enrolled but cannot add another fingerprint until an
operator reopens enrollment in the Cloud dashboard. An agent waiting for a
reopened window retries automatically at a low rate.

The agent also reports its own Tailscale node ID and tailnet name when it has a
tailnet. The node ID lets Cloud tell a dead agent from a dead host and match this
host against a service's advertising devices; the tailnet name groups the hosts
that share a control plane, so Cloud knows which single host to ask for tailnet
health. Neither is required — without them the agent simply reports fewer
signals.
