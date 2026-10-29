# Amazon Bedrock AgentCore Code Interpreter

| | |
|---|---|
| Status | **Tested with a fake server** (implemented from the published API, not run against AWS) |
| Adapter | `aws-agentcore` (`internal/adapters/agentcore`) |
| Trust | declared: every pool must state it. AWS manages the sessions (`external`), but they run in your own AWS account, so declare `private` if your policy treats that account as private |
| Isolation | A dedicated microVM per session, with its memory sanitized when the session ends (AWS's session-characteristics page; the overview page says "containerized environment") |
| API checked | [AgentCore data-plane API reference](https://docs.aws.amazon.com/bedrock-agentcore/latest/APIReference/API_StartCodeInterpreterSession.html), the botocore model `bedrock-agentcore/2024-02-28` and the [developer guide](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/code-interpreter-api-reference-examples.html), read 2026-10-28 |

Each Differently sandbox is one Code Interpreter session. The adapter signs
requests with AWS Signature Version 4 itself, so it needs no AWS SDK.

## Capabilities

| Feature | Supported | Notes |
|---|---|---|
| exec | yes | `executeCommand` returns stdout, stderr and the exit code. The working directory, environment, stdin and timeout are added to the command line; the timeout uses coreutils `timeout` (exit 124) |
| exec.stream | no | Output arrives when the command ends (buffered fallback) |
| files | yes | Through commands: `base64` and `find` in the session. Writes go in 64 KiB pieces, so a large file takes many calls |
| stdin | yes | Piped in through the command line |
| lifecycle.stop-start | no | A stopped session cannot restart |
| lifecycle.pause-resume | no | |
| snapshots.disk | no | |
| snapshots.memory | no | |
| fork | no | |
| ports.expose | no | |
| network.egress-control | no | Fixed by the code interpreter: the built-in one is in sandbox mode (limited egress, S3 reachable); custom interpreters can be public or VPC |
| max size | 2 vCPU, 8 GB memory, 10 GB disk, fixed per session | Placement rejects larger requests |
| provider TTL backstop | yes | `sessionTimeoutSeconds`: the sandbox TTL plus 5 minutes, at most 8 hours. Without a TTL the session gets 8 hours. It cannot be moved later |

## Configure

The credential is an AWS access key as JSON. The IAM principal needs
`bedrock-agentcore:StartCodeInterpreterSession`, `GetCodeInterpreterSession`,
`ListCodeInterpreterSessions`, `StopCodeInterpreterSession` and
`InvokeCodeInterpreter` on the code interpreter.

```json
{"accessKeyId": "AKIA...", "secretAccessKey": "...", "sessionToken": "optional"}
```

```sh
differently admin secret put agentcore-key --from-file ./agentcore-credential.json
differently admin pool add agentcore --adapter aws-agentcore --trust external \
  --config region=us-east-1 --max-concurrency 50 --secret agentcore-key
```

In the server config file:

```yaml
pools:
  - name: agentcore
    adapter: aws-agentcore
    trust: external                # required: external, or private if your policy allows
    region: us-east-1
    maxConcurrency: 50
    config:
      region: us-east-1            # defaults to the pool's region
      codeInterpreter: aws.codeinterpreter.v1   # the default; or a custom interpreter ID
      # apiURL: https://bedrock-agentcore.us-east-1.amazonaws.com  (default, from region)
    credentials: {secret: agentcore-key}
```

With Helm, use `credentials: {file: /etc/differently/agentcore.json}` (a
mounted Secret) or `{env: AGENTCORE_CREDENTIAL}` with `server.extraEnv`, and
allow the server's egress to the AWS endpoint
(`networkPolicy.server.egress.providerCIDRs`).

| Key | Default | Meaning |
|---|---|---|
| `region` | the pool's `region` | AWS region of the data plane |
| `codeInterpreter` | `aws.codeinterpreter.v1` | Built-in or custom code interpreter ID |
| `apiURL` | `https://bedrock-agentcore.<region>.amazonaws.com` | Override, for example the FIPS endpoint |

## Limits and differences

- **Sessions last at most 8 hours**, whatever the sandbox's TTL; a longer
  sandbox ends then and the reconciler marks it failed. Changing the TTL
  later does not change the session.
- **Static keys only.** Instance roles, IRSA and EKS Pod Identity are not
  read yet. Temporary keys with a `sessionToken` work until they expire.
- **Idempotent create**: the adapter first looks for a READY session named
  `dly:<installation>:<sandbox>`, then starts one with a `clientToken`
  derived from both IDs.
- **List** pages through every READY session of the code interpreter and
  keeps this installation's name prefix. AWS does not filter by name.
- **Rate limits**: Start, Get, List, Stop and Invoke are each limited to
  30 requests per second per account. Every file write piece is one Invoke.
  There are 1,000 concurrent sessions per account (adjustable).
- **Network**: the built-in interpreter runs in sandbox network mode (limited
  egress, S3 reachable). Custom interpreters can use public or VPC networking.
  There is no custom image.
- **Status names**: the API model's status enum is `READY | TERMINATED`, but
  the descriptions also mention `ACTIVE`, `STOPPING` and `STOPPED`; the adapter
  accepts both.
- **Errors**: `ServiceQuotaExceededException` (sent as 402) and
  `ThrottlingException` (429) are `insufficient_capacity`;
  `ValidationException` is `invalid_request`; `AccessDeniedException` and 5xx
  are `provider_unavailable`. Exceptions inside the event stream are
  classified the same way.
- **Pricing**: per second for vCPU and memory used; I/O wait and idle time are
  free.
- Other AWS services: [AgentCore Runtime](aws-agentcore-runtime.md) can run
  commands in your own container image but has no way to list sessions;
  AgentCore Browser is browser-only; ECS `ExecuteCommand` is an interactive
  SSM session with no exit code; Lambda has no sandbox sessions.

## How it was tested

`go test ./internal/adapters/agentcore/` runs the adapter against a fake
server built from the botocore model. The fake checks the SigV4
`Authorization` header and pages the session list two at a time. It answers
`executeCommand` by running the command with a local `/bin/sh` and framing
the result as an AWS event stream. The tests cover:

- create, a replayed create, the TTL backstop, exec with env and stdin;
- file write, read and list;
- paged List per installation, idempotent Delete;
- an exception inside the event stream;
- the 402, 429, 400, 403 and 500 classifications.

The SigV4 signer is checked against the worked example in the IAM user guide.
Nothing has run against a real AWS account.
