# agent-sandbox (Kubernetes)

> **Experimental adapter.** It is not in the default `differently-server`
> build or the published image. It needs a server built with
> `-tags experimental_adapters`: see
> [what the server contains](README.md#what-the-server-contains).

| | |
|---|---|
| Status | **Verified live** (2026-10-28): the adapter's live test and a Differently server, against agent-sandbox v1.0.8 on a single-node kind cluster (Kubernetes 1.37, runc, arm64). Snapshots, fork and pause/resume through GKE Pod Snapshots: **tested with a fake server, not run on GKE**. Pause/resume on a warm pool: the adapter ran against agent-sandbox v1.0.12 on kind (2026-11-20) with a stand-in for GKE's snapshot controller, which checkpoints and restores nothing ([how](#how-it-was-tested)) |
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
| lifecycle.pause-resume | on GKE with `podSnapshotPolicy`: a pod snapshot, then the pod is deleted; resume restores it ([GKE](#snapshots-fork-and-pauseresume-on-gke)). Elsewhere no: Kubernetes has no memory-keeping pause | on GKE with `podSnapshotPolicy` ([warm pools](#with-a-warm-pool)) |
| snapshots.disk | on GKE with `podSnapshotPolicy` (the snapshot holds memory too) | no |
| snapshots.memory | on GKE with `podSnapshotPolicy`: GKE Pod Snapshots of gVisor pods | no: nothing could be forked from it |
| fork | on GKE with `podSnapshotPolicy`: a new Sandbox restored from a snapshot | no: a claimed pod is already running, so nothing can restore into it |
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

### A GKE cluster, with Google credentials

A control plane outside the cluster, for example on the Compute Engine VMs
of [the Compute Engine guide](../deploy/gcp-vms.md), can authenticate to GKE
as a Google service account instead of holding a Kubernetes token. Set
`auth: google`; the pool's credential is then a Google credential, the same
kinds `gcp-sandboxes` takes: a service-account JSON key,
`{"type": "metadata_server"}` (the VM's service account, the default when the
pool has no credential), or a workload identity federation configuration.
The adapter sends access tokens with the `cloud-platform` and
`userinfo.email` scopes, refreshes them before they expire, and fetches a new
one when the API server rejects one (a revoked key, say).

```yaml
  - name: gke
    adapter: agent-sandbox
    trust: private
    config:
      apiServer: https://gke-0123456789abcdef.us-central1.gke.goog   # the DNS-based endpoint
      auth: google
      namespace: sandboxes
      # ... runtimeClassName, podSnapshotPolicy or warmPool as above
    # no credentials: the VM's service account, through the metadata server
```

The DNS-based endpoint (`gcloud container clusters describe <cluster>
--location=<region> --format='value(controlPlaneEndpointsConfig.dnsEndpointConfig.endpoint)'`)
has a publicly trusted certificate, so no `caFile` is needed; the IP endpoint
needs the cluster's CA in `caFile`. On the Google side:

- Give the service account `roles/container.clusterViewer` on the project
  (it holds `container.clusters.get` and `container.clusters.connect`, which
  GKE needs to let the account in) and nothing broader: what it may do in the
  cluster comes from Kubernetes RBAC.
- Bind the Role above to the account by its email:

  ```yaml
  apiVersion: rbac.authorization.k8s.io/v1
  kind: RoleBinding
  metadata: {name: differently-sandboxes, namespace: sandboxes}
  roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: differently-sandboxes}
  subjects:
  - kind: User
    name: dly-control@<project>.iam.gserviceaccount.com
  ```

- On Compute Engine, the VM needs the `userinfo-email` access scope besides
  `cloud-platform`: GKE identifies the account by email only from a token
  with that scope, and Compute Engine leaves it out by default. Without it
  the RoleBinding does not match and every call is refused.
- With the Helm chart's restricted server egress, allow the metadata server
  (`169.254.169.254`, port 80) and the GKE endpoint (port 443).

Status: tested with a fake metadata server and the fake API server (the
scopes asked for, a rejected token replaced, the configuration checks); not
run against GKE.

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
| `auth` | unset | `google`: authenticate to GKE with a Google credential ([GKE with Google credentials](#a-gke-cluster-with-google-credentials)) |
| `features` | as above | comma list replacing the defaults |

## Snapshots, fork and pause/resume on GKE

On GKE, sandboxes that run under gVisor can be checkpointed with
[GKE Pod Snapshots](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/agent-sandbox-pod-snapshots)
(`podsnapshot.gke.io/v1`). With `podSnapshotPolicy` set, the pool adds
`snapshots.memory`, `snapshots.disk`, `fork`, `lifecycle.pause-resume` and
`lifecycle.pause-checkpoint`:

| Operation | What the adapter does |
|---|---|
| Snapshot | Creates a `PodSnapshotManualTrigger` for the sandbox's pod and waits for the `PodSnapshot` to be `Ready`. GKE captures the whole pod: memory, processes and the root filesystem. Volumes are not captured. The sandbox keeps running. The snapshot is reported as `memory`, whatever kind was asked for |
| Fork | Creates a Sandbox with the source's pod spec, in the source's snapshot group, with `podsnapshot.gke.io/ps-name` naming the snapshot, so GKE restores it. Once the restore is verified, the fork moves to a snapshot group of its own and stops naming the snapshot (the controller patches the running pod's labels; it does not recreate it). GKE restores only a pod whose spec matches the snapshot's, so the request's image and env do not apply, and its size must be the source's: a fork asked for at another size is refused with `invalid_request`, so capacity and quotas count what it uses |
| Pause | Checkpoints the sandbox, then suspends it (`operatingMode: Suspended`), which deletes the pod. The sandbox reports `paused` once the pod is gone. A paused sandbox uses no node resources and holds no slot in the pool. Only the newest pause checkpoint of each sandbox is kept. If the suspend does not finish in time, the sandbox is set running again |
| Resume | Sets `operatingMode: Running`. The controller recreates the pod, and GKE restores it from the pause checkpoint. The pod template then stops naming the checkpoint |
| Delete snapshot | Deletes the `PodSnapshot` (GKE removes its files) and its trigger. Deleting a sandbox deletes its pause checkpoints, but not its snapshots, which can still be forked. The pause checkpoints of a Sandbox deleted some other way (the TTL backstop) are deleted by the reconciler's next list |

Three properties of GKE's design shape this:

- **GKE restores every new pod that matches.** A pod is restored from the
  snapshot it names, or else from the newest snapshot in its group whose
  pod spec matches. Without groups, two tenants' sandboxes with the same
  image and size match, so a new sandbox could start from another one's
  memory. The adapter gives every sandbox's pod the label
  `differently.dev/snapshot-group` with a value of its own. A fork is in its
  source's group only until its restore is verified. The pool is
  **unhealthy** unless the policy selects on that label and groups by it.
  A pod GKE recreates on its own (a node drain, an eviction) therefore comes
  back from that sandbox's own newest checkpoint, never from another
  sandbox's, and a sandbox with no checkpoint comes back fresh.
- **A failed restore is silent.** GKE starts the pod fresh when it cannot
  restore it: another machine series or CPU vendor, a different pod spec, or
  a snapshot that is not ready. Before each checkpoint the adapter creates a
  marker file named after it (`/tmp/.differently-snapshot-<token>`), and
  looks for it after a restore. A fork without it fails with
  `sandbox_failed` and is deleted. A resume without it suspends the sandbox
  again, so it never runs on without its state, and fails with
  `sandbox_failed`. The scheduler tries the resume again later, which
  succeeds once the pod lands on a node GKE can restore onto. After three
  such resumes (a check that could not run does not count) the sandbox
  reports an error, and Differently fails it. Its pause checkpoint, the only
  copy of its state, is then kept with the label
  `differently.dev/snapshot-purpose=unrestored`. Restore it by hand, or
  delete it: `kubectl -n <namespace> delete podsnapshots <name>` and the
  trigger `kubectl -n <namespace> delete podsnapshotmanualtriggers -l differently.dev/snapshot-purpose=unrestored`.
  A fork whose create was interrupted (the control plane restarted) is
  checked and moved to its own group by the next list, or deleted and
  created again if GKE did not restore it.
- **Restores come from the pod template.** A stop/start would restore the
  newest checkpoint over a newer volume, so a pool with `podSnapshotPolicy`
  does not offer stop/start, even with `volumeSize`. Pause/resume takes its
  place.

### Set it up

Requirements from Google: GKE 1.35.3-gke.1234000 or later, Standard or
Autopilot, a gVisor node pool (not E2), and a Cloud Storage bucket. The
commands below follow Google's guide. They were checked against its text,
not run.

```sh
gcloud container clusters create <cluster> --location=<region> --release-channel=rapid \
  --enable-pod-snapshots --workload-pool=<project>.svc.id.goog --workload-metadata=GKE_METADATA
gcloud container node-pools create gvisor --cluster=<cluster> --location=<region> \
  --machine-type=n2-standard-4 --sandbox type=gvisor
gcloud storage buckets create gs://<bucket> --location=<region> \
  --uniform-bucket-level-access --enable-hierarchical-namespace --soft-delete-duration=0d
```

Grant the bucket roles in Google's guide to the namespace's pods and to
GKE's snapshot controller, or set `tokenSource: federatedP4SA` on the
storage config. Then install agent-sandbox (or enable GKE's managed add-on)
and create the storage config and the policy:

```yaml
apiVersion: podsnapshot.gke.io/v1
kind: PodSnapshotStorageConfig
metadata: {name: differently}            # cluster-scoped
spec:
  snapshotStorageConfig:
    gcs: {bucket: <bucket>, path: differently}
---
apiVersion: podsnapshot.gke.io/v1
kind: PodSnapshotPolicy
metadata: {name: differently, namespace: sandboxes}
spec:
  storageConfigName: differently
  selector:
    matchExpressions:
      - {key: differently.dev/snapshot-group, operator: Exists}   # required
  triggerConfig: {type: manual, postCheckpoint: resume}           # required
  snapshotGroupingRules:                                          # required
    groupByLabelValue:
      labels: [differently.dev/snapshot-group]
```

The pool refuses a policy whose trigger is not `manual`, whose
`postCheckpoint` is `stop`, whose `snapshotScope` is `rootfs-only` (it keeps
no memory), or that sets `maxSnapshotCountPerGroup` (GKE would delete
snapshots Differently still lists). `retentionConfig.lastAccessTimeout` is
allowed. A snapshot it expires can no longer be forked: the fork fails with
`not_found`.

Add these rules to the Role:

```yaml
- apiGroups: [podsnapshot.gke.io]
  resources: [podsnapshotpolicies]
  verbs: [get]
- apiGroups: [podsnapshot.gke.io]
  resources: [podsnapshotmanualtriggers]
  verbs: [get, list, create, patch, delete]
- apiGroups: [podsnapshot.gke.io]
  resources: [podsnapshots]
  verbs: [get, delete]
```

and configure the pool:

```yaml
  - name: gke
    adapter: agent-sandbox
    trust: private
    config:
      namespace: sandboxes
      image: python:3.13
      runtimeClassName: gvisor           # required with podSnapshotPolicy
      podSnapshotPolicy: differently
      # snapshotTimeout: 10m             # how long a checkpoint may take
      # serviceAccountName: sandbox      # the pods' service account for the bucket roles
```

### With a warm pool

Fast starts and pause/resume together: a pool with both `warmPool` and
`podSnapshotPolicy` claims a running warm pod (110 ms per create on kind,
including the adapter's wait for it to be ready), and pauses it into a
checkpoint that frees its node resources. It offers `lifecycle.pause-resume`
and `lifecycle.pause-checkpoint`, but not snapshots or fork: a claimed pod is
already running, so nothing can be restored into it.

A warm pod has no snapshot group until it is claimed, so GKE never restores
it. The claim gives it its own group through `additionalPodMetadata`: the
claim controller copies that into the claimed Sandbox's pod template, and the
Sandbox controller onto the running pod, without restarting it. Pause and
resume then work as above, except that the claim, not the Sandbox, holds the
adapter's annotations and names the checkpoint to restore. The claim
controller rewrites the Sandbox's pod template from the claim on every pass,
so nothing else could keep it there. Resume waits until the Sandbox's pod
template names the checkpoint before it starts the pod.

agent-sandbox accepts claim labels only from an allowlist of domains
(default `sandbox.users.io`). Add `differently.dev`, keeping any domain
other clients use, and restart the controller, which reads the list once at
startup:

```yaml
apiVersion: v1
kind: ConfigMap
metadata: {name: agent-sandbox-config, namespace: agent-sandbox-system}
data:
  allowed-label-domains: |
    differently.dev
    sandbox.users.io
```

```sh
kubectl -n agent-sandbox-system rollout restart deploy/agent-sandbox-controller
```

Without that, every create fails with `invalid_request` and a message
naming the allowlist. The SandboxTemplate behind the warm pool must set
`runtimeClassName: gvisor`; the pool is unhealthy otherwise. Add these
rules to the Role (the adapter reads the template, and suspends and resumes
the claimed Sandbox):

```yaml
- apiGroups: [extensions.agents.x-k8s.io]
  resources: [sandboxtemplates]
  verbs: [get]
- apiGroups: [agents.x-k8s.io]
  resources: [sandboxes]
  verbs: [get, patch]
```

and configure the pool:

```yaml
  - name: gke-warm
    adapter: agent-sandbox
    trust: private
    config:
      namespace: sandboxes
      warmPool: python-warm              # its SandboxTemplate sets runtimeClassName: gvisor
      podSnapshotPolicy: differently
```

A paused sandbox's pod is gone, so a resume starts a new pod on whatever node
has room, and GKE restores it there if the node is of the same machine
series. The warm pool refills as soon as a pod is claimed, so paused
sandboxes do not use up its pods.

The claim and its Sandbox are two objects, so a pause or resume is two
patches. A pause writes the claim first and suspends the Sandbox last;
undoing a pause, or suspending a resume that was not restored, sets the
Sandbox's mode first. A failed patch therefore never leaves a sandbox
suspended without its claim saying so, or running unverified. If a paused
claim's Sandbox is deleted, the claim controller adopts another warm one; the
adapter follows it, and the next resumes restore the pause checkpoint into it
(the first finds the new pod running unrestored and suspends it). A claimed
Sandbox that cannot be read reports that sandbox `pending` without failing the
reconciler's list. agent-sandbox still honours a legacy
`agents.x-k8s.io/pod-name` annotation on old warm Sandboxes whose pod is named
differently; the adapter assumes the pod has the Sandbox's name, so a pause of
such a sandbox times out and is undone.

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
- Not wired: the sandbox router and `sandboxd`. Recording and replaying a real cluster's exchanges, as the
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
- **Live** (2026-10-28, `go test -tags live -run TestLiveAgentSandbox`; the
  test now also needs `-tags live,experimental_adapters`):
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
- **GKE Pod Snapshots** (`podsnapshot_test.go`, every `go test`): the fake
  API also plays GKE's snapshot controller: triggers that become ready
  PodSnapshots holding the pod's marker, group and pod spec, and restores
  that happen only when GKE's would (same group, same pod spec, a ready
  snapshot). The tests check the policy checks, snapshot, fork (the source's
  spec and group, the snapshot named, a deleted snapshot), both shapes of
  `status.snapshotCreated`, a failed checkpoint, two checkpoints at once,
  pause and resume twice with only the newest checkpoint kept, a pause whose
  suspend does not finish (undone, the sandbox running), a fork or resume
  that GKE did not restore, the limit on failed resumes, a source whose pod
  GKE recreates after its fork took a newer snapshot (it comes back from its
  own), a fork at another size, no stop/start, and the clean-up on delete and
  of a Sandbox deleted behind Differently's back. The fake restores as GKE
  documents it: the named snapshot, or else the newest in the pod's group.
  An independent review of the first version found the fork-group, rollback,
  failed-pause, endless-retry, marker-race, leak and fork-size issues these
  tests now cover; a second review of the fixes found three more (a
  cancelled pause undone, a transient exec error counted as a failed
  restore, an interrupted fork left in its source's group), also covered. `TestLivePodSnapshots` (live tag, with
  `AGENTSANDBOX_SNAPSHOT_POLICY`) checks a running process and a file through
  a fork and a pause/resume, that a new sandbox does not start from a
  snapshot, and logs each step's time. **It has not been run: nothing here
  has run on GKE.**
- **Warm pools with Pod Snapshots** (2026-11-20): the fake API's claim
  controller adopts a separate warm Sandbox whose pod starts without a
  group, checks claim labels against an allowlist, and rewrites the
  Sandbox's pod template from the claim. The tests check the claim's group,
  pause and resume twice through the claim, a resume GKE did not restore
  (counted on the claim), a paused
  sandbox's Sandbox replaced by the controller and restored into the new one,
  an unreadable Sandbox reported pending without failing the list, a claim
  controller that lags (a pause it never catches up with is undone; a
  resume does not start the pod before the Sandbox names the checkpoint),
  a failed claim patch that must not leave an unverified pod running, the
  template and policy checks, the allowlist message, and that snapshot and
  fork are refused. An independent review of the first version found the
  patch ordering, list, lag and re-adoption issues these tests now cover;
  each guard was removed once to see its test fail. Then, by hand,
  on agent-sandbox v1.0.12 on kind (Kubernetes 1.37, runc, a RuntimeClass
  named `gvisor` with the `runc` handler) with the Role above: a claim with
  the `differently.dev` label is refused (`InvalidMetadata`) until the
  domain is allowed; once allowed, the claim adopts a warm pod and the label
  reaches the running pod (same UID, no restart) in 0.2 s; a `ps-name`
  annotation set on the claim reaches the Sandbox's pod template at once, and
  its removal too; suspending the claimed Sandbox deletes the pod and is not
  undone by the claim controller (the claim reports `Ready=False`,
  `SandboxSuspended`); setting it running creates a new pod carrying the
  group label and `ps-name`, which is what GKE restores from. Finally the
  adapter itself, with stand-in `podsnapshot.gke.io` CRDs and a loop that
  marks each trigger's PodSnapshot ready: health, create in 110 ms, pause
  (the claimed pod checkpointed and deleted, `paused`), a resume that, with
  nothing restored, finds the marker missing and suspends the sandbox again
  (`paused` again 1.4 s later), and a delete that removed the claim, the
  trigger and the snapshot.
- **Not run:** GKE, Pod Snapshots (checkpoint and restore themselves), gVisor or Kata RuntimeClasses, a multi-node cluster,
  Kubernetes before 1.30, the in-cluster service account path (the live runs
  used a token and CA file), `ports.expose` fetched from outside the pod,
  and the Helm chart with these pools.
