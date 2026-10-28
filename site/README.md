# Differently website

The landing page and documentation published at
<https://different-ai.github.io/differently/>, built with
[Astro](https://astro.build) and [Starlight](https://starlight.astro.build).

Docs are single-sourced: `scripts/sync-docs.mjs` generates
`src/content/docs/docs/` (git-ignored) from the repository on every build:

| Source | Page |
|---|---|
| `docs/**/*.md` | `/docs/**` (a `README.md` becomes the section index) |
| `sdk/typescript/README.md`, `sdk/python/README.md` | `/docs/sdks/*` |
| `api/openapi.yaml` | `/docs/reference/api/` |
| `cmd/differently` help output (needs Go) | `/docs/reference/cli/` |
| `site/pages-src/**` | Hand-written pages such as the Go SDK |

Relative links are rewritten to site routes, or to GitHub for files that have
no page. Edit the source files, not the generated ones.

```sh
cd site
npm ci
npm run dev     # sync + dev server at http://localhost:4321/differently/
npm run build   # sync + static build into dist/, fails on broken internal links
```

`.github/workflows/site.yml` builds on pull requests and deploys to GitHub
Pages on pushes to `pre-release`.
