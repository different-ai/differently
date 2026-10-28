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

### Not yet verified

- No run on real KVM hardware or live Daytona/Freestyle accounts yet.
