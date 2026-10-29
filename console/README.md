# Differently console

A small admin app for Differently: sandboxes, snapshots, API keys, providers,
secrets, policies, usage, the audit log and the trust report. It is an Astro
site that talks only to Differently's public API. Every rule and every piece
of state stays in the control plane, so the console can run anywhere, run
several times, or be thrown away and rebuilt.

The server also ships a built-in dashboard at `/dashboard/`. Use the console
when you want the admin UI hosted somewhere else, or want to change it.

## Two ways to run it

| | Static (default) | Server |
|---|---|---|
| Build | `npm run build` gives plain files in `dist/` | `npm run build:server`, then `npm start` |
| Host | Any static host: Cloudflare Pages, GitHub Pages, S3, nginx, or your laptop | Any host with Node 22.13 or later |
| Sign-in | None; you paste a Differently API key | Better Auth: email and password, optionally GitHub |
| Where the API key lives | In the browser tab (`sessionStorage`, or `localStorage` if you tick "Remember") | On the console server; browsers never see it |
| Server-side setting | `cors.allowedOrigins` when the console is on another origin | None; the console server calls Differently |

Both modes build the same pages. Server mode adds two routes: `/api/auth/*`
for Better Auth, and `/api/*`, which forwards `/version` and `/v1/*` to
Differently with the server's key after checking the session.

## Static

```bash
npm ci
npm run build                 # PUBLIC_DIFFERENTLY_URL=https://… presets the server URL
```

Upload `dist/` to any static host. To serve it under a path, build with
`CONSOLE_BASE=/console/`. Open it, enter the Differently URL and a key, and the
console checks both with `GET /v1/me`.

If the console is not on the same origin as Differently, allow its origin on
the server:

```yaml
# Differently config file, or config.cors in the Helm values
cors:
  allowedOrigins: ["https://console.example.com"]
```

Origins must be exact (`scheme://host[:port]`, no wildcard and no path). CORS
covers `/v1/*`, `/version` and `/openapi.yaml`, nothing else.

Any script on the page can read the key, so give the console its own origin,
add no third-party scripts, and set a Content-Security-Policy on the host if
it lets you.

## Server

```bash
npm ci
npm run build:server
CONSOLE_MODE=server \
DIFFERENTLY_URL=https://differently.internal:8080 \
DIFFERENTLY_API_KEY=dly_… \
BETTER_AUTH_SECRET="$(openssl rand -base64 32)" \
BETTER_AUTH_URL=https://console.example.com \
CONSOLE_ALLOWED_EMAILS=you@example.com,teammate@example.com \
npm start
```

It listens on `HOST` and `PORT` (default `localhost:4321`) and also reads a
`.env` file in this folder ([`.env.example`](.env.example) lists everything).

| Variable | Meaning |
|---|---|
| `DIFFERENTLY_URL` | Where the console server reaches Differently. It can be a private address. |
| `DIFFERENTLY_API_KEY` | The key used for everyone who signs in. An operator key shows every page; a project key limits the console to that project, and admin pages say so. |
| `BETTER_AUTH_SECRET`, `BETTER_AUTH_URL` | Better Auth's signing secret and the console's public URL. |
| `CONSOLE_ALLOWED_EMAILS` | Comma-separated emails allowed to create an account. Empty means nobody can sign up. |
| `CONSOLE_DB_PATH` | The SQLite file for users and sessions (default `./console.db`, through Node's built-in `node:sqlite`). |
| `GITHUB_CLIENT_ID`, `GITHUB_CLIENT_SECRET` | Optional GitHub sign-in. The allowlist still applies. |

Keep `console.db` on a persistent volume and keep the secret stable, or every
session ends on restart.

### Changing the sign-in

All of it is in [`src/server/auth.ts`](src/server/auth.ts); the proxy only
asks Better Auth for a session. Change that file to fit your installation:
add a social or SSO provider, require two-factor authentication with a Better
Auth plugin, move users to Postgres with a Better Auth database adapter, or
replace the email allowlist with your own rule. See the
[Better Auth documentation](https://www.better-auth.com/docs) for the options.

## Reaching a Differently you do not expose

- **From your laptop.** Run `npm run dev` and set `DIFFERENTLY_URL` to a
  forwarded port (`ssh -L 8080:differently.internal:8080 jump-host`, or a
  Tailscale or WireGuard address). The dev server forwards `/v1` and
  `/version` to it, so no CORS setting is needed and nothing is deployed.
- **Next to Differently.** Run server mode on the same private network and
  publish only the console, behind a tunnel such as `cloudflared` or your
  usual reverse proxy. Differently itself stays private.
- **Static, anywhere.** Host `dist/` publicly and allow its origin. Each
  browser still needs network access to Differently, for example over a VPN.

## Developing

```bash
npm ci
npm run dev           # static mode on :4321, forwarding /v1 to DIFFERENTLY_URL (default http://localhost:8080)
npm run dev:server    # server mode, reading the variables above from .env
npm run typecheck
```

With `differently-server --dev` running, connect with the token in
`~/.differently/dev/token`.

- `src/pages/` has one file per page; each loads its data in the browser
  through the [TypeScript SDK](../sdk/typescript/README.md), imported from
  source.
- `src/lib/nav.ts` is the sidebar, `src/lib/ui.ts` the table, dialog and
  toast helpers, and `src/styles/console.css` the light and dark tokens.
- `src/lib/providers.ts` lists the provider kinds and their main fields; keep
  it in step with `internal/dashboard/static/app.js` and
  [docs/providers/](../docs/providers/README.md).
