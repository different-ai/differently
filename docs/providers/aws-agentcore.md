# Amazon Bedrock AgentCore Code Interpreter

> **Experimental adapter.** It is not in the default `differently-server`
> build or the published image. It needs a server built with
> `-tags experimental_adapters`: see
> [what the server contains](README.md#what-the-server-contains).

| | |
|---|---|
| Status | **Tested with a fake server** (implemented from the published API, not run against AWS) |
| Adapter | `aws-agentcore` (`internal/adapters/agentcore`) |
| Trust | declared: every pool must state it. AWS manages the sessions (`external`), but they run in your own AWS account, so declare `private` if your policy treats that account as private |
| Isolation | A dedicated microVM per session, with its memory sanitized when the session ends (AWS's session-characteristics page; the overview page says "containerized environment") |
| API checked | [AgentCore data-plane API reference](https://docs.aws.amazon.com/bedrock-agentcore/latest/APIReference/API_StartCodeInterpreterSession.html), the botocore model `bedrock-agentcore/2024-02-28` and the [developer guide](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/code-interpreter-api-reference-examples.html), read 2026-10-28; the botocore model was read again on 2026-11-17 |

Each Differently sandbox is one Code Interpreter session. The adapter signs
requests with AWS Signature Version 4 itself, so it needs no AWS SDK.

This is as far as AWS's sandbox APIs go today. Compared with
[Azure Container Apps Sandboxes](azure-sandboxes.md), a session has no stop
and start, no snapshots, no ports and no per-pool egress setting, and no
other AWS service fills the gap: see
[other AWS services](#limits-and-differences) below.

## Capabilities

| Feature | Supported | Notes |
|---|---|---|
| exec | yes | `executeCommand` returns stdout, stderr and the exit code. The working directory, environment, stdin and timeout are added to the command line; the timeout uses coreutils `timeout` (exit 124) |
| exec.stream | no | Output arrives when the command ends (buffered fallback) |
| files | yes | Through commands: `base64` and `find` in the session. Writes go in 64 KiB pieces and reads in 512 KiB pieces, so a large file takes several calls |
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

The IAM principal needs `bedrock-agentcore:StartCodeInterpreterSession`,
`GetCodeInterpreterSession`, `ListCodeInterpreterSessions`,
`StopCodeInterpreterSession` and `InvokeCodeInterpreter` on the code
interpreter. The credential is JSON, and says where the keys come from: an
access key, or a role that Differently reads and renews by itself.

```json
{"accessKeyId": "AKIA...", "secretAccessKey": "...", "sessionToken": "optional"}
{"source": "web-identity"}
{"source": "pod-identity"}
{"source": "instance-role"}
```

| `source` | Where it runs | What it reads |
|---|---|---|
| none | anywhere | The access key in the credential. Temporary keys with a `sessionToken` work until they expire; the operator replaces them |
| `web-identity` | EKS with IAM Roles for Service Accounts | `AWS_ROLE_ARN` and `AWS_WEB_IDENTITY_TOKEN_FILE`, which EKS sets on a pod whose service account is annotated with the role (`serviceAccount.annotations` in the chart). The token is exchanged at STS (`AssumeRoleWithWebIdentity`). Override with `roleArn`, `webIdentityTokenFile`, `roleSessionName`, `stsEndpoint` |
| `pod-identity` | EKS Pod Identity, ECS task roles | `AWS_CONTAINER_CREDENTIALS_FULL_URI` and `AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE`, which the platform sets. Override with `containerCredentialsUri`, `containerAuthorizationTokenFile`. The endpoint must be HTTPS, loopback or the ECS/EKS agent address |
| `instance-role` | EC2 with an instance profile | The instance metadata service (IMDSv2). A pod needs the instance's metadata hop limit set to 2. Override with `imdsEndpoint` |

Keys from a source are renewed five minutes before they expire. A source
needs no secret, so the credential can be a plain environment variable:
`credentials: {env: AGENTCORE_CREDENTIAL}` with the value
`{"source":"pod-identity"}`. Allow the server's egress to what the source
calls: `sts.<region>.amazonaws.com`, the Pod Identity agent
(`169.254.170.23`, port 80) or the metadata service (`169.254.169.254`,
port 80). The chart's NetworkPolicy allows only port 443 to providers by
default.

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
- **Not read:** the shared config and credentials files, SSO profiles and
  `AssumeRole` chains. Use one of the sources above, or a static key.
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
- **Not used:** the session's own file tools (`readFiles`, `writeFiles`,
  `listFiles`, `removeFiles`), background commands (`startCommandExecution`,
  `getTask`, `stopTask`) and mounts (`filesystemConfigurations`: S3 Files or
  EFS access points). Background commands could give streamed output and the
  mounts a disk that outlives a session; neither is a stop and start.
- Other AWS services: [AgentCore Runtime](aws-agentcore-runtime.md) can run
  commands in your own container image, stop a session and resume it, but
  has no way to list sessions (still true in the API model on 2026-11-17),
  so the reconciler could not find orphans; AgentCore Browser is
  browser-only; ECS `ExecuteCommand` is an interactive SSM session with no
  exit code; Lambda has no sandbox sessions.

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
- the 402, 429, 400, 403 and 500 classifications;
- a pool on Pod Identity, signing with keys from a fake credentials
  endpoint, and that endpoint refusing the pod's token.

The SigV4 signer is checked against the worked example in the IAM user guide.
The three credential sources are tested in `internal/adapters/cloudhttp`
against fakes of STS (the documented XML), the container credentials
endpoint and IMDSv2: caching, renewal near expiry, keeping valid keys
through a failed renewal, and refusals that do not echo the token.
Nothing has run against a real AWS account, an EKS cluster or an EC2
instance.
