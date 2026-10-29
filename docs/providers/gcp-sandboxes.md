# Google Cloud Agent Platform sandboxes

| | |
|---|---|
| Status | **Tested with a fake server** (implemented from the published API, not run against Google Cloud) |
| Adapter | `gcp-sandboxes` (`internal/adapters/gcpsandboxes`) |
| Trust | declared: every pool must state it. Google manages the sandboxes (`external`), but they run in your own project, so declare `private` if your policy treats that project as private |
| Isolation | "Secure container sandboxing" (Google does not name the runtime) |
| API checked | aiplatform v1 discovery document (revision 20261012), the [shell sandbox quickstart](https://docs.cloud.google.com/gemini-enterprise-agent-platform/scale/sandbox/shell-sandbox-quickstart), [manage sandboxes](https://docs.cloud.google.com/gemini-enterprise-agent-platform/scale/sandbox/manage-sandboxes), [templates](https://docs.cloud.google.com/gemini-enterprise-agent-platform/scale/sandbox/manage-templates) and [snapshots](https://docs.cloud.google.com/gemini-enterprise-agent-platform/scale/sandbox/manage-snapshots), read 2026-10-28 |

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
| fork | yes | A new sandbox from a snapshot (same owner) |
| ports.expose | no | The data plane needs a signed JWT and a routing token on every request, so there is no plain URL to return |
| network.egress-control | no | Set by the template (`egressControlConfig.internetAccess`, off by default), not per sandbox |
| max size | set by the template | Requested resources are not applied |
| provider TTL backstop | yes | `ttl`: the TTL plus 5 minutes, or 7 days without one. It cannot be moved later |

## Configure

Set up once per project and region:

1. Create an engine: `POST https://{location}-aiplatform.googleapis.com/v1/projects/{project}/locations/{location}/reasoningEngines`
   with `{"displayName": "differently"}`. Its ID is the last part of the
   returned name.
2. Create a shell template in it: `POST …/reasoningEngines/{engine}/sandboxEnvironmentTemplates`
   with `{"displayName": "shell", "defaultContainerEnvironment": {"defaultContainerCategory": "DEFAULT_CONTAINER_CATEGORY_SHELL_SANDBOX"}}`.
   Add `"egressControlConfig": {"internetAccess": true}` if sandboxes may
   reach the internet.
3. Give a service account `roles/aiplatform.user` and download a JSON key.

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

With Helm, mount the key file and use `credentials: {file: /etc/differently/gcp-key.json}`.
Allow the server's egress to `*-aiplatform.googleapis.com` and
`oauth2.googleapis.com`.

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
- **Service-account keys only.** GKE Workload Identity and the metadata
  server are not read yet.
- Long-running operations (create, pause, resume, snapshot) are polled every
  second, for up to 3 minutes. Delete returns once Google accepts it; the
  sandbox then lists as deprovisioning, which counts as gone.
- **Errors**: `RESOURCE_EXHAUSTED` (429) is `insufficient_capacity`;
  `FAILED_PRECONDITION` is `invalid_state`; `INVALID_ARGUMENT` is
  `invalid_request`; 401, 403 and `UNAVAILABLE` are `provider_unavailable`. A
  failed operation is `sandbox_failed`, except for quota (code 8) and invalid
  argument (code 3). `STATE_TERMINATED` (a runtime failure) is an error state.
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

- create, a replayed create, the create body (TTL, owner, template), exec
  with env, stdin and cwd;
- file write and read;
- pause and resume, including a resume refused with `FAILED_PRECONDITION`;
- snapshot, fork from a snapshot, the default TTL, deleting a snapshot;
- paged List per installation, Delete and a deleted sandbox;
- a 429, a failed operation and credential parsing.

The JWT signing is checked with an RSA key in `internal/adapters/cloudhttp`.
Nothing has run against a real project.
