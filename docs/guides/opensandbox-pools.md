# OpenSandbox pools

[OpenSandbox](https://github.com/opensandbox-group/OpenSandbox) is
Differently's default private runtime
([ADR 0020](../adr/0020-opensandbox-replaces-node-agent.md)). Differently
does not run sandboxes itself: you install an OpenSandbox server, choose its
runtime (Docker, Kubernetes, or Firecracker microVMs with Fast Sandbox), and
register it as a pool. Differently adds tenancy, scoped keys, quotas and fair
queueing, policy-gated placement, audit and the trust report on top.
Background: [spike](../research/opensandbox-spike.md),
[ADR 0019](../adr/0019-opensandbox-backend-first.md).

> Status: checked live against OpenSandbox release 1.2.0 with the Docker
> runtime and `execd` release-1.2.0 and v1.1.0 (arm64, Colima), through the adapter's live test
> and through `differently-server`. The Kubernetes runtime and Fast Sandbox
> are not verified yet
> ([POC 12](../proof-of-concepts.md#poc-12-opensandbox-kubernetes-runtime),
> [POC 11](../proof-of-concepts.md#poc-11-fast-sandbox-through-the-adapter)).

## Install OpenSandbox

Install it with its own Helm chart or manifests, following its
documentation; Differently's chart does not install it. Set the server's
`api_key`, pin the server and `execd` image versions, and decide who
operates it. One OpenSandbox server has one API key and no tenant model, so
it is one trust domain and should be registered as **one pool**. Steps and
cloud notes: [deploy/kubernetes.md](../deploy/kubernetes.md#2-install-opensandbox-and-choose-its-runtime),
[deploy/prerequisites.md](../deploy/prerequisites.md#opensandbox).

## Configure the pool with Helm

The chart's `opensandbox:` block is enabled by default and renders the
default pool:

```yaml
opensandbox:
  enabled: true
  apiURL: http://opensandbox-server.opensandbox.svc:8090/v1   # the lifecycle API, ending in /v1
  trust: private              # private if you run the server, external if someone else does
  pool:
    name: opensandbox
    region: dc1
    maxConcurrency: 50        # 0 means no limit beyond quotas
  image: ""                   # empty means ubuntu:24.04; a request's template overrides it
  # templateId: fsb-...       # a Fast Sandbox template instead of an image
  isolation: container        # container (Docker/Kubernetes runtime) or microvm (Fast Sandbox)
  features: []                # empty means the adapter defaults (below)
  useServerProxy: true        # exec and files through the server's proxy
  existingSecret: opensandbox-api-key   # the server's api_key; or set apiKey inline
  existingSecretKey: api-key
```

The key reaches the server as the environment variable
`DIFFERENTLY_OPENSANDBOX_API_KEY` (`opensandbox.env`), and the pool refers
to it by name. If you set neither `existingSecret` nor `apiKey`, the server
reads key `api-key` from a Secret named `<fullname>-opensandbox` (for
release `dly`, `dly-differently-opensandbox`), which you create before
installing; the chart's NOTES print the command, and
server pods wait until the Secret exists. The chart's NetworkPolicy allows
server egress to OpenSandbox through `networkPolicy.server.egress.opensandbox`
(namespace `opensandbox`, port 8090 by default). Complete examples:
`deploy/helm/differently/ci/opensandbox-values.yaml` (Fast Sandbox declared,
existing Secret) and `ci/hybrid-daytona-values.yaml` (OpenSandbox with
Daytona overflow).

## Configure a pool in the server config

Without Helm, or for a second OpenSandbox server, declare the pool in the
server's config file (with Helm, under `config.pools`):

```yaml
pools:
  - name: opensandbox
    adapter: opensandbox
    trust: private            # required: private if you run the server, external if someone else does
    region: dc1
    maxConcurrency: 50
    config:
      apiURL: http://opensandbox-server.opensandbox.svc:8090/v1
      image: ubuntu:24.04     # default image URI (a sandbox template overrides it)
      isolation: container    # container (Docker/Kubernetes runtime) or microvm (Fast Sandbox)
      # templateId: fsb-...   # Fast Sandbox template instead of an image
      # features: exec,exec.stream,files,lifecycle.pause-resume,snapshots.disk,fork,ports.expose
      # useServerProxy: "true"
    credentials:
      env: DIFFERENTLY_OPENSANDBOX_API_KEY   # the server's api_key, by reference
```

Pool `config` values are strings, so `features` is a comma list here. Pools
can also be registered at runtime with `POST /v1/admin/pools`; the credential
is still a reference to an environment variable or file on every server
replica.

Configuration is rejected when:

- `trust` is missing (Differently cannot tell who operates the server);
- `config.apiURL` is missing;
- `isolation` is not `container` or `microvm`.

An `external` OpenSandbox pool follows the usual rules: it receives work only
when the global policy allows external placement and lists it in
`externalPools` ([hybrid-overflow.md](hybrid-overflow.md)).

Prefer one pool per server. The adapter tags every sandbox with its
Differently ID and installation ID in OpenSandbox metadata, and the
reconciler deletes what it finds on a pool's backend that no live sandbox
uses. A second pool on the same server is safe, because each pool leaves the
other's live sandboxes alone, but it adds no isolation.

## What works, and what OpenSandbox does not offer

| | OpenSandbox pool |
|---|---|
| Create, delete, list, idempotent create | yes (Differently's IDs are kept in OpenSandbox metadata) |
| Exec, streamed exec, env, cwd, timeouts, exit codes | yes. With `execd` release-1.2.0 or later and `lineEndings: kept`, output is byte-exact. With `execd` 1.1 (`lineEndings: stripped`, the default) a newline is restored after each line, so a final fragment without one gains one |
| Exec stdin | **no**: `unsupported_operation`. OpenSandbox's command API takes no stdin |
| Files (upload, download, list) | yes |
| Pause / resume | yes |
| Stop / start | **no**: OpenSandbox has pause/resume and delete only, so stop returns `unsupported_operation` and the pool never offers `lifecycle.stop-start` or `persistence.disk` |
| Snapshots and fork | disk snapshots by default. Memory snapshots depend on the runtime: add `snapshots.memory` to `features` only when it captures memory (Fast Sandbox is documented to; unverified). Differently reports the kind the pool declares and cannot check it |
| Port URLs | only when OpenSandbox serves the port directly, without request headers. Endpoints that need the server's API key are refused, because that key must never reach tenants. Not checked live |
| Provider-side TTL backstop | yes: OpenSandbox's `timeout` is set from the sandbox's TTL at creation (minimum 60 s), and a TTL change moves it (`renew-expiration`) to five minutes after Differently's own expiry |
| Capacity | not reported: OpenSandbox's API exposes none, so the pool is limited only by `maxConcurrency` and quotas |

The adapter's default features are `exec`, `exec.stream`, `files`,
`lifecycle.pause-resume`, `snapshots.disk`, `fork` and `ports.expose`, plus
`isolation.container` or `isolation.microvm` from `isolation`. Setting
`features` replaces the list. A feature backed by an adapter method the
OpenSandbox adapter lacks is dropped, so stop/start cannot be added this way;
features with no adapter method behind them (such as
`network.egress-control` or `gpu`) are advertised as declared, so declare
them only if the runtime really provides them.

## Isolation, egress and credentials

- **Isolation is the runtime's.** Differently reports each pool's declared
  isolation in the trust report but does not enforce or detect it. The Docker
  and Kubernetes runtimes run containers that share the host kernel; only
  Fast Sandbox gives each sandbox a microVM. Declare what you deployed.
- **Egress is not wired.** Differently does not configure OpenSandbox's
  network policy, and the pool does not offer `network.egress-control`.
  Restrict sandbox egress in OpenSandbox's own configuration, with
  NetworkPolicy in its namespace, or with host and VPC firewalls, and block
  cloud metadata endpoints. Wiring it is
  [backlog E19](../backlog.md#e19-opensandbox-network-policy-and-egress).
- **The API key stays in the control plane.** It is sent only to the
  OpenSandbox server (as `OPEN-SANDBOX-API-KEY`, including through its
  proxy), never to tenants, sandboxes or logs, and appears in the trust report
  only as a reference. Anyone who can read the server's environment can use
  it. Whether OpenSandbox's Credential Vault could hold sandbox secrets is
  [backlog E20](../backlog.md#e20-credential-vault-evaluation).

## Upgrades

OpenSandbox's published spec has run ahead of the images its server deploys:
the spike found four differences between the 1.2 spec and `execd` v1.1.0,
all handled by the adapter and covered by its fake server tests. `execd`
`release-1.2.0` keeps line endings; set `lineEndings: kept` with it for
byte-exact output. Pin the
server and `execd` versions per pool, and run the live test against a
staging server on every OpenSandbox upgrade
([deploy/day-2.md](../deploy/day-2.md#opensandbox),
[backlog E21](../backlog.md#e21-opensandbox-version-pinning-and-upgrade-testing)).

## Verify

```bash
differently-server preflight --config config.yaml   # the credential reference resolves
differently admin pool list                         # the pool is healthy
differently sandbox create --pool opensandbox --wait
differently sandbox placement <id>
differently sandbox stop <id>                       # refused: unsupported_operation
differently admin trust-report                      # endpoint and isolation shown, credential only by reference
```

To check the adapter against a real OpenSandbox server, see
[test/opensandbox](../../test/opensandbox/README.md). The end-to-end suite
in `test/e2e` runs the real control plane and Postgres against a real
OpenSandbox server with the Docker runtime; see
[test/e2e/README.md](../../test/e2e/README.md) for what it checks and where
it passed.
