# Vercel Sandbox

| | |
|---|---|
| Status | **Tested with a fake server**: implemented from Vercel's REST API reference and OpenAPI document, not run against a Vercel account |
| Adapter | `vercel` (`internal/adapters/vercel`) |
| Trust | external (provider-managed; forced by the server) |
| Isolation | Firecracker microVM per sandbox, operated by Vercel |
| API checked | [REST API: Sandboxes](https://vercel.com/docs/rest-api/sandboxes) and [openapi.json](https://vercel.com/openapi.json); [concepts](https://vercel.com/docs/sandbox/concepts), [persistent sandboxes](https://vercel.com/docs/sandbox/concepts/persistent-sandboxes), [snapshots](https://vercel.com/docs/sandbox/concepts/snapshots), [pricing and limits](https://vercel.com/docs/sandbox/pricing). Read 2026-10-28 |

A Vercel sandbox is a named, persistent resource in a Vercel project. Each
time it runs, it runs as a *session*: commands, files, stop and snapshots
address the current session, and a stopped sandbox resumes as a new session
from the snapshot Vercel takes when it stops. The adapter names each sandbox
after its Differently sandbox ID and tags it with `dly-sandbox` and
`dly-installation`.

## Capabilities

| Feature | Supported | Notes |
|---|---|---|
| `exec` | yes | `POST /v2/sandboxes/sessions/{id}/cmd` with `wait` and `logs`; separate stdout and stderr; `args` run directly, `command` runs through `sh -c`; timeout sent in milliseconds |
| `exec.stream` | yes | The same NDJSON response, emitted as it arrives. If the connection drops, the rest of the output comes from the command's `logs` endpoint without repeating what was already sent |
| `files` | yes | Write: a one-file `.tar.gz` extracted into the file's directory (created first). Read: `fs/read`. List: there is no endpoint, so it runs GNU `find` in the sandbox |
| stdin | yes | `attachStdin`, then base64 writes of at most 512 KiB each, with offsets so a resend is safe |
| `lifecycle.stop-start` | yes | Stop ends the session; Vercel snapshots the filesystem first. Start resumes from it (`GET …?resume=true`) |
| `lifecycle.pause-resume` | no | Memory is not kept across a stop |
| `snapshots.disk` | yes | `POST /v3/sandboxes/sessions/{id}/snapshot` with no expiry. Taking it ends the session, so the adapter resumes the sandbox afterwards |
| `snapshots.memory` | no | Vercel documents filesystem snapshots only |
| `fork` | yes | A new sandbox with `source: {type: snapshot}` |
| `ports.expose` | when `config.ports` is set | Only ports declared at creation are routed; the URL is public (`https://<subdomain>.vercel.run`) |
| max size | 8 vCPU / 16 GiB reported | Pro plan limits. Hobby allows 4 vCPU / 8 GiB, Enterprise 32 vCPU / 64 GiB. Memory is fixed at 2 GiB per vCPU; vCPUs are 1 or an even number, so requests are rounded up |
| provider TTL backstop | yes | The session `timeout`, the smaller of the sandbox TTL and `maxTimeoutSeconds`. When it passes, Vercel stops the session (the files are kept); it does not delete the sandbox. A longer TTL is followed with `extend-timeout`, up to `maxTimeoutSeconds` |

## Configure

Store a Vercel access token (one that can reach the project's team), then
register the pool:

```sh
differently admin secret put vercel-token --from-file ./vercel-token.txt
differently admin pool add vercel --adapter vercel --trust external \
  --config project=prj_123 --config team=team_456 --config region=iad1 \
  --config maxTimeoutSeconds=86400 --config ports=3000 \
  --region iad1 --max-concurrency 20 --secret vercel-token
```

Or in the server config file:

```yaml
pools:
  - name: vercel
    adapter: vercel
    trust: external
    region: iad1
    maxConcurrency: 20
    config:
      project: prj_123          # required: project ID or name
      team: team_456            # the team that owns the project
      region: iad1              # default: Vercel's default (iad1)
      image: ""                 # default: Vercel's default image; a request's template overrides it
      architecture: amd64       # amd64 (default) or arm64
      ports: "3000,8080"        # optional; at most 15, 1024-65535
      maxTimeoutSeconds: "2700" # default 2700 (45 min, the Hobby maximum); 86400 on Pro and Enterprise
      apiURL: https://api.vercel.com
    credentials: {secret: vercel-token}
```

The Helm chart's `config.pools` accepts `credentials: {env: VERCEL_TOKEN}`
with the variable supplied through `server.extraEnv` from a Kubernetes
Secret; stored `secret:` credentials are registered with the CLI or API as
above. Pool `config` values must be strings.

## Limits and differences

- A session cannot outlive the plan's maximum (45 minutes on Hobby, 24 hours
  on Pro and Enterprise). With the default `maxTimeoutSeconds`, a sandbox
  stops after 45 minutes and shows as `stopped`; start it again to continue.
  Raise `maxTimeoutSeconds` on paid plans.
- Snapshots are tied to the region they were made in. A pool restoring a
  snapshot must use the same `region`.
- Delete removes the sandbox but keeps its snapshots, so a snapshot can
  outlive its source; the snapshots Vercel takes on every stop expire on
  Vercel's schedule.
- Disk size is not configurable; `diskGiB` is not sent (quota and cost still
  use the requested size).
- Commands are capped at 5 hours each; stdin writes time out after 30 s if
  the command does not read.
- Plan limits: concurrent sandboxes (10 on Hobby, 10,000 on Pro), control
  plane requests per minute, and a vCPU allocation rate. Hitting them answers
  429, which Differently treats as `insufficient_capacity` and retries.
- 19 regions; the default is `iad1`.
- Pricing: active CPU time, provisioned memory, sandbox creations, data
  transfer and snapshot storage, billed by usage (Pro in `iad1`: $0.128 per
  CPU-hour, $0.0212 per GB-hour of memory).
- Error classification: 400 and 422 (a bad image or snapshot), and a
  sandbox name already held by a sandbox Differently did not create →
  `invalid_request`; 429 → `insufficient_capacity`; a session that fails or
  ends before it runs → `sandbox_failed`; 401, 402, 403 and 5xx →
  `provider_unavailable`. Vercel's `{"error": {"code"}}` is kept as
  `providerCode`.

## How it was tested

`go test ./internal/adapters/vercel/` runs the adapter against a fake server
built from the published request and response shapes: create with
resources, tags, timeout and env; idempotent create; exec with a shell
command, with argument lists, with stdin, and a stream that drops before the
command finishes; files (tarball upload, read, missing file, `find`
listing); ports; TTL extension and its cap; stop and start; snapshot,
resume and fork; paged `List` filtered to one installation; idempotent
delete of sandboxes and snapshots; and error classification (bad image,
429, 5xx, failed session, rejected token). Nothing has run against
`api.vercel.com`: no Vercel token was available.
