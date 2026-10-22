<!--
description: DockTail exposes Docker containers as native Tailscale Services from Docker labels. Docs for setup, labels, Funnel, examples and DockTail Cloud.
title: DockTail Docs: Docker Containers as Tailscale Services
-->

# DockTail Documentation

DockTail exposes Docker containers as Tailscale Services using label-based configuration. It watches Docker events, reads `docktail.*` labels, and advertises matching containers through the local Tailscale daemon.

## Why DockTail?

DockTail uses native Tailscale Services, not per-container Tailscale devices.

<div class="comparison-table-wrap">
  <table class="comparison-table">
    <colgroup>
      <col>
      <col>
      <col>
      <col>
      <col>
      <col>
    </colgroup>
    <thead>
      <tr>
        <th scope="col" aria-label="Feature"></th>
        <th scope="col">DockTail</th>
        <th scope="col">TSDProxy</th>
        <th scope="col">ScaleTail</th>
        <th scope="col">tsbridge</th>
        <th scope="col">Plain Services</th>
      </tr>
    </thead>
    <tbody>
      <tr>
        <th scope="row">Native Tailscale Services</th>
        <td aria-label="Yes"><span class="status-mark">✅</span></td>
        <td aria-label="No"><span class="status-mark">❌</span></td>
        <td aria-label="No"><span class="status-mark">❌</span></td>
        <td aria-label="No"><span class="status-mark">❌</span></td>
        <td aria-label="Yes"><span class="status-mark">✅</span></td>
      </tr>
      <tr>
        <th scope="row">Configured via Docker labels</th>
        <td aria-label="Yes"><span class="status-mark">✅</span></td>
        <td aria-label="Yes"><span class="status-mark">✅</span></td>
        <td aria-label="No"><span class="status-mark">❌</span></td>
        <td aria-label="Yes"><span class="status-mark">✅</span></td>
        <td aria-label="No"><span class="status-mark">❌</span></td>
      </tr>
      <tr>
        <th scope="row">
          Apps do not consume separate Tailscale device slots
          <span class="info-tip" tabindex="0" aria-label="DockTail advertises apps as services from one tagged host instead of creating a separate Tailscale device identity per app. Exact plan limits depend on Tailscale.">i<span role="tooltip">DockTail advertises apps as services from one tagged host instead of creating a separate Tailscale device identity per app. Exact plan limits depend on Tailscale.</span></span>
        </th>
        <td aria-label="Yes"><span class="status-mark">✅</span></td>
        <td aria-label="No"><span class="status-mark">❌</span></td>
        <td aria-label="No"><span class="status-mark">❌</span></td>
        <td aria-label="No"><span class="status-mark">❌</span></td>
        <td aria-label="Yes"><span class="status-mark">✅</span></td>
      </tr>
      <tr>
        <th scope="row">No app port publishing</th>
        <td aria-label="Yes"><span class="status-mark">✅</span></td>
        <td aria-label="Depends on proxy or network setup"><span class="status-mark">⚠️</span><span class="info-tip" tabindex="0" aria-label="Depends on proxy and Docker network setup.">i<span role="tooltip">Depends on proxy and Docker network setup.</span></span></td>
        <td aria-label="Depends on sidecar setup"><span class="status-mark">⚠️</span><span class="info-tip" tabindex="0" aria-label="Depends on the sidecar template and app network setup.">i<span role="tooltip">Depends on the sidecar template and app network setup.</span></span></td>
        <td aria-label="Depends on proxy or network setup"><span class="status-mark">⚠️</span><span class="info-tip" tabindex="0" aria-label="Depends on proxy and Docker network setup.">i<span role="tooltip">Depends on proxy and Docker network setup.</span></span></td>
        <td aria-label="Manual backend setup required"><span class="status-mark">⚠️</span><span class="info-tip" tabindex="0" aria-label="You configure how the service host reaches the backend yourself.">i<span role="tooltip">You configure how the service host reaches the backend yourself.</span></span></td>
      </tr>
      <tr>
        <th scope="row">Automatic Docker reconciliation</th>
        <td aria-label="Yes"><span class="status-mark">✅</span></td>
        <td aria-label="Yes"><span class="status-mark">✅</span></td>
        <td aria-label="No"><span class="status-mark">❌</span></td>
        <td aria-label="Yes"><span class="status-mark">✅</span></td>
        <td aria-label="No"><span class="status-mark">❌</span></td>
      </tr>
      <tr>
        <th scope="row">Low manual setup after install</th>
        <td aria-label="Yes"><span class="status-mark">✅</span></td>
        <td aria-label="Yes"><span class="status-mark">✅</span></td>
        <td aria-label="Template setup per app"><span class="status-mark">⚠️</span><span class="info-tip" tabindex="0" aria-label="ScaleTail is template-based, so each app usually starts from its own Compose recipe.">i<span role="tooltip">ScaleTail is template-based, so each app usually starts from its own Compose recipe.</span></span></td>
        <td aria-label="Yes"><span class="status-mark">✅</span></td>
        <td aria-label="No"><span class="status-mark">❌</span></td>
      </tr>
    </tbody>
  </table>
</div>

## What DockTail Does

- Discovers labeled Docker containers automatically.
- Proxies directly to container IPs by default, so app containers do not need published Docker ports.
- Advertises HTTP, HTTPS, TCP, and TLS-terminated TCP services through Tailscale.
- Supports Tailscale HTTPS with automatic certificates.
- Supports Tailscale Funnel for public internet access.
- Supports multiple Tailscale services from one container.
- Reconciles state when containers restart and container IPs change.
- Runs as a stateless Docker container.
- Optionally reports to [DockTail Cloud](06-cloud.md), a paid hosted service, for multi-host monitoring and alerting. Opt-in via one environment variable; inert when unset.

## Recommended Reading Order

1. Start with [Quick Start](01-quick-start.md) for a minimal Compose setup.
2. Read [Installation](02-installation.md) for host Tailscale and sidecar options, [Platform Guides](02-platforms.md) for Podman, NAS systems, macOS, Windows, Swarm and Kubernetes, and [Hardening](02-security.md) to narrow what DockTail can reach.
3. Configure Tailscale permissions in [Tailscale Admin Setup](03-tailscale-admin.md).
4. Use [Labels](04-labels.md) and [Examples](05-examples.md) when exposing real services.
5. See [DockTail Cloud](06-cloud.md) if you want monitoring and alerting across your hosts.
6. Check [Reference](07-reference.md) for all labels, environment variables, protocols, and behavior notes, and [How It Works](08-how-it-works.md) for the reconciliation loop.
7. Follow a [how-to guide](guides/README.md) for a complete setup of a specific app, such as Jellyfin or Home Assistant, or for moving over from TSDProxy.
