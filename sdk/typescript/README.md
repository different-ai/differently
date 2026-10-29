# @differently/sdk

TypeScript SDK for Differently, the self-hosted sandbox and microVM
platform. ESM-only, zero runtime dependencies, Node.js 20 or newer.

Your code asks for a profile, capabilities and a placement policy. Differently
picks the pool; the SDK never needs to know which backend runs the sandbox.

## Install

```sh
npm install @differently/sdk
```

Until the package is published, install from a checkout:
`npm install ./sdk/typescript` (run `npm run build` there first).

## Quickstart

```ts
import { Differently } from "@differently/sdk";

// Defaults: DIFFERENTLY_API_KEY and DIFFERENTLY_URL (or http://localhost:8080).
const differently = new Differently({ apiKey: process.env.DIFFERENTLY_API_KEY });

// Waits until the sandbox is running. Pass { wait: false } as a second argument to return at once.
const sandbox = await differently.sandboxes.create({
  profile: "small",
  placement: { strategy: "private-first", external: "never" },
});

const result = await sandbox.exec("node --version"); // a string runs via /bin/sh -c
console.log(result.exitCode, result.stdout);
await sandbox.exec(["ls", "-la", "/work"], { cwd: "/work", timeoutSeconds: 30 }); // an array runs directly

await sandbox.files.write("/work/a.txt", "hi");
const text = await sandbox.files.readText("/work/a.txt");

await sandbox.delete();
```

## Streaming output

```ts
for await (const ev of sandbox.execStream("make test")) {
  if (ev.type === "stdout") process.stdout.write(ev.data);
  else if (ev.type === "stderr") process.stderr.write(ev.data);
  else if (ev.type === "exit") console.log("exit code", ev.exitCode);
  else console.error("backend error:", ev.error);
}
```

The stream ends after `exit` or `error`. Breaking out of the loop closes the
connection. If the connection drops before either event arrives, the loop
throws a `connection_error`.

## Errors

Every failure is a `DifferentlyError` with `code`, `status`, `details` and
`requestId`. Branch on `code`, not on the message.

```ts
import { DifferentlyError } from "@differently/sdk";

try {
  await differently.sandboxes.create({ profile: "gpu-large" });
} catch (err) {
  if (err instanceof DifferentlyError && err.code === "quota_exceeded") {
    console.log("over quota on", err.details.dimension, "request", err.requestId);
  } else {
    throw err;
  }
}
```

Server codes include `invalid_request`, `unauthenticated`, `permission_denied`,
`not_found`, `conflict`, `quota_exceeded`, `no_eligible_pool`,
`unsupported_capability`, `unsupported_operation`, `provider_unavailable`,
`invalid_state`, `rate_limited`, `suspended` and `internal`. The SDK adds
`timeout` and `connection_error` (with `status: 0`). Waiting throws
`invalid_state` if the sandbox fails or terminates first.

**Retries.** GET, PUT and DELETE requests, and sandbox creation, are retried
up to `maxRetries` times (default 3). Retries happen on 503, on 429
`rate_limited` (never `quota_exceeded`) and on connection errors, with
exponential backoff that respects `Retry-After`. Each `create` call sends its
own `Idempotency-Key`, so a retried create never makes a second sandbox. Pass
`idempotencyKey` to keep the key stable across process restarts. Exec and other
POSTs are never retried.

## What's covered

| Namespace | Methods |
|---|---|
| `sandboxes` | `create`, `get`, `list`, `iterate`, `update`, `delete`, `wait` |
| `Sandbox` handle | `exec`, `execStream`, `files.list/read/readText/write`, `stop`, `start`, `pause`, `resume`, `snapshot`, `fork`, `exposePort`, `explainPlacement`, `refresh`, `update`, `wait`, `delete` |
| `snapshots` | `list`, `get`, `delete` |
| `projects` | `list`, `create`, `get`, `delete` |
| `apiKeys` | `list`, `create`, `revoke` |
| `quotas`, `usage` | `get` |
| `auditEvents` | `list`, `iterate` |
| `policies` | `list`, `put`, `delete` |
| `capabilities`, `profiles`, `templates`, `classifications` | `list` |
| `admin` | `organizations.{list,create,get,update,suspend,resume}`, `projects.setQuota`, `pools.{list,get,create,update,drain,undrain,delete}`, `agents.list`, `trustReport`, `verifyAudit`, `terminateSandbox`, `reconcile` |
| client | `me`, `version` |

`list` returns one page (`{ items, nextCursor }`); `iterate` walks every page.

```ts
for await (const sb of differently.sandboxes.iterate({ status: "running", labels: { team: "ml" } })) {
  console.log(sb.id);
}
```

## Options

```ts
new Differently({
  apiKey: "dly_...",        // default: DIFFERENTLY_API_KEY
  baseUrl: "https://...",   // default: DIFFERENTLY_URL, then http://localhost:8080
  timeoutMs: 120_000,       // per request; waits and streams set their own
  maxRetries: 3,
  headers: {},              // extra headers on every request
  fetch,                    // custom fetch implementation
});
```

Plain `exec` uses the client timeout unless you pass `timeoutSeconds`. For long
commands, pass `timeoutSeconds` or use `execStream`, which has no client
timeout. Every waiting method also accepts an `AbortSignal`.

## Development

```sh
npm install
npm test   # builds, then runs node:test against an in-process mock server
```
