# Release Tagging

Pushing a git tag triggers the Docker build workflow. The tag format determines whether it's a stable release or a preview/prerelease.

## Stable Release

Standard semver tag — publishes `latest`, major, major.minor, and full version tags.

```bash
git tag 1.2.3
git push origin 1.2.3
```

Docker tags produced: `1.2.3`, `1.2`, `1`, `latest`

## Preview / Prerelease

Add a hyphen followed by a channel name (e.g. `alpha`, `beta`, `rc`) and an optional revision number. This publishes the full version tag plus a channel tag, but does **not** update `latest`.

```bash
git tag 1.3.0-alpha.1
git push origin 1.3.0-alpha.1
```

Docker tags produced: `1.3.0-alpha.1`, `alpha`

```bash
git tag 1.3.0-beta.2
git push origin 1.3.0-beta.2
```

Docker tags produced: `1.3.0-beta.2`, `beta`

The channel tag (e.g. `alpha`) always points to the most recently pushed prerelease in that channel.

## After a stable release: publish the docs

The site at docktail.org renders `docs/*.md` from a submodule pinned to a
specific commit of this repository, so a stable tag does not reach the site on
its own. Whenever you push a stable tag, bump that pin to the same commit:

```bash
# in the docktail-website repo
make update-agent
git commit -m "Bump docktail docs pin to <tag>"
git push
```

Skipping this leaves `docktail.org/docs` describing an older agent than
`ghcr.io/marvinvr/docktail:latest`. Anchors that log messages and `docs/*.md`
link to — `https://docktail.org/docs/#tailscale-socket-loss`, for one — only
exist on the site once the pin covers the commit that introduced them.
