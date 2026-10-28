# Changelog

All notable changes are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased] — on `pre-release`

### Added

- Control plane (`differently-server`): v1 API, API keys with scopes,
  organizations/projects, atomic quota admission and activation, fair queueing,
  policy-driven placement with fail-closed external providers, scheduler and
  reconciler, tamper-evident audit chain, trust report, usage estimates,
  Prometheus metrics, built-in dashboard, memory and Postgres stores.
- Node agent (`differently-agent`) running Firecracker microVMs with per-VM
  network isolation, vsock guest channel, stop/start, pause/resume and disk
  snapshots; guest agent (`differently-guest`).
- Adapters: Firecracker pools, Daytona, Freestyle, remote (plugin protocol),
  devhost (dev only).
- CLI (`differently`), Go, TypeScript and Python SDKs, OpenAPI 3.1 spec.
- Helm chart, AWS Terraform module, systemd units, guest image build scripts.
- Documentation: architecture RFC, ADRs, security model, adapter spec,
  placement policy, feature parity, deployment guides, roadmap and backlog.

### Security

- Node agents are bound to per-agent secrets pinned at first registration;
  the control plane calls each agent with its own secret, so one node can no
  longer impersonate or command another (`DELETE /v1/admin/agents/{id}`
  retires a rebuilt node's identity).
- The agent signals a Firecracker PID only after confirming it is that VM's
  process.
- `audit.failClosed` refuses state-changing requests that cannot be audited;
  `differently_audit_append_failures_total` counts audit failures.
- `metrics.listen` serves `/metrics` on its own port; the Helm chart does this
  by default (port 9091), keeping metrics off the ingress.

### Added (OpenSandbox)

- `opensandbox` adapter: OpenSandbox servers as pools with operator-declared
  trust, covering create, exec and streamed exec, files, pause/resume,
  snapshots and port endpoints. Checked live against OpenSandbox 1.2.0 with
  the Docker runtime ([spike](docs/research/opensandbox-spike.md),
  [ADR 0019](docs/adr/0019-opensandbox-backend-first.md)).

### Fixed (found by the end-to-end suite)

- `start` refused every request after the first one: the lifecycle handler
  reused state between requests.
- A restarting microVM was hidden from the agent's instance list, so the
  reconciler failed the sandbox mid-start; the reconciler also now waits
  30 seconds after any change before correcting a sandbox.
- Stopping on aarch64 (no Ctrl-Alt-Del) could lose unflushed guest writes;
  the agent now syncs the guest first.
- Sandboxes whose microVM stopped behind the control plane's back (for
  example on an agent restart) stayed `running`; they are now reported
  `stopped` and can be started again.
- `differently sandbox list -o json` printed an object instead of an array
  for a single sandbox.
- `deploy/images/build-rootfs.sh` failed on macOS with Colima (temporary
  files outside the shared home directory; unpacking into a host mount).

### Verified

- End-to-end suite (`make e2e`) on real Firecracker microVMs: arm64, nested
  virtualization on Colima. Not yet verified: x86_64, cloud or bare-metal
  KVM, Kubernetes, live Daytona/Freestyle accounts.
