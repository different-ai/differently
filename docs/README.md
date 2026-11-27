# Differently documentation

New here? Read [Differently explained](explainer.md) for what it is in plain
words, then run it with the [quickstart](guides/quickstart.md): three
commands, no account or cluster. Working through a coding agent? Point it
at [Use Differently with a coding agent](guides/agents.md).

## Use it

| Guide | For |
|---|---|
| [Differently explained](explainer.md) | What it is, the five words to know, what you need to start and what can wait |
| [Quickstart](guides/quickstart.md) | Install, a local server, the first sandbox, SDK snippets, the step up to a real backend |
| [One baseline on every provider](guides/baselines.md) | One template name for each provider's own image or snapshot; what stays on its pool (sandboxes, snapshots) and how to see what each provider has and can do |
| [Questions people ask](faq.md) | Short answers on snapshots, switching providers, outages, trust and readiness |
| [Use Differently with a coding agent](guides/agents.md) | A prompt to paste, and step-by-step instructions written for the agent (also served as `/agents.txt`) |
| [Organization onboarding](guides/organization-onboarding.md) | One installation per organization vs shared; orgs, projects, quotas, scoped keys, policies |
| [Hybrid overflow](guides/hybrid-overflow.md) | Authorizing Daytona or Freestyle as policy-gated external capacity |
| [OpenWork on Differently](guides/openwork.md) | OpenWork's sandbox contract mapped onto Differently; Workbot computers routed across OpenSandbox, Daytona and Freestyle |
| [Keys and providers](guides/keys-and-providers.md) | Storing provider keys in Differently, switching providers for new sandboxes, rotating provider and API keys, the bootstrap token, backing up the secrets key |
| [OpenSandbox pools](guides/opensandbox-pools.md) | The default private runtime: registering an OpenSandbox server you (or someone else) run |
| [Admin console](../console/README.md) | The separate admin app: static files on any host, or on Node with Better Auth sign-in; CORS and reaching a private Differently |
| [Provider catalog](providers/README.md) | Every backend Differently can place sandboxes on, with its tested status, capabilities and configuration, and how to add one |
| [API reference](../api/openapi.yaml) | OpenAPI 3.1 contract (also served at `/openapi.yaml`) |
| SDKs | [TypeScript](../sdk/typescript/README.md), [Python](../sdk/python/README.md), [Go](../pkg/client/client.go) |

## Deploy and operate it

| Guide | For |
|---|---|
| [Deployment overview and support matrix](deploy/README.md) | Where to run it, and what is tested, documented or unverified |
| [One Docker host](deploy/docker.md) | The real stack from published images: one downloaded Compose file, then a first sandbox in a container |
| [One Linux VM on any cloud](deploy/single-vm.md) | The quickest team setup: k3s, OpenSandbox and Differently on Azure, AWS, Google Cloud or bare metal |
| [Azure VMs serving OpenWork](deploy/azure-vms.md) | A verified two-VM install with Fast Sandbox microVMs, managed Postgres, a registry and Key Vault, and Freestyle and Daytona as external pools |
| [Google Cloud VMs](deploy/gcp-vms.md) | The same two-VM layout on Compute Engine with Cloud SQL, Artifact Registry and Secret Manager (documented, not run) |
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
| [Fast Sandbox pause time](research/fast-sandbox-pause-time.md) | Where a desktop's pause spends its time, what cut it, and the upstream patches for the rest |
| [Architecture RFC](architecture/README.md) | Layers, components, request lifecycle, state machine, HA |
| [Security and trust model](security/trust-model.md) | Threats, boundaries, enforced controls, residual risks |
| [Adapter specification](adapters/specification.md) | The backend contract and the HTTP plugin protocol |
| [Store drivers](adapters/stores.md) | Where Differently keeps its own records, the drivers in a build and the contract for adding one |
| [Maintaining and improving Differently](maintaining.md) | For agents and maintainers: where to change what, the verification ladder, recorded runtimes, performance, traps |
| [Placement policy](scheduling/placement-policy.md) | Merge rules, strategies, overflow, quotas, fairness |
| [Capacity](scheduling/capacity.md) | Pool slots, paused sandboxes that give theirs back, the resume queue, preemption and lifecycle limits, with a model of real usage |
| [Storage and lifecycle](storage-and-lifecycle.md) | Persistence, snapshots, fork, portability limits |
| [Reliability and performance](reliability-and-performance.md) | SLOs, benchmarks to run, chaos tests |
| [Sandbox acid test](acid-test.md) | One incremental test for every sandbox provider, to publish real numbers and file issues upstream; levels 0–2 run today |
| [Acceptance scenarios](acceptance-scenarios.md) | Scenarios A–L and the tests that cover them |
| [End-to-end suite](../test/e2e/README.md) | The real stack against an OpenSandbox server, and where it was verified |

## Plan it

| Document | Contents |
|---|---|
| [Roadmap](roadmap.md) | Phases 0–4 with current status |
| [Backlog](backlog.md) | Epics and tasks with acceptance criteria |
| [Proofs of concept](proof-of-concepts.md) | Risks and the experiments that retire them |
