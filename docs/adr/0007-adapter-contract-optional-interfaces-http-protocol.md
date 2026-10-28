# ADR 0007: Small adapter contract with optional interfaces and an HTTP plugin protocol

- Status: Accepted
- Date: 2026-10-28

## Context

Backends differ widely. Firecracker on our agents can pause and snapshot disks;
Daytona containers cannot pause; Freestyle has no stop endpoint; a community
backend might only run commands. Applications must learn precisely what is
missing rather than get a silent no-op. Community backends should not have to
be written in Go or compiled into the server, and the node agent should not
need a private protocol of its own.

## Decision

- The required interface `adapter.ExecutionAdapter` (`pkg/adapter/adapter.go`)
  has nine methods: `Info`, `Capabilities`, `Capacity`, `Health`, `Create`,
  `Get`, `List`, `Delete`, `Exec`.
- Optional behaviour is expressed by optional interfaces: `Streamer`,
  `FileSystem`, `Lifecycle`, `Pauser`, `Snapshotter`, `PortExposer`.
- Features are named strings (`exec.stream`, `snapshots.disk`, ...).
  `adapter.EffectiveFeatures` keeps a declared feature only if the adapter
  implements the matching interface, so declaring `files` without
  `FileSystem` drops it.
- `Create` is idempotent on the Differently sandbox ID, `Delete` succeeds for a
  missing instance, and `List` returns only instances owned by the given
  installation, so the reconciler can find orphans.
- `insufficient_capacity` is a distinct, transient error code that does not
  count against pool health.
- `pkg/adapter/adapterhttp` serves any adapter over HTTP under
  `/adapter/v1` (JSON bodies, NDJSON streamed exec, optional bearer token, the
  `Differently-Adapter-Protocol` response header) and provides a client that
  implements the full interface. The node agent speaks exactly this protocol;
  the `remote` adapter kind connects to any server that does.

Full contract: [adapters/specification.md](../adapters/specification.md).

## Consequences

- A backend can start with exec only and add features later; placement routes
  around missing capabilities and the API returns `501 unsupported_operation`
  for lifecycle operations a pool cannot perform.
- The agent and community plugins share one tested protocol, and the agent's
  tests drive it exactly as the control plane does
  (`TestMicroVMLifecycleOverAdapterProtocol`).
- The HTTP client implements every optional interface, so for remote pools the
  feature list is whatever the remote declares (the remote's handler filters it
  with `EffectiveFeatures` first).
- Versioning is one integer (`ProtocolVersion = "1"`). The client does not yet
  check the protocol header or `info.protocol`; mismatches would surface as
  decode errors. Known issue, [backlog E0](../backlog.md#e0-correctness-and-security-fixes-from-the-code-review).
- Adapters that call external APIs need to map provider errors onto these
  codes carefully; `internal/adapters/httpx` does it for HTTP providers
  (429 to `insufficient_capacity`, 401/403 to `provider_unavailable`, ...).

## Alternatives considered

- **One large interface with every operation.** Rejected: forces stubs that
  silently do nothing or fail generically.
- **Go plugins (`plugin` package) or compiled-in community adapters only.**
  Rejected: fragile across Go versions, Go-only, and requires rebuilding the
  server. In-process registration (`pools.Register`) still exists for built-ins.
- **gRPC.** A reasonable choice; HTTP and JSON were chosen so plugins are easy
  to write in any language and debug with curl, and to reuse the public API's
  error envelope.
