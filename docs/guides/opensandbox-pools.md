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

> Status: checked live against OpenSandbox release 1.2.0 with `execd`
> release-1.2.0, through the adapter's live test and the end-to-end suite:
> the Docker runtime (arm64, Colima) and the Kubernetes runtime (single-node
> k3s on Azure, x86_64, installed from OpenSandbox's and Differently's Helm
> charts). Both runs are recorded and replayed in every `go test`
> ([test/opensandbox](../../test/opensandbox/README.md)). Fast Sandbox is not
> verified yet
> ([POC 11](../proof-of-concepts.md#poc-11-fast-sandbox-through-the-adapter));
> for the Kubernetes runtime's open items see
> [POC 12](../proof-of-concepts.md#poc-12-opensandbox-kubernetes-runtime).

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
  apiURL: http://opensandbox-server.opensandbox-system.svc/v1   # the lifecycle API, ending in /v1
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
(namespace `opensandbox-system`, port 80 by default, matching OpenSandbox's
own chart). Complete examples:
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
      apiURL: http://opensandbox-server.opensandbox-system.svc/v1
      image: ubuntu:24.04     # default image URI (a sandbox template overrides it)
      isolation: container    # container (Docker/Kubernetes runtime) or microvm (Fast Sandbox)
      # templateId: fsb-...   # Fast Sandbox template instead of an image
      # features: exec,exec.stream,files   # Kubernetes runtime without a snapshot registry
      # useServerProxy: "true"
    credentials:
      env: DIFFERENTLY_OPENSANDBOX_API_KEY   # the server's api_key, by reference
```

Pool `config` values are strings, so `features` is a comma list here. Pools
can also be registered at runtime with `POST /v1/admin/pools`. The
credential is then a reference to an environment variable or file on every
server replica, or `{secret: name}` for a key stored in Differently. A
stored key can be replaced without a restart
([keys and providers](keys-and-providers.md)).

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
| Exec stdin | yes: OpenSandbox's command API takes no stdin, so the adapter writes it to a file the command reads and removes the file afterwards |
| Files (upload, download, list) | yes |
| Image that cannot be pulled | the sandbox fails or is retried with a reason naming the image, and the pool stays healthy for other tenants. The Docker runtime reports the failed pull at once (the sandbox fails after one attempt); the Kubernetes runtime only reports that the pod did not become ready within 60 s (`POD_READY_TIMEOUT`), which is also what a slow first pull looks like, so it is retried up to `scheduler.maxAttempts` |
| Pause / resume (memory kept) | the Docker runtime only. The Kubernetes runtime keeps no memory, so with `runtime: kubernetes` the pool offers stop/start instead and refuses pause |
| Stop / start (disk kept) | with `runtime: kubernetes` and the controller's snapshot registry: OpenSandbox's pause commits the root filesystem as an image and frees the pod, and resume recreates it. About 3.7 s and 2.7 s on single-node k3s. The Docker runtime has no stop |
| Snapshots and fork | disk snapshots on both runtimes; the Kubernetes runtime needs the snapshot registry (otherwise `unsupported_operation`, from OpenSandbox's `RegistryNotConfigured`). Memory snapshots: not on Docker or Kubernetes; add `snapshots.memory` only for a runtime that captures memory (Fast Sandbox, unverified) |
| Port URLs | with `ports: signed`: OpenSandbox's ingress gateway in route mode `uri` with Secure Access signing keys. Sandboxes are created with secure access, and `ports.expose` returns `https://<gateway>/{sandbox}/{port}/{expires}/{signature}`, valid for `portURLSeconds` (default 3600), with no header or key. Without it there are no port URLs: direct endpoints are runtime-internal. Endpoints that need a header are always refused |
| Provider-side TTL backstop | yes: OpenSandbox's `timeout` is set from the sandbox's TTL at creation (minimum 60 s), and a TTL change moves it (`renew-expiration`) to five minutes after Differently's own expiry |
| Capacity | not reported: OpenSandbox's API exposes none, so the pool is limited only by `maxConcurrency` and quotas |

The adapter's default features are `exec`, `exec.stream`, `files`,
`lifecycle.pause-resume`, `snapshots.disk` and `fork`, plus
`isolation.container` or `isolation.microvm` from `isolation`. Setting
`features` replaces the list. A feature backed by an adapter method the
OpenSandbox adapter lacks is dropped, so stop/start cannot be added this way;
features with no adapter method behind them (such as
`network.egress-control` or `gpu`) are advertised as declared, so declare
them only if the runtime really provides them.

## Kubernetes runtime: snapshot registry, signed ports and egress

Verified on single-node k3s with OpenSandbox 1.2.0 from its own charts: the
end-to-end suite passes 13/13, and the run is recorded as
`internal/adapters/opensandbox/testdata/recordings/kubernetes-registry-1.2.0.json`.

1. **A snapshot registry** for stop/start, snapshots and fork: any OCI
   registry the node's container runtime can push to and pull from. For a
   single-node test, a `registry:2` with `hostNetwork` on the node's private
   address works. Point `controller.snapshot.registry` at
   `<node-ip>:5000/opensandbox-snapshots` with
   `controller.snapshot.registryInsecure: true`, set
   `controller.snapshot.containerdSocketPath` to
   `/run/k3s/containerd/containerd.sock` on k3s, and trust the registry over
   HTTP in `/etc/rancher/k3s/registries.yaml`. In production, use a real
   registry (Azure Container Registry, Harbor) with push and pull secrets.
2. **Signed port URLs.** Install OpenSandbox's `ingress-gateway` chart with
   `gateway.gatewayRouteMode: uri` and Secure Access keys, and set the server
   chart's `server.gateway` (`enabled`, `host`, `gatewayRouteMode: uri` and
   the same keys). Expose the gateway Service with a DNS name and TLS, then
   set the pool's `ports: signed` (and `portScheme: http` only behind a test
   tunnel). The gateway also offers activity tracking and wake-on-access
   (`gateway.activity`, `gateway.wake`, with Redis), not yet used by
   Differently.
3. **Egress.** `egress: none` denies all outbound traffic, and
   `egress: allowlist` with `egressAllow: pypi.org,github.com` allows only
   those hosts. OpenSandbox's egress sidecar enforces it, and the pool then
   advertises `network.egress-control`.

Pool values with all three:

```yaml
opensandbox:
  runtime: kubernetes
  ports: signed
  egress: allowlist
  egressAllow: pypi.org,files.pythonhosted.org
  lineEndings: kept
```

## Isolation, egress and credentials

- **Isolation is the runtime's.** Differently reports each pool's declared
  isolation in the trust report but does not enforce or detect it. The Docker
  and Kubernetes runtimes run containers that share the host kernel; only
  Fast Sandbox gives each sandbox a microVM. Declare what you deployed.
- **Egress is a pool setting.** `egress: none` or `allowlist` (with
  `egressAllow`) sends OpenSandbox's network policy with every sandbox, and
  the pool advertises `network.egress-control`. It is checked live on the
  Kubernetes runtime only. With `egress` empty or `public`, outbound traffic
  is open: restrict it in OpenSandbox's own configuration, with NetworkPolicy
  in its namespace, or with host and VPC firewalls, and block cloud metadata
  endpoints. Per-request allowlists are
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
