# Differently documentation

Start with the [project README](../README.md) for what Differently is and a
two-minute local quickstart.

## Use it

| Guide | For |
|---|---|
| [Quickstart](guides/quickstart.md) | Laptop dev mode, first sandbox, SDK snippets |
| [Organization onboarding](guides/organization-onboarding.md) | One installation per organization vs shared; orgs, projects, quotas, scoped keys, policies |
| [Hybrid overflow](guides/hybrid-overflow.md) | Authorizing Daytona or Freestyle as policy-gated external capacity |
| [OpenSandbox pools](guides/opensandbox-pools.md) | The default private runtime: registering an OpenSandbox server you (or someone else) run |
| [API reference](../api/openapi.yaml) | OpenAPI 3.1 contract (also served at `/openapi.yaml`) |
| SDKs | [TypeScript](../sdk/typescript/README.md), [Python](../sdk/python/README.md), [Go](../pkg/client/client.go) |

## Deploy and operate it

| Guide | For |
|---|---|
| [Deployment overview and support matrix](deploy/README.md) | What is tested, documented or unverified |
| [Prerequisites](deploy/prerequisites.md) | Cluster, Postgres and an OpenSandbox server (or another backend) |
| [Kubernetes (existing cluster)](deploy/kubernetes.md) | Helm install to first sandbox |
| [AWS](deploy/aws.md) · [GCP](deploy/gcp.md) · [Azure](deploy/azure.md) · [Bare metal](deploy/bare-metal.md) | Environment-specific notes |
| [Air-gapped](deploy/air-gapped.md) | Mirroring artifacts, no-egress operation |
| [Day-2 operations](deploy/day-2.md) | Upgrades, migrations, backups, drains, uninstall |
| [Hardening checklist](security/hardening-checklist.md) | Before production |

## Understand it

| Document | Contents |
|---|---|
| [Feature parity](feature-parity.md) | Differently vs Boat, Daytona, Freestyle, E2B, OpenSandbox; backend capability matrix |
| [Competitive landscape](competitive-landscape.md) | Ownership models and where Differently fits |
| [OpenSandbox spike](research/opensandbox-spike.md) | What OpenSandbox offers as a backend or foundation, observed and read |
| [Architecture RFC](architecture/README.md) | Layers, components, request lifecycle, state machine, HA |
| [Decision records](adr/) | Why each major choice was made and what was rejected ([ADR 0020](adr/0020-opensandbox-replaces-node-agent.md): OpenSandbox replaces the node agent) |
| [Security and trust model](security/trust-model.md) | Threats, boundaries, enforced controls, residual risks |
| [Adapter specification](adapters/specification.md) | The backend contract and the HTTP plugin protocol |
| [Placement policy](scheduling/placement-policy.md) | Merge rules, strategies, overflow, quotas, fairness |
| [Storage and lifecycle](storage-and-lifecycle.md) | Persistence, snapshots, fork, portability limits |
| [Reliability and performance](reliability-and-performance.md) | SLOs, benchmarks to run, chaos tests |
| [Acceptance scenarios](acceptance-scenarios.md) | Scenarios A–L and the tests that cover them |
| [End-to-end suite](../test/e2e/README.md) | The real stack against an OpenSandbox server, and where it was verified |

## Plan it

| Document | Contents |
|---|---|
| [Roadmap](roadmap.md) | Phases 0–4 with current status |
| [Backlog](backlog.md) | Epics and tasks with acceptance criteria |
| [Proofs of concept](proof-of-concepts.md) | Risks and the experiments that retire them |
