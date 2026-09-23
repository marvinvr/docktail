## Platform Guides

DockTail has two hard requirements wherever it runs:

- **A Docker Engine API.** DockTail discovers containers and reads their `docktail.*` labels through the Docker socket (or `DOCKER_HOST`). It only reads: it lists and inspects containers and follows container events, plus engine info, stats and log tails when [DockTail Cloud](06-cloud.md#docktail-cloud) is on.
- **A `tailscaled` socket it can write serve config to.** Either the host's daemon on Linux ([Tailscale On Host](02-installation.md#tailscale-on-host)) or a `tailscale/tailscale` container next to it ([Tailscale Sidecar](02-installation.md#tailscale-sidecar)). That daemon's node must carry a tag your policy lets advertise services ([ACL Configuration](03-tailscale-admin.md#acl-configuration)); `service hosts must be tagged nodes` in the logs means it does not.

A third, softer one: a **restart policy**. DockTail exits on purpose when it loses the Tailscale socket and relies on the restart to come back with a fresh mount ([Tailscale Socket Loss](07-reference.md#tailscale-socket-loss)).

The guides below say how each platform meets those, and how well each is known to work. The maintainers test DockTail on plain Linux Docker; everything else is based on user reports in the issue tracker and on the requirements above.

### Podman

Reported working. Podman serves a Docker-compatible API socket, and DockTail uses it like the Docker one: mount it at `/var/run/docker.sock`.

```bash
# Rootful Podman: the socket is /run/podman/podman.sock
sudo systemctl enable --now podman.socket

# Rootless Podman: the socket is /run/user/<uid>/podman/podman.sock
systemctl --user enable --now podman.socket
```

```yaml
services:
  docktail:
    image: ghcr.io/marvinvr/docktail:latest
    restart: unless-stopped
    volumes:
      - /run/podman/podman.sock:/var/run/docker.sock:ro   # or /run/user/1000/podman/podman.sock
      - /var/run/tailscale:/var/run/tailscale
    environment:
      - TAILSCALE_OAUTH_CLIENT_ID=${TAILSCALE_OAUTH_CLIENT_ID}
      - TAILSCALE_OAUTH_CLIENT_SECRET=${TAILSCALE_OAUTH_CLIENT_SECRET}
```

- **One DockTail per `tailscaled`.** One instance watches one engine. If rootful and rootless Podman (or Podman and Docker) run on the same host, a second DockTail against the same `tailscaled` will fight the first over which services the node serves. `IGNORE_SERVICE_NAMES` on each instance keeps them apart; separate machines or VMs, each with its own Tailscale node, avoid the problem.
- **Rootless Podman** has the same two issues as [Rootless Docker](02-installation.md#rootless-docker): the host's `tailscaled` must trust the user (`tailscale set --operator`), and container IPs are often unreachable from the host, so use `docktail.service.direct=false` with a published port or run the Tailscale sidecar on the app's network.
- **SELinux.** On an enforcing host (Fedora, RHEL and derivatives), SELinux normally keeps a container from connecting to a host socket it has mounted; relaxing labelling for the DockTail container (`security_opt: [label=disable]`) is the usual fix. Not tested with DockTail.
- DockTail Cloud reporting with Podman has not been tested.

### Synology

Not tested by the maintainers. Container Manager (DSM 7.2 and later) is Docker, so its socket is at `/var/run/docker.sock` and a Compose file runs as a **Project**.

Use the [Tailscale Sidecar](02-installation.md#tailscale-sidecar) setup, or start from [`docker-compose.sidecar.yaml`](https://github.com/marvinvr/docktail/blob/main/docker-compose.sidecar.yaml). DockTail has not been tried against the Tailscale package from the Package Center; the sidecar gives DockTail a daemon of its own instead of depending on where the package keeps its socket.

The sidecar needs `/dev/net/tun`. If the `tailscale` container logs that it is missing, follow Tailscale's Synology documentation for loading the TUN module at boot.

### Unraid

Reported to reach the host's `tailscaled` at `/var/run/tailscale` with the [Tailscale On Host](02-installation.md#tailscale-on-host) setup; the one problem reported was an untagged node (below). The rest of this section is not tested by the maintainers. DockTail can run as a Compose stack (Compose Manager plugin) or as a container added from the Docker tab:

| Setting | Value |
| --- | --- |
| Repository | `ghcr.io/marvinvr/docktail:latest` |
| Path | `/var/run/docker.sock` → `/var/run/docker.sock`, read-only |
| Path | `/var/run/tailscale` → `/var/run/tailscale` |
| Variables | `TAILSCALE_OAUTH_CLIENT_ID`, `TAILSCALE_OAUTH_CLIENT_SECRET` |

Make sure the container restarts on its own after it exits; if yours does not, add `--restart unless-stopped` under Extra Parameters.

- **Tag the Unraid node.** `Failed to add service ... service hosts must be tagged nodes` means the host is not tagged. Run `tailscale up --advertise-tags=tag:server` in the Unraid terminal and add the tag to your policy ([ACL Configuration](03-tailscale-admin.md#acl-configuration)).
- A `Warning: client version ... != tailscaled server version ...` line only says that the `tailscale` CLI in the image and the plugin's daemon are different versions; on its own it is not an error.
- Containers with their own address on a macvlan or ipvlan network (Unraid's `br0`) are not reachable from the host by default, and in direct mode it is the host's `tailscaled` that connects to them. Keep exposed apps on a bridge network, or enable host access to custom networks in Unraid's Docker settings.

### TrueNAS SCALE

Not tested by the maintainers. TrueNAS SCALE 24.10 (Electric Eel) and later run apps on Docker, so DockTail can be installed as a custom app from YAML. Releases before 24.10 run apps on Kubernetes and are not supported (see [Kubernetes](#kubernetes)).

Paste the [Tailscale Sidecar](02-installation.md#tailscale-sidecar) Compose file as the custom app's YAML, so DockTail and its `tailscaled` are deployed together. The Docker socket is at `/var/run/docker.sock`. Apps you expose must be reachable from the sidecar: it runs with host networking, which reaches container IPs on the host's Docker networks.

### macOS And Windows

Supported with the [Tailscale Sidecar](02-installation.md#tailscale-sidecar) only. The Tailscale app on macOS and Windows does not expose a Unix socket, and Docker Desktop, OrbStack and Colima run containers in a Linux VM that cannot mount host Unix sockets anyway. `dial unix /var/run/tailscale/tailscaled.sock: connect: no such file or directory` is the symptom of trying the host setup.

- The sidecar needs its own auth key (`TAILSCALE_AUTH_KEY`) in addition to DockTail's OAuth credentials, and joins the tailnet as its own device.
- The sidecar uses `network_mode: host`. On Docker Desktop, enable host networking under Settings -> Resources -> Network, or drop that line and attach the sidecar to the same Docker network as the apps you expose.
- With DockTail Cloud, host vitals and disk usage describe the Linux VM the containers run in, not the Mac or PC.

### Docker Swarm

No native Swarm support: DockTail reads **container** labels from the engine it is connected to, so it sees only the tasks running on its own node and ignores service-level `deploy.labels`. The layout below is based on the one users reported working on multi-node clusters in [issue #43](https://github.com/marvinvr/docktail/issues/43):

- Run DockTail as a `global` service, so every node has one, each with its own `tailscaled` — installed on the host, or a `global` sidecar as below. Each node joins the tailnet as a tagged device.
- Put the `docktail.*` labels under the service's top-level `labels:`, which Swarm applies to each task container, not under `deploy.labels`.
- Every node that runs a replica advertises the service, and Tailscale Services routes each client to one of the advertising hosts.
- Allow at most one replica of a labelled service per node (`deploy.placement.max_replicas_per_node: 1`, or `mode: global`). Two replicas on one node claim the same service name and port, which DockTail treats as a conflict and does not resolve ([Service Name Conflicts Between Containers](04-labels.md#service-name-conflicts-between-containers)).
- Avoid `update_config.order: start-first` for the `tailscale` service. It briefly runs two daemons on the same node and state volume; the working reports use the default order.

```yaml
services:
  tailscale:
    image: tailscale/tailscale:latest
    environment:
      - TS_AUTHKEY=${TAILSCALE_AUTH_KEY}   # a reusable key: every node uses it
      - TS_EXTRA_ARGS=--advertise-tags=tag:server
      - TS_STATE_DIR=/var/lib/tailscale
      - TS_SOCKET=/var/run/tailscale/tailscaled.sock
      - TS_USERSPACE=false
    volumes:
      - tailscale-state:/var/lib/tailscale
      - tailscale-socket:/var/run/tailscale
      - /dev/net/tun:/dev/net/tun
    cap_add:
      - NET_ADMIN
      - SYS_MODULE
    deploy:
      mode: global

  docktail:
    image: ghcr.io/marvinvr/docktail:latest
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - tailscale-socket:/var/run/tailscale
    environment:
      - TAILSCALE_OAUTH_CLIENT_ID_FILE=/run/secrets/tailscale_oauth_client_id
      - TAILSCALE_OAUTH_CLIENT_SECRET_FILE=/run/secrets/tailscale_oauth_client_secret
    secrets:
      - tailscale_oauth_client_id
      - tailscale_oauth_client_secret
    deploy:
      mode: global

  whoami:
    image: traefik/whoami:latest
    labels:
      - "docktail.service.enable=true"
      - "docktail.service.name=whoami"
      - "docktail.service.port=80"
    deploy:
      replicas: 2
      placement:
        max_replicas_per_node: 1

volumes:
  tailscale-state:
  tailscale-socket:

secrets:
  tailscale_oauth_client_id:
    external: true
  tailscale_oauth_client_secret:
    external: true
```

Named volumes are local to each node, so each node's `tailscale` task keeps its own state and shares its socket only with the DockTail task on the same node. The sidecar here is not on the host network; it reaches the apps over the stack's overlay network, so deploy them in the same stack or attach them to a shared network.

### Kubernetes

Not supported, and there is no Helm chart. Kubernetes nodes run containerd or CRI-O without a Docker Engine API for DockTail to watch, and workloads are described by pod annotations and Services rather than Docker labels. For Kubernetes, use Tailscale's own Kubernetes operator.
