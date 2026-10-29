# microsandbox

| | |
|---|---|
| Status | **Not supported**: since its 2026 rewrite microsandbox has no self-hosted server API for a control plane to call; the runtime is embedded in each SDK process |
| Adapter | none |
| Trust | would be declared (self-hostable) |
| Isolation | libkrun microVM with its own kernel per sandbox: KVM on Linux, Hypervisor.framework on Apple Silicon macOS, WHP on Windows (preview) |
| API checked | [superradcompany/microsandbox](https://github.com/superradcompany/microsandbox) (formerly `zerocore-ai/microsandbox`) v0.7.11 (release 2026-10-24), source at commit `8ff916f3`: the `msb` CLI, the SDKs, the protocol crates, `COMPATIBILITY.md`, `docs/`; read 2026-10-28 |

## Why there is no adapter

Older releases (0.2.x) had `msb server start`: a JSON-RPC API on port 5555
(`/api/v1/rpc`, `sandbox.start`, `sandbox.command.run`…) with bearer keys.
The April 2026 rewrite (v0.3.3 onwards, marked breaking) removed it. In
v0.7.11:

- The project states there is "no daemon to install and no server to
  connect to" (`docs/sdk/overview.mdx`). The `msb` CLI has no server
  command.
- The Go, Python, Node and Ruby SDKs embed the Rust runtime (CGO, pyo3,
  napi). Each sandbox is an `msb machine` process that the SDK spawns,
  reached over two Unix sockets on that host (`agent.sock`, `control.sock`)
  with a length-prefixed CBOR protocol. Nothing listens on the network.
- `msb ssh serve` listens on TCP (default `127.0.0.1:2222`) for one sandbox
  at a time: exec and SFTP, but no lifecycle.
- The MCP server (`microsandbox-mcp`) speaks stdio, for a local agent.
- **microsandbox cloud** (private beta) has a REST API at
  `https://api.microsandbox.dev/v1` (sandboxes with labels, start/stop,
  disk snapshots, volumes), but exec and files go over a WebSocket carrying
  the CBOR agent protocol, which its OpenAPI document leaves out. The cloud
  also lacks pause/resume, published ports and memory snapshots. Its server
  is not published, so it cannot be self-hosted.

An in-process adapter would have to link the CGO runtime into the
Differently server (built with `CGO_ENABLED=0`) and run microVMs on the
control plane's own host, which Differently does not do.

## What support would take

A [`remote` plugin](../adapters/specification.md#10-writing-a-community-adapter)
on each microVM host, serving Differently's adapter protocol and driving
microsandbox there, through the Go SDK (CGO) or the `msb` CLI. The CLI
already has what the contract needs: `create` with labels, `list --label …
--format json`, `exec`, `copy`, `stop`/`start`, `pause`/`resume`,
`snapshot`, `fork`, published ports (`-p`), and `--max-duration` and
`--idle-timeout` lifetimes. Each host would then be one pool with a
declared trust.

| Capability | What microsandbox offers locally | Through a plugin |
|---|---|---|
| exec | yes (separate stdout and stderr, env, cwd, stdin; timeouts enforced by the SDK) | possible |
| exec.stream | yes | possible |
| files | yes (`copy`, SDK filesystem calls) | possible |
| stdin | yes | possible |
| lifecycle.stop-start | yes (disk kept, fresh VM boot) | possible |
| lifecycle.pause-resume | yes (memory resident) | possible |
| snapshots.disk | yes | possible |
| snapshots.memory | yes (full snapshots) | possible |
| fork | yes | possible |
| ports.expose | yes (`-p`, bound to 127.0.0.1 by default) | host-local only |
| max size | the host's | the host's |
| provider TTL backstop | yes (`--max-duration`) | possible |

## Configure

Nothing to configure in Differently today.

## Limits and differences

- Linux hosts need `/dev/kvm`; in a VM, the hypervisor must expose nested
  virtualization. On macOS only Apple Silicon is supported.
- Published ports bind to `127.0.0.1` unless told otherwise, so tenant-facing
  URLs would need a proxy on each host.

## How it was tested

Not tested: no adapter exists. The findings come from reading the source
and documentation at the commit above. Neither the runtime nor the cloud
API was run.
