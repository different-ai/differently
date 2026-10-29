# Modal Sandboxes

| | |
|---|---|
| Status | **Documented**: no adapter. Modal has no REST API for Sandboxes; its SDKs speak a gRPC API that Modal says is not public. Run it as a `remote` plugin built on Modal's Go SDK |
| Adapter | none (a `remote` plugin) |
| Trust | external (provider-managed) |
| Isolation | gVisor by default; a VM runtime gives each sandbox its own Linux kernel (GPU sandboxes require gVisor) |
| API checked | [Sandboxes guide](https://modal.com/docs/guide/sandbox), [Go SDK](https://modal.com/docs/sdk/go/latest) and its [Client](https://modal.com/docs/sdk/go/latest/Client), [Sandbox](https://modal.com/docs/sdk/go/latest/Sandbox) and [Errors](https://modal.com/docs/sdk/go/latest/Errors) pages, [snapshots](https://modal.com/docs/guide/sandbox-snapshots), [networking](https://modal.com/docs/guide/sandbox-networking), [resources](https://modal.com/docs/guide/resources), [pricing](https://modal.com/pricing). Read 2026-10-28 |

## Why there is no in-process adapter

- Modal documents Sandboxes only through its Python, JavaScript and Go SDKs.
  There is no HTTP management API to build an adapter against.
- The SDKs speak gRPC to `api.modal.com`. Modal's Go SDK says "the Modal
  gRPC API is not considered a public API, and can change without warning",
  and the proto file recommends the official SDKs instead.
- The official Go SDK (`github.com/modal-labs/modal-client/go`, 0.11.1 on
  2026-10-15) is in beta: 0.X.0 releases may break callers, and it pulls in
  `google.golang.org/grpc`. Linking it into the control plane would tie
  every Differently release to a beta client.

A plugin built on that SDK keeps the dependency, and its upgrades, out of
the server.

## Capabilities

What the Go SDK documents, and how the plugin maps it:

| Feature | Provider support | Notes |
|---|---|---|
| `exec` | yes | `Exec` returns a process with stdin, stdout and stderr streams; no default timeout |
| `exec.stream` | yes | Read stdout and stderr as they arrive |
| `files` | yes | `ReadBytes`, `WriteBytes`, `ListFiles`, `MakeDirectory` |
| stdin | yes | The process's `Stdin` writer |
| `lifecycle.stop-start` | no | Sandboxes run until they finish or are terminated; there is no documented stop and start |
| `lifecycle.pause-resume` | no | Only experimental, Python-only and undocumented in the guides |
| `snapshots.disk` | yes | `SnapshotFilesystem` returns an image, kept 30 days by default (the TTL is configurable); mounted volumes are excluded |
| `snapshots.memory` | no | Experimental: they terminate the sandbox, expire after 7 days and cannot be pinned to a region |
| `fork` | yes | Create a sandbox from the snapshot image |
| `ports.expose` | ports declared at create | `EncryptedPorts` give tunnel URLs on `*.modal.host` (`Tunnels()`); connect tokens give authenticated HTTP access |
| max size | not published | Modal enforces a maximum at creation (`InvalidError`) but does not publish it; disk up to 3 TiB |
| provider TTL backstop | yes | `Timeout`, default 5 minutes, at most 24 hours; also `IdleTimeout`. A sandbox is terminated when it passes |

## Configure

Run the plugin next to Differently with a Modal token
(`MODAL_TOKEN_ID` and `MODAL_TOKEN_SECRET`, from a service user on Team and
Enterprise plans, scoped to one environment), then register it:

```sh
differently admin secret put modal-plugin-token --from-file ./plugin-token.txt
differently admin pool add modal --adapter remote --trust external \
  --config endpoint=https://modal-adapter.internal:8443 \
  --max-concurrency 50 --secret modal-plugin-token
```

```yaml
pools:
  - name: modal
    adapter: remote
    trust: external
    maxConcurrency: 50
    config:
      endpoint: https://modal-adapter.internal:8443
    credentials: {secret: modal-plugin-token}
```

Plugin shape: a Go program serving `adapterhttp.Handler` (option A in the
[adapter specification](../adapters/specification.md#10-writing-a-community-adapter))
around a `modal.Client`, with these settings of its own: Modal app name,
environment, default image, region, CPU and memory defaults, and the
sandbox timeout.

- **Create**: look the sandbox up by name (`FromName`, the Differently
  sandbox ID) in the plugin's deployed app; create it only if none is
  running, with tags `dly-sandbox` and `dly-installation`, the image (a
  template, or the snapshot image for `fromSnapshot`), `CPU`, `MemoryMiB`,
  `Env`, `Timeout` from the TTL (capped at 24 h), and declared encrypted
  ports. An `AlreadyExistsError` means a replay won: return that sandbox.
  Wait with `WaitUntilReady`.
- **List**: `List` filtered by the app and the tag `dly-installation`;
  report `sandboxId` from the `dly-sandbox` tag. Fail the call if any page
  fails.
- **Exec**: `Exec` with `/bin/sh -c` for a command string, the argument list
  as it is otherwise; stream stdout and stderr; write stdin and close it.
- **Files**, **snapshots** (`SnapshotFilesystem` with a TTL; the guides
  describe no image delete, so unless the SDK offers one, `DeleteSnapshot`
  answers success and the TTL removes the image), **ports** (`Tunnels()`
  for a declared port).
- **Delete**: `Terminate`; a `NotFoundError` is success.
- **Errors**: `ResourceExhaustedError` (capacity, quotas and rate limits) →
  `insufficient_capacity`; `InvalidError` (a bad image or an oversized
  request) → `invalid_request`; `NotFoundError` → `not_found`; a sandbox
  that exits before it is ready → `sandbox_failed`; `InternalFailure`,
  `RemoteError` and authentication errors → `provider_unavailable`.
- **Info**: declare `exec`, `exec.stream`, `files`, `snapshots.disk`,
  `fork`, `ports.expose` (when ports are configured) and
  `isolation.container` (gVisor).

## Limits and differences

- A sandbox lives at most 24 hours; a longer TTL is cut off by Modal, and the
  reconciler then fails the sandbox as lost.
- Names are unique per app only among running sandboxes, and only in a
  deployed app.
- Up to 10 tags per sandbox.
- Region pinning costs extra (×1.15 for broad regions such as `us`, ×1.75
  for narrow ones such as `us-east`).
- Plan caps on containers (Starter 100, Team 5,000); no sandbox-specific
  creation rate is documented.
- Pricing: per second on the larger of requested and used CPU and memory, at
  non-preemptible rates (about $0.0000394 per core-second and $0.0000067 per
  GiB-second).

## How it was tested

Not tested: no adapter or plugin exists. The facts above come from Modal's
guides and Go SDK reference listed in the header.
