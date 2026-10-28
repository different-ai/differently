# ADR 0018: Per-agent secrets pinned at first registration until mTLS

- Status: Accepted (interim; superseded when the authenticated agent channel lands)
- Date: 2026-10-28

## Context

Node agents registered with one token shared by every node and the control
plane, and the same token authenticated the control plane's calls to agents.
A single compromised node could therefore re-register another node's ID with
its own endpoint, receiving that node's create, exec and file traffic
(including environment values), and could call any other agent's adapter API
directly. mTLS with per-node certificates is the right end state
([backlog E3](../backlog.md#e3-authenticated-agent-channel)), but it needs
certificate issuance and rotation that the first release does not have.

The fix also has to keep working when an agent pod restarts and its pod IP,
and therefore its endpoint, changes.

## Decision

- Each agent creates a random secret on first start and keeps it in
  `<state-dir>/agent-secret` (mode 0600), which survives agent restarts.
- Heartbeats carry the shared token (permission to register) and the agent's
  secret (`Differently-Agent-Secret`). The first heartbeat for an agent ID pins
  that secret; later heartbeats for the ID must present the same secret and
  pool, or get `409` and an `agent.register` failure audit event. Endpoint
  changes from the owner are allowed and audited as `agent.endpoint_change`.
- The control plane calls each agent with that agent's own secret; agents no
  longer accept the shared token on their adapter API.
- Agent secrets are stored with the agent record and never returned by the
  API. An operator retires a rebuilt node's identity with
  `DELETE /v1/admin/agents/{id}`.

## Consequences

- A compromised node can no longer impersonate or command another node. It
  can still register new agent IDs with the shared token, so unexpected
  registrations need monitoring.
- Traffic is still unencrypted HTTP; keep the agent network private or use an
  encrypting CNI or mesh until mTLS.
- Agent secrets sit unencrypted in the control-plane database next to sandbox
  environment values, under the same database protections.
- Rebuilding a node with a wiped state directory needs one operator action.

## Alternatives considered

- **Operator approval of every endpoint change.** Rejected: pod restarts
  change IPs, so every agent restart would need a human.
- **Per-agent tokens derived from the shared token (HMAC of the agent ID).**
  Rejected: any holder of the shared token could derive every other agent's
  token.
- **mTLS now.** Preferred long term; deferred because certificate issuance and
  rotation (for example via cert-manager) are their own epic.
