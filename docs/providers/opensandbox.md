# OpenSandbox

| | |
|---|---|
| Status | **Verified live** (2026-10-28 and 29): the end-to-end suite passes 14/14 against OpenSandbox 1.2.0 on its Docker runtime (arm64, Colima) and on its Kubernetes runtime (single-node k3s on Azure, with a snapshot registry, signed ports and egress policy) |
| Adapter | `opensandbox` (`internal/adapters/opensandbox`) |
| Trust | declared: `private` when you run the server, `external` when someone else does |
| Isolation | the runtime's: containers on the Docker and Kubernetes runtimes (shared host kernel), Firecracker microVMs with Fast Sandbox. The pool declares which with `isolation` |
| API checked | [opensandbox-group/OpenSandbox](https://github.com/opensandbox-group/OpenSandbox) release 1.2.0 (server, `execd` release-1.2.0), 2026-10-28 |

OpenSandbox is Differently's default private runtime. The full
installation guide is [OpenSandbox pools](../guides/opensandbox-pools.md).

## Capabilities

| Capability | Docker runtime | Kubernetes runtime |
|---|---|---|
| exec | yes | yes |
| exec.stream | yes | yes |
| files | yes | yes |
| stdin | yes (staged in a file the command reads) | yes |
| lifecycle.stop-start | no | yes, with a snapshot registry (root filesystem kept, pod freed) |
| lifecycle.pause-resume | yes (memory kept) | no (no memory is kept; offered as stop/start) |
| snapshots.disk | yes | yes, with a snapshot registry |
| snapshots.memory | no | no (Fast Sandbox documents it; not verified) |
| fork | yes | yes, with a snapshot registry |
| ports.expose | no (direct endpoints are runtime-internal) | yes, with `ports: signed` (ingress gateway, signed time-limited URLs) |
| network.egress-control | not verified | yes, with `egress: none` or `allowlist` |
| Max size | server and runtime limits | server and runtime limits |
| Provider TTL backstop | `timeout` (at least 60 s), moved on TTL change | same |

**Fast Sandbox runtime** (`runtime: fastsandbox`): Firecracker microVMs
created from a Fast Sandbox template, pause that keeps memory, disk snapshots
and fork, and OpenSandbox's own idle pause with wake on access. Run live
on 2026-10-29 (single node, nested KVM) and recorded; OpenWork's Workbot
desktop streamed through it. Pause is slow: a full checkpoint, read back and
uploaded, about 3.5 minutes for a 4 GiB desktop (about 3 with sparse
templates, [shortening pauses](../guides/opensandbox-pools.md#shortening-pauses));
resume and wake take under two seconds
([setup, timings and gaps](../guides/opensandbox-pools.md#fast-sandbox-runtime-microvms-pause-with-memory-native-idle)).

## Configure

```yaml
pools:
  - name: opensandbox
    adapter: opensandbox
    trust: private                 # required
    region: dc1
    maxConcurrency: 50
    config:
      apiURL: http://opensandbox-server.opensandbox-system.svc/v1   # required
      image: ubuntu:24.04
      isolation: container         # or microvm (Fast Sandbox)
      runtime: kubernetes          # docker (default), kubernetes or fastsandbox
      lineEndings: kept            # with execd release-1.2.0 or later
      # ports: signed              # tenant port URLs through the ingress gateway
      # egress: allowlist          # none, public or allowlist
      # egressAllow: pypi.org,github.com
    credentials:
      secret: opensandbox-key      # or {env: DIFFERENTLY_OPENSANDBOX_API_KEY}
```

With Helm, the chart's `opensandbox` block writes this pool for you
(`runtime`, `ports`, `egress`, `egressAllow`, `lineEndings`). Store the key
with:

```sh
differently admin secret put opensandbox-key --from-file ./opensandbox-api-key.txt
```

Other keys: `templateId` (a Fast Sandbox template instead of an image),
`poolCPU` and `poolMemory` (Fast Sandbox: the SandboxPool's size, sent
when restoring from a snapshot),
`features` (a comma list that replaces the defaults), `useServerProxy`
(default `true`), `portScheme` (the scheme for gateway endpoints returned without one;
default `https` with signed ports, else `http`) and `portURLSeconds`
(default 3600).

## Limits and differences

- OpenSandbox reports no capacity, so a pool is limited only by
  `maxConcurrency` and quotas.
- `diskGiB` is not sent to OpenSandbox; quota and cost still use the
  requested size.
- On the Kubernetes runtime, an image that cannot be pulled looks like a slow
  pull (`POD_READY_TIMEOUT` after 60 s), so the sandbox is retried up to
  `scheduler.maxAttempts`. The pool stays healthy either way.
- Stop/start, snapshots and fork on Kubernetes need OpenSandbox's snapshot
  registry. Without one, they return `unsupported_operation`.
- Idle auto-stop (`idleTimeoutSeconds`) stops a sandbox on the Kubernetes
  runtime and pauses it on Docker; the next exec, file or port call wakes it
  (about 1.7 s and under 0.1 s).
- Open source (Apache-2.0); no pricing beyond the machines you run it on.

## How it was tested

- Unit tests against a fake OpenSandbox server, and real Docker and
  Kubernetes responses recorded in `testdata/recordings` and replayed in
  every `go test`.
- The [end-to-end suite](../../test/e2e/README.md), 14/14, on the Docker
  runtime (Colima, arm64) and the Kubernetes runtime (single-node k3s on an
  Azure Standard_D8s_v6, both charts installed with Helm), 2026-10-29,
  including stop/start, snapshots, fork, signed port URLs, stdin and idle
  auto-stop with wake.
- The Fast Sandbox runtime, live on single-node k3s (Azure
  Standard_D8s_v6, nested KVM), 2026-10-29: the adapter scenario, recorded
  as `fastsandbox-1.2.0`, and OpenWork's Workbot desktop through
  Differently: create, viewer streamed over a port URL, pause and resume
  keeping memory, idle pause and wake by a port request.
- The end-to-end suite on a Fast Sandbox pool, on two-node k3s with a
  Standard_D16s_v6 KVM node, 2026-10-29: 14 of 14 run passed
  ([azure-vms.md](../deploy/azure-vms.md#verify)).
- Not run: node drains, more than one KVM node, managed EKS, GKE and AKS.
