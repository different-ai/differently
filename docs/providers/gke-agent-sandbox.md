# GKE Agent Sandbox

| | |
|---|---|
| Status | **Tested with a fake server**: the experimental `agent-sandbox` adapter drives it, with snapshots, fork and pause/resume through GKE Pod Snapshots. Not run on GKE |
| Adapter | `agent-sandbox` (`internal/adapters/agentsandbox`), build tag `experimental_adapters` |
| Trust | declared (self-hostable): the sandboxes run in your own GKE cluster |
| Isolation | gVisor through a `RuntimeClass` (Kata is possible upstream but "not a Google Cloud product") |
| API checked | [Agent Sandbox on GKE](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/agent-sandbox), [install](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/how-install-agent-sandbox), [Pod snapshots](https://docs.cloud.google.com/kubernetes-engine/docs/how-to/agent-sandbox-pod-snapshots) and [kubernetes-sigs/agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) v1.0.8, read 2026-10-28 |

Agent Sandbox is a Kubernetes controller, generally available as a managed
GKE add-on since 2026-05-20 (`--enable-agent-sandbox`). It has no Google HTTP
API of its own: sandboxes are Kubernetes objects. Commands run through an
HTTP server inside the sandbox image, not through the controller.

## Capabilities

| Feature | What it offers |
|---|---|
| exec | Through the in-image runtime: the legacy Python runtime's `POST /execute {command}` returns `{stdout, stderr, exit_code, timed_out}`; the newer `sandboxd` streams processes over gRPC on port 9090. The `sandbox-router` proxies by `X-Sandbox-ID` and port |
| exec.stream | with `sandboxd` (gRPC) |
| files | `POST /upload`, `GET /download/{path}` (legacy), or `PUT`/`GET v1/files/{path}` (`sandboxd`) |
| stdin | depends on the runtime image |
| lifecycle.stop-start | `operatingMode: Suspended` keeps the object and its volumes |
| lifecycle.pause-resume | with GKE Pod Snapshots |
| snapshots.disk / snapshots.memory | GKE Pod Snapshots (`podsnapshot.gke.io/v1`), GA on 1.35.3-gke.1234000 or later |
| fork | from a Pod snapshot |
| ports.expose | the Sandbox's `service`; exposure is up to you |
| max size | the Pod's resources |
| provider TTL backstop | `shutdownTime` with `shutdownPolicy: Delete` |

## Configure

GKE's add-on installs the same controller as
[kubernetes-sigs/agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox),
so the [`agent-sandbox` adapter](agent-sandbox.md) drives it: Sandboxes and
SandboxClaims through the Kubernetes API, commands through `pods/exec`. Set
`runtimeClassName: gvisor` and `podSnapshotPolicy` on the pool for memory
snapshots, fork and a pause that frees the pod
([set it up](agent-sandbox.md#snapshots-fork-and-pauseresume-on-gke)).

Which agent-sandbox version GKE's add-on runs, and whether its API matches
v1.0.8's `v1beta1` that the adapter was written against, is **unverified**.
The adapter does not use the sandbox router or `sandboxd`.

OpenSandbox's Kubernetes runtime on GKE, behind the built-in `opensandbox`
adapter, remains the supported path for private Kubernetes capacity in the
default build ([OpenSandbox pools](../guides/opensandbox-pools.md)).

## Limits and differences

- In v1beta1 a `SandboxClaim` must reference a `SandboxWarmPool` (use
  `replicas: 0` for cold starts). The Pod snapshots page still shows
  v1alpha1.
- The exec contract depends on the runtime image, so a plugin has to pin one
  (the Python runtime or `sandboxd`).
- GKE Agent Substrate (limited GA since 2026-09-11) is a related Kubernetes
  layer and would be the same kind of plugin.
- Pricing: your GKE cluster's nodes.

## How it was tested

The adapter's GKE Pod Snapshots support is tested against a fake API server
that plays GKE's snapshot controller
([how](agent-sandbox.md#how-it-was-tested)). Nothing has run on GKE.
