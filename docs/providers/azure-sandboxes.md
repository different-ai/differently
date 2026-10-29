# Azure Container Apps Sandboxes

| | |
|---|---|
| Status | **Tested with a fake server** (implemented from the published API, not run against Azure) |
| Adapter | `azure-sandboxes` (`internal/adapters/azuresandboxes`) |
| Trust | declared: every pool must state it. Microsoft manages the sandboxes (`external`), but they run in your own subscription, so declare `private` if your policy treats that subscription as private |
| Isolation | Each sandbox "runs in its own secure boundary" (Microsoft does not name the technology) |
| API checked | Data-plane OpenAPI spec `2026-09-01-preview` in [azure-rest-api-specs](https://github.com/Azure/azure-rest-api-specs) (`specification/app/data-plane/ContainerApps`) and the [sandboxes overview](https://learn.microsoft.com/en-us/azure/container-apps/sandboxes-overview), read 2026-10-28 |

Sandboxes are Azure's general-purpose agent sandbox service, newer than
[dynamic sessions](azure-sessions.md). It is the better fit on Azure:
shell commands with exit codes, a files API, stop and resume, snapshots,
ports and an auto-delete policy. Each Differently sandbox is one sandbox in
a sandbox group (`Microsoft.App/sandboxGroups`).

## Capabilities

| Feature | Supported | Notes |
|---|---|---|
| exec | yes | `executeShellCommand` returns the exit code, stdout and stderr. The working directory, environment, stdin and timeout are added to the command line; the timeout uses coreutils `timeout` (exit 124) |
| exec.stream | no | Azure streams over a WebSocket (`exec/stream`); the adapter uses the buffered fallback |
| files | yes | `files` (download and upload with `createDirs`), `files/list` |
| stdin | yes | Piped in through the command line |
| lifecycle.stop-start | yes | `stop` saves the sandbox as a snapshot; `resume` starts it from that snapshot. `persistence.disk` is advertised |
| lifecycle.pause-resume | no | Whether memory survives stop is not documented |
| snapshots.disk | yes | `snapshot`; restore with `sourcesRef.snapshot` |
| snapshots.memory | no | |
| fork | yes | A new sandbox from a snapshot |
| ports.expose | yes | `ports/add` with anonymous access: **the URL is public**, so anyone who has it can connect |
| network.egress-control | yes | `egressPolicy.defaultAction` is `Deny` unless the pool sets `egress: public` |
| max size | 16 cores, 32 GiB, 320 GiB (tier 3XL); default tier M is 1 core, 2 GiB, 20 GiB | CPU, memory and disk are sent as Kubernetes quantities |
| provider TTL backstop | yes | An auto-delete policy (`AfterCreation`) set to the TTL plus 5 minutes, moved when the TTL changes (`SetExpiry`). Auto-suspend is off |

## Configure

Create the sandbox group in Azure first (resource `Microsoft.App/sandboxGroups`)
and give a service principal the **Container Apps SandboxGroup Data Owner**
role on it. The credential is the principal as JSON: a client secret, or a
federated token file for AKS workload identity.

```json
{"tenantId": "...", "clientId": "...", "clientSecret": "..."}
{"tenantId": "...", "clientId": "...", "federatedTokenFile": "/var/run/secrets/azure/tokens/azure-identity-token"}
```

```sh
differently admin secret put azure-sandboxes-sp --from-file ./azure-sp.json
differently admin pool add azure-sandboxes --adapter azure-sandboxes --trust external \
  --config endpoint=https://<data-plane endpoint for your region> \
  --config subscriptionId=<subscription> --config resourceGroup=<group> \
  --config sandboxGroup=<sandbox group> --config diskImage=<public image name> \
  --max-concurrency 50 --secret azure-sandboxes-sp
```

In the server config file:

```yaml
pools:
  - name: azure-sandboxes
    adapter: azure-sandboxes
    trust: external                # required: external, or private if your policy allows
    region: westus3
    maxConcurrency: 50
    config:
      endpoint: https://...        # the sandbox data-plane endpoint for the region
      subscriptionId: 00000000-0000-0000-0000-000000000000
      resourceGroup: rg-sandboxes
      sandboxGroup: differently
      diskImage: ubuntu            # a public disk image name; a request's template overrides it
      egress: none                 # or public
      # apiVersion: 2026-09-01-preview
    credentials: {secret: azure-sandboxes-sp}
```

With Helm, mount the JSON as a file (`credentials: {file: ...}`) or pass it
in an environment variable. For workload identity, set `federatedTokenFile`
to the token path that AKS mounts. Allow the server's egress to the
data-plane endpoint and to `login.microsoftonline.com`.

| Key | Default | Meaning |
|---|---|---|
| `endpoint` | required | The data-plane endpoint (the SDK derives it from the region; take it from the portal or SDK) |
| `subscriptionId`, `resourceGroup`, `sandboxGroup` | required | The sandbox group |
| `diskImage` | none (the service default) | Public disk image name; a request's `template` overrides it |
| `egress` | `none` | `public` allows outbound traffic |
| `apiVersion` | `2026-09-01-preview` | Data-plane API version |

## Limits and differences

- **Preview.** The data-plane API is `2026-09-01-preview`. The Learn page has
  no reference for exec, files or ports, so the spec is the source.
- **Idempotent create**: sandbox IDs are assigned by Azure. The adapter looks
  for the label `dly-sandbox=<id>` first, then creates with `dly-sandbox` and
  `dly-installation` labels. Two creates racing could make two sandboxes; the
  reconciler does not yet catch duplicates (backlog E0.21).
- **List** uses the label selector `dly-installation=<id>`, follows
  `nextLink`, and re-checks the label locally.
- **Resources** are sent only when the request sets CPU; otherwise Azure's
  default tier applies. Memory defaults to 2 GiB per core.
- **Errors**: 429 is `insufficient_capacity`; 400 `invalid_request`; 404
  `not_found`; 409 `invalid_state`; 401, 403 and 5xx `provider_unavailable`.
  Azure's own error code is kept in `providerCode`.
- **Quotas**: 300 concurrent cores and 3,000 requests a minute per region and
  subscription (sandboxes.azure.com; another Azure page says 2,000 cores).
- **Pricing**: not checked for this page; see Azure Container Apps pricing.
- Not used: Azure's managed identity and egress-rule features, volumes,
  content packages, the exec WebSocket.

## How it was tested

`go test ./internal/adapters/azuresandboxes/` runs the adapter against a fake
server built from the spec. The fake includes an Entra ID token endpoint,
checks `api-version` and the bearer token on every call, and runs exec
commands with a local `/bin/sh`. The tests cover:

- create, a replayed create, the create body (resources, auto-delete policy,
  disk image, egress), exec with stdin;
- file write, read, missing file and list;
- stop and resume, moving the expiry, snapshot, fork from a snapshot, ports
  (including adding a port twice);
- paged List per installation, idempotent Delete, token reuse;
- a rejected client secret and the 429, 400, 403 and 500 classifications.

Nothing has run against a real sandbox group.
