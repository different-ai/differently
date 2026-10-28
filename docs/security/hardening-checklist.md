# Hardening checklist for operators

Work through this before putting real workloads on Differently. Each item says
what to do and why, with the setting or code it relates to. Items marked
**(gap)** cannot be fully closed today; the note says how to reduce the risk.
Background: [trust model](trust-model.md).

## Before you start

- [ ] Read the [trust model](trust-model.md), especially §6 (known issues) and
      §7 (residual risks). Decide whether they are acceptable for your data.
- [ ] Remember the evidence level: the Firecracker path has not yet run on
      real KVM and the Daytona and Freestyle adapters have not run against live
      accounts. Run your own validation ([proof-of-concepts.md](../proof-of-concepts.md))
      before production.
- [ ] Never run `differently-server --dev` or a `devhost` pool outside a
      laptop. `devhost` runs commands on the server host with no isolation.

## Control plane

- [ ] Terminate TLS in front of the server (ingress, load balancer or
      sidecar). The server listens on plain HTTP (`listen`, default `:8080`).
- [ ] Run `differently-server preflight` (the Helm chart can run it as an init
      container). It checks the bootstrap token format, that every credential
      reference resolves, that the agent token is set for Firecracker pools,
      that Postgres is reachable, and that external placement is either off or
      names approved pools.
- [ ] Use the Postgres store (`store.driver: postgres`) and at least two
      replicas with a `PodDisruptionBudget`. The memory store loses all state
      on restart and cannot be shared.
- [ ] Serve `/metrics` on its own port (`metrics.listen`; the Helm default is
      `9091`, which the ingress never routes to) and restrict scrapers with
      `networkPolicy.server.metricsFrom`.
- [ ] Consider `audit.failClosed: true` so state-changing requests are refused
      when they cannot be audited, and alert on
      `differently_audit_append_failures_total`.
- [ ] Restrict who can reach `/internal/v1/agents/heartbeat`: only agent nodes
      need it (Helm `networkPolicy.server.ingressFrom` plus the agent selector).
- [ ] Configure your load balancer for long-lived, unbuffered responses on
      `POST /v1/sandboxes/{id}/exec` (SSE) and `GET /v1/sandboxes/{id}/wait`
      (up to 300 s).
- [ ] Set `rateLimit.requestsPerSecond` and `burst` for your traffic. Limits
      are per key and per replica, so the effective limit is multiplied by the
      replica count.
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

- [ ] Store `DIFFERENTLY_DATABASE_URL`, `DIFFERENTLY_BOOTSTRAP_TOKEN`,
      `DIFFERENTLY_AGENT_TOKEN` and provider keys in a secret store and pass
      them as environment variables or mounted files. Config files hold only
      references (`env:NAME`, `file:/path`).
- [ ] Generate tokens with `differently-server token`.
- [ ] After first start, create a named operator key, store it, and unset the
      bootstrap token variable. Note that changing the bootstrap variable's
      value later re-activates the bootstrap key even if it was revoked.
- [ ] Give CI and applications project-scoped keys with the fewest scopes
      (`sandboxes:read`, `sandboxes:write`, `sandboxes:exec` as needed). Keep
      `org:admin` and `keys:write` for humans.
- [ ] Set `ttlHours` on keys you create. There is no enforced maximum, and
      revoking a key does not revoke keys it created **(gap)**.
- [ ] Rotate the shared agent registration token on a schedule and after any
      node compromise. It lets a node register new agent IDs; it cannot take
      over existing ones, which are pinned to per-agent secrets. After
      rebuilding a node (wiped state directory), remove its old identity with
      `DELETE /v1/admin/agents/{id}` so it can register again
      **(remaining gap: unencrypted channel, [backlog E3](../backlog.md#e3-authenticated-agent-channel))**.

## Network

- [ ] Enable the chart's `NetworkPolicy` and make sure your CNI enforces it.
      Agents should accept connections only from server pods on the agent
      port (default 9090).
- [ ] Restrict server egress (`networkPolicy.server.egress.restricted`) to
      DNS, agents, Postgres and the CIDRs of approved providers.
- [ ] Treat the agent port as sensitive: traffic is plain HTTP and carries
      environment values and file contents **(gap: backlog E3)**. If agents
      and servers cross an untrusted network, put them on a private network or
      an encrypted overlay (WireGuard, a service mesh with mTLS).
- [ ] Decide the egress mode per node pool: `--egress allow` (default; public
      destinations only) or `--egress deny`.
- [ ] Block what the built-in deny list does not cover: public IPs of your own
      nodes and load balancers, IPv6 (no `ip6tables` rules are created), and
      any internal ranges outside `10/8`, `172.16/12`, `192.168/16`,
      `100.64/10`, `169.254/16`.
- [ ] Give guests a DNS resolver they can reach. The VM cannot reach the node
      or private ranges, so cluster DNS is unreachable; configure a resolver in
      the rootfs or provide one through an allowed path (untested).
- [ ] Prefer `agent.hostNetwork: false` (the chart default) so VM taps and
      rules live in the agent pod's network namespace and NetworkPolicy still
      applies to the pod.

## KVM nodes and the agent

- [ ] Use hardware that matches Firecracker's tested platforms (AWS `.metal`
      instance families) for production. Nested virtualization is documented
      by Firecracker for development only.
- [ ] Keep host kernels on a Firecracker-supported line (5.10, 6.1 or 6.18),
      with current microcode and CPU mitigations.
- [ ] Use cgroup v2 on hosts.
- [ ] Label KVM nodes `differently.dev/kvm=true` and taint them so only agents
      run there.
- [ ] Size `--reserve-cpu`, `--reserve-memory-mib` and `--cpu-overcommit`
      for the host. Kubernetes does not see VM memory.
- [ ] Run `differently-agent doctor` (the chart's doctor init container) on
      every node.
- [ ] Keep the state directory (`/var/lib/differently/agent`) and images
      directory on local disk readable only by root. VM records include
      environment values (`vm.json`, mode 0600) and snapshots are full disk
      copies.
- [ ] Prefer the chart's unprivileged mode (`agent.privileged: false` with
      `NET_ADMIN` and a KVM device plugin) if your platform supports it; it
      narrows the agent's privileges.
- [ ] **(gap)** The Firecracker jailer is not integrated
      ([ADR 0015](../adr/0015-defer-jailer-integration.md)). Setting
      `agent.jailer.enabled` only makes `doctor` check that the jailer binary
      exists; microVMs still run without it. Do not count it as a control.
- [ ] Use the `OnDelete` update strategy for the agent DaemonSet (chart
      default) and drain a pool (`differently admin pool drain NAME`) before
      replacing agents. Expect running VMs on a node to stop when its agent
      container restarts ([POC 2](../proof-of-concepts.md#poc-2-agent-restart-semantics-in-kubernetes)).
- [ ] Keep the control-plane-to-agent link reliable. If agents miss heartbeats
      for longer than `scheduler.agentTimeoutSeconds` (default 30 s), the
      reconciler currently treats their sandboxes as lost and fails them
      (known issue, [backlog E0](../backlog.md#e0-correctness-and-security-fixes-from-the-code-review)).
      Consider raising the timeout until that is fixed.

## Guest images

- [ ] Build kernels and rootfs images in a reproducible, reviewed pipeline and
      verify checksums before they reach nodes. The chart's `guest-images` init
      container copies whatever its image contains.
- [ ] Install `differently-guest` as the VM's service on vsock port 10000.
- [ ] Do not bake secrets into images; every sandbox from a template shares
      them.
- [ ] Keep images minimal and patched; they are the attack surface every
      tenant starts from.

## External providers

- [ ] Use provider API keys scoped to a dedicated provider organization or
      account for Differently.
- [ ] Keep the global policy at `external: never` until you have decided which
      classifications may leave. Then approve each external pool by name in
      `externalPools`.
- [ ] Map classifications deliberately. The default classes are
      `private-only` (never), `internal` (overflow, the default class) and
      `public` (allowed).
- [ ] Set `maxConcurrency` on external pools; external providers report
      unbounded capacity and the cap is what limits spend.
- [ ] For Freestyle, leave `config.egress` unset (deny-all) unless guests need
      the internet. Exposed Freestyle ports are public URLs.
- [ ] For `remote` pools that run on third-party infrastructure, set
      `trust: external`. The default is `private`.
- [ ] Review `GET /v1/admin/trust-report` after every policy change.

## Policy and tenancy

- [ ] Give organization and project admins `policies:write` only if they need
      to tighten placement. Until the known issue is fixed, an organization
      key with `policies:write` can delete other tenants' project policies
      (backlog E0); prefer operator-managed project policies.
- [ ] Set organization and project quotas (`maxActiveSandboxes`, `maxCPU`,
      `maxMemoryMiB`, `maxDiskGiB`, `maxQueued`, `maxTTLSeconds`,
      `maxSnapshots`). Zero means unlimited. Note the known start-after-stop
      quota bypass (backlog E0); `maxActiveSandboxes` still bounds the total.
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
- [ ] Alert on `differently_reconcile_actions_total` (orphans deleted,
      instances lost, adoptions), on pools whose health is not `healthy`, and
      on `differently_placement_decisions_total{outcome="rejected"}`.
- [ ] Alert on `agent.register` audit events with outcome `failure`: they mean
      a node presented another agent's ID with the wrong secret (a hijack
      attempt, or a rebuilt node that needs its old identity removed). Review
      `agent.endpoint_change` events too.

## Upgrades and incidents

- [ ] Test upgrades on a staging installation; run `differently-server migrate`
      (the chart's migrate Job) before rolling the Deployment.
- [ ] Know the kill switches: `POST /v1/admin/organizations/{id}/suspend` with
      `{"terminate": true}`, `POST /v1/admin/pools/{name}/drain`, `PATCH
      /v1/admin/pools/{name}` with `{"enabled": false}`, a global policy with
      `external: never`, and `POST /v1/admin/sandboxes/{id}/terminate`.
- [ ] After a suspected node compromise: drain its pool, rotate the agent
      token everywhere, check `agent.register` events and the agents list for
      endpoint changes, and treat environment values of sandboxes on that pool
      as exposed.
