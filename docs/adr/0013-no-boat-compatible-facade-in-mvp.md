# ADR 0013: No Boat-compatible facade in the MVP

- Status: Accepted (deferred; revisit on demand)
- Date: 2026-10-28

## Context

Boat (boat.dev) is a hosted sandbox service with a polished developer
experience and SDKs, and a reference point for API design. Offering its API
shape on a self-hosted platform could ease migration. The research inventory
of Boat's public documentation (2026-10-28) shows:

- a portable core that maps onto Differently: sandbox CRUD, stop/resume/fork,
  exec (sync, detached and NDJSON stream), files, ports, SSH keys, snapshots,
  webhooks, cursor pagination and `Idempotency-Key`;
- a large product-specific surface: integrated coding agents (prompt, steer,
  conversations), AI credential proxy, environments and repositories, CI
  runners, orgs, seats and wallets, desktop streaming;
- compatibility details SDK users depend on: `bx_` IDs, an `ok`/`type`
  envelope, `X-Ascii-Confirm-Delete`, `X-Boat-Org`, `boat_` key prefixes and
  state names such as `archived`;
- no stated licence on the published OpenAPI document.

## Decision

Do not ship a Boat-compatible facade in the MVP. Differently's own
vendor-neutral API (`/v1`) is the contract. Document the mapping and semantic
differences in [feature-parity.md](../feature-parity.md) so users can port by
hand, and re-evaluate when there is concrete demand
([backlog E16](../backlog.md#e16-boat-compatible-facade-evaluation)).

If built later, the facade would live at `/compat/boat/v1`, implement only the
portable core on top of the existing handlers, return `501` for the
product-specific surface, and never vendor Boat's spec or SDK code until its
licence is clear.

## Consequences

- No second API to keep compatible while the core is still changing.
- Users migrating from Boat must change client code; the mapping table reduces
  the effort.
- Some Boat-core features are missing in Differently regardless of a facade:
  detached processes, SSH, webhooks. They are backlog items on their own merit.

## Alternatives considered

- **Full facade now.** Rejected: high cost, licence uncertainty, and much of
  the surface adds nothing to a self-hosted substrate.
- **Facade for the portable core now.** Rejected for the MVP; revisit with a
  real migration candidate, because the details SDK users hit (IDs, envelopes,
  headers, state names) are where the cost is.
