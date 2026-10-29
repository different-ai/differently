# Acceptance scenarios

| | |
|---|---|
| Status | 2026-10-28, after [ADR 0020](adr/0020-opensandbox-replaces-node-agent.md) removed the Firecracker node agent. Every test named below exists on this branch. CI runs the Go tests, including the Postgres conformance suite (`internal/store/postgres` `TestConformance`) in its `postgres conformance` job. |
| Evidence level | The server tests run in-process with fake backends. The `opensandbox` adapter is also checked against a real OpenSandbox server (release 1.2.0, Docker runtime, arm64 on Colima) by `TestLiveOpenSandbox`. The [end-to-end suite](../test/e2e/README.md) runs the real control plane (Postgres and `differently-server`) against a real OpenSandbox server with the Docker runtime and passed 10/10 on arm64 Colima on 2026-10-28. Earlier end-to-end results on real Firecracker microVMs tested the removed agent and no longer apply. OpenSandbox's Kubernetes and Fast Sandbox runtimes, cloud KVM, and live Daytona or Freestyle accounts are not validated. |

Scenario definitions follow the product brief. Stateful sandbox behaviour and
operator controls, which several scenarios rely on, are covered under J and K
respectively.

| | Scenario | Status | Main tests |
|---|---|---|---|
| A | Own-cloud quickstart | Covered in-process; adapter checked live (Docker runtime) | `TestOwnCloudQuickstart`, `TestLiveOpenSandbox` |
| B | Fully private deployment | Covered in-process | `TestFullyPrivateDeployment`, `TestUnapprovedExternalPoolIsNeverUsed` |
| C | Multi-tenant quotas and fairness | Covered in-process | `TestMultiTenantQuotas`, store conformance |
| D | Hybrid overflow | Covered in-process | `TestHybridOverflowAndPrivateOnly`, `TestOverflowWhenPrivateFull` |
| E | Private-only data stays private | Covered in-process | `TestHybridOverflowAndPrivateOnly`, `TestPrivateOnlyNeverLeaves` |
| F | Provider substitution by policy | Covered in-process | `TestProviderSubstitution` |
| G | Register an existing OpenSandbox server or remote plugin as a pool | Partially covered; a `remote` pool registered through the API with a stored key is tested, runtime OpenSandbox registration is not | `TestOpenSandboxCredentialsAndConfig`, `TestOpenSandboxLifecycleExecFiles`, `TestLiveOpenSandbox`, `TestStoredProviderSecretReachesBackendAndRotatesLive` |
| H | Scoped keys and tenant isolation | Covered in-process | `TestScopedKeyIsolation`, `TestProjectKeyIsContained` |
| I | Capability mismatch | Covered in-process | `TestCapabilityMismatch` (server and policy), `TestOpenSandboxLifecycleExecFiles` |
| J | Failure recovery and reconciliation | Covered in-process; backend outages untested | `TestFailureRecoveryAndReconcile`, `TestIdempotentCreateAndSnapshots` |
| K | Upgrades and replacing a backend | Partially covered | Postgres conformance (migrations), `TestDrainingAndAllowedPools`, `TestProviderSubstitution`, `TestSuspendOrganization`, `TestUsePoolSwitchesWhereNewSandboxesGo` |
| L | Trust visibility | Covered in-process | `TestHybridOverflowAndPrivateOnly`, `TestVerifyDetectsTampering`, `TestAuditFailClosed` |

The server tests share a harness (`newHarness` in
`internal/server/server_test.go`): the real HTTP server, scheduler and memory
store with two fake pools, `fc` (private, 4 CPU, 16 GiB; the name is
historical) and `burst` (external, unbounded, `maxConcurrency` 10, features
exec, files and ports).

---

## A. Own-cloud quickstart

**Statement.** An operator installs Differently on infrastructure they
control, with no hosted service, creates an organization, a project and a
scoped key, and a developer runs a sandbox through its whole lifecycle.

**How it is satisfied.** The operator installs OpenSandbox with its own
tooling and Differently's chart pointing at it (the chart's `opensandbox:`
values render a private pool named `opensandbox`). The bootstrap token (or the
dev-mode token file) creates an operator key (`server.Bootstrap`); the admin
API creates the organization and default project; `POST /v1/api-keys` mints a
project key; the sandbox is admitted, placed on the private pool, provisioned,
used for exec, streamed exec and files, and deleted. No code path contacts a
vendor service (`telemetry: none`).

**Code path.** `cmd/differently-server` → `internal/server/server.go`
(`Bootstrap`, `authenticate`) → `admin.go` (`createOrg`) → `tenancy.go`
(`createKey`) → `sandboxes.go` (`createSandbox`, `exec`, `streamExec`,
`writeFile`, `readFile`, `deleteSandbox`) → `internal/scheduler` → adapter
(`internal/adapters/opensandbox` for the default private pool).

**Tests.** `TestOwnCloudQuickstart` (also checks that `envKeys` hide values
and that the audit chain verifies with at least six events);
`TestLiveOpenSandbox` (create, idempotent create, exec with sandbox and
per-command env, stderr, exit codes, argument lists, cwd, binary files,
listing, pause and resume, snapshot, delete against a real OpenSandbox
server); `TestTokenRoundTrip`; `TestParseAppliesSafeDefaults`.

**Needs real-infrastructure validation.** The end-to-end suite on
OpenSandbox's Kubernetes and Fast Sandbox runtimes; a Helm install on a cluster with
OpenSandbox installed from its own chart; the quickstart guide followed end
to end by someone new to the project.

## B. Fully private deployment

**Statement.** With external placement disabled, no sandbox reaches an
external provider, whatever its classification or request.

**How it is satisfied.** The global external mode defaults to `never`
(`internal/config/config.go`); no lower level can raise it; external pools
must also be named in `externalPools`. Registering a provider authorizes
nothing. Daytona and Freestyle pools are always `external`; an `opensandbox`
pool must declare its trust explicitly.

**Code path.** `internal/policy/policy.go` `Merge`, `Decide`;
`internal/config/config.go` `applyDefaults`, `Validate`.

**Tests.** `TestFullyPrivateDeployment` (four `public` sandboxes, zero
reach the external fake); `TestUnapprovedExternalPoolIsNeverUsed`;
`TestParseAppliesSafeDefaults` (a Daytona pool cannot claim private trust);
`TestOpenSandboxCredentialsAndConfig` (an OpenSandbox pool without `trust` is
rejected).

**Known gaps.** The trust of `opensandbox` and `remote` pools is the
operator's declaration. An OpenSandbox server run by someone else but
declared `private` defeats this scenario, and Differently cannot detect it.

**Needs real-infrastructure validation.** Network-level proof that the
control plane opens no connections to provider endpoints when external
placement is off (the chart's NetworkPolicy, packet capture); an air-gapped
install with OpenSandbox images mirrored.

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
`PoolConcurrencyCap`, `FairOrderAcrossOrgs`, `Idempotency`,
`ConcurrentStartRespectsCPUQuota` (starting a stopped sandbox re-reserves
CPU and memory atomically, on backends with stop/start).

**Known gaps.** Quota counts the requested `diskGiB`, but the `opensandbox`
adapter does not send a disk size to OpenSandbox.

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

**Known gaps.** OpenSandbox pools report no capacity, so a private
OpenSandbox pool is "full" only when it reaches `maxConcurrency` or turns
unhealthy. Set `maxConcurrency` for overflow to happen.

**Needs real-infrastructure validation.** Overflow from a real OpenSandbox
pool to live Daytona and Freestyle accounts
([POC 7](proof-of-concepts.md#poc-7-external-provider-live-account-verification)):
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
property is enforced before any backend is called. It holds only as far as
pool trust declarations are true (see B).

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
OpenSandbox pool and a live provider, including the semantic differences
applications see (no stop/start or exec stdin on OpenSandbox, combined output
on Daytona, exec time limits on Freestyle).

## G. Register an existing OpenSandbox server or remote plugin as a pool

**Statement.** An operator registers an existing OpenSandbox server, or
another sandbox system behind a remote plugin, as a pool and makes its
capacity available through Differently.

**How it is satisfied.**

- *An OpenSandbox server:* declare an `opensandbox` pool in the config file,
  through the Helm chart's `opensandbox:` values, or at runtime with
  `POST /v1/admin/pools`. It needs `config.apiURL` (the lifecycle API, ending
  in `/v1`), an explicit `trust` (`private` if you operate the server and its
  runtime, `external` if someone else does; configuration without it is
  rejected), optionally `config.isolation` (`container` or `microvm`), and the
  server's API key as a credential reference (`credentials.env`,
  `credentials.file`, or `credentials.secret` for a key stored in
  Differently). OpenSandbox has one API key per server and no tenant
  model, so one server is one trust domain and one pool.
- *Any other existing VM or sandbox system:* implement the adapter protocol
  (`pkg/adapter/adapterhttp`) in front of it and register a `remote` pool with
  `config.endpoint` and a token by reference. A `remote` pool defaults to
  `trust: private`; declare `external` if it fronts a third-party service.
- Pools registered through the API are validated by the same parser as the
  config file, keep credentials as references (a `secret:` reference names a
  value stored sealed, never returned), are audited
  (`pool.create` with adapter, trust and credential reference) and become
  placeable only as policy allows. Pools declared in the config file cannot
  be deleted through the API.

**Code path.** `internal/server/admin.go` (`createPool`, `validatePool`),
`internal/config/config.go` (`Validate`, `privateByDefaultKinds`),
`internal/wiring/wiring.go` (`opensandbox`, `remote`),
`internal/adapters/opensandbox`, `pkg/adapter/adapterhttp`.

**Tests.** `TestNewAdapterKindsMustDeclareTrust` (an adapter kind not
listed as external or private by default is rejected until its pool declares
trust); `TestOpenSandboxCredentialsAndConfig` (missing `trust` and
`apiURL` rejected, unknown isolation rejected, features from config, a
rejected API key reported as `provider_unavailable` without echoing it);
`TestOpenSandboxLifecycleExecFiles` (the adapter against a fake OpenSandbox
server, including the spec-versus-image differences found in the spike);
`TestLiveOpenSandbox` (against a real server); `TestCredentialRef`;
`TestValidateReportsProblems`;
`TestStoredProviderSecretReachesBackendAndRotatesLive`. That test registers a
`remote` pool through the API with a stored key, and checks that the key
reaches the plugin, is never returned, and rotates without a restart.

**Known gaps.**

- No test calls `POST /v1/admin/pools`.
- `pkg/adapter/adapterhttp`, the plugin protocol behind `remote` pools, has no
  tests: the node-agent tests were its only coverage
  ([backlog E0.34](backlog.md#e0-correctness-and-security-fixes-from-the-code-review)).
- The adapter client does not check the protocol version (E0.25).
- No InfrastructureAdapter provisions or autoscales the backend.

**Needs real-infrastructure validation.** Registering OpenSandbox servers
with the Kubernetes and Fast Sandbox runtimes
([POC 11](proof-of-concepts.md#poc-11-fast-sandbox-through-the-adapter),
[POC 12](proof-of-concepts.md#poc-12-opensandbox-kubernetes-runtime)); a remote
plugin in front of an existing VM pool.

## H. Scoped keys and tenant isolation

**Statement.** A project key reaches only its own project's resources, cannot
escalate, and cannot see other tenants or operator configuration.

**How it is satisfied.** Key tenancy and scopes; 404 for other tenants'
resources; no-escalation grants; admin routes require `admin`; the tenant pool
view omits configuration and credential references. Backend credentials such
as the OpenSandbox API key are never handed to tenants: the adapter refuses
port endpoints that would need it.

**Code path.** `internal/auth/auth.go` (`CanAccess`, `Has`, `CheckGrant`),
`sandboxes.go` (`resolveProject`, `loadSandbox`), `admin.go` (`adminRoutes`),
`tenancy.go` (`capabilities`), `opensandbox.go` (`ExposePort`).

**Tests.** `TestScopedKeyIsolation` (cross-tenant read is 404,
cross-project create is 403, minting `admin` is 403, admin API is 403,
capabilities view has no credentials, bad key is 401, an organization admin
cannot change or delete another tenant's project policy);
`TestProjectKeyIsContained`; `TestOrgAdminAndOperator`; `TestTokenRoundTrip`;
`TestOpenSandboxLifecycleExecFiles` (header-guarded endpoints are not handed
out).

**Needs real-infrastructure validation.** An external security review of the
API and of the link between the control plane and OpenSandbox (one
server-wide API key held by the control plane).

## I. Capability mismatch

**Statement.** A request for a capability no permitted backend offers fails
with a precise error; if another authorized backend offers it, that backend is
used; operations a backend lacks fail precisely after placement.

**How it is satisfied.** Capability filtering with an
`unsupported_capability` outcome; `EffectiveFeatures`; `501
unsupported_operation` for calls a pool cannot perform. On OpenSandbox pools
that includes `stop`, `start`, exec with stdin, and memory snapshots unless
the pool declares `snapshots.memory`. Worked example in
[placement-policy.md §13](scheduling/placement-policy.md#scenario-i-capability-mismatch).

**Code path.** `policy.Decide` (`capabilityOnly`), `pkg/adapter`
(`EffectiveFeatures`), `sandboxes.go` (`unsupported`, `lifecycle`),
`opensandbox.go` (`run`, `Snapshot`, `ExposePort`).

**Tests.** `TestCapabilityMismatch` in `internal/server` (`gpu` →
`422 unsupported_capability`; pause on a backend without it →
`501 unsupported_operation`) and in `internal/policy` (routing `ports.expose`
to the capable external pool); `TestOpenSandboxLifecycleExecFiles` (stdin
unsupported, memory snapshot refused without the feature).

**Needs real-infrastructure validation.** Feature lists of OpenSandbox
runtimes other than Docker and of live providers (for example Daytona VM class
versus container class).

## J. Failure recovery and reconciliation

**Statement.** Provider failures are retried safely without duplicate
instances; orphaned and lost instances are reconciled; quota stays correct;
crashes during provisioning recover.

**How it is satisfied.** Idempotent `Create` (the `opensandbox` adapter looks
up `differently-sandbox-id` metadata before creating); retry with backoff and
attempt limits; the reconciler deletes orphans, fails lost sandboxes, follows
instance state, adopts or requeues interrupted provisioning, and skips a pool
whose `List` fails rather than acting on a partial view; quota derived from
state.

**Code path.** `internal/scheduler/scheduler.go` (`retry`, `Reconcile`,
`reconcilePool`, `reconcileState`), `opensandbox.go` (`Create`, `List`).

**Tests.** `TestFailureRecoveryAndReconcile` (an injected create failure
requeues with one attempt; retry converges to exactly one instance; an
instance that stops behind the control plane's back makes the sandbox
`stopped`; an injected orphan is deleted; a vanished instance fails its
sandbox; usage returns to zero); `TestIdempotentCreateAndSnapshots`
(`Idempotency-Key` replay); idempotent create in
`TestOpenSandboxLifecycleExecFiles` and `TestLiveOpenSandbox`.

**Known gaps.** The scheduler package has no unit tests of its own; it is
exercised through the server tests. A terminating sandbox whose backend is
gone retries forever
([backlog E0.22](backlog.md#e0-correctness-and-security-fixes-from-the-code-review)).
Behaviour across OpenSandbox outages and restarts is untested.

**Needs real-infrastructure validation.** Chaos tests C1–C12 in
[reliability-and-performance.md](reliability-and-performance.md#5-chaos-tests),
especially leader loss against Postgres and OpenSandbox outages
([POC 13](proof-of-concepts.md#poc-13-opensandbox-outage-and-recovery)).

## K. Upgrades and replacing a backend

**Statement.** An operator upgrades Differently, upgrades a backend, or
replaces one backend with another using documented procedures, without losing
management of existing workloads and without application changes.

**How it is satisfied.**

- *Upgrading Differently.* All control-plane state lives in Postgres. API
  replicas are stateless and one scheduler leader is elected with a session
  advisory lock, so replicas can be rolled one at a time. Migrations are
  idempotent and serialized by an advisory lock (`differently-server migrate`,
  run by the Helm chart's migrate Job); migration `0002_drop_agents.sql`
  removes the old agents table. Interrupted provisioning is adopted or retried
  by the reconciler; backend `Create` is idempotent on the sandbox ID.
- *Upgrading OpenSandbox.* OpenSandbox is installed and upgraded with its own
  tooling. Pin its server and `execd` versions and run `TestLiveOpenSandbox`
  (and the end-to-end suite) against the new version before upgrading, because
  its published spec runs ahead of its default images
  ([ADR 0019](adr/0019-opensandbox-backend-first.md),
  [backlog E21](backlog.md#e21-opensandbox-version-pinning-and-upgrade-testing)).
- *Replacing a backend.* Register the new pool, drain the old one
  (`POST /v1/admin/pools/{name}/drain`) and change policy if needed; existing
  sandboxes stay managed on the drained pool while new work goes to the new
  one, with no application change (Scenario F). Running state is never
  migrated between backends. Moving from the removed `firecracker` pool kind
  to OpenSandbox (ADR 0020) follows this procedure, with one difference: the
  new server no longer knows the `firecracker` kind. A config file naming it
  is rejected, and a pool of that kind registered through the API stays in
  the store with no adapter, so its sandboxes can be neither reached nor
  terminated (E0.22). End those sandboxes and remove the pool before
  upgrading.
- Operator controls used during these procedures — suspend a tenant, drain or
  disable a pool, terminate any sandbox — are enforced immediately.

**Code path.** `internal/store/postgres` (`Migrate`, `TryLeader`),
`internal/scheduler` (`Reconcile`), `internal/server/admin.go` (`drainPool`,
`updatePool`, `suspendOrg`, `forceTerminate`), `internal/policy` (draining
pools excluded), [day-2 operations](deploy/day-2.md).

**Tests.** Postgres conformance suite (migrations applied from scratch per
test); `TestDrainingAndAllowedPools` (a draining pool takes no new work);
`TestProviderSubstitution`; `TestSuspendOrganization`;
`TestFailureRecoveryAndReconcile`.

**Known gaps.** No version-to-version upgrade test exists for Differently or
for OpenSandbox, and no CI job runs the live OpenSandbox test; the adapter
client does not check the plugin protocol version; whether OpenSandbox keeps
running sandboxes across its own upgrades is unverified (POC 13).

**Needs real-infrastructure validation.** A Helm upgrade across two released
versions with running sandboxes; an in-place OpenSandbox upgrade with running
sandboxes; Postgres failover.

## L. Trust visibility

**Statement.** An operator can see where workloads may go, where they went,
why, who changed the rules, whether the audit log was altered, and what
Differently cannot prove.

**How it is satisfied.** The trust report (pools with their trust, declared
isolation, endpoint and credential reference; placements; recent admin
actions; the limitations list), per-sandbox placement explanations,
`sandbox.placed` audit events and audit chain verification. The limitations
state that Differently does not isolate sandboxes itself: isolation and
egress come from each pool's runtime. The field-by-field mapping is in
[trust-model.md §8](security/trust-model.md#8-how-the-trust-report-answers-scenario-l).

**Code path.** `admin.go` (`trustReport`, `limitations`, `verifyAudit`),
`sandboxes.go` (`explainPlacement`), `internal/audit/audit.go`.

**Tests.** `TestHybridOverflowAndPrivateOnly` (placement explanation shows
external trust and a decision; the trust report lists exactly the one external
placement, non-empty limitations and a valid chain);
`TestDecisionExplainsEveryPool`; `TestVerifyDetectsTampering`; store
conformance `AuditChain`; `TestOwnCloudQuickstart` (verify endpoint);
`TestAuditFailClosed` (the fail-open limitation appears only when
`audit.failClosed` is off).

**Known gaps.** The report reads every sandbox and the whole audit chain on
each call, which will need paging at scale. Declared isolation is what the
pool says, not something Differently checks.

**Needs real-infrastructure validation.** Anchoring the audit mirror in
write-once storage and verifying a chain after a Postgres failover. The
manual OpenSandbox run showed the trust report with the pool's endpoint and
only a reference to its credential ([spike](research/opensandbox-spike.md)).

---

## Other tests

| Test | Covers |
|---|---|
| `TestOpenAPICoversRoutes` | Every server route appears in `api/openapi.yaml` |
| `TestOpenSandboxLifecycleExecFiles`, `TestOpenSandboxCredentialsAndConfig` | OpenSandbox adapter against a fake server that reproduces the deployed images' behaviour |
| `TestLiveOpenSandbox` (`-tags live`) | OpenSandbox adapter against a real server ([test/opensandbox](../test/opensandbox/README.md)) |
| `TestDaytonaAdapter`, `TestDaytonaErrorMapping` | Daytona adapter against a fake server built from the published spec |
| `TestFreestyleAdapter` | Freestyle adapter against a fake server built from the published spec |
| `TestLifecycleAndSnapshotRestore` | `devhost` adapter (dev mode only) |
| `TestRunnerExecAndFiles`, `TestRunnerTimeoutKillsProcessGroup` | `internal/localexec`, the command runner `devhost` uses |
| `TestMetricsCanLeaveTheAPIListener` | `/metrics` off the API listener with `metrics.listen` |
| `TestCostStrategyAndCeiling` | Cost strategy and cost ceiling |
| `TestValidateReportsProblems`, `TestCredentialRef` | Config validation and credential references |
| `TestRotateAPIKey` | API key rotation: grace period, immediate revocation, scope and tenancy checks |
| `TestSealOpenBindsName`, `TestLoad` | Sealing stored provider credentials and loading the secrets key |
| `TestNewIsSortableAndUnique` | ID generation |
