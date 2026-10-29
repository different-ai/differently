# Runloop

| | |
|---|---|
| Status | **Not supported**: Runloop is winding down its service. Its docs say transition support for existing customers ends on 2026-11-13, and they now carry migration guides to other providers |
| Adapter | none |
| Trust | external (provider-managed) |
| Isolation | VM-based devboxes; the docs don't name the hypervisor |
| API checked | [OpenAPI spec](https://docs.runloop.ai/openapi-specs/stainless-processed-openapi.json) (last modified 2026-10-24), [docs](https://docs.runloop.ai) and the [transition notice](https://docs.runloop.ai/docs/transitions/overview), read 2026-10-28 |

Runloop's API would map onto the adapter contract. Building and maintaining
an adapter for a service that is ending isn't worth it, so Differently has
none. Runloop's own migration guides list [Freestyle](freestyle.md), which
Differently supports, among the destinations.

If Runloop keeps serving after the transition date, the mapping below is
what an adapter would use.

## Capabilities

What Runloop's published API offers, mapped to Differently's features:

| Capability | Runloop's API |
|---|---|
| exec | yes; `POST /v1/devboxes/{id}/execute` waits up to 25 s, then `wait_for_status` long-polls. Commands run in a login shell from the home directory, with no per-command `cwd` or `env` |
| exec.stream | yes; server-sent events per stream (`stream_stdout_updates`, `stream_stderr_updates`) with byte offsets |
| files | partly: read, write, upload and download; no directory listing |
| stdin | yes; `execute_async` with `attach_stdin`, then `send_std_in` |
| lifecycle.stop-start | yes; `suspend` and `resume` keep the disk only |
| lifecycle.pause-resume | no; suspend does not keep memory |
| snapshots.disk | yes; `snapshot_disk`, and a devbox starts from one with `snapshot_id` |
| snapshots.memory | no ("Currently only disk snapshots are supported") |
| fork | through a disk snapshot: snapshot, then create devboxes from it |
| ports.expose | yes: one tunnel per devbox, `https://{port}-{tunnel_key}.tunnel.runloop.ai`; open (public) by default, or token-protected |
| network.egress-control | yes; network policies referenced by `network_policy_id` |
| Max size | 16 vCPU / 64 GiB memory / 64 GiB disk (custom sizes) |
| Provider TTL backstop | `keep_alive_time_seconds`, at most 48 hours |

## Configure

Nothing to configure: there is no `runloop` adapter. A
[remote plugin](remote.md) could wrap Runloop's API for the remaining
weeks, but Differently doesn't ship one.

## Limits and differences

- The service ends: transition support stops on 2026-11-13 at 11:59 PM PT.
  The docs don't say when the API stops answering.
- There is no delete: `shutdown` is final, and shut-down devboxes stay in
  listings.
- Create has no idempotency key, names aren't documented as unique, and
  the devbox list can't filter by metadata, so idempotent create and
  installation-scoped listing would need client-side work.
- A devbox lives at most 48 hours.
- Pricing: compute is billed per second while a devbox is initializing,
  running, suspending or resuming; suspended devboxes and snapshots pay for
  storage.

## How it was tested

Nothing ran: there is no adapter. The API mapping above comes from the
published OpenAPI spec and docs, read on 2026-10-28.
