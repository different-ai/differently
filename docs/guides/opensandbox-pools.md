# OpenSandbox pools

Differently can place sandboxes on an
[OpenSandbox](https://github.com/opensandbox-group/OpenSandbox) server. You
get OpenSandbox's runtimes (Docker, Kubernetes, or Firecracker microVMs with
Fast Sandbox) under Differently's tenancy, quotas, placement policy, audit
and trust report. Background: [spike](../research/opensandbox-spike.md),
[ADR 0019](../adr/0019-opensandbox-backend-first.md).

> Status: checked live against OpenSandbox release 1.2.0 with the Docker
> runtime and `execd` v1.1.0 (arm64, Colima). Kubernetes and Fast Sandbox
> runtimes are not verified yet.

## Configure a pool

```yaml
pools:
  - name: opensandbox
    adapter: opensandbox
    trust: private            # required: private if you run the server, external if someone else does
    region: dc1
    maxConcurrency: 50
    config:
      apiURL: http://opensandbox-server.opensandbox.svc:8090/v1
      image: ubuntu:24.04     # default image URI (a sandbox template overrides it)
      isolation: container    # container (Docker/Kubernetes runtime) or microvm (Fast Sandbox)
      # templateId: fsb-...   # Fast Sandbox template instead of an image
      # features: exec,exec.stream,files,lifecycle.pause-resume,snapshots.disk,fork,ports.expose
      # useServerProxy: "true"
    credentials:
      env: DIFFERENTLY_OPENSANDBOX_API_KEY   # the server's api_key, by reference
```

With Helm, put the pool under `config.pools`, expose the key with
`server.extraEnv` from a Secret, and set `agent.enabled: false` if there are no
Firecracker nodes. `deploy/helm/differently/ci/opensandbox-values.yaml` is a
complete example.

Configuration is rejected when:

- `trust` is missing (Differently cannot tell who operates the server);
- `apiURL` is missing;
- `isolation` is not `container` or `microvm`.

An `external` OpenSandbox pool follows the usual rules: it receives work only
when the global policy approves it in `externalPools`.

## What works, and what differs from Firecracker pools

| | OpenSandbox pool |
|---|---|
| Create, delete, list, idempotent create | yes (Differently's IDs are kept in OpenSandbox metadata) |
| Exec, streamed exec, env, cwd, timeouts, exit codes | yes; output is line-based |
| Exec stdin | no (`unsupported_operation`) |
| Files (upload, download, list) | yes |
| Pause / resume | yes |
| Stop / start | no: OpenSandbox has none, so stop returns `unsupported_operation` |
| Snapshots and fork | disk snapshots by default; add `snapshots.memory` to `features` when the runtime captures memory (Fast Sandbox) |
| Port URLs | only when OpenSandbox serves the port directly without request headers; endpoints that need the server's API key are refused, because that key must never reach tenants |
| Provider-side TTL backstop | yes (`timeout`, minimum 60 s) |

## Verify

```bash
differently-server preflight --config config.yaml   # the credential reference resolves
differently admin pool list                         # the pool is healthy
differently sandbox create --pool opensandbox --wait
differently sandbox placement <id>
differently admin trust-report                      # endpoint shown, credential only by reference
```

To check the adapter against a real OpenSandbox server, see
[test/opensandbox](../../test/opensandbox/README.md).
