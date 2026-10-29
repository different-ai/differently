# Hardening checklist for operators

Work through this before putting real workloads on Differently. Each item says
what to do and why, with the setting or code it relates to. Items marked
**(gap)** cannot be fully closed today; the note says how to reduce the risk.
Background: [trust model](trust-model.md).

Differently does not isolate sandboxes itself
([ADR 0020](../adr/0020-opensandbox-replaces-node-agent.md)). Most of the
isolation and egress work below is done in the backend runtime, usually an
OpenSandbox server, and is marked as such.

## Before you start

- [ ] Read the [trust model](trust-model.md), especially §6 (known issues) and
      §7 (residual risks). Decide whether they are acceptable for your data.
- [ ] Remember the evidence level: OpenSandbox has run through Differently
      only with the Docker runtime; its Kubernetes runtime and Fast Sandbox
      have not, and the Daytona and Freestyle adapters have not run against
      live accounts. Run your own validation ([proof-of-concepts.md](../proof-of-concepts.md))
      before production.
- [ ] Never run `differently-server --dev` or a `devhost` pool outside a
      laptop. `devhost` runs commands on the server host with no isolation.

## Control plane

- [ ] Terminate TLS in front of the server (ingress, load balancer or
      sidecar). The server listens on plain HTTP (`listen`, default `:8080`).
- [ ] Run `differently-server preflight` (the Helm chart can run it as an init
      container). It checks the bootstrap token format, that every pool
      credential reference resolves, that Postgres is reachable, and that
      external placement is either off or names approved pools. It does not
      contact OpenSandbox or the providers; check pool health with
      `differently admin pool list` after start-up.
- [ ] Use the Postgres store (`store.driver: postgres`) and at least two
      replicas with a `PodDisruptionBudget`. The memory store loses all state
      on restart and cannot be shared.
- [ ] Serve `/metrics` on its own port (`metrics.listen`; the Helm default is
      `9091`, which the ingress never routes to) and restrict scrapers with
      `networkPolicy.server.metricsFrom`.
- [ ] Consider `audit.failClosed: true` so state-changing requests are refused
      when they cannot be audited, and alert on
      `differently_audit_append_failures_total`.
- [ ] Configure your load balancer for long-lived, unbuffered responses on
      `POST /v1/sandboxes/{id}/exec` (SSE) and `GET /v1/sandboxes/{id}/wait`
      (up to 300 s).
- [ ] Set `rateLimit.requestsPerSecond` and `burst` for your traffic. Limits
      are per key and per replica, so the effective limit is multiplied by the
      replica count.
- [ ] Size server memory for synchronous exec: the OpenSandbox adapter
      buffers a command's whole output in the replica **(gap: backlog E0.35)**.
      Prefer streamed exec for commands with large output.
- [ ] Leave the dashboard on only if you need it (`dashboard.enabled`). It is
      static and uses the viewer's own key.

## PostgreSQL

- [ ] Require TLS in the DSN (`sslmode=verify-full`) and use a dedicated
      database role that owns only the Differently schema.
- [ ] Encrypt the database and its backups at rest. It stores sandbox
      environment values in plaintext (`sandboxes.doc`), which often contain
      secrets **(gap: [backlog E13](../backlog.md#e13-sandbox-secrets-by-reference))**.
- [ ] Back up and test restores. All quota, queue and audit state is here.
- [ ] Set server-side TCP keepalives (`tcp_keepalives_idle`,
      `tcp_keepalives_interval`, `tcp_keepalives_count`) so a partitioned
      leader's advisory lock is released promptly
      ([ADR 0016](../adr/0016-single-leader-scheduler-advisory-lock.md)).
- [ ] Use an HA setup (for example CloudNativePG with synchronous replicas)
      and test failover ([POC 6](../proof-of-concepts.md#poc-6-postgres-failover-and-leader-handover)).
- [ ] Limit who has write access. A database writer can rewrite the audit
      chain undetectably unless heads are anchored externally.

## Secrets and keys

- [ ] Store `DIFFERENTLY_DATABASE_URL` and `DIFFERENTLY_BOOTSTRAP_TOKEN` in
      a secret store and pass them as environment variables or mounted files.
      For the OpenSandbox API key (`DIFFERENTLY_OPENSANDBOX_API_KEY` in the
      chart) and provider keys, do the same, or store them in Differently
      (`secret:` references, sealed in the database). Config files hold only
      references (`env:NAME`, `file:/path`, `secret:name`). With Helm, prefer
      `opensandbox.existingSecret` over the inline `opensandbox.apiKey`.
- [ ] If you store credentials in Differently, back up the secrets key
      (`dly-differently-secrets-key`, or your `secrets.existingSecret`) to a
      vault, apart from database backups. Losing it makes every stored
      credential unreadable; anyone holding it and a database copy can read
      them all. With GitOps renderers that cannot `lookup`, set
      `secrets.existingSecret`, or every render generates a new key. Do not
      change the key: it cannot be rotated yet **(gap:
      [backlog E23](../backlog.md#e23-provider-credential-follow-ups))**. If
      you store nothing, `secrets.enabled: false` gives the server no key.
- [ ] Generate tokens with `differently-server token`, and the secrets key
      with `differently-server secrets-key` (or `openssl rand -base64 32`) if
      you bring your own.
- [ ] After first start, create a named operator key and store it, then
      revoke the bootstrap key (`differently key revoke <id>`). Unsetting
      the variable does not revoke the key. A revoked bootstrap key stays
      revoked even if the variable's value changes
      ([the bootstrap operator token](../guides/keys-and-providers.md#the-bootstrap-operator-token)).
- [ ] Rotate API keys with a grace period instead of revoking and
      re-creating them: `differently key rotate <id> --grace 1h`, then
      update the caller and let the grace lapse.
- [ ] Give CI and applications project-scoped keys with the fewest scopes
      (`sandboxes:read`, `sandboxes:write`, `sandboxes:exec` as needed). Keep
      `org:admin` and `keys:write` for humans.
- [ ] Set `ttlHours` on keys you create. There is no enforced maximum, and
      revoking a key does not revoke keys it created **(gap)**.
- [ ] Treat the OpenSandbox API key as root for that server: it is
      server-wide, and anyone holding it can act on every sandbox there
      without going through Differently. Give it only to the control plane,
      and rotate it on the OpenSandbox server and in the control plane
      (the Secret, or `differently admin secret put`) together.

## Network

- [ ] Enable the chart's `NetworkPolicy` and make sure your CNI enforces it.
- [ ] Restrict server egress (`networkPolicy.server.egress.restricted`) to
      DNS, Postgres, OpenSandbox (`networkPolicy.server.egress.opensandbox`,
      port 80 by default) and the CIDRs of approved providers. Leave
      `networkPolicy.server.egress.runtimeProviders` off unless operators add
      providers from the dashboard or API; it opens `providerCIDRs` on 443. With
      `opensandbox.useServerProxy: false` the server also needs each
      sandbox's endpoint (`networkPolicy.server.egress.extra`); keep the
      default proxy unless you have measured a need.
- [ ] Restrict who can reach the OpenSandbox server's port: only the control
      plane needs it. Apply this in OpenSandbox's namespace or on its host.
- [ ] Use TLS between the control plane and OpenSandbox when they cross an
      untrusted network: an `https` `apiURL` or an encrypting mesh. The
      chart's default URL is plain HTTP inside the cluster and carries the
      API key, environment values and file contents.
- [ ] Configure sandbox egress in the runtime, not in Differently: block
      cloud metadata endpoints (on AWS, IMDSv2 with a hop limit of 1 on the
      hosts that run sandboxes), private ranges, the control plane and
      Postgres. Differently does not wire OpenSandbox's network policy yet
      **(gap: [backlog E19](../backlog.md#e19-opensandbox-network-policy-and-egress))**.

## Backend runtimes (OpenSandbox)

- [ ] Install OpenSandbox with its own chart or manifests and pin the server,
      `execd` and runtime image versions. OpenSandbox's published spec has run
      ahead of its default images; run the adapter's live test
      (`go test -tags live ./internal/adapters/opensandbox/`) against a
      staging server on every upgrade
      ([backlog E21](../backlog.md#e21-opensandbox-version-pinning-and-upgrade-testing)).
- [ ] Choose the runtime for the data. The Docker and Kubernetes runtimes run
      containers that share the host kernel. Fast Sandbox runs Firecracker
      microVMs and needs KVM hosts (Linux ≥ 5.10, `/dev/kvm`, `/dev/net/tun`,
      cgroup v2, an XFS filesystem with reflink for images, an enlarged
      `/dev/shm`) **(gap: not yet verified through Differently,
      [backlog E18](../backlog.md#e18-fast-sandbox-spike))**.
- [ ] Declare each pool's `isolation` (`container` or `microvm`) to match the
      runtime you deployed. The trust report shows the declaration and
      Differently does not check it.
- [ ] Declare `trust` truthfully: `private` only if you operate the
      OpenSandbox server and its hosts.
- [ ] Prefer one pool per OpenSandbox server. A server has one API key and
      no tenant model, so a second pool on it adds no isolation.
- [ ] Harden the runtime in OpenSandbox's configuration: drop container
      capabilities, set `no_new_privileges`, limit PIDs, use seccomp or
      AppArmor profiles. `test/opensandbox/config.toml` shows the Docker
      options the live test used; it is a test setup, not a recommendation.
- [ ] Keep sandbox hosts dedicated to sandboxes, patched, and administered by
      as few people as possible. Their administrators can read every
      sandbox.
- [ ] Pull images and templates only from registries you control, keep them
      minimal and patched, and do not bake secrets into them; every sandbox
      from an image shares its contents.
- [ ] Set `maxConcurrency` on OpenSandbox pools. OpenSandbox reports no
      capacity, so without a cap the pool is limited only by quotas, and
      overflow to external pools never happens while it is healthy.
- [ ] Declare `snapshots.memory` in `features` only if the runtime really
      captures memory, and declaration-only features (`network.egress-control`,
      `gpu`) only if it really provides them; they are advertised as
      declared.

## External providers

- [ ] Use provider API keys scoped to a dedicated provider organization or
      account for Differently.
- [ ] Keep the global policy at `external: never` until you have decided which
      classifications may leave. Then approve each external pool by name in
      `externalPools`. Prefer that explicit change to
      `differently admin pool use <pool> --approve-external`, which approves a
      pool as a side effect of switching, and does not withdraw the approval
      when you switch back.
- [ ] Map classifications deliberately. The default classes are
      `private-only` (never), `internal` (overflow, the default class) and
      `public` (allowed).
- [ ] Set `maxConcurrency` on external pools; external providers report
      unbounded capacity and the cap is what limits spend.
- [ ] For Freestyle, leave `config.egress` unset (deny-all) unless guests need
      the internet. Exposed Freestyle ports are public URLs.
- [ ] For `remote` pools that run on third-party infrastructure, set
      `trust: external`. The default is `private`. Serve plugins over TLS
      with a bearer token.
- [ ] For an OpenSandbox server someone else operates, set `trust: external`;
      it then follows the same approval rules as Daytona and Freestyle.
- [ ] Review `GET /v1/admin/trust-report` after every policy change.
- [ ] Rotate provider keys within the same provider account. A pool pointed
      at another account loses sight of its existing sandboxes; register a
      new pool and switch to it instead.

## Policy and tenancy

- [ ] Give organization and project admins `policies:write` only if they need
      to tighten placement.
- [ ] Set organization and project quotas (`maxActiveSandboxes`, `maxCPU`,
      `maxMemoryMiB`, `maxDiskGiB`, `maxQueued`, `maxTTLSeconds`,
      `maxSnapshots`). Zero means unlimited. The OpenSandbox adapter does not
      pass `diskGiB` to OpenSandbox, so `maxDiskGiB` limits what tenants ask
      for, not what they get.
- [ ] Set `maxQueueSeconds` in a policy if waiting work should fail rather
      than queue indefinitely.

## Audit and monitoring

- [ ] Set `audit.file` and ship the JSON lines to write-once storage or a SIEM.
      Periodically record the `headHash` from `GET /v1/admin/audit/verify`
      outside the database.
- [ ] Alert on `differently_audit_append_failures_total` and `audit append
      failed` log lines. Unless `audit.failClosed` is set, the action still
      happens.
- [ ] Leave `audit.recordCommands` off unless you need command lines; they can
      contain secrets.
- [ ] Alert on `secret.put`, `secret.delete`, `pool.create`, `pool.use` and
      `policy.update` events. The trust report's `recentAdminActions` does not
      include `secret.*` events, so query
      `GET /v1/audit-events?action=secret.put` directly.
- [ ] Alert on `differently_reconcile_actions_total` (orphans deleted,
      instances lost, adoptions), on pools whose health is not `healthy`, and
      on `differently_placement_decisions_total{outcome="rejected"}`. On an
      OpenSandbox pool, unexpected orphans or lost instances can mean someone
      is using the server's API key directly.
- [ ] Keep OpenSandbox's own logs. Actions taken with its API key outside
      Differently appear only there.

## Upgrades and incidents

- [ ] Test upgrades on a staging installation; run `differently-server migrate`
      (the chart's migrate Job) before rolling the Deployment. Upgrade
      OpenSandbox separately, with the live test above.
- [ ] Know the kill switches: `POST /v1/admin/organizations/{id}/suspend` with
      `{"terminate": true}`, `POST /v1/admin/pools/{name}/drain`, `PATCH
      /v1/admin/pools/{name}` with `{"enabled": false}`, a global policy with
      `external: never`, and `POST /v1/admin/sandboxes/{id}/terminate`. Every
      replica picks up a pool change within about 5 seconds.
- [ ] After a suspected compromise of an OpenSandbox server, its hosts or its
      API key: drain its pool, rotate the key on the server and in the
      control plane, review OpenSandbox's logs and the reconciler's actions,
      and treat the environment values and files of sandboxes on that pool as
      exposed.
- [ ] After a suspected leak of a provider key: create a new key at the
      provider, `differently admin secret put` it (or update the Secret or
      file), and revoke the old one at the provider.
- [ ] After a suspected leak of the secrets key together with a database copy
      or backup: treat every stored credential as exposed. Rotate each one
      at its provider and store the new values.
