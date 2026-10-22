<!--
description: Step-by-step DockTail guides: expose Jellyfin and Home Assistant over Tailscale with Docker labels, and migrate from TSDProxy.
nav: All guides
title: DockTail How-To Guides: Docker Apps on Tailscale
-->

## How-To Guides

Each guide takes one real setup from start to finish. They assume DockTail already runs on the Docker host and the Tailscale side is set up; if not, start with the [Quick Start](../01-quick-start.md) and [Tailscale Admin Setup](../03-tailscale-admin.md).

- [Expose Jellyfin over Tailscale](01-jellyfin-over-tailscale.md): stream your media library to every device on your tailnet over HTTPS, without opening a port.
- [Expose Home Assistant over Tailscale](02-home-assistant-over-tailscale.md): reach Home Assistant and the companion app from anywhere, including the `trusted_proxies` setting it needs.
- [Migrate from TSDProxy to DockTail](03-migrate-from-tsdproxy.md): translate `tsdproxy.*` labels to `docktail.*` labels and switch over one app at a time.

For single-label recipes (databases over TCP, SSH, PROXY protocol, public Funnel), see [Examples](../05-examples.md).
