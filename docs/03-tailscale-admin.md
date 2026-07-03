## Tailscale Admin Setup

DockTail can advertise services locally without Tailscale API credentials, but OAuth or API key credentials allow it to create service definitions automatically in the Tailscale Admin Console.

### OAuth Credentials

OAuth is recommended. It enables automatic service creation and avoids expiring API keys.

1. Open Tailscale Admin Console -> Settings -> OAuth clients.
2. Create an OAuth client scoped to your server tag, for example `tag:server`.
3. Grant these permissions:
   - General -> Services: Write
   - Devices -> Core: Write
   - Keys -> Auth Keys: Write, only when using the sidecar method

   These same permissions also cover the optional `DELETE_UNUSED_SERVICES` cleanup, which lists Services, inspects their advertising hosts, and deletes the ones no host advertises. No extra scope is required.
4. Add the credentials to DockTail:

```yaml
environment:
  - TAILSCALE_OAUTH_CLIENT_ID=your-client-id
  - TAILSCALE_OAUTH_CLIENT_SECRET=your-client-secret
```

Or mount the credentials as files:

```yaml
services:
  docktail:
    volumes:
      - ./secrets:/run/secrets/docktail:ro
    environment:
      - FILE__TAILSCALE_OAUTH_CLIENT_ID=/run/secrets/docktail/tailscale_oauth_client_id
      - TAILSCALE_OAUTH_CLIENT_SECRET_FILE=/run/secrets/docktail/tailscale_oauth_client_secret
```

If OAuth and API key credentials are both configured, DockTail uses OAuth.

### API Key

An API key also enables automatic service creation, but Tailscale API keys expire.

```yaml
environment:
  - TAILSCALE_API_KEY=tskey-api-...
```

API keys can also be loaded from `FILE__TAILSCALE_API_KEY` or `TAILSCALE_API_KEY_FILE`.

### Manual Mode

DockTail can run without credentials. It advertises services locally through the Tailscale CLI, but you must manually create service definitions in the Tailscale Admin Console and configure ACL auto-approvers.

### ACL Configuration

Services require tag definitions in `tagOwners` and an `autoApprovers.services` rule that allows the host to advertise container services.

```json
{
  "tagOwners": {
    "tag:server": ["autogroup:admin"],
    "tag:container": ["tag:server"]
  },
  "autoApprovers": {
    "services": {
      "tag:container": ["tag:server"]
    }
  }
}
```

`tag:server` is assigned to the host machine or sidecar auth key that runs DockTail. `tag:container` is the default tag DockTail assigns to services it creates.

If you manage ACLs through GitOps, both tags must exist in `tagOwners`; otherwise Tailscale rejects references to undefined tags.

### Approve Services

The first time a new service is advertised, it may need approval in the Tailscale Admin Console Services tab. After approval, the service continues to work across container restarts. OAuth or API key credentials can create service definitions automatically, but first approval may still be required depending on your ACL policy.
