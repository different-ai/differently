---
title: Features
description: What OpenSandbox provides, what Differently adds on top, and how that compares to using a single provider.
---

Differently is an orchestration and resource-management layer. It does not run sandboxes itself: it decides where each one runs and keeps account of it. Its default private runtime is [OpenSandbox](https://github.com/opensandbox-group/OpenSandbox), run by the operator; other pools can be any of the [built-in provider adapters](/differently/docs/providers/), from E2B and Daytona to the cloud providers' sandbox services, or any service that speaks the [adapter protocol](/differently/docs/adapters/specification/).

:::caution[Pre-release]
The end-to-end suite passes 14/14 against real OpenSandbox servers on the Docker runtime and on the Kubernetes runtime (single-node k3s). Fast Sandbox (microVM) ran live on one node, without the end-to-end suite yet; multi-node and managed clusters, and live accounts on managed providers, are not verified yet. See the [roadmap](/differently/docs/roadmap/).
:::

## What OpenSandbox provides

These come from the runtime. Differently exposes them through its own API when the pool behind a sandbox supports them.

| Feature | Notes |
|---|---|
| Runtimes | Docker, Kubernetes, and Fast Sandbox (Firecracker microVMs managed by a per-node agent) |
| Lifecycle | Create and delete everywhere. Pause and resume keep memory on Docker and Fast Sandbox (a Fast Sandbox pause uploads a full checkpoint and takes minutes); stop and start keep the disk on Kubernetes, through a snapshot registry |
| Exec | Commands with env, cwd, timeouts, exit codes and streamed output; stdin is staged in a file the command reads |
| Files | Upload, download and list |
| Snapshots and fork | Disk snapshots and fork on Docker, on Fast Sandbox, and on Kubernetes through a snapshot registry; memory snapshots are not exposed yet |
| Port URLs and egress | Signed, time-limited port URLs through its ingress gateway, and per-pool egress (`none` or an allowlist) through its egress sidecar, both checked on Kubernetes |
| TTL backstop | OpenSandbox's own timeout is set from the sandbox TTL at creation |
| Isolation | Containers on Docker and Kubernetes (shared kernel); a microVM per sandbox only on Fast Sandbox |
| License | Apache-2.0, self-hostable |

Details, including what OpenSandbox does not offer: [OpenSandbox pools](/differently/docs/guides/opensandbox-pools/) and the [OpenSandbox spike](/differently/docs/research/opensandbox-spike/).

## What Differently adds

OpenSandbox is a single-trust-domain runtime: one server-wide API key, no organizations, projects or quotas. Differently supplies the platform around it.

| Feature | What it does |
|---|---|
| Multi-tenancy | Organizations, projects and scoped API keys; the OpenSandbox key stays in the control plane and never reaches tenants or sandboxes |
| Quotas and fair queueing | Usage derived from sandbox state, enforced atomically at admission, queued fairly across tenants |
| Policy-gated placement | Global, organization and project policies plus data classification decide which pools may run each sandbox; a request can only narrow them |
| Fail-closed external capacity | Registering an external provider authorizes nothing until policy explicitly allows it |
| One API across backends | OpenSandbox pools, 15 more [provider adapters](/differently/docs/providers/) and custom plugins behind one HTTP contract, CLI and TypeScript, Python and Go SDKs |
| Idle auto-stop | Sandboxes left idle are stopped or paused and wake on the next call |
| Placement explanations | `GET /v1/sandboxes/{id}/placement` lists the pools considered, the ones rejected and why |
| Tamper-evident audit | Hash-chained audit events with a verify endpoint and JSON-lines export |
| Trust report | Endpoints, credentials by reference, external placements, admin actions, audit chain status and what Differently cannot prove |
| Reconciliation | The control plane reconciles its state against every backend |
| Self-hosted end to end | No hosted service, license server or telemetry; Postgres is the only production dependency |

## Compared to a single provider

Using one sandbox provider or one runtime directly is simpler to start with. These are the limits Differently is built to remove.

| | Single provider or runtime | Differently |
|---|---|---|
| Where sandboxes can run | One runtime or one vendor's cloud | Every pool you register, chosen per request |
| When that pool is full or down | Wait, fail, or change application code | Queue under quota, or overflow to another pool where policy allows |
| Switching providers | New SDK, new API, code changes | Change placement policy; application code stays the same |
| Who may use external capacity | Anyone holding the key | Only the tenants, classifications and requests policy allows |
| Tenancy and quotas | Per vendor account, or none (OpenSandbox: one key) | Organizations, projects, scoped keys and quotas across all pools |
| Data residency | Wherever the provider runs | `private-only` classification keeps work on your own pools |
| Knowing where work ran | Each provider's own logs | One placement record, audit chain and trust report |
| Lock-in | The provider's API and pricing | Apache-2.0 control plane you run, adapters you can write |

What Differently does not do: it does not enforce isolation or egress itself. Those belong to the runtime behind each pool, and Differently reports what each pool declares. For the endpoint-by-endpoint comparison with other products, see [feature parity](/differently/docs/feature-parity/) and the [competitive landscape](/differently/docs/competitive-landscape/).
