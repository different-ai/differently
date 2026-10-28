# Acceptance scenarios

| | |
|---|---|
| Status | 2026-10-28. `go test -race ./...` passes locally, including the Postgres conformance suite (`internal/store/postgres` `TestConformance`) run against a disposable Postgres container; CI runs it on every PR. |
| Evidence level | The tests below run in-process with fake backends. In addition, the [end-to-end suite](../test/e2e/README.md) runs scenarios A, C, G, J and L, plus sandbox isolation, against real Firecracker microVMs (arm64, nested virtualization on Colima, 2026-10-28). Cloud and bare-metal KVM, x86_64 and live Daytona or Freestyle accounts are not validated yet. |

Scenario definitions follow the product brief. Stateful sandbox behaviour and
operator controls, which several scenarios rely on, are covered under G and K
respectively.

| | Scenario | Status | Main tests |
|---|---|---|---|
| A | Own-cloud quickstart | Covered in-process | `TestOwnCloudQuickstart` |
| B | Fully private deployment | Covered in-process | `TestFullyPrivateDeployment` |
| C | Multi-tenant quotas and fairness | Covered in-process | `TestMultiTenantQuotas`, store conformance |
| D | Hybrid overflow | Covered in-process | `TestHybridOverflowAndPrivateOnly`, `TestOverflowWhenPrivateFull` |
| E | Private-only data stays private | Covered in-process | `TestHybridOverflowAndPrivateOnly`, `TestPrivateOnlyNeverLeaves` |
| F | Provider substitution by policy | Covered in-process | `TestProviderSubstitution` |
| G | Existing infrastructure integration | Covered in-process | `TestPoolPlacesAndRoutesAcrossAgents`, `TestMicroVMLifecycleOverAdapterProtocol` |
| H | Scoped keys and tenant isolation | Covered in-process | `TestScopedKeyIsolation`, `TestProjectKeyIsContained` |
| I | Capability mismatch | Covered in-process | `TestCapabilityMismatch` (server and policy) |
| J | Failure recovery and reconciliation | Partially covered; known failure modes | `TestFailureRecoveryAndReconcile`, `TestRestartRecoversVMs` |
| K | Upgrade and portability | Partially covered | Postgres conformance (migrations), `TestRestartRecoversVMs`, `TestFailureRecoveryAndReconcile`, `TestProviderSubstitution` |
| L | Trust visibility | Covered in-process | `TestHybridOverflowAndPrivateOnly`, `TestVerifyDetectsTampering` |

The server tests share a harness (`newHarness` in
`internal/server/server_test.go`): the real HTTP server, scheduler and memory
store with two fake pools, `fc` (private, 4 CPU, 16 GiB) and `burst`
(external, unbounded, `maxConcurrency` 10, features exec, files and ports).

---

## A. Own-cloud quickstart

**Statement.** An operator installs Differently on infrastructure they
control, with no hosted service, creates an organization, a project and a
scoped key, and a developer runs a sandbox through its whole lifecycle.

**How it is satisfied.** The bootstrap token (or the dev-mode token file)
creates an operator key (`server.Bootstrap`); the admin API creates the
organization and default project; `POST /v1/api-keys` mints a project key;
the sandbox is admitted, placed on the private pool, provisioned, used for
exec, streamed exec and files, and deleted. No code path contacts a vendor
service (`telemetry: none`).

**Code path.** `cmd/differently-server` → `internal/server/server.go`
(`Bootstrap`, `authenticate`) → `admin.go` (`createOrg`) → `tenancy.go`
(`createKey`) → `sandboxes.go` (`createSandbox`, `exec`, `streamExec`,
`writeFile`, `readFile`, `deleteSandbox`) → `internal/scheduler` → adapter.

**Tests.** `TestOwnCloudQuickstart` (also checks that `envKeys` hide values
and that the audit chain verifies with at least six events);
`TestMicroVMLifecycleOverAdapterProtocol` (the agent path over HTTP with a fake
VMM and guest); `TestTokenRoundTrip`; `TestParseAppliesSafeDefaults`.

**Needs real-infrastructure validation.** Helm install on a cluster with KVM
nodes; real Firecracker boot with the project's kernel and rootfs; the guest
agent over real vsock; `differently-agent doctor` on real hosts; the
quickstart guide followed end to end by someone new to the project.

## B. Fully private deployment

**Statement.** With external placement disabled, no sandbox reaches an
external provider, whatever its classification or request.

**How it is satisfied.** The global external mode defaults to `never`
(`internal/config/config.go`); no lower level can raise it; external pools
must also be named in `externalPools`. Registering a provider authorizes
nothing.

**Code path.** `internal/policy/policy.go` `Merge`, `Decide`.

**Tests.** `TestFullyPrivateDeployment` (four `public` sandboxes, zero
reach the external fake); `TestUnapprovedExternalPoolIsNeverUsed`;
`TestParseAppliesSafeDefaults`.

**Needs real-infrastructure validation.** Network-level proof that the
control plane opens no connections to provider endpoints when external
placement is off (egress-restricted NetworkPolicy, packet capture); an
air-gapped install.

## C. Multi-tenant quotas and fairness

**Statement.** Per-tenant concurrency and queue limits hold under concurrent
load; a tenant at its limit queues instead of failing; one tenant's burst does
not starve another; released capacity lets queued work start.

**How it is satisfied.** Admission checks queue and size limits; activation
re-checks concurrency, CPU, memory and disk atomically; usage is derived from
state; dispatch uses fair ordering; termination nudges waiting work
([ADR 0005](adr/0005-quota-derived-from-state-atomic-activation.md),
[placement policy §11–12](scheduling/placement-policy.md#11-fairness)).

**Code path.** `internal/store/store.go` (`CheckAdmission`,
`CheckActivation`, `Tally`, `FairOrder`), `memory.go` and `postgres.go`
(`AdmitSandbox`, `ActivateSandbox`), `internal/scheduler` (`dispatch`,
`nudge`).

**Tests.** `TestMultiTenantQuotas` (queue limit returns `429
quota_exceeded`, concurrency limit queues, oversized request rejected with
`422`, deletion releases quota); store conformance
`ConcurrentAdmissionRespectsQueueLimit`, `ConcurrentActivationRespectsQuota`,
`PoolConcurrencyCap`, `FairOrderAcrossOrgs`, `Idempotency`.

Starting a stopped sandbox re-reserves CPU and memory atomically
(`ConcurrentStartRespectsCPUQuota` in the store conformance suite).

**Needs real-infrastructure validation.** Multiple replicas against one
database under load (benchmark B3); fairness with deep queues.

## D. Hybrid overflow

**Statement.** Work whose classification allows it goes to an explicitly
approved external pool only when private capacity cannot take it.

**How it is satisfied.** Effective mode `overflow` holds external candidates
while any eligible private pool can take the sandbox; the approved pool is
used once private pools are full. Worked example in
[placement-policy.md §13](scheduling/placement-policy.md#scenario-d-hybrid-overflow).

**Code path.** `policy.Decide` (overflow hold), `Scheduler.activate`.

**Tests.** `TestHybridOverflowAndPrivateOnly`; `TestOverflowWhenPrivateFull`;
`TestPrivateFirstPrefersPrivateCapacity`.

**Needs real-infrastructure validation.** Overflow to live Daytona and
Freestyle accounts ([POC 7](proof-of-concepts.md#poc-7-external-provider-live-account-verification)):
create, exec, files, delete, error mapping at plan limits, and reconciliation
of provider-side instances.

## E. Private-only data stays private

**Statement.** Work classified `private-only` never runs outside private
pools: it waits for private capacity, or is rejected if no private pool could
ever take it. Requests and narrower policies can opt out of external
placement but never opt in.

**How it is satisfied.** Classification ceilings and stricter-wins merging;
`waiting` versus `rejected` outcomes; fail-fast at admission.

**Code path.** `policy.Merge`, `policy.Decide`; `sandboxes.go` `admit`.

**Tests.** `TestHybridOverflowAndPrivateOnly` (private-only stays queued while
an `internal` sandbox overflows; per-request `external: never` stays queued);
`TestPrivateOnlyNeverLeaves`; `TestNarrowerScopesOnlyTighten`;
`TestDecisionExplainsEveryPool`.

**Needs real-infrastructure validation.** None specific beyond A and D; the
property is enforced before any backend is called.

## F. Provider substitution by policy

**Statement.** An operator moves new work to a different backend by changing
policy, without application changes.

**How it is satisfied.** Applications never name backends; placement reads the
current policies on every attempt. Running sandboxes are not migrated.

**Code path.** `tenancy.go` `putPolicy` → `Scheduler.policies` →
`policy.Decide`.

**Tests.** `TestProviderSubstitution` (global policy denies `fc` and allows
`burst`; the next sandbox lands on `burst`).

**Needs real-infrastructure validation.** Substitution between a real
Firecracker pool and a live provider, including the semantic differences
applications see (combined output on Daytona, exec time limits on Freestyle).

## G. Existing infrastructure integration

**Statement.** An operator registers an existing private cloud environment or
compatible Linux node pool and makes its capacity available through
Differently.

**How it is satisfied.**

- *Linux KVM hosts, in or out of Kubernetes:* install `differently-agent` as a
  DaemonSet (Helm) or as a systemd service on plain hosts
  (`deploy/systemd/`), pointing it at a `firecracker` pool. Agents
  self-register through the heartbeat endpoint; the pool aggregates their
  capacity, places sandboxes on the roomiest fitting node and routes every
  operation to the right node.
- *Any other existing VM or sandbox system:* implement the adapter protocol
  (`pkg/adapter/adapterhttp`) in front of it and register it as a `remote`
  pool, declaring its trust boundary. The node agent itself speaks this
  protocol, so the same contract is exercised by the Firecracker path.
- Pools are added in config or at runtime (`POST /v1/admin/pools`), with
  credentials by reference only, and become placeable only as policy allows.

**Code path.** `internal/server/tenancy.go` (`agentHeartbeat`),
`internal/adapters/firecracker/pool.go`, `internal/wiring/wiring.go`
(`remote`), `pkg/adapter/adapterhttp`, `internal/server/admin.go`
(`createPool`).

**Tests.** `TestPoolPlacesAndRoutesAcrossAgents` (two nodes aggregated,
stale node excluded, placement on the roomier node, routing, restore pinned to
the snapshot's node, partition guard); `TestMicroVMLifecycleOverAdapterProtocol`
(the full adapter protocol over HTTP against the node agent, including token
auth); `TestConfigureAndStart` (Firecracker API calls).

**Known gaps.** No InfrastructureAdapter that provisions or autoscales nodes;
the agent channel is unencrypted until mTLS (backlog E3). Agent identity
pinning is covered by `TestAgentIdentityIsPinned`.

**Needs real-infrastructure validation.** Registering real KVM hosts (EKS
metal or nested virtualization, bare-metal k3s/RKE2, systemd hosts) and a
remote plugin in front of an existing VM pool.

## H. Scoped keys and tenant isolation

**Statement.** A project key reaches only its own project's resources, cannot
escalate, and cannot see other tenants or operator configuration.

**How it is satisfied.** Key tenancy and scopes; 404 for other tenants'
resources; no-escalation grants; admin routes require `admin`; the tenant pool
view omits configuration and credential references.

**Code path.** `internal/auth/auth.go` (`CanAccess`, `Has`, `CheckGrant`),
`sandboxes.go` (`resolveProject`, `loadSandbox`), `admin.go` (`adminRoutes`),
`tenancy.go` (`capabilities`).

**Tests.** `TestScopedKeyIsolation` (cross-tenant read is 404,
cross-project create is 403, minting `admin` is 403, admin API is 403,
capabilities view has no credentials, bad key is 401);
`TestProjectKeyIsContained`; `TestOrgAdminAndOperator`; `TestTokenRoundTrip`.

`TestScopedKeyIsolation` also checks that an organization admin cannot change
or delete another tenant's project policy.

**Needs real-infrastructure validation.** An external security review of the
API, including the agent channel (shared registration token, per-agent secrets, no encryption yet).

## I. Capability mismatch

**Statement.** A request for a capability no permitted backend offers fails
with a precise error; if another authorized backend offers it, that backend is
used; operations a backend lacks fail precisely after placement.

**How it is satisfied.** Capability filtering with an
`unsupported_capability` outcome; `EffectiveFeatures`; `501
unsupported_operation` for lifecycle calls a pool cannot perform. Worked
example in [placement-policy.md §13](scheduling/placement-policy.md#scenario-i-capability-mismatch).

**Code path.** `policy.Decide` (`capabilityOnly`), `pkg/adapter`
(`EffectiveFeatures`), `sandboxes.go` (`unsupported`, `lifecycle`).

**Tests.** `TestCapabilityMismatch` in `internal/server` (`gpu` →
`422 unsupported_capability`; pause on a backend without it →
`501 unsupported_operation`) and in `internal/policy` (routing `ports.expose`
to the capable external pool).

**Needs real-infrastructure validation.** Feature lists of real agents and
live providers (for example Daytona VM class versus container class).

## J. Failure recovery and reconciliation

**Statement.** Provider failures are retried safely without duplicate
instances; orphaned and lost instances are reconciled; quota stays correct;
crashes during provisioning recover.

**How it is satisfied.** Idempotent `Create`; retry with backoff and attempt
limits; the reconciler deletes orphans, fails lost sandboxes, adopts or
requeues interrupted provisioning; quota derived from state; agent restart
recovery.

**Code path.** `internal/scheduler/scheduler.go` (`retry`, `Reconcile`,
`reconcilePool`), `internal/agent/agent.go` (`recover`).

**Tests.** `TestFailureRecoveryAndReconcile` (an injected create failure
requeues with one attempt; retry converges to exactly one instance; an
injected orphan is deleted; a vanished instance fails its sandbox; usage
returns to zero); `TestRestartRecoversVMs`; idempotent create in
`TestMicroVMLifecycleOverAdapterProtocol`.

**Known gaps.** A node silent for longer than the 10-minute partition grace is
treated as lost; instance state drift (a stopped VM under a `running` sandbox)
is not reconciled
([backlog E0](backlog.md#e0-correctness-and-security-fixes-from-the-code-review)).
The scheduler package has no unit tests of its own; it is exercised through
the server tests.

**Needs real-infrastructure validation.** Chaos tests C1–C12 in
[reliability-and-performance.md](reliability-and-performance.md#5-chaos-tests),
especially leader loss against Postgres, agent partitions and container
restarts.

## K. Upgrade and portability

**Statement.** An operator upgrades Differently or replaces a runtime adapter
using documented procedures without losing management of existing workloads.

**How it is satisfied.**

- All control-plane state lives in Postgres. API replicas are stateless and
  one scheduler leader is elected with a session advisory lock, so replicas can
  be rolled one at a time.
- Migrations are idempotent and serialized by an advisory lock
  (`differently-server migrate`, run by the Helm pre-upgrade hook).
- Interrupted provisioning is adopted or retried by the reconciler; backend
  `Create` is idempotent on the sandbox ID.
- Node agents persist VM records; after an agent upgrade VMs come back as
  stopped sandboxes restartable from their retained disks (the chart uses the
  `OnDelete` update strategy so operators drain nodes first).
- Replacing a backend: drain the old pool (`POST /v1/admin/pools/{name}/drain`),
  register the new one, and change policy; existing sandboxes stay managed on
  the drained pool while new work goes to the new one, with no application
  change (Scenario F). Running state is never migrated between backends.
- Operator controls used during these procedures — suspend a tenant, drain or
  disable a pool, terminate any sandbox — are enforced immediately
  (`TestSuspendOrganization`, `TestDrainingAndAllowedPools`).

**Code path.** `internal/store/postgres` (`Migrate`, `TryLeader`),
`internal/scheduler` (`Reconcile`), `internal/agent/agent.go` (`recover`),
`internal/server/admin.go` (`drainPool`, `suspendOrg`, `forceTerminate`),
[day-2 operations](deploy/day-2.md).

**Tests.** Postgres conformance suite (migrations applied from scratch per
test); `TestRestartRecoversVMs`; `TestFailureRecoveryAndReconcile`;
`TestProviderSubstitution`; `TestSuspendOrganization`;
`TestDrainingAndAllowedPools`.

**Known gaps.** Agent upgrades stop the node's microVMs (no live agent
upgrade); the adapter client does not yet check the protocol version; no
version-to-version upgrade test exists yet.

**Needs real-infrastructure validation.** A Helm upgrade across two released
versions with running sandboxes; Postgres failover; agent rollout with node
drain.

## L. Trust visibility

**Statement.** An operator can see where workloads may go, where they went,
why, who changed the rules, whether the audit log was altered, and what
Differently cannot prove.

**How it is satisfied.** The trust report, per-sandbox placement
explanations, `sandbox.placed` audit events and audit chain verification. The
field-by-field mapping is in
[trust-model.md §8](security/trust-model.md#8-how-the-trust-report-answers-scenario-l).

**Code path.** `admin.go` (`trustReport`, `verifyAudit`), `sandboxes.go`
(`explainPlacement`), `internal/audit/audit.go`.

**Tests.** `TestHybridOverflowAndPrivateOnly` (placement explanation shows
external trust and a decision; the trust report lists exactly the one external
placement, non-empty limitations and a valid chain);
`TestDecisionExplainsEveryPool`; `TestVerifyDetectsTampering`; store
conformance `AuditChain`; `TestOwnCloudQuickstart` (verify endpoint).

**Known gaps.** The report reads every sandbox and the whole audit chain on
each call, which will need paging at scale.

**Needs real-infrastructure validation.** Anchoring the audit mirror in
write-once storage and verifying a chain after a Postgres failover.

---

## Other tests

| Test | Covers |
|---|---|
| `TestOpenAPICoversRoutes` | Every server route appears in `api/openapi.yaml` |
| `TestDaytonaAdapter`, `TestDaytonaErrorMapping` | Daytona adapter against a fake server built from the published spec |
| `TestFreestyleAdapter` | Freestyle adapter against a fake server built from the published spec |
| `TestConfigureAndStart` | Firecracker API client call sequence |
| `TestRunnerExecAndFiles`, `TestRunnerTimeoutKillsProcessGroup`, `TestClientOverHandler` | Guest agent |
| `TestEgressDeny` | Egress-deny rules and subnet calculation |
| `TestCostStrategyAndCeiling` | Cost strategy and cost ceiling |
| `TestValidateReportsProblems`, `TestCredentialRef` | Config validation and credential references |
| `TestNewIsSortableAndUnique` | ID generation |
