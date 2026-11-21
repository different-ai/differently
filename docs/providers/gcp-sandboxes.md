# Google Cloud Agent Platform sandboxes

> **Experimental adapter.** It is not in the default `differently-server`
> build or the published image. It needs a server built with
> `-tags experimental_adapters`: see
> [what the server contains](README.md#what-the-server-contains).

| | |
|---|---|
| Status | **Tested with a fake server** (implemented from the published API, not run against Google Cloud) |
| Adapter | `gcp-sandboxes` (`internal/adapters/gcpsandboxes`) |
| Trust | declared: every pool must state it. Google manages the sandboxes (`external`), but they run in your own project, so declare `private` if your policy treats that project as private |
| Isolation | "Secure container sandboxing" (Google does not name the runtime) |
| API checked | aiplatform v1 discovery document (revision 20261012, and 20261107 on 2026-11-17 for templates, egress and access), the [shell sandbox quickstart](https://docs.cloud.google.com/gemini-enterprise-agent-platform/scale/sandbox/shell-sandbox-quickstart), [manage sandboxes](https://docs.cloud.google.com/gemini-enterprise-agent-platform/scale/sandbox/manage-sandboxes), [templates](https://docs.cloud.google.com/gemini-enterprise-agent-platform/scale/sandbox/manage-templates) and [snapshots](https://docs.cloud.google.com/gemini-enterprise-agent-platform/scale/sandbox/manage-snapshots), read 2026-10-28 |

This is the `sandboxEnvironments` resource of Gemini Enterprise Agent
Platform, formerly Vertex AI Agent Engine code execution. The adapter uses
its **shell** sandboxes, generally available since 2026-09-09, which return
an exit code. Code Execution sandboxes run only Python or JavaScript, have no
exit code and no network, so they are not used. Each Differently sandbox is
one sandbox environment under an Agent Platform engine (a `reasoningEngines`
resource).

## Capabilities

| Feature | Supported | Notes |
|---|---|---|
| exec | yes | `:execute` forwards `{command, cwd, timeout}` to the shell container's `/exec` and returns `stdout`, `stderr` and `returncode`. Each command runs in a new shell, as user `appuser` without sudo |
| exec.stream | no | Only `bidiExecute` (v1beta1), which is undocumented over REST |
| files | yes | Through commands (`base64` and `find`), in 64 KiB pieces |
| stdin | yes | Piped in through the command line |
| lifecycle.stop-start | yes | `:pause` and `:resume`; Google keeps the ID, endpoint and filesystem. `persistence.disk` is advertised |
| lifecycle.pause-resume | no | Whether processes and memory survive pause is not documented |
| snapshots.disk | yes | A snapshot always captures memory and filesystem |
| snapshots.memory | yes | Reported as `kind: memory` whatever was requested |
| fork | yes | A new sandbox from a snapshot. Google restores only from a snapshot of the same owner, so a snapshot takes its source's `owner` (the installation). Google deletes snapshots after 30 days |
| ports.expose | no | The data plane proxies a request only when it carries the caller's Google credential (`authorizeAccess`) and a routing token, so there is no plain URL to return. A custom-container template can declare ports, but reaching them needs the same credential |
| network.egress-control | yes, by template | Google sets egress on the template (`egressControlConfig.internetAccess`, off unless set), not per sandbox. The pool advertises the feature when its template has internet access off, and then refuses a request whose `template` allows it. A pool whose template allows internet access does not advertise it |
| max size | set by the template | Requested resources are not applied |
| provider TTL backstop | no | Google cannot move a sandbox's `ttl` once set, so a backstop would delete a sandbox whose TTL was extended. None is set: Differently's own expiry deletes the sandbox, and the reconciler any orphan |

## Configure

Set up once per project and region:

1. Create an engine: `POST https://{location}-aiplatform.googleapis.com/v1/projects/{project}/locations/{location}/reasoningEngines`
   with `{"displayName": "differently"}`. Its ID is the last part of the
   returned name.
2. Create a shell template in it: `POST …/reasoningEngines/{engine}/sandboxEnvironmentTemplates`
   with `{"displayName": "shell", "defaultContainerEnvironment": {"defaultContainerCategory": "DEFAULT_CONTAINER_CATEGORY_SHELL_SANDBOX"}}`.
   Add `"egressControlConfig": {"internetAccess": true}` if sandboxes may
   reach the internet.
3. Give a service account `roles/aiplatform.user`.

The credential says how the server gets that account's tokens:

| Credential | Where it runs | Notes |
|---|---|---|
| A service-account JSON key (`"type": "service_account"`) | anywhere | A long-lived key that you store and rotate |
| `{"type": "metadata_server"}` | GKE with Workload Identity, Compute Engine | Tokens come from the metadata server: the Google service account bound to the server's Kubernetes service account, or the VM's. Optional `serviceAccount` (default `default`) and `metadataHost` (default `metadata.google.internal`, or `GCE_METADATA_HOST`) |
| A workload identity federation configuration (`"type": "external_account"`) | EKS, AKS, any cluster with an OIDC issuer | The file `gcloud iam workload-identity-pools create-cred-config` writes, with `credential_source.file` pointing at a projected service-account token. The token is exchanged at Google's STS, then for the service account's token when the file has `service_account_impersonation_url`. URL, AWS and executable sources are not supported |

The last two need no secret, so the credential can be a plain environment
variable: `credentials: {env: GCP_SANDBOXES_CREDENTIAL}` with the value
`{"type":"metadata_server"}`.

```sh
differently admin secret put gcp-sandboxes-key --from-file ./service-account.json
differently admin pool add gcp-sandboxes --adapter gcp-sandboxes --trust external \
  --config project=my-project --config location=us-central1 \
  --config reasoningEngine=1234567890 --config template=9876543210 \
  --max-concurrency 50 --secret gcp-sandboxes-key
```

```yaml
pools:
  - name: gcp-sandboxes
    adapter: gcp-sandboxes
    trust: external                # required: external, or private if your policy allows
    region: us-central1
    maxConcurrency: 50
    config:
      project: my-project
      location: us-central1        # defaults to the pool's region
      reasoningEngine: "1234567890"
      template: "9876543210"       # a request's template overrides it
      # apiURL: https://us-central1-aiplatform.googleapis.com
    credentials: {secret: gcp-sandboxes-key}
```

With Helm, mount the key or configuration file and use
`credentials: {file: /etc/differently/gcp-key.json}`. Allow the server's
egress to `*-aiplatform.googleapis.com`, and to what the credential calls:
`oauth2.googleapis.com` for a key, the metadata server (`169.254.169.254`,
port 80) for `metadata_server`, or `sts.googleapis.com` and
`iamcredentials.googleapis.com` for federation. The chart's NetworkPolicy
allows only port 443 to providers by default.

| Key | Default | Meaning |
|---|---|---|
| `project` | required | Google Cloud project ID |
| `location` | the pool's `region` | Region, such as `us-central1` |
| `reasoningEngine` | required | Engine ID, or its full resource name |
| `template` | required | Shell template ID in that engine, or its full name |
| `apiURL` | `https://{location}-aiplatform.googleapis.com` | Override, for example a multi-region endpoint |

## Limits and differences

- **No labels and no client-chosen IDs.** Create looks for the display name
  `dly:<installation>:<sandbox>` before creating, and sets `owner` to the
  installation. Two creates racing could make two sandboxes; the reconciler
  does not yet catch duplicates (backlog E0.21).
- **List** pages through every sandbox of the engine (100 a page) and keeps
  this installation's prefix. Use an engine dedicated to Differently.
- **Not read:** user credentials (`authorized_user`) and federation
  configurations that take their token from a URL, AWS or an executable.
- **No TTL at Google.** Sandboxes have no update method, so a `ttl` set at
  create could never follow a TTL change, and Google would delete a sandbox
  Differently still counts as alive. The adapter sets none, so a sandbox
  outlives a control plane that is gone for good; delete the engine's
  sandboxes by hand then. Azure's auto-delete policy can be moved, so
  `azure-sandboxes` keeps its backstop.
- **Snapshots last 30 days**, Google's default; the adapter sets no `ttl` on
  them. A fork from an expired or deleted snapshot fails with
  `invalid_request`.
- **A fork's egress is not checked.** A create from a snapshot names no
  template, and Google does not document which egress the restored sandbox
  gets.
- Long-running operations (create, pause, resume, snapshot) are polled every
  second, for up to 3 minutes. Delete returns once Google accepts it; the
  sandbox then lists as deprovisioning, which counts as gone. A second
  delete succeeds whether Google answers `NOT_FOUND` or refuses it with
  `FAILED_PRECONDITION` while the first is under way.
- **Errors**: `RESOURCE_EXHAUSTED` (429) is `insufficient_capacity`;
  `FAILED_PRECONDITION` is `invalid_state`; `INVALID_ARGUMENT` is
  `invalid_request`; 401, 403 and `UNAVAILABLE` are `provider_unavailable`. A
  failed operation is `sandbox_failed`, except for quota (code 8) and invalid
  argument (code 3). `STATE_TERMINATED` (a runtime failure) is an error state.
  On create, what the request names fails that one sandbox with
  `invalid_request` rather than marking the pool unhealthy: a template other
  than the pool's that Google does not have or refuses (404, 403), and a
  snapshot that is gone, expired or of another owner. A create or start that
  outlasts its wait, the adapter's or the scheduler's provision timeout, is
  `sandbox_failed`.
- **Quotas** per project and region: 1,000 execute requests a minute, 500
  write requests a minute, 1,000 sandboxes.
- A third party measured about 1 MB per request or response through the
  `:execute` proxy and a 502 after 600 s. Google documents neither; keep
  commands' output small.
- **Pricing**: $0.085 per vCPU-hour and $0.009 per GiB-hour, rounded per second.

## How it was tested

`go test ./internal/adapters/gcpsandboxes/` runs the adapter against a fake
server built from the discovery document. The fake includes the OAuth JWT
bearer grant and runs long-running operations. It answers `:execute` by
decoding the three input chunks and running the command with a local
`/bin/sh`. The tests cover:

- create, a replayed create, the create body (no TTL, owner, template), exec
  with env, stdin and cwd;
- file write and read;
- pause and resume, including a resume refused with `FAILED_PRECONDITION`;
- snapshot, fork from a snapshot (the fake refuses a snapshot of another
  owner, as Google documents), deleting a snapshot;
- a request's missing template, a missing snapshot and a create that does
  not finish fail that sandbox, while the pool's own template missing does
  not;
- paged List per installation, Delete and a deleted sandbox;
- a 429, a failed operation and credential parsing;
- egress control advertised from the template, a request for a template
  with internet access refused on such a pool, and an unreadable template;
- a pool that takes its token from a fake metadata server.

`live_test.go` (build tags `live,experimental_adapters`) runs the same
scenario against a real engine: create and a replayed create, exec with env,
cwd, stdin and an exit code, a 128 KiB file through several pieces, list,
stop and start with the file kept, snapshot, a fork with the file, and the
deletes. It also reports what Google does not document: whether a process
survives `:pause` and `:resume`, and whether one survives a snapshot into a
fork. The answers decide whether the pool could advertise
`lifecycle.pause-resume`. **It has not been run.**

```sh
GCP_SANDBOXES_PROJECT=my-project GCP_SANDBOXES_LOCATION=us-central1 \
GCP_SANDBOXES_ENGINE=1234567890 GCP_SANDBOXES_TEMPLATE=9876543210 \
GCP_SANDBOXES_CREDENTIAL_FILE=./service-account.json \
go test -tags live,experimental_adapters -v -run TestLiveGCPSandboxes ./internal/adapters/gcpsandboxes/
```

On Compute Engine or GKE, leave `GCP_SANDBOXES_CREDENTIAL_FILE` unset to use
the metadata server.

The JWT signing is checked with an RSA key in `internal/adapters/cloudhttp`,
which also tests the metadata server route and workload identity federation
(the token exchange, impersonation, a JSON token file, a refused token)
against fakes. Nothing has run against a real project, a GKE cluster or a
Compute Engine VM.
