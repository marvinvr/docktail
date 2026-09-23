# Agent Instructions

## Links

- [Tailscale Services Documentation](https://tailscale.com/kb/1552/tailscale-services)
- [Tailscale Service Configuration Reference](https://tailscale.com/kb/1589/tailscale-services-configuration-file)
- [Tailscale ACL Documentation](https://tailscale.com/kb/1337/policy-syntax)
- [Docker SDK for Go](https://docs.docker.com/engine/api/sdk/)

## Documentation Maintenance

When changing user-facing behavior, labels, environment variables, setup steps, examples, networking behavior, Tailscale permissions, supported protocols, or cleanup/reconciliation behavior, update the documentation in the same change.

The canonical documentation source is `docs/*.md`, and it lives here on purpose:
the documentation should be reviewable next to the AGPL code it describes.

Keep these aligned:

- `docs/*.md` for canonical human-facing documentation.
- `README.md` for the short overview, quick start, common examples, and links only.

`docs/*.md` is the *only* place this prose exists. The published site
(docktail.org) is built from a **separate private repo**, `docktail-website`,
which pins this repository as a git submodule and runs `tools/docsgen` at image
build time to render `docs/index.html`, `docs.md`, `llms.txt`, `llms-full.txt`
and `sitemap.xml`. Those are build outputs — never commit them here, and never
copy documentation prose into that repo.

Because the submodule is pinned by commit, documentation changes merged here do
not reach docktail.org until someone bumps the pin in `docktail-website`. That
is deliberate: publishing is an explicit act, not a side effect of merging.

`tools/docsgen` stays in this repo (the website repo consumes it through the
submodule). To see how your markdown renders, `make docs-generate` writes to the
gitignored `.docs-preview/`; for a full preview with styling and assets, use
`make serve` in the `docktail-website` repo.

When adding a new feature, include the relevant docs page update and at least one concise example if the feature changes configuration.

## Project-Specific Constraints

- Do not run tests, start the software, start the dev server, start Docker Compose, or execute migrations unless explicitly asked by the developer.
- Do not read entire translation files. Make targeted reads and edits only.
- Do not add yourself as a co-author in commits.
- `cloud/proto` is a verbatim copy of the DockTail Cloud wire contract, not code owned here. Never edit it on its own: every change is made identically in the control plane's copy (the source of truth) so the two stay byte-identical, and every wire addition is `omitempty` and backward-compatible. Keep it stdlib-only, and never add exec, deploy, shell, or command message types.
