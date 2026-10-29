import assert from "node:assert/strict";
import { after, before, beforeEach, test } from "node:test";
import { Differently, DifferentlyError, type ExecEvent } from "../src/index.js";
import { delay, MockServer, reply, sandboxInfo, sendError, sendJson } from "./mock-server.js";

let mock: MockServer;
let client: Differently;

before(async () => {
  mock = await MockServer.start();
  client = new Differently({ apiKey: "dly_test_secret", baseUrl: mock.url });
});
after(() => mock.close());
beforeEach(() => mock.reset());

test("create waits until running and sends an idempotency key", async () => {
  mock.on("POST /v1/sandboxes", reply(202, sandboxInfo({ status: "queued" })));
  mock.on("GET /v1/sandboxes/sbx_1/wait", reply(200, sandboxInfo({ status: "running" })));

  const params = { profile: "small", placement: { strategy: "private-first" as const, external: "never" as const } };
  const sandbox = await client.sandboxes.create(params);

  assert.equal(sandbox.id, "sbx_1");
  assert.equal(sandbox.status, "running");
  const [create] = mock.calls("POST /v1/sandboxes");
  assert.ok(create);
  assert.deepEqual(create.json(), params, "only API fields are sent");
  assert.equal(create.headers.authorization, "Bearer dly_test_secret");
  assert.equal(create.headers["content-type"], "application/json");
  assert.match(String(create.headers["idempotency-key"]), /^[0-9a-f-]{36}$/);
  const [wait] = mock.calls("GET /v1/sandboxes/sbx_1/wait");
  assert.equal(wait?.query.get("status"), "running");
  assert.equal(wait?.query.get("timeoutSeconds"), "300");
});

test("create with wait: false returns the queued sandbox", async () => {
  mock.on("POST /v1/sandboxes", reply(202, sandboxInfo({ status: "queued" })));
  const sandbox = await client.sandboxes.create({}, { wait: false, idempotencyKey: "my-key" });
  assert.equal(sandbox.status, "queued");
  assert.equal(mock.calls("POST /v1/sandboxes")[0]?.headers["idempotency-key"], "my-key");
  assert.equal(mock.calls("GET /v1/sandboxes/sbx_1/wait").length, 0);
});

test("create is retried on 429 rate_limited with the same idempotency key", async () => {
  mock.on("POST /v1/sandboxes", (_req, res) =>
    sendError(res, 429, "rate_limited", "slow down", { headers: { "Retry-After": "0" } }),
  );
  mock.on("POST /v1/sandboxes", reply(202, sandboxInfo({ status: "queued" })));
  await client.sandboxes.create({}, { wait: false });
  const calls = mock.calls("POST /v1/sandboxes");
  assert.equal(calls.length, 2);
  assert.equal(calls[0]?.headers["idempotency-key"], calls[1]?.headers["idempotency-key"]);
});

test("quota_exceeded is not retried", async () => {
  mock.on("POST /v1/sandboxes", (_req, res) =>
    sendError(res, 429, "quota_exceeded", "org cpu limit reached", { details: { dimension: "cpu" } }),
  );
  await assert.rejects(client.sandboxes.create(), (err: unknown) => {
    assert.ok(err instanceof DifferentlyError);
    assert.equal(err.code, "quota_exceeded");
    assert.equal(err.status, 429);
    assert.deepEqual(err.details, { dimension: "cpu" });
    assert.equal(err.requestId, "req_test");
    return true;
  });
  assert.equal(mock.calls("POST /v1/sandboxes").length, 1);
});

test("wait throws invalid_state when the sandbox fails", async () => {
  mock.on("GET /v1/sandboxes/sbx_1/wait", reply(200, sandboxInfo({ status: "failed", statusReason: "no capacity" })));
  await assert.rejects(client.sandboxes.wait("sbx_1"), (err: unknown) => {
    assert.ok(err instanceof DifferentlyError);
    assert.equal(err.code, "invalid_state");
    assert.equal(err.details.status, "failed");
    assert.equal(err.details.statusReason, "no capacity");
    return true;
  });
});

test("wait throws timeout once its deadline passes", async () => {
  mock.on("GET /v1/sandboxes/sbx_1/wait", async (_req, res) => {
    await delay(300);
    sendJson(res, 200, sandboxInfo({ status: "provisioning" }));
  });
  await assert.rejects(client.sandboxes.wait("sbx_1", "running", { timeoutSeconds: 0.2 }), { code: "timeout" });
  assert.equal(mock.calls("GET /v1/sandboxes/sbx_1/wait")[0]?.query.get("timeoutSeconds"), "1");
});

test("exec sends a shell command or argv with options", async () => {
  mock.on("GET /v1/sandboxes/sbx_1", reply(200, sandboxInfo()));
  mock.on("POST /v1/sandboxes/sbx_1/exec", reply(200, { exitCode: 0, stdout: "v20.0.0\n", stderr: "", durationMs: 12 }));
  const sb = await client.sandboxes.get("sbx_1");

  const result = await sb.exec("node --version");
  assert.equal(result.stdout, "v20.0.0\n");
  await sb.exec(["ls", "-la"], { cwd: "/work", env: { A: "1" }, timeoutSeconds: 5 });

  const [first, second] = mock.calls("POST /v1/sandboxes/sbx_1/exec");
  assert.deepEqual(first?.json(), { command: "node --version" });
  assert.deepEqual(second?.json(), { args: ["ls", "-la"], cwd: "/work", env: { A: "1" }, timeoutSeconds: 5 });
});

test("exec is not retried on 503", async () => {
  mock.on("POST /v1/sandboxes/sbx_1/exec", (_req, res) =>
    sendError(res, 503, "provider_unavailable", "backend down", { headers: { "Retry-After": "0" } }),
  );
  mock.on("GET /v1/sandboxes/sbx_1", reply(200, sandboxInfo()));
  const sandbox = await client.sandboxes.get("sbx_1");
  await assert.rejects(sandbox.exec("true"), { code: "provider_unavailable", status: 503 });
  assert.equal(mock.calls("POST /v1/sandboxes/sbx_1/exec").length, 1);
});

test("execStream yields events split across network chunks", async () => {
  mock.on("GET /v1/sandboxes/sbx_1", reply(200, sandboxInfo()));
  mock.on("POST /v1/sandboxes/sbx_1/exec", async (_req, res) => {
    res.writeHead(200, { "Content-Type": "text/event-stream" });
    const wire =
      'event: stdout\ndata: {"stream":"stdout","data":"building\\n"}\n\n' +
      ': keep-alive\n\n' +
      'event: stderr\ndata: {"stream":"stderr","data":"warn ünïcode"}\n\n' +
      'event: exit\ndata: {"exitCode":2}\n\n';
    const bytes = Buffer.from(wire, "utf8");
    for (let i = 0; i < bytes.length; i += 7) {
      res.write(bytes.subarray(i, i + 7));
      await delay(1);
    }
    res.end();
  });
  const sandbox = await client.sandboxes.get("sbx_1");
  const events: ExecEvent[] = [];
  for await (const ev of sandbox.execStream("make test")) events.push(ev);

  assert.deepEqual(events, [
    { type: "stdout", data: "building\n" },
    { type: "stderr", data: "warn ünïcode" },
    { type: "exit", exitCode: 2 },
  ]);
  const [call] = mock.calls("POST /v1/sandboxes/sbx_1/exec");
  assert.equal(call?.query.get("stream"), "1");
  assert.equal(call?.headers.accept, "text/event-stream");
  assert.deepEqual(call?.json(), { command: "make test" });
});

test("execStream reports backend errors and truncated streams", async () => {
  mock.on("GET /v1/sandboxes/sbx_1", reply(200, sandboxInfo()));
  mock.on("POST /v1/sandboxes/sbx_1/exec", (_req, res) => {
    res.writeHead(200, { "Content-Type": "text/event-stream" });
    res.end('event: error\ndata: {"error":"agent lost"}\n\n');
  });
  mock.on("POST /v1/sandboxes/sbx_1/exec", (_req, res) => {
    res.writeHead(200, { "Content-Type": "text/event-stream" });
    res.end('event: stdout\ndata: {"stream":"stdout","data":"partial"}\n\n');
  });
  const sandbox = await client.sandboxes.get("sbx_1");

  const events: ExecEvent[] = [];
  for await (const ev of sandbox.execStream("x")) events.push(ev);
  assert.deepEqual(events, [{ type: "error", error: "agent lost" }]);

  const seen: ExecEvent[] = [];
  await assert.rejects(
    (async () => {
      for await (const ev of sandbox.execStream("x")) seen.push(ev);
    })(),
    { code: "connection_error" },
  );
  assert.deepEqual(seen, [{ type: "stdout", data: "partial" }]);
});

test("execStream surfaces errors returned before the stream starts", async () => {
  mock.on("GET /v1/sandboxes/sbx_1", reply(200, sandboxInfo({ status: "stopped" })));
  mock.on("POST /v1/sandboxes/sbx_1/exec", (_req, res) =>
    sendError(res, 409, "invalid_state", "sandbox sbx_1 is stopped, not running", { details: { status: "stopped" } }),
  );
  const sandbox = await client.sandboxes.get("sbx_1");
  await assert.rejects(
    (async () => {
      for await (const _ of sandbox.execStream("x")) assert.fail("no events expected");
    })(),
    { code: "invalid_state", status: 409 },
  );
});

test("files write, read, readText and list", async () => {
  const files = new Map<string, Buffer>();
  mock.on("GET /v1/sandboxes/sbx_1", reply(200, sandboxInfo()));
  mock.on("PUT /v1/sandboxes/sbx_1/files/content", (req, res) => {
    files.set(req.query.get("path") ?? "", req.body);
    res.writeHead(204).end();
  });
  mock.on("GET /v1/sandboxes/sbx_1/files/content", (req, res) => {
    const data = files.get(req.query.get("path") ?? "");
    if (!data) return sendError(res, 404, "not_found", "no such file");
    res.writeHead(200, { "Content-Type": "application/octet-stream" }).end(data);
  });
  mock.on(
    "GET /v1/sandboxes/sbx_1/files",
    reply(200, {
      items: [{ path: "/work/a.txt", name: "a.txt", size: 2, mode: "-rw-r--r--", isDir: false, modTime: "2026-10-28T12:00:00Z" }],
    }),
  );
  const sandbox = await client.sandboxes.get("sbx_1");

  await sandbox.files.write("/work/a.txt", "hi");
  await sandbox.files.write("/work/b.bin", new Uint8Array([0, 255, 7]));
  assert.equal(await sandbox.files.readText("/work/a.txt"), "hi");
  assert.deepEqual([...(await sandbox.files.read("/work/b.bin"))], [0, 255, 7]);
  const listing = await sandbox.files.list("/work");
  assert.equal(listing[0]?.name, "a.txt");

  const [put] = mock.calls("PUT /v1/sandboxes/sbx_1/files/content");
  assert.equal(put?.headers["content-type"], "application/octet-stream");
  assert.equal(mock.calls("GET /v1/sandboxes/sbx_1/files")[0]?.query.get("path"), "/work");
  await assert.rejects(sandbox.files.read("/missing"), { code: "not_found" });
});

test("lifecycle, snapshot, fork, ports and placement", async () => {
  mock.on("GET /v1/sandboxes/sbx_1", reply(200, sandboxInfo()));
  mock.on("POST /v1/sandboxes/sbx_1/stop", reply(200, sandboxInfo({ status: "stopped" })));
  mock.on("POST /v1/sandboxes/sbx_1/start", reply(200, sandboxInfo({ status: "running" })));
  mock.on(
    "POST /v1/sandboxes/sbx_1/snapshots",
    reply(201, { id: "snp_1", sandboxId: "sbx_1", orgId: "o", projectId: "p", pool: "local", kind: "memory", status: "ready", createdAt: "t", createdBy: "k" }),
  );
  mock.on("POST /v1/sandboxes/sbx_1/fork", reply(202, sandboxInfo({ id: "sbx_2", status: "queued" })));
  mock.on("GET /v1/sandboxes/sbx_2/wait", reply(200, sandboxInfo({ id: "sbx_2", status: "running" })));
  mock.on("POST /v1/sandboxes/sbx_1/ports", reply(200, { port: 3000, url: "https://p3000.example.test" }));
  mock.on(
    "GET /v1/sandboxes/sbx_1/placement",
    reply(200, { sandboxId: "sbx_1", status: "running", pool: "local", trust: "private", decision: null }),
  );
  mock.on("DELETE /v1/sandboxes/sbx_1", reply(202, sandboxInfo({ status: "terminating" })));

  const sandbox = await client.sandboxes.get("sbx_1");
  assert.equal((await sandbox.stop()).status, "stopped");
  assert.equal((await sandbox.start()).status, "running");
  assert.equal((await sandbox.snapshot({ kind: "memory" })).id, "snp_1");
  assert.deepEqual(mock.calls("POST /v1/sandboxes/sbx_1/snapshots")[0]?.json(), { kind: "memory" });
  const fork = await sandbox.fork();
  assert.equal(fork.id, "sbx_2");
  assert.equal(fork.status, "running");
  assert.equal((await sandbox.exposePort(3000)).url, "https://p3000.example.test");
  assert.deepEqual(mock.calls("POST /v1/sandboxes/sbx_1/ports")[0]?.json(), { port: 3000 });
  assert.equal((await sandbox.explainPlacement()).trust, "private");
  await sandbox.delete();
  assert.equal(sandbox.status, "terminating");
  assert.deepEqual(JSON.parse(JSON.stringify(sandbox)).id, "sbx_1");
});

test("list and iterate follow cursors and encode filters", async () => {
  mock.on("GET /v1/sandboxes", reply(200, { items: [sandboxInfo({ id: "a" }), sandboxInfo({ id: "b" })], nextCursor: "b" }));
  mock.on("GET /v1/sandboxes", reply(200, { items: [sandboxInfo({ id: "c" })] }));

  const ids: string[] = [];
  for await (const sb of client.sandboxes.iterate({ status: ["running", "paused"], labels: { team: "ml" }, limit: 2 })) {
    ids.push(sb.id);
  }
  assert.deepEqual(ids, ["a", "b", "c"]);
  const [first, second] = mock.calls("GET /v1/sandboxes");
  assert.equal(first?.query.get("status"), "running,paused");
  assert.deepEqual(first?.query.getAll("label"), ["team=ml"]);
  assert.equal(first?.query.get("limit"), "2");
  assert.equal(first?.query.get("cursor"), null);
  assert.equal(second?.query.get("cursor"), "b");
});

test("update sends a PATCH with only the given fields", async () => {
  mock.on("PATCH /v1/sandboxes/sbx_1", reply(200, sandboxInfo({ labels: { team: "ml" }, ttlSeconds: 600 })));
  const sandbox = await client.sandboxes.update("sbx_1", { labels: { team: "ml" }, ttlSeconds: 600 });
  assert.deepEqual(sandbox.labels, { team: "ml" });
  assert.deepEqual(mock.calls("PATCH /v1/sandboxes/sbx_1")[0]?.json(), { labels: { team: "ml" }, ttlSeconds: 600 });
});
