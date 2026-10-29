# agent-sandbox (Kubernetes)

| | |
|---|---|
| Status | **Verified live** (2026-10-28): the adapter's live test and a Differently server, against agent-sandbox v1.0.8 on a single-node kind cluster (Kubernetes 1.37, runc, arm64) |
| Adapter | `agent-sandbox` (`internal/adapters/agentsandbox`) |
| Trust | declared (self-hostable) |
| Isolation | The pod's RuntimeClass: runc containers by default, gVisor (`runsc`) or Kata microVMs when the cluster has them and the pool sets `runtimeClassName`. Only runc was run |
| API checked | [kubernetes-sigs/agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) v1.0.8 release manifests and source at commit `b1277676` (`api/v1beta1`, `extensions/api/v1beta1`, the controllers), read 2026-10-28; [docs](https://agent-sandbox.sigs.k8s.io/docs/) |

[agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) is a
Kubernetes SIG Apps project: a `Sandbox` custom resource
(`agents.x-k8s.io/v1beta1`) that a controller turns into one stateful pod,
plus `SandboxTemplate`, `SandboxWarmPool` and `SandboxClaim`
(`extensions.agents.x-k8s.io/v1beta1`) for pools of pre-started sandboxes.

The adapter talks to the Kubernetes API server with the standard library
(no client-go): it creates Sandboxes, or SandboxClaims on a warm pool, and
runs commands and file operations through `pods/exec` over WebSocket
(`v5.channel.k8s.io`). Nothing runs inside the sandbox besides the image's
own shell, so any image with `/bin/sh` and the usual coreutils or BusyBox
works.

## Capabilities

| Capability | Direct Sandboxes | Warm pool (`warmPool`) |
|---|---|---|
| exec | yes: `pods/exec`; env through `env(1)`, cwd through `sh`, stdin streamed | yes |
| exec.stream | yes: WebSocket frames as they arrive, stdout and stderr separate | yes |
| files | yes: `cat`, `mkdir -p` and `stat -c` over exec; reads capped at 100 MiB | yes |
| stdin | yes; needs Kubernetes 1.30 or later (the v5 exec protocol closes stdin) | yes |
| lifecycle.stop-start | with `volumeSize`: `spec.operatingMode: Suspended` deletes the pod and keeps its volume; only files under `volumePath` survive | no |
| lifecycle.pause-resume | no (Kubernetes has no memory-keeping pause) | no |
| snapshots.disk | no | no |
| snapshots.memory | no | no |
| fork | no | no |
| ports.expose | opt-in (`features`): the headless Service's DNS name, `http://<sandbox>.<namespace>.svc.cluster.local:<port>`, reachable inside the cluster only | opt-in: the adopted pod's IP |
| max size | none from the adapter; the cluster's nodes and ResourceQuotas | the SandboxTemplate's |
| provider TTL backstop | yes: `spec.shutdownTime` with `shutdownPolicy: Delete`, moved when the TTL changes | yes: the claim's `lifecycle.shutdownTime` |

Default features: `exec`, `exec.stream`, `files`, plus
`lifecycle.stop-start` and `persistence.disk` when `volumeSize` is set, plus
`isolation.container` or `isolation.microvm` from `isolation`.

## Configure

Install agent-sandbox from its release manifests (with the extensions for
warm pools):

```bash
kubectl apply -f https://github.com/kubernetes-sigs/agent-sandbox/releases/download/v1.0.8/sandbox-with-extensions.yaml
```

Give Differently a namespace and a service account that may manage
Sandboxes there (WebSocket exec needs both `get` and `create` on
`pods/exec`):

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: differently-sandboxes, namespace: sandboxes}
rules:
- apiGroups: [agents.x-k8s.io]
  resources: [sandboxes]
  verbs: [get, list, create, patch, delete]
- apiGroups: [extensions.agents.x-k8s.io]
  resources: [sandboxclaims]
  verbs: [get, list, create, patch, delete]
- apiGroups: [extensions.agents.x-k8s.io]
  resources: [sandboxwarmpools]
  verbs: [get]
- apiGroups: [""]
  resources: [pods]
  verbs: [get]
- apiGroups: [""]
  resources: [pods/exec]
  verbs: [get, create]
```

Bind it to the Differently server's service account (in the same cluster)
or to one whose token you give the pool (another cluster). Then declare the
pool in the server config (with Helm, under `config.pools`):

```yaml
pools:
  - name: k8s
    adapter: agent-sandbox
    trust: private              # required: private if you run the cluster, external if someone else does
    region: dc1
    maxConcurrency: 50
    config:
      namespace: sandboxes      # default "default"
      image: ubuntu:24.04       # default; a request's template overrides it
      isolation: container      # or microvm with a Kata RuntimeClass
      # runtimeClassName: gvisor
      # volumeSize: 10Gi        # a PVC per sandbox at volumePath; enables stop/start
      # volumePath: /workspace
      # storageClassName: fast
      # serviceAccountName: sandbox  # the pod's; its token is not mounted
      # features: exec,exec.stream,files,ports.expose
```

In the cluster, the adapter uses the server pod's service account token
(re-read as it rotates) and the in-cluster CA; no `credentials` are needed.
For another cluster set `apiServer` and `caFile`, and pass a token:

```yaml
    config:
      apiServer: https://k8s.example.com:6443
      caFile: /etc/differently/k8s-ca.crt
      namespace: sandboxes
    credentials: {env: DIFFERENTLY_K8S_TOKEN}   # or {secret: <name>}, {file: /path}
```

Tokens from `kubectl create token` expire (one hour by default); for a
pool, use a long-lived service account token Secret.

With Differently's Helm chart, server egress is restricted by default
(`networkPolicy.server.egress.restricted`). Allow the API server with an
entry in `networkPolicy.server.egress.extra`: NetworkPolicy sees the
`kubernetes` Service's endpoints, usually the control-plane nodes on TCP
6443. The chart creates no RBAC for agent-sandbox; bind the Role above to
the server's service account yourself.

For warm pools, create a `SandboxTemplate` and a `SandboxWarmPool`, then
point a pool at it. The template fixes the image, resources, RuntimeClass
and volumes, so the pool advertises the warm pool's name as its only
template and rejects `image`, `runtimeClassName`, `volumeSize`,
`storageClassName` and `serviceAccountName`:

```yaml
  - name: k8s-warm
    adapter: agent-sandbox
    trust: private
    config:
      namespace: sandboxes
      warmPool: python-warm     # a SandboxWarmPool in that namespace
      # container: sandbox      # only if the template's pod has several containers
```

| Key | Default | Meaning |
|---|---|---|
| `apiServer` | in-cluster (`KUBERNETES_SERVICE_HOST`) | Kubernetes API URL |
| `caFile` | the service account's `ca.crt` in-cluster | PEM file for the API server's certificate |
| `namespace` | `default` | where Sandboxes or claims are created |
| `image` | `ubuntu:24.04` | image when the request names no template |
| `runtimeClassName` | none | the pod's RuntimeClass (`gvisor`, `kata-qemu`…) |
| `isolation` | none | `container` or `microvm`, declared, not checked |
| `volumeSize`, `volumePath`, `storageClassName` | none, `/workspace`, cluster default | a PersistentVolumeClaim per sandbox |
| `serviceAccountName` | namespace default | the sandbox pod's service account (its token is never mounted) |
| `warmPool`, `container` | none | claim from a SandboxWarmPool instead of creating Sandboxes |
| `features` | as above | comma list replacing the defaults |

## Limits and differences

- The adapter replaces the image's entrypoint with a shell loop that keeps
  the pod alive, as the OpenSandbox pools do; services the image would
  start must be started with exec.
- Commands need `/bin/sh`; env, cwd, timeouts and files use `env`,
  `timeout`, `cat`, `mkdir`, `dirname` and `stat`, which GNU coreutils and
  BusyBox provide. A timeout ends the command with exit code 124. A command
  the runtime cannot start reports 127 (not found) or 126 (not executable).
- Stop/start keeps only the volume. The pod and its root filesystem are
  deleted while suspended, and processes restart from the shell loop.
- `resources.cpu` and `memoryMiB` become the container's requests and
  limits; `diskGiB` becomes its `ephemeral-storage` request and limit, so
  the kubelet evicts a sandbox that writes more than that outside its
  volume.
- In warm-pool mode a claim adopts a running pod in well under a second.
  Request env makes the controller start a fresh Sandbox instead, and needs
  the template's `envVarsInjectionPolicy: Allowed`. Request resources and
  images do not apply.
- Templates with the default managed NetworkPolicy admit ingress only from
  agent-sandbox's router, so `ports.expose` URLs from such pods only work
  through the router. Exec is unaffected: it goes through the kubelet.
- An image that does not exist fails at once with `invalid_request`, and
  the Sandbox is removed. A pod in a crash loop is `sandbox_failed`. A slow
  pull keeps the create waiting, up to the scheduler's provision timeout.
- `Capacity` is unbounded; set `maxConcurrency`, and Kubernetes
  ResourceQuotas in the namespace.
- Not wired: the GKE Pod Snapshot extension, the sandbox router and
  `sandboxd`. Recording and replaying a real cluster's exchanges, as the
  OpenSandbox adapter does, is not done: exec is a WebSocket stream, which
  the HTTP recorder does not capture.

## How it was tested

- **Fake API server** (`agentsandbox_test.go`, every `go test`): a TLS
  Kubernetes API with Sandboxes, SandboxClaims, pods and WebSocket exec
  (including v4-only servers), checking the manifests sent, idempotent
  create, readiness against the current generation, paged and failing
  lists, exec arguments and exit codes, stdin, streaming, files, stop/start,
  warm-pool claims, token rotation and that credentials are not echoed.
  `kube_test.go` covers the WebSocket framing.
- **Live** (2026-10-28, `go test -tags live -run TestLiveAgentSandbox`):
  agent-sandbox v1.0.8 (`registry.k8s.io/agent-sandbox/agent-sandbox-controller:v1.0.8`,
  `sandbox-with-extensions.yaml`) on kind v0.33.0 (Kubernetes 1.37.0,
  containerd 2.3.4, runc) on an arm64 Colima VM, with a service account
  holding only the Role above. Image `debian:bookworm-slim`.
  - Direct Sandboxes with a 1 GiB volume and `ports.expose`: create (about
    5 s with the image cached), idempotent create, list, exec with env, cwd
    and argument lists, exit code 7, stdin, a missing binary (127), a 1 s
    timeout (124), streaming, a 300 KiB file written, read back and listed,
    missing files and directories (`not_found`), stop (the pod is deleted)
    and start in 2 to 5 s with the volume's file intact, exec while stopped
    (`invalid_state`), the Service DNS name resolving in the cluster, the TTL
    backstop moved, delete and idempotent delete, and an image that does not
    exist (`invalid_request`, the Sandbox removed). 17 s in all.
  - A warm pool of one (`SandboxTemplate` with `debian:bookworm-slim`):
    create in 0.5 to 1 s by adopting the warm pod, exec, stdin, streaming,
    files, delete of the claim. 3 s in all.
- **Through Differently** (2026-10-28): a `differently-server` built from
  this branch (memory store) with one direct pool and one warm-pool pool,
  driven through the public API. Both pools were healthy with the expected
  capabilities. A tenant's sandbox was running in 5.3 s (direct) and 0.6 s
  (warm pool). Exec, a file write, stop and start (direct; the volume's file
  kept) and delete all worked. Stop on the warm pool answered `501
  unsupported_operation`. A reconcile pass found nothing to clean up, and no
  Sandbox, PVC or Service was left behind.
- **Not run:** gVisor or Kata RuntimeClasses, a multi-node cluster,
  Kubernetes before 1.30, the in-cluster service account path (the live runs
  used a token and CA file), `ports.expose` fetched from outside the pod,
  and the Helm chart with these pools.
