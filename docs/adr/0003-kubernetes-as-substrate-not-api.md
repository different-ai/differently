# ADR 0003: Kubernetes is a deployment substrate, not the sandbox API

- Status: Accepted
- Date: 2026-10-28

## Context

Most target operators already run Kubernetes, and a Helm chart is the expected
way to install infrastructure software. It is tempting to go further and make
the sandbox itself a Kubernetes object (a CRD with a controller), as
kubernetes-sigs/agent-sandbox does. But:

- Placement must span private pools and external providers. A CRD reconciled
  inside one cluster cannot place work on Daytona or Freestyle without
  becoming a second control plane anyway.
- Interactive sandboxes need sub-second admission with quota re-checked
  atomically; a CRD round trip through the API server, a controller queue and
  etcd adds latency and a second source of truth.
- Operators without Kubernetes (bare-metal hosts, small labs) should be able to
  run the same software.
- Application developers should not need Kubernetes credentials to create a
  sandbox.

## Decision

Kubernetes is used to **deploy and run** Differently: the control-plane
`Deployment`, migration `Job`, optional embedded Postgres, the node agent
`DaemonSet` on `differently.dev/kvm=true` nodes, `NetworkPolicy` and `Secret`
objects (`deploy/helm/differently`). The sandbox API is Differently's own
REST API (`/v1`, `api/openapi.yaml`), state lives in Postgres, and no CRDs or
operators are installed. Both binaries run without Kubernetes.

## Consequences

- One API and one policy engine regardless of where sandboxes run.
- No Kubernetes RBAC integration for sandbox access; Differently has its own
  keys and scopes (`internal/auth`).
- kube-scheduler does not see microVMs. VM memory is not in pod requests, so
  KVM nodes must be tainted for agents and the agent's host reserve sized
  correctly. The chart sets no memory limit on the agent because VM memory may
  be charged to its cgroup.
- GitOps users manage Differently's configuration (pools, policies,
  classifications) through the config file in a ConfigMap and the admin API,
  not through custom resources.
- A Kubernetes-native batch lane (Kueue) remains possible as an optional
  adapter ([architecture §13](../architecture/README.md#13-why-kueue-and-kubernetes-jobs-are-not-the-mvp-sandbox-abstraction)).

## Alternatives considered

- **Sandbox CRD and controller.** Rejected for the latency, cross-provider and
  "Kubernetes required" reasons above.
- **agent-sandbox as the API.** It delegates isolation to a RuntimeClass and
  its memory snapshots depend on GKE Pod Snapshots; it would tie Differently to
  pod semantics and still not route across providers.
- **No Kubernetes support at all.** Rejected: the Helm path is how most target
  operators will install it.
