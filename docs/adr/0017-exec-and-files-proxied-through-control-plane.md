# ADR 0017: Proxy exec and file traffic through the control plane

- Status: Accepted. Amended by [ADR 0020](0020-opensandbox-replaces-node-agent.md): adapters now call OpenSandbox or a provider rather than a node agent, and the guest's 10 MiB synchronous exec output cap went with the guest
- Date: 2026-10-28

## Context

Every exec, file read or write and streamed command needs authentication,
tenancy checks, rate limiting and an audit event. Node agents run on KVM hosts
that are usually not reachable from where clients run, and external providers
have their own per-sandbox credentials (Daytona's toolbox URLs, Freestyle's
VM-scoped endpoints) that should not be handed to tenants.

## Decision

All data-plane operations go through the API replica that receives the
request: `POST /v1/sandboxes/{id}/exec` (sync or SSE stream), `GET/PUT
.../files/content`, `GET .../files`. The replica checks scope and tenancy,
records the audit event and calls the pool adapter, which calls the node agent
or provider (`internal/server/sandboxes.go` `exec`, `streamExec`, `readFile`,
`writeFile`). Responses stream back through the same path. Clients never receive
agent addresses or provider credentials.

## Consequences

- One enforcement point: scopes (`sandboxes:exec`), tenancy (cross-tenant IDs
  return 404) and audit apply to every operation, and backend credentials stay
  in the control plane.
- Control-plane replicas carry exec output and file bytes. Uploads and
  downloads are buffered in memory and capped at 100 MiB per request
  (`maxUpload` in the server, `MaxFileBytes` in the protocol and guest);
  synchronous exec output is capped at 10 MiB per stream in the guest.
- Long streams hold an HTTP connection on the replica; load balancers must
  allow long-lived responses and must not buffer SSE (the server sets
  `X-Accel-Buffering: no`).
- Interactive protocols that need direct connections (SSH, preview URLs, PTY)
  need a gateway component rather than this path
  ([backlog E6 and E7](../backlog.md#e6-preview-urls-for-firecracker-pools)).

## Alternatives considered

- **Signed direct URLs to agents or providers.** Lower latency and no
  control-plane bandwidth, but requires agents to be reachable from clients and
  to verify signed tokens, and spreads enforcement across components.
  Reconsider for large file transfer.
- **A separate data-plane gateway now.** More moving parts before the need is
  measured; the gateway in the backlog will start with ports and SSH.
