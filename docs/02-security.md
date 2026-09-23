## Hardening

DockTail needs two sockets, and both are powerful. This section says what they grant and how to narrow it. None of it is required, and none of it is covered by DockTail's automated tests; what DockTail needs is taken from its source.

### The Docker Socket

Every example mounts `/var/run/docker.sock:ro`. The `:ro` flag makes the socket file read-only, which does not matter for a socket: it does not stop anyone from connecting to it, and a connection gets the full Docker API — start, exec, create privileged containers. Treat access to the Docker socket as root on the host.

DockTail itself only reads from it: it lists and inspects containers and follows container events, and with [DockTail Cloud](06-cloud.md#docktail-cloud) on it also reads engine info and version, one-shot container stats, and log tails. It never creates, starts, stops or execs anything. To make that a guarantee rather than a property of the code, put a read-only proxy in front of the socket.

### Read-Only Socket Proxy

[`tecnativa/docker-socket-proxy`](https://github.com/Tecnativa/docker-socket-proxy) allows only the API sections you enable and, with `POST=0` (its default), only `GET` and `HEAD` requests. DockTail needs `CONTAINERS` on top of the sections the proxy allows by default (`EVENTS`, `PING`, `VERSION`), and `INFO` as well when DockTail Cloud is on:

```yaml
services:
  docker-socket-proxy:
    image: tecnativa/docker-socket-proxy:latest
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    environment:
      - CONTAINERS=1 # list, inspect, stats, logs
      - INFO=1       # DockTail Cloud only: engine ID and host specs
      - EVENTS=1     # default; container start/stop/die/oom/health events
      - PING=1       # default; API version negotiation
      - VERSION=1    # default
      - POST=0       # default; read-only
    networks:
      - docker-api

  docktail:
    image: ghcr.io/marvinvr/docktail:latest
    restart: unless-stopped
    depends_on:
      - docker-socket-proxy
    volumes:
      - /var/run/tailscale:/var/run/tailscale
    environment:
      - DOCKER_HOST=tcp://docker-socket-proxy:2375
      - TAILSCALE_OAUTH_CLIENT_ID=${TAILSCALE_OAUTH_CLIENT_ID}
      - TAILSCALE_OAUTH_CLIENT_SECRET=${TAILSCALE_OAUTH_CLIENT_SECRET}
    networks:
      - docker-api

networks:
  docker-api:
    internal: true
```

- Keep the proxy on an `internal` network that only DockTail joins, and never publish port 2375: anyone who can reach it can read what DockTail reads.
- Read-only is not the same as harmless. `CONTAINERS=1` allows every `GET` under `/containers`: inspecting any container (including environment variables that may hold other apps' secrets), reading its logs, and downloading files from it through the archive and export endpoints.
- If the proxy closes the event stream, DockTail logs `Docker event stream error` and reconnects after five seconds; the periodic reconcile covers anything that changed in between.

### Container Privileges

The image runs DockTail as root. It needs no Linux capabilities for its own work — it talks to Unix sockets (or the proxy above), reads files, and runs the `tailscale` CLI against `tailscaled`'s socket — so you can drop them all and block privilege escalation:

```yaml
services:
  docktail:
    image: ghcr.io/marvinvr/docktail:latest
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
```

Dropping `CAP_DAC_OVERRIDE` also takes away root's ability to read files it does not own. Credential files loaded through `*_FILE` or `FILE__*` must then be owned by root or readable by everyone inside the container; Docker Swarm secrets are, by default. A `0600` file owned by your own user, bind-mounted from the host, is not, and DockTail exits at startup when it cannot read it.

Running DockTail as a non-root `user:` instead is possible under the same conditions as [Rootless Docker](02-installation.md#rootless-docker): that user needs access to the Docker socket (or the proxy), and with Tailscale on the host it must be the Tailscale operator.
