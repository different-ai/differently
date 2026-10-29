# ADR 0011: Go for control plane and agents; TypeScript and Python SDKs

- Status: Accepted. Amended by [ADR 0020](0020-opensandbox-replaces-node-agent.md): `differently-agent` and `differently-guest` were removed; the server and CLI remain Go
- Date: 2026-10-28

## Context

The control plane, node agent and in-guest agent need static binaries,
low memory, good concurrency, a mature Postgres driver, Prometheus metrics, and
access to Linux primitives (vsock, process groups, signals). The ecosystem
around Firecracker, Kubernetes and containerd is largely Go. Users of the API
are mostly AI-agent and developer-tool builders writing TypeScript and Python.

## Decision

- Write `differently-server`, `differently-agent`, `differently-guest` and the
  `differently` CLI in Go (`go.mod`: Go 1.27.3). Keep dependencies few:
  `pgx/v5`, `mdlayher/vsock`, `prometheus/client_golang`, `sigs.k8s.io/yaml`,
  `cobra` (CLI only).
- Ship SDKs in TypeScript (`sdk/typescript`) and Python (`sdk/python`) over
  the public REST API, plus a Go client (`pkg/client`) used by the CLI.
- The OpenAPI document (`api/openapi.yaml`) is the contract;
  `TestOpenAPICoversRoutes` fails if a server route is missing from it.

## Consequences

- One toolchain for every server-side component. The code uses no cgo, so
  the binaries can be built statically (`CGO_ENABLED=0`) and the guest agent
  can be copied into any Linux rootfs. How release binaries are built is a
  packaging decision outside this record.
- Contributors to the core need Go; community adapters can be written in any
  language through the HTTP protocol ([ADR 0007](0007-adapter-contract-optional-interfaces-http-protocol.md)).
- The SDKs are hand-written and must be kept in step with the OpenAPI document;
  generated clients are an option later. The Go client is functional but not
  yet a polished public SDK ([backlog E15](../backlog.md#e15-go-sdk-polishing)).

## Alternatives considered

- **Rust for the agent and guest.** Strong fit for low-level code
  (rust-vmm, Firecracker itself), but splitting the core across two languages
  costs more than it saves at this stage.
- **TypeScript control plane.** Weaker fit for vsock, process control and
  static deployment.
- **Generated SDKs only.** Faster to cover the API but poorer ergonomics for
  streaming exec and wait helpers; may complement the hand-written SDKs later.
