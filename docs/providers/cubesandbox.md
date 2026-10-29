# CubeSandbox

| | |
|---|---|
| Status | **Tested with a fake server** (implemented from CubeSandbox's API source and OpenAPI document, not run against CubeSandbox; see [How it was tested](#how-it-was-tested)) |
| Adapter | `cubesandbox` (`internal/adapters/cubesandbox`) |
| Trust | declared (self-hostable) |
| Isolation | KVM microVM per sandbox, run by `cube-hypervisor` (a Cloud Hypervisor fork) through a containerd shim; x86_64 and aarch64 hosts with `/dev/kvm` |
| API checked | [TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox) v0.7.3 (release 2026-10-12), source at commit `e298a426`: `openapi.yml`, CubeAPI's routes, models and services, CubeProxy's routing, the Go SDK's envd client; read 2026-10-28. [Docs](https://cubesandbox.com/guide/quickstart.html) |

[CubeSandbox](https://github.com/TencentCloud/CubeSandbox) runs each sandbox
in a KVM microVM and exposes E2B's API: CubeAPI (port 3000) mirrors E2B's
lifecycle routes, and every sandbox runs E2B's `envd` (built from
`e2b-dev/infra`), reached through CubeProxy at
`<port>-<sandboxID>.<domain>`.

**One E2B-style adapter for both?** Mostly. The lifecycle routes and envd
(Connect `process.Process/Start`, `/files`, `filesystem.Filesystem/*`) are
E2B's, and `internal/adapters/cubesandbox/envd.go` has no CubeSandbox-specific
code besides how CubeProxy is addressed. The differences an E2B adapter
would need to switch on: CubeSandbox's `timeout` counts idle seconds, its
`state` list filter takes one value, `secure` is ignored so envd needs no
access token (E2B's hosted service sends `X-Access-Token`), `connect`
always answers 200, and snapshots, forks and restores are CubeSandbox's own
routes (`/sandboxes/{id}/snapshots`, a snapshot ID used as `templateID`,
`DELETE /templates/{id}`). A shared E2B protocol package with a CubeSandbox
profile would serve both; this adapter keeps the two apart until the E2B
adapter exists.

## Capabilities

| Capability | Support |
|---|---|
| exec | yes: envd `process.Process/Start` with `/bin/sh -c`; separate stdout and stderr; env and cwd native; timeout through `Connect-Timeout-Ms`, reported as exit code 124 |
| exec.stream | yes: Connect stream frames as envd sends them |
| files | yes: envd `GET`/`POST /files` (multipart) and `filesystem.Filesystem/ListDir`; parents created with `MakeDir`; reads capped at 100 MiB |
| stdin | yes, staged in a file the command reads (envd takes stdin only as later `SendInput` calls) |
| lifecycle.stop-start | no: CubeSandbox has pause/resume and delete only |
| lifecycle.pause-resume | yes: `POST /sandboxes/{id}/pause` (memory and disk snapshot in place), resume through `POST /sandboxes/{id}/connect` |
| snapshots.disk | yes, but every snapshot captures memory and disk; reported as `kind: memory` |
| snapshots.memory | yes: `POST /sandboxes/{id}/snapshots` |
| fork | yes: create with the snapshot ID as the template |
| ports.expose | opt-in (`features`): `https://<port>-<id>.<domain>`, or with `sandboxURL` the path form `<sandboxURL>/sandbox/<id>/<port>` |
| max size | set by the template (CubeAPI's create takes no CPU or memory) |
| provider TTL backstop | partial: `timeout` at create and `POST /sandboxes/{id}/timeout`, which CubeSandbox counts as **idle** time, so it never fires before the TTL but may fire later, or never while the sandbox is in use |

Default features: `exec`, `exec.stream`, `files`, `lifecycle.pause-resume`,
`snapshots.disk`, `snapshots.memory`, `fork`, `isolation.microvm`.

## Configure

Install CubeSandbox following its
[quickstart](https://cubesandbox.com/guide/quickstart.html) (one-click on a
KVM host, or its Helm chart), create a template, and turn on CubeAPI's auth
(`CUBE_API_KEY`, or `AUTH_CALLBACK_URL`): the one-click default has none.
Then declare the pool in the server config (with Helm, under
`config.pools`):

```yaml
pools:
  - name: cube
    adapter: cubesandbox
    trust: private              # required: private if you run CubeSandbox, external if someone else does
    region: dc1
    maxConcurrency: 50
    config:
      apiURL: http://cube-api.internal:3000   # required: CubeAPI
      templateId: tpl-python                 # default template; a request's template overrides it
      sandboxURL: http://cubeproxy.internal:80  # CubeProxy, with header routing
      # domain: cube.example.com     # host routing instead: <port>-<id>.<domain> (default: CubeAPI's, else cube.app)
      # proxyScheme: https           # for host routing (default https)
      # caFile: /etc/differently/cube-ca.pem   # CubeProxy's CA (one-click uses mkcert)
      # user: root                   # envd user
      # features: exec,exec.stream,files,lifecycle.pause-resume,snapshots.memory,fork,isolation.microvm,ports.expose
    credentials: {env: DIFFERENTLY_CUBE_API_KEY}   # or {secret: <name>}; omit if CubeAPI has no auth
```

With Differently's Helm chart, server egress is restricted by default
(`networkPolicy.server.egress.restricted`): add entries to
`networkPolicy.server.egress.extra` for CubeAPI (TCP 3000) and CubeProxy
(TCP 80 or 443).

| Key | Default | Meaning |
|---|---|---|
| `apiURL` | required | CubeAPI base URL |
| `templateId` | none | template when the request names none; checked by the health probe |
| `sandboxURL` | none | CubeProxy for header routing (`E2b-Sandbox-Id`/`-Port`); port URLs then use its path form |
| `domain` | CubeAPI's, else `cube.app` | host routing domain; needs wildcard DNS to CubeProxy |
| `proxyScheme` | `https` | scheme for host routing |
| `caFile` | system roots | CA for CubeProxy's TLS certificate |
| `user` | `root` | envd user for commands and files |
| `features` | as above | comma list replacing the defaults |

## Limits and differences

- **envd and ports have no authentication** of their own: anyone who can
  reach CubeProxy can run commands in any sandbox whose ID they know.
  Keep CubeProxy on a network only the Differently server reaches. The
  adapter does not use CubeSandbox's private traffic tokens yet.
- Create is idempotent through metadata (`differently.sandbox-id`,
  `differently.installation`): CubeAPI takes no client-chosen ID. Its list
  walks the whole cluster, so the health probe calls `/health` and, with a
  default template, `GET /templates/{id}` instead.
- Request `resources` are not sent: the template decides CPU and memory.
  Quota and cost still use the requested size. `env` is sent as `envVars`
  (names must match `[A-Za-z_][A-Za-z0-9_]*`).
- A template that does not exist is `invalid_request`. A create that runs
  past CubeAPI's 30 s budget (HTTP 408) is retried as `sandbox_failed`; the
  next attempt finds the sandbox if it did start.
- CubeAPI's rate limit is 100 requests per second per `X-API-Key`.

## How it was tested

- **Fake server** (`cubesandbox_test.go`, every `go test`): CubeAPI and,
  behind CubeProxy's header and host routing, envd with Connect-framed
  process streams, `/files` and the filesystem RPCs, shaped after
  `openapi.yml`, CubeAPI's handlers and the Go SDK. It checks the create
  body, idempotent create, paged lists (`x-next-token`), pause and resume,
  the idle timeout computed from the start, snapshot, restore from a
  snapshot and snapshot deletion, delete, a missing template, exec with
  env and cwd, argument lists without a shell, exit 0 with no `exitCode`
  field (proto3 JSON), a missing binary (127), a timeout (124), stdin,
  streaming, files, host and path port URLs, health with a missing template
  and a rejected key.
- **Live:** not run. `live_test.go` (`go test -tags live -run
  TestLiveCubeSandbox`, settings in the file) is ready for a host. The
  one-click install needs a dedicated Linux host with `/dev/kvm`, at least
  7.5 GiB of memory (its preflight), XFS at `/data/cubelet`, about 50 GB of
  disk, root and Docker. On arm64 the bundle is installed by hand, and
  nested KVM must provide GICv3/ITS; running it in a Linux VM on Apple
  Silicon is not documented. The machine available for this work (a shared
  4 vCPU / 8 GiB arm64 VM with 20 GB of disk) did not meet that.
