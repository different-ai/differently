import assert from "node:assert/strict";
import { after, before, beforeEach, test } from "node:test";
import { inspect } from "node:util";
import { Differently, DifferentlyError } from "../src/index.js";
import { delay, MockServer, reply, sandboxInfo, sendError, sendJson } from "./mock-server.js";

let mock: MockServer;
let client: Differently;

before(async () => {
  mock = await MockServer.start();
  client = new Differently({ apiKey: "dly_test_secret", baseUrl: `${mock.url}/` });
});
after(() => mock.close());
beforeEach(() => mock.reset());

test("reads DIFFERENTLY_API_KEY and DIFFERENTLY_URL by default", async () => {
  const saved = { key: process.env.DIFFERENTLY_API_KEY, url: process.env.DIFFERENTLY_URL };
  process.env.DIFFERENTLY_API_KEY = "dly_env_key";
  process.env.DIFFERENTLY_URL = mock.url;
  try {
    mock.on("GET /v1/me", reply(200, { keyId: "env", orgId: "o", projectId: "", scopes: ["org:admin"], operator: false }));
    const fromEnv = new Differently();
    assert.equal(fromEnv.baseUrl, mock.url);
    assert.equal((await fromEnv.me()).keyId, "env");
    assert.equal(mock.requests[0]?.headers.authorization, "Bearer dly_env_key");
    assert.match(String(mock.requests[0]?.headers["user-agent"]), /^differently-typescript\//);
  } finally {
    restoreEnv("DIFFERENTLY_API_KEY", saved.key);
    restoreEnv("DIFFERENTLY_URL", saved.url);
  }
});

test("a missing API key fails fast", () => {
  const saved = process.env.DIFFERENTLY_API_KEY;
  process.env.DIFFERENTLY_API_KEY = "";
  try {
    assert.throws(() => new Differently(), { name: "DifferentlyError", code: "unauthenticated" });
  } finally {
    restoreEnv("DIFFERENTLY_API_KEY", saved);
  }
});

function restoreEnv(name: string, value: string | undefined): void {
  if (value === undefined) delete process.env[name];
  else process.env[name] = value;
}

test("the API key is not exposed on the client object", () => {
  assert.doesNotMatch(JSON.stringify(client), /dly_test_secret/);
  assert.doesNotMatch(inspect(client, { depth: 10, showHidden: true }), /dly_test_secret/);
});

test("error envelopes become DifferentlyError", async () => {
  mock.on("GET /v1/snapshots/snp_x", (_req, res) =>
    sendError(res, 404, "not_found", "snapshot snp_x not found", { details: { id: "snp_x" } }),
  );
  await assert.rejects(client.snapshots.get("snp_x"), (err: unknown) => {
    assert.ok(err instanceof DifferentlyError);
    assert.equal(err.code, "not_found");
    assert.equal(err.status, 404);
    assert.equal(err.message, "snapshot snp_x not found");
    assert.deepEqual(err.details, { id: "snp_x" });
    assert.equal(err.requestId, "req_test");
    return true;
  });
});

test("non-JSON error bodies keep the status and header request ID", async () => {
  mock.on("GET /v1/profiles", (_req, res) => {
    res.writeHead(502, { "Content-Type": "text/plain", "X-Request-Id": "req_proxy" }).end("Bad Gateway");
  });
  await assert.rejects(client.profiles.list(), { code: "internal", status: 502, requestId: "req_proxy" });
});

test("redirects are not followed", async () => {
  mock.on("GET /v1/profiles", (_req, res) => void res.writeHead(302, { Location: `${mock.url}/elsewhere` }).end());
  mock.on("GET /elsewhere", reply(200, { items: [] }));
  await assert.rejects(client.profiles.list(), { status: 302 });
  assert.equal(mock.calls("GET /elsewhere").length, 0);
});

test("GET is retried on 503 and honors Retry-After", async () => {
  const unavailable = (_req: unknown, res: Parameters<typeof sendJson>[0]) =>
    sendError(res, 503, "provider_unavailable", "store unavailable", { headers: { "Retry-After": "0" } });
  mock.on("GET /v1/templates", unavailable).on("GET /v1/templates", unavailable);
  mock.on("GET /v1/templates", reply(200, { items: [{ name: "ubuntu-24.04" }] }));
  assert.deepEqual(await client.templates.list(), [{ name: "ubuntu-24.04" }]);
  assert.equal(mock.calls("GET /v1/templates").length, 3);
});

test("retries stop after maxRetries", async () => {
  const limited = new Differently({ apiKey: "dly_x_y", baseUrl: mock.url, maxRetries: 1 });
  mock.on("GET /v1/classifications", (_req, res) =>
    sendError(res, 429, "rate_limited", "slow down", { headers: { "Retry-After": "0" } }),
  );
  await assert.rejects(limited.classifications.list(), { code: "rate_limited", status: 429 });
  assert.equal(mock.calls("GET /v1/classifications").length, 2);
});

test("POST without an idempotency key is never retried", async () => {
  mock.on("POST /v1/projects", (_req, res) =>
    sendError(res, 503, "provider_unavailable", "busy", { headers: { "Retry-After": "0" } }),
  );
  await assert.rejects(client.projects.create({ name: "web" }), { code: "provider_unavailable" });
  assert.equal(mock.calls("POST /v1/projects").length, 1);
});

test("connection failures raise connection_error", async () => {
  const closed = await MockServer.start();
  await closed.close();
  const offline = new Differently({ apiKey: "dly_x_y", baseUrl: closed.url, maxRetries: 0 });
  await assert.rejects(offline.version(), { code: "connection_error", status: 0 });
});

test("slow responses raise timeout", async () => {
  mock.on("GET /v1/capabilities", async (_req, res) => {
    await delay(300);
    sendJson(res, 200, { items: [] });
  });
  const impatient = new Differently({ apiKey: "dly_x_y", baseUrl: mock.url, timeoutMs: 50, maxRetries: 0 });
  await assert.rejects(impatient.capabilities.list(), { code: "timeout" });
});

test("a caller's AbortSignal cancels the request", async () => {
  mock.on("GET /v1/sandboxes/sbx_1/wait", async (_req, res) => {
    await delay(300);
    sendJson(res, 200, sandboxInfo());
  });
  const controller = new AbortController();
  const pending = client.sandboxes.wait("sbx_1", "running", { signal: controller.signal });
  setTimeout(() => controller.abort(), 20);
  await assert.rejects(pending, { name: "AbortError" });
});

test("tenant resources map to their routes", async () => {
  mock.on("GET /v1/projects", reply(200, { items: [{ id: "prj_1", orgId: "o", name: "web", quota: {}, createdAt: "t" }] }));
  mock.on("POST /v1/api-keys", reply(201, { id: "key_1", name: "ci", scopes: ["sandboxes:exec"], createdBy: "k", createdAt: "t", revoked: false, token: "dly_key_1_s" }));
  mock.on("DELETE /v1/api-keys/key_1", (_req, res) => void res.writeHead(204).end());
  mock.on("GET /v1/quotas", reply(200, { orgId: "o", orgQuota: {}, orgUsage: { activeSandboxes: 1, queued: 0, cpu: 1, memoryMiB: 512, diskGiB: 1, snapshots: 0 } }));
  mock.on("GET /v1/usage", reply(200, { from: "a", to: "b", groupBy: "trust", lines: [], total: { key: "total" }, note: "" }));
  mock.on("PUT /v1/policies/org:o", reply(200, { scope: "org:o", external: "never", updatedAt: "t" }));

  assert.equal((await client.projects.list({ orgId: "o" }))[0]?.name, "web");
  assert.equal(mock.calls("GET /v1/projects")[0]?.query.get("orgId"), "o");
  assert.equal((await client.apiKeys.create({ name: "ci", scopes: ["sandboxes:exec"] })).token, "dly_key_1_s");
  await client.apiKeys.revoke("key_1");
  assert.equal((await client.quotas.get({ projectId: "prj_1" })).orgUsage.activeSandboxes, 1);
  assert.equal(mock.calls("GET /v1/quotas")[0]?.query.get("projectId"), "prj_1");
  await client.usage.get({ from: new Date("2026-10-01T00:00:00Z"), groupBy: "trust" });
  assert.equal(mock.calls("GET /v1/usage")[0]?.query.get("from"), "2026-10-01T00:00:00.000Z");
  assert.equal((await client.policies.put("org:o", { external: "never" })).external, "never");
  assert.deepEqual(mock.calls("PUT /v1/policies/org:o")[0]?.json(), { external: "never" });
});

test("keys rotate, provider secrets and switching map to their routes", async () => {
  const key = { id: "key_2", name: "ci", scopes: ["sandboxes:exec"], createdBy: "k", createdAt: "t", revoked: false };
  mock.on("POST /v1/api-keys/key_1/rotate", reply(201, { ...key, token: "dly_key_2_s", replaced: { ...key, id: "key_1", expiresAt: "later" } }));
  mock.on("PUT /v1/admin/secrets/daytona-key", reply(201, { name: "daytona-key", fingerprint: "abc", createdAt: "t", createdBy: "k", updatedAt: "t", updatedBy: "k", usedBy: [] }));
  mock.on("POST /v1/admin/pools/daytona/use", reply(200, { pool: { name: "daytona" }, drained: ["opensandbox"] }));

  const rotated = await client.apiKeys.rotate("key_1", { graceSeconds: 3600 });
  assert.equal(rotated.token, "dly_key_2_s");
  assert.equal(rotated.replaced?.id, "key_1");
  assert.deepEqual(mock.calls("POST /v1/api-keys/key_1/rotate")[0]?.json(), { graceSeconds: 3600 });
  assert.equal((await client.admin.secrets.put("daytona-key", "dtn_value")).fingerprint, "abc");
  assert.deepEqual(mock.calls("PUT /v1/admin/secrets/daytona-key")[0]?.json(), { value: "dtn_value" });
  assert.deepEqual((await client.admin.pools.use("daytona", { approveExternal: true })).drained, ["opensandbox"]);
  assert.deepEqual(mock.calls("POST /v1/admin/pools/daytona/use")[0]?.json(), { approveExternal: true });
});

test("audit events iterate with afterSeq", async () => {
  const event = (seq: number) => ({ seq, time: "t", actor: "k", action: "sandbox.create", resource: "sbx", outcome: "ok", prevHash: "", hash: "" });
  mock.on("GET /v1/audit-events", reply(200, { items: [event(1), event(2)], nextCursor: "2" }));
  mock.on("GET /v1/audit-events", reply(200, { items: [event(3)] }));
  const seqs: number[] = [];
  for await (const ev of client.auditEvents.iterate({ action: "sandbox.create", limit: 2 })) seqs.push(ev.seq);
  assert.deepEqual(seqs, [1, 2, 3]);
  const [, second] = mock.calls("GET /v1/audit-events");
  assert.equal(second?.query.get("afterSeq"), "2");
  assert.equal(second?.query.get("action"), "sandbox.create");
});

test("admin namespace", async () => {
  const org = { id: "org_1", name: "Acme", suspended: true, quota: {}, createdAt: "t" };
  mock.on("POST /v1/admin/organizations", reply(201, { organization: { ...org, suspended: false }, project: { id: "prj_1" } }));
  mock.on("POST /v1/admin/organizations/org_1/suspend", reply(200, { organization: org, terminated: 2 }));
  mock.on("POST /v1/admin/pools/edge/drain", reply(200, { name: "edge", draining: true }));
  mock.on("DELETE /v1/admin/pools/edge", (_req, res) => void res.writeHead(204).end());
  mock.on("POST /v1/admin/sandboxes/sbx_1/terminate", reply(202, sandboxInfo({ status: "terminating" })));
  mock.on("GET /v1/admin/audit/verify", reply(200, { ok: true, events: 42, headHash: "abc" }));

  const created = await client.admin.organizations.create({ name: "Acme", defaultProject: "default" });
  assert.equal(created.project?.id, "prj_1");
  const suspended = await client.admin.organizations.suspend("org_1", { terminate: true, reason: "billing" });
  assert.equal(suspended.terminated, 2);
  assert.deepEqual(mock.calls("POST /v1/admin/organizations/org_1/suspend")[0]?.json(), { terminate: true, reason: "billing" });
  assert.equal((await client.admin.pools.drain("edge")).draining, true);
  await client.admin.pools.delete("edge");
  assert.equal((await client.admin.terminateSandbox("sbx_1", { reason: "abuse" })).status, "terminating");
  assert.equal((await client.admin.verifyAudit()).ok, true);
});
