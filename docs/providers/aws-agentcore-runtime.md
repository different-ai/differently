# Amazon Bedrock AgentCore Runtime (command execution)

| | |
|---|---|
| Status | **Documented** (configuration described, no adapter yet; needs a `remote` plugin or a future adapter) |
| Adapter | none |
| Trust | declared on the plugin pool: AWS manages the sessions (`external`), but they run in your own AWS account, so declare `private` if your policy treats that account as private |
| Isolation | microVM per runtime session (AWS-managed) |
| API checked | [Execute commands in a runtime session](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/runtime-execute-command.html), [runtime sessions](https://docs.aws.amazon.com/bedrock-agentcore/latest/devguide/runtime-sessions.html), the botocore model `bedrock-agentcore/2024-02-28`, read 2026-10-28 |

AgentCore Runtime hosts your own container image. It can also run shell
commands in a runtime session (`InvokeAgentRuntimeCommand`), which
makes it the closest thing on AWS to a general-purpose sandbox with a custom
image. Differently does not adapt it yet, for one reason: **there is no API
to list runtime sessions.**

## Capabilities

| Feature | What AWS offers |
|---|---|
| exec | `POST /runtimes/{agentRuntimeArn}/commands` with `{command, timeout}` (1–3600 s). Each command starts a fresh bash, so cwd and env do not carry over |
| exec.stream | yes: `contentDelta` events with stdout and stderr, then `contentStop` with the exit code |
| files | through commands, or session storage mounts |
| stdin | not documented |
| lifecycle.stop-start | `StopRuntimeSession`; the next call resumes. Managed session storage (preview) keeps files for 14 days idle |
| lifecycle.pause-resume | no |
| snapshots.disk / snapshots.memory / fork | no |
| ports.expose | no |
| max size | set by the runtime's configuration |
| provider TTL backstop | idle timeout and maximum lifetime per runtime, up to 8 hours |

## Configure

There is nothing to configure in Differently today. A plugin would be
registered as a `remote` pool
([adapter specification](../adapters/specification.md#10-writing-a-community-adapter)):

```yaml
pools:
  - name: agentcore-runtime
    adapter: remote
    trust: external
    config: {endpoint: https://agentcore-plugin.internal:8443}
    credentials: {secret: agentcore-plugin-token}
```

## Limits and differences

- **No session list.** Sessions are created on first use of a session ID
  (33+ characters, chosen by the caller), but nothing lists them. Without
  `List`, the reconciler cannot find orphans or notice lost sessions. Worse,
  an exec against a session that has expired would silently create a new,
  empty one. A plugin would need its own record of sessions, with the risk
  that the record and AWS drift apart.
- An agent runtime (`CreateAgentRuntime` with a container image that serves
  the runtime contract) must exist before any session.
- Agents deployed before 2026-03-17 need a redeploy to accept commands.
- Retry `RetryableConflictException` (409).
- The data plane allows 1,000 requests a second across the runtime APIs;
  new sessions are limited to 25 a second.

## How it was tested

Not tested. This page is from AWS's documentation and API model only.
