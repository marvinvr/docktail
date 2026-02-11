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
