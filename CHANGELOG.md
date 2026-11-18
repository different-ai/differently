# Changelog

All notable changes are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased] — on `pre-release`

### Added

- The answer to "don't I need my snapshot on every provider?", in the
  product and in the docs. Each backend boots its own images, so the
  baseline has to exist on each provider; a template gives those baselines
  one name (`templates[].images`, which existed but was only described in
  the OpenWork guide).
  - **`differently templates`** shows each template and what every pool
    boots for it, with a dash where a pool has none.
  - **`differently capabilities`** now puts the pools side by side, one
    feature per row, so what a provider cannot do is a dash next to the
    others' yes. `-o json` prints the API response as before.
  - **`--from-snapshot`** on `differently sandbox create` and
    `differently run` starts a sandbox from a snapshot. The API and the SDKs
    already could.
  - **Refusals say what is missing.** A request for a template that no
    permitted pool has fails with "no permitted pool has template <name>"
    (detail `template`), and a restore or fork that the snapshot's pool
    cannot take names that pool (detail `pool`). Both keep the code
    `no_eligible_pool`; before, both said only that no pool satisfied the
    policy.
  - [One baseline on every provider](docs/guides/baselines.md): what is
    shared across providers (the name) and what is not (the image, a running
    sandbox, a snapshot), the three steps, and a two-pool example that runs
    in dev mode.
  - [Questions people ask](docs/faq.md): short answers on snapshots,
    switching providers, outages, trust and readiness, each saying where
    Differently stops. The landing page shows six of them.

- A first sandbox takes three commands and no toolchain, account, cluster or
  database: install, `differently-server --dev`, `differently run -- echo hello`.
  - **`install.sh`**, served at
    `https://different-ai.github.io/differently/install.sh`. It downloads the
    release archive for the machine (macOS or Linux, amd64 or arm64), checks
    it against the release's `checksums.txt`, and puts `differently` and
    `differently-server` in `~/.local/bin` without `sudo`.
    `DIFFERENTLY_VERSION`, `DIFFERENTLY_INSTALL_DIR` and
    `DIFFERENTLY_DOWNLOAD_URL` (for a mirror) change what it installs, where
    to, and where from. `test/install` runs it against a release served
    locally; it has not been run against the GitHub release itself.
  - **`differently run -- COMMAND`** creates a sandbox, waits for it, runs
    the command with its output streamed, deletes the sandbox and exits with
    the command's code. `--keep` leaves the sandbox. It is deleted on a
    failure or an interrupt too.
  - **The SDKs find a local dev server by themselves**, as the CLI already
    did. With no key set and a URL on this machine, the TypeScript and Python
    clients read `~/.differently/dev/token` and use `prj_default`, so
    `new Differently()` and `sandboxes.create()` need no arguments. The token
    is never read for another host. Both gain a default project
    (`projectId` / `project_id`, or `DIFFERENTLY_PROJECT`) for organization
    and operator keys.
  - Dev mode prints its address, the token's path and the next command to
    type, and the CLI says how to start a server when nothing answers on
    this machine.
- On a real installation a first sandbox takes three commands after reading
  the bootstrap operator token, and no ID is copied from one into the next:
  `differently login`, `differently admin org create NAME`,
  `differently run -- echo hello`. Only the CLI changed: organization and
  operator keys must still name a project on every create, and the server
  creates no tenant by itself.
  - **`differently admin org create NAME`** prints the IDs of the
    organization and its first project in a short table instead of raw JSON
    (`-o json` prints the response as before). When the CLI has no default
    project (none saved, no `--project`, no `DIFFERENTLY_PROJECT`, no local
    dev server), it saves the new one as the default and says so. It never
    replaces a saved project, and it does not write into a config saved for
    another server or into one it cannot parse.
  - **A saved project belongs to one server.** The CLI uses it only with the
    server it was saved for (the URL `login` saved, or the one
    `admin org create` ran against), so it is never sent to another
    installation and never hides a local dev server's `prj_default`. Saving
    a project does not change which server the CLI talks to, and a key from
    the environment is never written.
  - **`differently login`** with a key that is not limited to one project
    saves the only project the key can see. With none it says which command
    makes one; with several it lists them and saves none
    (`differently login --project ID` saves a chosen one). A project saved
    before is kept only while the key can still see it, and the dev
    server's `prj_default` is no longer saved when logging in to another
    server on the same machine.
  - **`differently key create NAME`** without `--org` and `--key-project`
    limits a key that holds only `sandboxes:` scopes (the default) to the
    default project and looks up its organization, so
    `differently key create dev` works after the commands above. A key with
    any other scope (`projects:write`, `keys:write`, `org:admin`, `admin`,
    ...) is not narrowed, since the server refuses a project-limited key to
    create or delete projects; `--key-project` limits it. `--key-project`
    alone looks up the organization too, and `--org` alone still makes a
    key for the whole organization. The output says where the new key
    works, and when that is the default project.
  - A create refused because no project was named now says how to get a
    default project.
  - The Helm chart's notes, the [Kubernetes](docs/deploy/kubernetes.md) and
    [single-VM](docs/deploy/single-vm.md) guides and
    [organization onboarding](docs/guides/organization-onboarding.md) show
    this sequence instead of `curl` with `jq` or ID placeholders.

  Run by hand against a server started from a config file without `--dev`
  (memory store, one `remote` pool); not run on a cluster.
- Entry points for people and for agents.
  - [Differently explained](docs/explainer.md): what it is in plain words,
    and what a newcomer can ignore until later.
  - [Use Differently with a coding agent](docs/guides/agents.md): a prompt to
    paste, and tasks written for the agent with a check after each step. The
    website serves it as `/agents.txt`.
  - The website serves `/llms.txt`, an index of the documentation, and every
    docs page as Markdown at its address plus `index.md`. A link in them to a
    page the site does not have fails the build.
  - The README, the quickstart and the landing page open with the same three
    commands; the landing page no longer opens with `helm install`.

- The load simulator at `/simulator/` shows how routing is set up and lets
  the visitor configure more of it.
  - **Priority.** A Policy tab walks through who picks the provider, the rule
    (strategy and external gate) and the priority for the next sandbox. That
    list is the placement policy's own ranking, asked several times a second
    for one more sandbox, with the policy's reason for each provider it holds
    back; badges on the provider cards match it, and a fold-out shows the
    configuration that produces it.
  - **Quotas.** The tenant's `maxActiveSandboxes` and `maxQueued`, set
    through the admin API: over the first a sandbox stays queued, over the
    second a create is refused.
  - **Audit.** The page can show every audit entry with its sequence number
    and hash, and asks the control plane to verify the chain. The words
    policy, quotas and audit in the diagram open these tabs.
  - **Providers.** Any provider of the comparison can be added while the
    simulation runs, and each can be removed. Adding one says nothing about
    whether Differently has an adapter for it: every pool is the fake
    adapter.
  - **Lifecycle.** Starts and lengths are random, and a run begins empty. A
    chosen share of sandboxes is kept after its session: it hibernates when
    idle (a pause, a snapshot where the pool takes one), is woken by its
    client's next session, and is deleted by `maxPausedSeconds`. What a
    snapshot, a wake and a cold start take is set on the page.
  - **Timeline.** A run lasts one, four or eight simulated hours or has no
    limit; the bar shows the simulated time, the progress and what has been
    spent at list price (running time only: storage for hibernated
    sandboxes, minimums and plans are not counted).

  The sidebar is a scenario pick list and the load. `internal/loadsim` gains
  `AddProvider`, `SetQuota`, `VerifyAudit`, a keep share in the load and the
  policy's ranking in each snapshot. The WebAssembly build is about 5 MB
  compressed.

### Changed

- `differently capabilities` prints a table by default. Scripts that read
  its JSON need `-o json`.
- `differently admin org create` prints a table by default. Scripts that
  read its JSON need `-o json`.
- `differently key create` without `--org` and `--key-project` limits a
  key that holds only `sandboxes:` scopes to the CLI's default project when
  there is one. An organization key with a default project used to get an
  organization-wide key; pass `--org` for that. A key with any other scope
  is made as before.
- A project saved by `differently login` is used only with the URL that
  login saved. With `--url` or `DIFFERENTLY_URL` pointing somewhere else it
  is no longer sent.
- `docs/storage-and-lifecycle.md` said the `templates` list was
  informational. It routes: a template with `images` is placed only on the
  pools it maps.

### Fixed (documentation)

- The guides no longer contradict each other about what has been verified.
  Several pages still said that only OpenSandbox's Docker runtime had run
  with Differently, or that the Helm chart had never been installed on a
  cluster, while others recorded the Kubernetes and Fast Sandbox runs of
  2026-10-28 and 29. They now say the same thing: all three runtimes have
  run; the Kubernetes and Fast Sandbox runs and the installs on a cluster
  have not been repeated since 2026-10-29, and no published release after
  0.1.0-alpha.1 is recorded as installed on a cluster; and isolation inside
  a microVM has not been probed.
- End-to-end counts say what they count. "14/14" was every test the suite
  had on 2026-10-29. The suite has grown since, and CI runs it on the Docker
  runtime on every pull request; `test/e2e/README.md` keeps the current
  count.
- Corrected values: the default OpenSandbox URL points at namespace
  `opensandbox-system`, not `opensandbox`; the chart's NetworkPolicy allows
  OpenSandbox on port 80, so a standalone server on 8090 needs
  `networkPolicy.server.egress.opensandbox.port` set; the adapter was checked
  with `execd` `release-1.2.0` and `v1.1.0`; and a checkout's chart is
  version 0.3.0 while a release packages it as `differently-<version>.tgz`.
  The comment in `values.yaml` lists the config keys the chart really writes
  and refuses.

## [0.1.0-alpha.5] — 2026-11-17

Fifth alpha, published as a GitHub pre-release. Not for production. It starts
making Differently the place to see a whole deployment, not only its
sandboxes: it can watch the services around them (an OpenWork installation's
Den, Gateway, Workbot and runner have their health routes built in), and the
Helm chart can run the admin console with sign-in. The console also manages
each pool's prices, budget and attestations, the experimental AWS and Google
Cloud adapters take their credentials from the platform, and the website
gains a provider comparison and a load simulator.

What ran for this release: the unit and in-process tests, the Postgres
conformance and acceptance job, the end-to-end suite on OpenSandbox's Docker
runtime and the container image builds (now including the console image), in
CI on each pull request (#98 to #109). Stack health was also run by hand
against a local server with a stand-in for OpenWork's health routes, and the
console's server was run from its image's layout against a local server.

What has not run: stack probes against a live OpenWork installation; the
chart with `console.enabled` on a cluster; the live adapter tests against
Daytona and Freestyle accounts and the Kubernetes and Fast Sandbox runtimes,
which last ran on earlier code (see `Verified` under 0.1.0-alpha.2); and the
AWS and Google Cloud credential sources against those clouds.

Upgrading from 0.1.0-alpha.4: the Postgres store gains migration
`0005_stacks` (one new table), applied by `differently-server migrate` as the
chart already runs it; like every migration it is forward only. Nothing else
has to change. The audit log gains the actions `stack.put`, `stack.delete`
and `stack.health`, and `/metrics` gains `differently_stack_component_up`.
The console in the chart is off unless `console.enabled` is set, and this is
the first release with a `differently-console` image.

### Added

- The Helm chart can run the admin console with sign-in. `console.enabled`
  with `console.url` and `console.allowedEmails` adds one console pod, a
  volume for its accounts, a generated session secret kept across upgrades, a
  Service, an Ingress for the console's host and a NetworkPolicy that lets
  the pod reach only DNS and the Differently server. The console calls
  Differently inside the cluster with the bootstrap operator token, or with a
  key from `console.apiKey.existingSecret`. A new image,
  `ghcr.io/different-ai/differently-console` (`build/console.Dockerfile`), is
  built in CI and published with releases. Off by default. The templates are
  linted and rendered and the console's server was run from the image's
  layout against a local Differently; the chart with the console has not yet
  been installed on a cluster.
- Differently can watch the services around the sandboxes. A *stack*
  (`PUT /v1/admin/stacks/{name}`) names a deployment's services and where
  each answers for its health; with kind `openwork`, the roles `den-api`,
  `den-web`, `gateway`, `den-gateway`, `workbot` and `headless-runner` fill in
  OpenWork's own health and readiness routes. Every replica probes each
  component about every 10 seconds and reports `healthy`, `degraded` (up but
  not ready), `unavailable` or `unknown`, with the version the service
  states and how long it took to answer. A status that holds for two probes
  is written to the audit log as `stack.health`, once, by the leader, and
  `differently_stack_component_up` exports it for alerts. The console has a
  **Stacks** page that shows each stack beside the sandbox pools, and both
  SDKs have `admin.stacks`. Probes do not follow redirects and may carry a
  bearer token from a credential reference. This only reads: Differently does
  not deploy or configure these services, and their health never affects
  placement. Tested against stand-in servers; not yet run against a live
  OpenWork installation. The Postgres store gains migration `0005_stacks`.
- The experimental AWS and Google Cloud adapters take their credentials from
  the platform, as `azure-sandboxes` already did with a federated token.
  `aws-agentcore` accepts `{"source": "web-identity"}` (IAM Roles for
  Service Accounts), `{"source": "pod-identity"}` (EKS Pod Identity) or
  `{"source": "instance-role"}` (the EC2 instance profile) and renews the
  keys itself. `gcp-sandboxes` accepts `{"type": "metadata_server"}` (GKE
  Workload Identity, Compute Engine) or a workload identity federation
  configuration that reads a token file. `gcp-sandboxes` also advertises
  `network.egress-control` when its template has internet access off, and
  then refuses a request for a template that allows it. Tested against
  fakes only; nothing ran against AWS or Google Cloud. AWS still has no
  sandbox API with stop and start, snapshots or ports
  ([AWS](docs/providers/aws-agentcore.md),
  [Google Cloud](docs/providers/gcp-sandboxes.md)).
- The console's Providers page manages what each pool charges and may spend.
  Every pool shows its prices, this month's estimated spend against its
  budget and its attestations; Manage sets the prices, a monthly budget, the
  attestations, the concurrency limit and whether the pool is enabled. The
  page carries the provider catalog from the website's comparison (list
  prices read on 16 November 2026, how each provider bills, what it documents
  and states), so a pool can take its provider's list price in one click and
  record a certification the provider states, and a provider in the default
  build can be added from the catalog with a price and a budget. Spend is
  still the estimate from the pool's prices, not an invoice; for providers
  that bill on use the list rates give an upper bound; a recorded attestation
  is the operator's declaration, which Differently does not verify. Checked
  in a browser against a local server with simulated pools (the fake
  adapter); no real provider account was involved.
- Both SDKs can set a pool's `budget`, `attestations` and `preemption` when
  updating it (`admin.pools.update`), and the Python SDK can set `budget` and
  `attestations` when creating one. The server already accepted them.

- The website has a provider comparison at `/compare/`: 27 sandbox services
  and runtimes side by side, with list prices normalised to one sandbox of
  1 vCPU and 2 GiB for an hour, isolation, state, snapshots, self-hosting,
  what each states about SOC 2, HIPAA and ISO 27001, and how far Differently
  supports each. Prices were read on 16 November 2026 from the pages the
  table links to; nothing was benchmarked and no audit report was read. The
  data is one file, `site/src/data/providers.ts`, and the page carries Open
  Graph and Twitter metadata with a share image
  (`node scripts/compare-og.mjs` in `site/` redraws it). The page opens with
  the eight places providers differ, each explained in a box with a count
  taken from the data, and has a feature grid with one mark for each provider
  and capability: documented, with a condition, not offered or not
  documented, or not collected. A mark is a reading of the provider's
  documentation, not a test. Sections arrive with a short animation that is
  off under `prefers-reduced-motion`.
- The website has a load simulator at `/simulator/`. It runs the control
  plane itself in the visitor's browser: the HTTP API, admission, the
  placement policy, the scheduler and the in-memory store, compiled to
  WebAssembly, with simulated clients. The page fills the window and is built
  around the console's live routing diagram: your apps, Differently, and five
  pools named after providers (OpenSandbox as the private cluster, Daytona,
  Freestyle, E2B and Northflank) with their logos and list prices from the
  comparison's data, a dot for every slot and a packet for every placement.
  While it runs the visitor can turn a provider on or off, change its slots
  or make its backend fail, change the strategy and the external gate, and
  switch from placement by policy to a split by percentage, where each create
  is pinned to a provider with `placement.allowedPools`. Provider and policy
  changes go through the admin API. Clients run tasks (a sandbox per task,
  deleted after) or keep one long-lived computer that pauses and resumes.
  Every pool is the fake adapter with delays and the clock is sped up (it
  stands still while the program computes), so the page shows routing and
  scheduling decisions and says nothing about a provider or about
  performance. The program is `internal/loadsim` (tested natively), built for
  the browser by `cmd/differently-loadsim`; the site build compiles it
  (3.8 MB compressed) and the `site` workflow also runs when `internal/**`,
  `pkg/**` or `go.mod` change. `make vet` vets the browser build. The landing
  page links to it from the hero and shows the diagram in a section of its
  own.

### Fixed

- Idle preemption no longer pauses a sandbox as it starts. A create records
  its request as activity, so a sandbox that had waited in the queue longer
  than the pool's `idleSeconds` counted as idle the moment it ran, and was
  the first one paused to make room for the next waiting sandbox, before its
  caller's first command. Idle time now counts from the last call or from
  when the sandbox started running, whichever is later. Found with simulated
  load on a full private pool with no overflow: 26 placements for 19 sessions
  before, 22 for 22 after.

## [0.1.0-alpha.4] — 2026-11-17

Fourth alpha, published as a GitHub pre-release. Not for production. It adds
three controls for an operator who places work on several providers:
placement can require provider attestations (a SOC 2 Type II report, a signed
BAA and the like), a pool can carry a monthly budget, and the console shows
routing as it happens and can start a batch of sandboxes on chosen pools.

What ran for this release: the unit and in-process tests, the Postgres
conformance and acceptance job and the end-to-end suite on OpenSandbox's
Docker runtime, in CI on each pull request that added these features (#92 to
#95). The console's live view and launcher were also run by hand against a
local server with simulated pools (the fake adapter).

What has not run: the live adapter tests against Daytona and Freestyle
accounts, and the Kubernetes and Fast Sandbox runtimes. They last ran on
earlier code (see `Verified` under 0.1.0-alpha.2). Neither attestations nor
budgets have been exercised against a real provider. Attestations are
declarations Differently does not verify, and a pool's spend is an estimate
from its cost model, not a provider invoice.

Upgrading from 0.1.0-alpha.3: nothing has to change, and there is no database
migration (pools and policies are stored as documents). The new fields are
all optional: `attestations`, `budget` and `spend` on pools,
`requiredAttestations` on classifications, policies and requests, and
`attestations` in `GET /v1/capabilities`. A consumer of the audit log will
see one new action, `sandbox.rejected`, for create requests that placement
refuses.

### Added

- Placement filters on provider attestations. A pool can declare what
  whoever runs it is certified for or has signed (`attestations`, by name:
  `soc2-type2`, `iso27001`, `hipaa-baa`, ...), each with an evidence
  reference and an optional `validUntil`. A classification, a policy at any
  scope or a single request lists `requiredAttestations`; the requirements
  add up and a pool that lacks one, or holds an expired one, is ruled out
  with the reason `missing attestations: ...`. Private pools are not exempt.
  With no qualifying pool the create call answers `422 no_eligible_pool`
  and names what was required. Available in the config file, the Helm
  chart, `POST`/`PATCH /v1/admin/pools`, the CLI (`pool add --attestation`,
  `sandbox create --require-attestation`), the SDK types, the placement
  explanation, `GET /v1/capabilities` (names only) and the trust report.
  Differently enforces these declarations and does not verify them.
- A monthly budget per pool (`budget: {monthly: N}`, in the cost model's
  currency). Once the pool's estimated spend for the calendar month reaches
  it, the pool takes no new sandboxes: work goes to the next ranked pool, an
  overflow policy may use an approved external pool, and work with nowhere
  else to go waits. Running sandboxes are not stopped. The estimate prices
  this month's run time with the pool's cost model; it is not a provider
  invoice, and a budget on a pool without a cost model is refused. Pools
  report `spend` (`month`, `estimated`, `budgetReached`). Available in the
  config file, the Helm chart, `POST`/`PATCH /v1/admin/pools`, the CLI
  (`pool add --cost ... --monthly-budget`, `pool budget NAME AMOUNT`), the
  SDK types and the OpenAPI document. The per-tenant `poolBudgetsPerHour`
  quota field is unchanged and still not enforced.
- A **Live routing** page in the console. It draws your apps, Differently and
  every pool the way the intro video does, from live data: sandboxes on each
  pool, placements in the last five minutes, the external gate from the
  global policy, budgets and attestations, a running decision log, and the
  sandboxes that are waiting with the reason for each. Packets move as
  sandboxes are placed, and a click on a decision shows every pool that was
  considered and why. It polls the public API every two seconds and needs a
  key that can read the audit log.
- **Launch sandboxes** from the console's Live routing page: set a count per
  pool, choose the profile, template and time to live, and create the batch.
  Each sandbox is pinned to its pool, so policy, quotas and budgets still
  apply; pools the policy does not approve cannot be chosen, and a full pool
  takes its sandboxes as slots free up. The dialog shows free slots and an
  estimated hourly cost, then what was accepted or refused and why.
- A create request that placement refuses (`422 no_eligible_pool` or
  `unsupported_capability`) is now recorded on the audit log as
  `sandbox.rejected`, with the error code, the reason, the classification
  and any required attestations. Refusals used to leave no trace.

### Tests

- The server acceptance scenarios (`internal/server`) run on Postgres as well
  as the memory store: every pull request runs them against Postgres 17 in
  the `postgres conformance and acceptance` job, and `make test-postgres`
  runs them locally. They used to run only on the memory store, which
  production does not use. The first run found no defect. A run that is meant
  to use Postgres now fails, instead of skipping, when it has no database.
- CI runs the end-to-end suite (`test/e2e`) on every pull request: Postgres,
  `differently-server` and an OpenSandbox 1.2.0 server on the Docker runtime,
  driven through the public API. It had only been run by hand, and not at all
  for 0.1.0-alpha.3; its first CI run, on that release's code, passed 16
  tests and skipped the two that need a desktop image or a microVM pool. The
  Kubernetes and Fast Sandbox runtimes are still run by hand.

## [0.1.0-alpha.3] — 2026-11-10

Third alpha, published as a GitHub pre-release. Not for production. It is
mostly a correctness release: a review of the code found ten defects in
placement, the scheduler, the reconciler and the clients, and each is fixed
with a test that fails on the old behaviour. The default build is cut to the
adapters that have run against real backends, and the release workflow now
runs the checks before it publishes.

What has not run for this release: the end-to-end suite and the live adapter
tests. They last ran on the code before these changes (see `Verified` under
0.1.0-alpha.2 and the sections below). The fixes are covered by in-process
tests and, for the store, by the Postgres conformance job in CI.

Upgrading from 0.1.0-alpha.2:

- A config file or API call that names one of the 13 experimental adapter
  kinds is refused by the published image (first section below).
- A client that lists sandboxes by status needs to add `pausing`.
- `PATCH /v1/sandboxes/{id}` with `ttlSeconds: 0` answers 400; use `DELETE`.

### Documentation and CI

- The SDK READMEs install the SDKs from this repository. Neither is
  published: `pip install differently` installs an unrelated project of that
  name, and `npm install @differently/sdk` finds nothing. Their method
  tables no longer list `admin.agents.list`, which does not exist.
- `make vet` and CI compile and vet the end-to-end and live test files
  (`-tags "e2e live"`), which no job runs, so they cannot stop compiling
  unnoticed.
- The release workflow runs gofmt, vet, the Go tests (default and
  experimental builds) and the Helm lint before it publishes binaries, the
  chart or images.
- The chart's `appVersion` names this release, so installing the chart from
  a checkout pulls an image that exists. It used to name `0.1.0`, which was
  never published; the air-gapped and day-2 guides named the same tag.

### Changed (breaking): the default build holds only the verified adapters

The published image, the release binaries and `make build` now contain the
`opensandbox`, `daytona` and `freestyle` adapters, the `remote` plugin
protocol and the dev-mode `devhost` and `fake`. The other 13 adapter kinds
are experimental and compile in only with the Go build tag
`experimental_adapters`: `agent-sandbox`, `cubesandbox`, `e2b`, `boat`,
`morph`, `vercel`, `fly`, `blaxel`, `aws-agentcore`, `azure-sandboxes`,
`azure-sessions`, `gcp-sandboxes` and `northflank`. Apart from
`agent-sandbox`, which ran live on a kind cluster, they have only been
tested against fake servers. No adapter code was removed, and CI still
builds, vets and tests it with the tag.

- **If you use one of the 13 kinds**, the next published image will not
  start with a config file that names it: the error names the pool, the
  kind and the build tag. A pool of such a kind added earlier through the
  API stays registered and reports `unavailable` with the same reason.
  Build the server with `go build -tags experimental_adapters`, or the image
  with `--build-arg GO_TAGS=experimental_adapters`
  ([provider catalog](docs/providers/README.md#what-the-server-contains)).
- `POST /v1/admin/pools` answers 400 `invalid_request` for these kinds on a
  default build.
- Helm: pools of these kinds are refused unless `server.experimentalAdapters`
  is `true`, which you set together with a `server.image` built with the
  tag.
- The dashboard, the console and `differently admin pool add --help` list
  only the default kinds. Experimental pools are added in the config file,
  with the CLI or through the API.
- `make verify` and CI gained `make experimental` (build, vet and test with
  the tag). The live tests of `agent-sandbox` and `cubesandbox` now need
  `-tags live,experimental_adapters`.

### Fixed (placement, scheduler and clients)

Found by reading the code, each with a test that fails on the old behaviour.
None has run against a live backend.

- Placement no longer treats a policy it could not read as a policy that
  does not exist. During a database error, a sandbox whose organization sets
  `external: never` or denies a pool could be placed with that scope left
  out. The sandbox now stays queued, and create, the placement explanation
  and `GET /v1/capabilities` answer 503 `provider_unavailable`.
- The reconciler never deletes or adopts a backend instance that carries no
  sandbox ID. The `daytona` and `freestyle` adapters check ownership
  themselves instead of trusting the provider's label or metadata filter,
  and Daytona's create reuses a sandbox only when its label carries the ID.
  A provider that ignored the filter would have had every sandbox in the
  account deleted as an orphan. The other provider adapters still rely on
  their backend's filter for the installation match.
- A negative `ttlSeconds` on create is rejected; it used to pass the
  `maxTTLSeconds` quota and give a sandbox with no expiry. A `PATCH` must set
  a positive `ttlSeconds`: 0 used to delete the sandbox within a second and
  now answers 400, so a client that ended sandboxes that way needs `DELETE`.
- A pool that stops answering no longer slows placement on the others. Pool
  probes and the reconciler ran on the scheduler loop and held it for their
  timeouts (5 s per probe, 30 s per list); they now run beside it.
- A delete or a TTL extension that lands while the scheduler is deciding now
  wins. Expiry, the reconciler's "failed" and crash adoption wrote the status
  without reading the record again, so an extended sandbox could still be
  expired and a deleted one could come back as running.
- OpenSandbox, Daytona and Freestyle are given the TTL plus five minutes as
  their own deletion deadline. Theirs counts from the create request and
  Differently's from the moment the sandbox is ready, so the backend deleted
  first and the sandbox ended as failed instead of expired.
- A pool keeps its working adapter when its credential cannot be read for a
  moment. One failed read used to leave the pool without an adapter until a
  later refresh. A deleted secret or file still takes the pool down, and a
  changed value still rebuilds the adapter.
- The CLI uses the dev server's operator token only for `localhost`,
  `*.localhost` and loopback addresses. It used to send it to any URL
  containing "localhost", and not to `127.0.0.1`.
- A streamed exec through the Go client (`differently sandbox exec`) is no
  longer cut off after ten minutes.
- The audit views show the latest events. `differently usage audit`, the
  dashboard and the console asked for the first page, which is the oldest
  events, so anything after the first 100 or 200 never appeared.
  `GET /v1/audit-events` takes `order=desc` (newest first) and `beforeSeq`;
  the TypeScript and Python SDKs take `order` and `beforeSeq`/`before_seq`.
- CI: the Python SDK job runs on `ubuntu-24.04`, the last runner image with
  a Python 3.9 build; on `ubuntu-latest` it failed at setup.

### Fixed (lifecycle)

- A sandbox being paused reports the new status `pausing` instead of `running`
  and refuses exec, file and port calls. A Fast Sandbox VM runs on for minutes
  after its memory dump, and what it did then was lost on resume while every
  call reported success. A resume during a pause calls it off on Fast Sandbox,
  keeping the sandbox as it is; on other backends it waits for the pause. A
  client that lists sandboxes by status needs to add `pausing`.
- Idle preemption no longer pauses sandboxes in use. Without an idle timeout of
  their own their activity was never recorded, so they all looked idle a minute
  after starting, and a running command now counts as activity until it ends.

### Added (capacity)

- A paused sandbox on a pool that checkpoints on pause
  (`lifecycle.pause-checkpoint`: OpenSandbox Fast Sandbox, Freestyle) gives
  its slot back. Resuming takes one again; on a full pool the resume waits in
  a queue (202) and runs when a slot frees.
- Pool preemption (`preemption.mode: idle | lru`) pauses running sandboxes to
  make room for waiting work; `preemptible: false` opts a sandbox out.
- Lifecycle limits `maxRunSeconds` and `maxPausedSeconds` (request, `PATCH`
  and operator defaults).
- Capacity metrics (`differently_pool_slots`, `differently_resume_wait_seconds`,
  `differently_preemptions_total` and more) and a capacity model,
  `cmd/differently-capsim`, with results in `docs/scheduling/capacity.md`.
- The TypeScript and Python SDKs take `maxRunSeconds`, `maxPausedSeconds`
  and `preemptible` on create and update, and return `pausedAt`,
  `slotReleased`, `resumeRequestedAt`, and a pool's `preemption` and
  `released`. The console marks queued resumes, released slots and idle
  sandboxes, and the Providers page counts paused sandboxes that hold no slot.
- `differently_pause_seconds{pool}`: how long each pause Differently asks for
  takes on its backend, the time a checkpointing pool needs to free a slot.
- `differently-capsim -pause` and `-resume`; the Workbot model's pause is the
  measured 182 s.

### Documentation

- `docs/research/fast-sandbox-pause-time.md`: where a Workbot desktop's
  pause on Fast Sandbox spends its time (memory dump, a checksum pass that
  reads the whole checkpoint back, a single-stream upload, all limited by
  the state-root disk), what was measured before and after leaving templates
  sparse (median 216 s → 180 s, resumes unchanged, state kept), ranked
  candidates, and two Fast Sandbox patches (sparse pull, parallel multipart
  upload). The pools guide gains "Shortening pauses"; `test/pause` holds the
  harness that measured it.
- `docs/research/fast-sandbox-shared-image-cache.patch`: stops Fast Sandbox's
  image cache GC deleting the checkpoints paused sandboxes resume from, which
  turned a 2 s resume into two minutes.
- `docs/deploy/azure-vms.md`: a verified install on two Azure VMs with Fast
  Sandbox microVMs, managed Postgres, ACR and Key Vault, serving OpenWork's
  Workbot computers, and what it took (guest kernel, template registry,
  Key Vault CSI on VMs, ACR token scopes, sandbox network policy).
- The OpenWork guide records the end-to-end run, Freestyle template slugs
  and OpenSandbox's maximum timeout; the e2e suite runs on installs with
  several pools and replicas, and gives Fast Sandbox checkpoints time.
- The README, roadmap, feature parity, end-to-end and keys pages, and the
  website now state the same verified results: the end-to-end suite on a
  Fast Sandbox pool, two-node k3s, and stored provider keys in use on Azure.
  The website lists capacity and the admin console, and its sidebar gains
  Capacity, Keys and providers, OpenWork and Maintaining.
- `AGENTS.md` and `docs/maintaining.md` record what building and releasing
  taught so far: where capacity, the console and the website live and how to
  check them, the lists a new provider kind touches, new traps (Fast Sandbox
  clock and checkpoints, template builds, zero times, CI on conflicting PRs),
  and how release tags and `main` syncs are done.

### Added (admin console)

- `console/`: an admin app built with Astro, separate from the server. It has
  a sidebar and pages for sandboxes, snapshots, API keys, providers, secrets,
  policies, usage, the audit log and the trust report. It builds to static
  files for any host (the browser holds the API key), or runs on Node with
  Better Auth sign-in, an email allowlist and optional GitHub, where only the
  console server holds the key. See [console/README.md](console/README.md).
- `cors.allowedOrigins` (Helm `config.cors.allowedOrigins`): exact origins
  that browsers may call the API from. Empty by default, so nothing changes
  unless you set it.
- The TypeScript SDK runs in browsers: it no longer imports `node:crypto`.

### Fixed (Fast Sandbox)

- A Fast Sandbox microVM's clock is set right after create and resume; it
  used to start at the time its template or checkpoint was written.
- Pause and snapshot on Fast Sandbox wait up to `checkpointTimeoutSeconds`
  (default 900) instead of the general three-minute ready timeout, which a
  4 GiB desktop's checkpoint could exceed.
- The `fastsandbox-1.2.0` recording was made again on a two-node cluster
  with Workbot's desktop template.

### Changed

- `GET /v1/capabilities` answers a tenant key with the pools its placement
  policies allow (denied, allowed, external and region rules, for the default
  classification); operator keys still see every pool. OpenWork's runner
  reads it to decide whether idle computers can be paused, so a pool the
  organization may not use no longer turns pausing off.

### Fixed (Daytona and Freestyle, run live)

The `daytona` and `freestyle` adapters ran against live accounts for the
first time (`go test -tags live`, and a Differently server). Daytona's VM
class has not run.

- Freestyle: the env file was written with a mode Freestyle reads as
  decimal, and rewriting a file left it owned by root, so commands could not
  read the sandbox env; stop's `poweroff` ran as a user who may not power
  off; and a stopped VM was booted again by Freestyle's automatic restart.
- Daytona: stop and start returned while still in progress; a command
  timeout was reported as `provider_unavailable` (now exit code 124); a
  repeated delete, or one during a stop, failed with `invalid_state`;
  snapshot delete sent the name where Daytona needs the id; and a snapshot
  was returned before it could be used.

### Fixed (Helm chart)

- A pool under `config.pools` can use a provider key stored in Differently
  (`credentials: {secret: name}`), as the server already allowed; the
  values schema refused it.
- An external pool declared under `config.pools` now opens server egress to
  `providerCIDRs` on 443, like `providers.*` does.
- The `preflight` init container gets `server.extraVolumeMounts` too, so a
  `file:` credential on a mounted volume (such as a Key Vault CSI mount)
  resolves there as it does in the server.
### Fixed (lifecycle)

- A stop, start, pause or resume now finishes on the backend and is recorded
  even when the caller stops waiting. A Fast Sandbox pause writes a full
  checkpoint (about three minutes for a 4 GiB desktop), so a client with a
  shorter timeout used to cancel it halfway: the backend went on pausing
  while Differently still showed the sandbox running.

## [0.1.0-alpha.2] — 2026-10-29

Second alpha, published as a GitHub pre-release. Not for production.
OpenSandbox pools come close to Freestyle on the Kubernetes runtime, Fast
Sandbox microVMs run live, idle sandboxes stop and wake, and 15 more
provider adapters join OpenSandbox. The managed-provider adapters are tested
only against fakes of each provider's API, and multi-node and managed
clusters are not verified yet (see the provider catalog and
`docs/roadmap.md`).

### Added (OpenSandbox parity with Freestyle)

- `runtime: kubernetes` for OpenSandbox pools. With the controller's snapshot
  registry, the pool offers stop/start with the disk kept (OpenSandbox's
  pause commits the root filesystem and frees the pod), disk snapshots and
  fork, and refuses a memory pause it cannot keep.
- `ports: signed`: port URLs through OpenSandbox's ingress gateway (route
  mode `uri`, Secure Access), signed and time-limited, with no header or key.
- `egress: none` or `allowlist` (`egressAllow`), enforced by OpenSandbox's
  egress sidecar; the pool advertises `network.egress-control`.
- Exec stdin on OpenSandbox pools, staged in a file the command reads.
- Helm values `opensandbox.runtime`, `ports`, `portScheme`,
  `portURLSeconds`, `egress` and `egressAllow`; the e2e test `TestPortURL`;
  the recording `kubernetes-registry-1.2.0`.

### Added (Fast Sandbox runtime)

- `runtime: fastsandbox` for OpenSandbox pools: Firecracker microVMs created
  from a Fast Sandbox template, pause/resume with memory kept, disk snapshots
  and fork. A sandbox's idle timeout becomes OpenSandbox's own idle policy
  (pause on idle, wake on access); the scheduler leaves those pools to it and
  the reconciler follows the backend's pauses and gateway wakes. New optional
  adapter interface `adapter.NativeIdler`.

### Fixed (Fast Sandbox, run live)

- `portScheme` now applies to every gateway endpoint OpenSandbox returns
  without a scheme, not only signed ones, so a Fast Sandbox gateway behind
  TLS in route mode `uri` gives `https://` port URLs and execd traffic.
  Before, those came back as `http://` on a TLS-only port.
- Fork on Fast Sandbox: a restore from a snapshot sends the pool's size
  (new pool config `poolCPU` and `poolMemory`), which OpenSandbox requires.
- Fast Sandbox ran live on one node (nested KVM): a `fastsandbox-1.2.0`
  recording replays in every `go test`, and the pools guide lists measured
  timings with Workbot's desktop and the gaps found, each marked as an
  OpenSandbox limitation or not built yet.

### Added (idle auto-stop)

- `idleTimeoutSeconds` on sandboxes (create and `PATCH`) and
  `defaults.idleTimeoutSeconds`: the scheduler leader pauses (pools that keep
  memory) or stops (disk kept) a sandbox with no exec, file, port or
  keepalive call for that long, and the next such call wakes it. A sandbox a
  caller stopped or paused is never woken this way. New
  `POST /v1/sandboxes/{id}/keepalive`, sandbox fields `lastActivityAt` and
  `idledAt`, SDK and CLI (`--idle-timeout`) support, and audit event
  `sandbox.idled` / `sandbox.woke`.

### Added (provider catalog)

- `docs/providers/`: 26 providers, each with a tested status (verified live,
  tested with a fake server, documented, or not supported), capabilities,
  configuration and exactly what was tested, and a guide to adding one.
- A new adapter kind can no longer become private by omission: only
  `remote`, `devhost` and `fake` default to private, and a test fails until
  every kind is in exactly one trust list.
- `vercel` (Vercel Sandbox) and `fly` (Fly.io Machines) adapters, always
  external; pages for Modal, Cloudflare and Sprites (documented: a `remote`
  plugin). Tested with fake servers, not live accounts.
- `boat`, `morph` (always external) and `e2b` (declared trust: it is
  self-hostable) adapters; a page for CodeSandbox (documented). Tested with
  fake servers, not live accounts.

### Added (cloud providers' own sandboxes)

- Adapters for the cloud providers' sandbox services, each tested against a
  fake server built from the provider's published API (not yet live):
  `aws-agentcore` (Amazon Bedrock AgentCore Code Interpreter),
  `azure-sandboxes` (Azure Container Apps Sandboxes), `azure-sessions`
  (Azure Container Apps dynamic sessions, Python code-interpreter pools),
  `gcp-sandboxes` (Google Cloud Agent Platform shell sandboxes) and
  `northflank`. Pools of these kinds must declare `trust` (external, or
  private if the operator's policy treats their own cloud account so).
- Cloud IAM credentials without SDKs: Microsoft Entra ID (client secret or
  workload identity federation), Google service-account keys, and AWS
  Signature Version 4, in `internal/adapters/cloudhttp`.
- Provider pages in `docs/providers/` for these, plus AgentCore Runtime and
  GKE Agent Sandbox (documented, no adapter).

### Added (self-hosted runtimes)

- `agent-sandbox` pools: Sandboxes from kubernetes-sigs/agent-sandbox, or
  SandboxClaims on a warm pool, through the Kubernetes API with no client-go
  dependency; exec and files over WebSocket `pods/exec` (stdin and
  streaming included); stop/start with a per-sandbox volume. Checked live on
  kind with agent-sandbox v1.0.8.
- `cubesandbox` pools: CubeSandbox's KVM microVMs through its E2B-compatible
  CubeAPI and envd: exec (streamed), files, pause/resume, memory snapshots
  and fork. Tested with a fake server; not run against CubeSandbox yet.
- Provider pages `docs/providers/agent-sandbox.md`, `cubesandbox.md` and
  `microsandbox.md` (not supported: no self-hosted server API since its
  rewrite).

### Added (Blaxel provider)

- `blaxel` adapter for Blaxel sandboxes (always `trust: external`): exec
  and streamed exec, files, stdin, token-protected port previews and a TTL
  backstop. Tested against a fake server built from Blaxel's published
  specs, not a live account.
- Provider pages for Blaxel, Runloop (not supported: the service is
  winding down), Beam and Koyeb (documented: a `remote` plugin on their
  SDKs) under `docs/providers/`.

### Added (end-to-end checks for microVM pools)

- `TestPauseKeepsMemory`, `TestIdlePauseAndWakeThroughPortURL`,
  `TestMemorySnapshot` and `TestDesktopStreams` (VNC over a port URL's
  WebSocket), each skipped where the pool lacks the feature. The suite runs
  against Fast Sandbox pools by template (`DIFFERENTLY_E2E_TEMPLATE`), checks
  a microVM guest's size instead of cgroup limits, and expects Fast Sandbox's
  precise refusals of sandbox env and of a template the pool does not serve.

### Fixed (found against real OpenSandbox runtimes)

- A sandbox whose image cannot be pulled no longer marks its pool unhealthy
  for every other tenant (which could move their work to another pool or
  external provider). The Docker runtime's failed pull fails the sandbox
  after one attempt; the Kubernetes runtime's pod-ready timeout is retried
  as this sandbox's failure (new adapter error `sandbox_failed`), not a
  provider outage.
- Pause on a runtime that accepts it but never starts it (the Kubernetes
  runtime without a snapshot registry) returns `unsupported_operation` after
  30 s instead of `provider_unavailable` after 3 minutes; snapshots without a
  registry say so instead of reporting an outage.
- A sandbox being deleted on the Kubernetes runtime is treated as gone
  instead of as pending.
- The Helm chart's default `opensandbox.apiURL` and NetworkPolicy now match
  OpenSandbox's own chart (namespace `opensandbox-system`, port 80).
- A sandbox whose create outlived the provision timeout stayed
  `provisioning` and was provisioned again on every scheduler tick: the retry
  ran on the expired context, so its PostgreSQL writes failed. Only the
  backend call is bounded by the timeout now.

### Security

- `devhost` refs must match the generated ID pattern; before, `..` reached
  the adapter root, so deleting it removed the root.
- Sandbox file reads from the API and the adapter protocol are served as
  downloads (`Content-Disposition: attachment`, `nosniff` and a sandbox CSP),
  so browsers never render sandbox content on the API origin.
- The SDKs trim trailing slashes from the base URL without a backtracking
  regular expression.

### Changed

- OpenSandbox pools no longer advertise `ports.expose` by default: their
  direct endpoints are runtime-internal addresses (`host.docker.internal`,
  pod IPs).
- The end-to-end suite adapts to the pool's capabilities, adds
  `TestUnpullableImageKeepsPoolHealthy`, and runs against any deployment
  (`OPENSANDBOX_E2E_INTERNAL_URL`, `DIFFERENTLY_E2E_POOL`).
- CI approves a pull request from this repository once every CI job passes,
  which satisfies `main`'s one-approval rule; forks are never approved and a
  new push dismisses the approval.
- Documentation: a maintainer guide (`docs/maintaining.md`), deployment
  guides for any cloud or bare metal with a verified single-VM recipe, the
  provider catalog, and a website and README that describe the project
  plainly. The internal architecture decision records were removed.

### Verified

- OpenSandbox's Docker runtime (arm64, Colima) and Kubernetes runtime
  (single-node k3s on Azure, installed with Helm): the end-to-end suite
  passes 14/14, including stop/start, snapshots and fork through a snapshot
  registry, signed port URLs (WebSockets included), egress policy, stdin, and
  idle auto-stop with wake.
- Fast Sandbox on one node (nested KVM on Azure), with OpenWork's Workbot
  desktop through Differently: create in 1.2 s, the desktop streamed over the
  HTTPS port URL, pause in about 3 minutes and resume in 0.9 s with memory
  kept, and an idle sandbox woken by a port request in 0.67 s. A partial
  end-to-end run passed files and pause/resume; fork failed on a 1 vCPU,
  1 GiB template (not yet diagnosed), and the port URL, idle-wake, memory and
  desktop checks did not run.
- The `agent-sandbox` adapter against agent-sandbox v1.0.8 on a kind cluster.
- OpenWork's Workbot computers through Differently as a sandbox gateway,
  behind a flag.
- Recorded runs of the Docker, Kubernetes (with and without a snapshot
  registry) and Fast Sandbox runtimes replay in every `go test`.
- Not verified: the managed-provider adapters against live accounts (each is
  tested against a fake of its provider's published API), multi-node clusters
  and node drains, and managed EKS, GKE and AKS.

## [0.1.0-alpha.1] — 2026-10-28

First alpha, published as a GitHub pre-release for early testing. Not for
production: OpenSandbox's Kubernetes and Fast Sandbox runtimes, real
clusters and live Daytona/Freestyle accounts are not verified yet (see
`docs/roadmap.md`). The end-to-end suite covers the real stack against an
OpenSandbox server on its Docker runtime.

### Added

- Control plane (`differently-server`):
  - v1 API, API keys with scopes, organizations and projects;
  - atomic quota admission and activation, fair queueing;
  - policy-driven placement with fail-closed external providers;
  - scheduler and reconciler;
  - tamper-evident audit chain, trust report, usage estimates;
  - Prometheus metrics, built-in dashboard;
  - memory and Postgres stores.
- Adapters:
  - `opensandbox`, the default private runtime: OpenSandbox servers as
    pools with operator-declared trust and isolation, covering create, exec
    and streamed exec, files, pause/resume, snapshots, fork and port
    endpoints;
  - Daytona, Freestyle;
  - `remote` (plugin protocol);
  - `devhost` (dev only).
- CLI (`differently`), Go, TypeScript and Python SDKs, OpenAPI 3.1 spec.
- Helm chart for the control plane, with an `opensandbox` block that
  registers an existing OpenSandbox server as the default pool; AWS
  Terraform module.
- End-to-end suite (`make e2e`): Postgres, the server and an OpenSandbox
  server in Docker, driven through the public API.
- Templates can map one name to each pool's own image (`templates[].images`),
  so a template routes to OpenSandbox, Daytona or Freestyle; placement only
  considers pools the template lists.
- `opensandbox` pools: `lineEndings: kept` gives byte-exact exec output with
  `execd` release-1.2.0 or later, and a TTL change moves OpenSandbox's own
  expiry (`renew-expiration`).
- [OpenWork on Differently](docs/guides/openwork.md): OpenWork's sandbox
  contract mapped onto this API.
- Documentation: architecture RFC, ADRs, security model, adapter spec,
  placement policy, feature parity, deployment guides, roadmap and backlog.
- Provider credentials stored in Differently:
  - `PUT /v1/admin/secrets/{name}` stores a Daytona, Freestyle, OpenSandbox
    or plugin key sealed in Postgres (migration `0004_secrets.sql`);
    `GET /v1/admin/secrets` lists and `DELETE /v1/admin/secrets/{name}`
    removes. Listings show fingerprints and the pools using each
    secret, never values. A secret in use cannot be deleted;
  - pools reference a stored key as `credentials: {secret: name}`, and
    `PATCH /v1/admin/pools/{name}` can move an API-registered pool to another
    `credentialRef`;
  - replacing a stored value, or the file behind a `file:` reference, takes
    effect within about 5 seconds on every replica, without a restart.
    `env:` still needs a restart;
  - config `secrets.keyEnv` (default `DIFFERENTLY_SECRETS_KEY`) and
    `secrets.keyFile`. Without a key, storing fails with `invalid_state`.
- `POST /v1/admin/pools/{name}/use` sends new sandboxes to one pool by
  draining the others; running sandboxes stay. An external pool needs the
  global policy's approval, or `approveExternal`.
- `POST /v1/api-keys/{id}/rotate` replaces a key with a grace period of up to
  7 days, and `GET /v1/api-keys/{id}` reads one key.
- The surfaces for these:
  - CLI: `admin secret put|list|delete`, `admin pool add` (with
    `--api-key-stdin` or `--secret`), `admin pool use`, `key rotate`,
    `key get`, and `key list` as a table;
  - the dashboard's Add provider, Use for new sandboxes, Secrets and Keys
    views;
  - rotation and secrets in the Go, TypeScript and Python SDKs.
- Helm:
  - `secrets.enabled`, `secrets.existingSecret` and `secrets.existingSecretKey`.
    By default the chart generates the key once, in
    `<fullname>-secrets-key`, and keeps it across upgrades and uninstalls;
  - `networkPolicy.server.egress.runtimeProviders` for providers added at
    runtime.
- Guide: [keys and providers](docs/guides/keys-and-providers.md).

### Changed

- OpenSandbox replaces Differently's own Firecracker node agent as the
  private runtime.
  The following were removed before any release:
  - `differently-agent`, `differently-guest` and the `firecracker` adapter;
  - agent registration, heartbeats and per-agent secrets;
  - the guest image scripts and systemd units.

  The server's config parser and the Helm schema reject the removed
  settings (`auth.agentTokenEnv`, `scheduler.agentTimeoutSeconds`,
  `agent.*`). Postgres migration `0002` drops the `agents` table.
- The trust report shows each pool's declared isolation and states that
  isolation and egress come from the backend runtime, not from Differently.

- An `Idempotency-Key` names a live sandbox: once that sandbox is terminated
  or failed, the same key creates a new one (Postgres migration `0003`).

### Security

- `audit.failClosed` refuses state-changing requests that cannot be audited;
  `differently_audit_append_failures_total` counts audit failures.
- `metrics.listen` serves `/metrics` on its own port; the Helm chart does this
  by default (port 9091), keeping metrics off the ingress.
- Stored provider credentials:
  - Values are sealed with AES-256-GCM, with the secret's name bound in,
    under a 32-byte key-encryption key that never enters the database. A
    database dump alone reveals no stored credential.
  - The key is now critical. Losing it makes every stored credential
    unreadable, and the key plus a database copy opens them all. Back it up
    apart from database backups. It cannot be rotated yet.
  - Storing, replacing and deleting are `admin`-only and audited
    (`secret.put` with the fingerprint, `secret.delete`).
- Rotating an API key requires being able to grant every scope it holds,
  and is audited as `key.rotate`.
- Known gaps, not yet fixed:
  - with more than one server replica, pool changes made through the API
    (`use`, drain, undrain, `PATCH`, new pools) reach the other replicas
    only after a restart (backlog E0.36);
  - the trust report does not list `secret.*` events among recent admin
    actions (E0.37).

### Fixed (found by the end-to-end suite)

- `start` refused every request after the first one: the lifecycle handler
  reused state between requests.
- The reconciler could correct a sandbox in the middle of a change. It now
  waits 30 seconds after any change, and follows the backend's state: a
  sandbox that stopped behind the control plane's back is reported `stopped`.
- `differently sandbox list -o json` printed an object instead of an array
  for a single sandbox.

### Verified

- End-to-end suite against OpenSandbox 1.2.0 (Docker runtime) on arm64
  Colima: 10/10, twice. Not yet verified:
  - OpenSandbox's Kubernetes and Fast Sandbox runtimes;
  - x86_64 hosts and real clusters;
  - live Daytona and Freestyle accounts;
  - stored provider credentials, switching providers and API key rotation
    beyond unit and in-process API tests: no live provider account, real
    cluster or multi-replica run yet.

[Unreleased]: https://github.com/different-ai/differently/compare/v0.1.0-alpha.5...pre-release
[0.1.0-alpha.5]: https://github.com/different-ai/differently/releases/tag/v0.1.0-alpha.5
[0.1.0-alpha.4]: https://github.com/different-ai/differently/releases/tag/v0.1.0-alpha.4
[0.1.0-alpha.3]: https://github.com/different-ai/differently/releases/tag/v0.1.0-alpha.3
[0.1.0-alpha.2]: https://github.com/different-ai/differently/releases/tag/v0.1.0-alpha.2
[0.1.0-alpha.1]: https://github.com/different-ai/differently/releases/tag/v0.1.0-alpha.1
