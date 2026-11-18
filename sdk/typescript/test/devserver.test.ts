import assert from "node:assert/strict";
import { rmSync } from "node:fs";
import { after, before, beforeEach, test } from "node:test";
import { isLoopbackUrl } from "../src/devserver.js";
import { Differently } from "../src/index.js";
import { homeEnv, tempHome, withEnv } from "./env.js";
import { MockServer, reply, sandboxInfo } from "./mock-server.js";

const DEV_TOKEN = "dly_dev_operator_token";
const NO_CONFIG = { DIFFERENTLY_API_KEY: undefined, DIFFERENTLY_PROJECT: undefined, DIFFERENTLY_URL: undefined };

let mock: MockServer;
let devHome: string;
let emptyHome: string;

before(async () => {
  mock = await MockServer.start();
  devHome = tempHome(`${DEV_TOKEN}\n`);
  emptyHome = tempHome();
});
after(async () => {
  await mock.close();
  for (const dir of [devHome, emptyHome]) rmSync(dir, { recursive: true, force: true });
});
beforeEach(() => {
  mock.reset();
  mock.on("POST /v1/sandboxes", reply(201, sandboxInfo()));
});

/** The body and Authorization header of the only create call. */
function created(): { body: unknown; authorization: string | undefined } {
  const calls = mock.calls("POST /v1/sandboxes");
  assert.equal(calls.length, 1);
  return { body: calls[0]?.json(), authorization: calls[0]?.headers.authorization };
}

test("only this machine counts as loopback", () => {
  const cases: Record<string, boolean> = {
    // The table of TestDevTokenOnlyForLoopback in cmd/differently/main_test.go.
    "http://localhost:8080": true,
    "http://LOCALHOST:8080/": true,
    "http://api.localhost:8080": true,
    "http://127.0.0.1:18180": true,
    "http://[::1]:8080": true,
    "https://localhost.example.com": false,
    "https://differently.example.com": false,
    "https://example.com/localhost": false,
    "https://example.com/?next=localhost": false,
    "https://notlocalhost": false,
    "http://10.0.0.5:8080": false,
    "://bad": false,
    // More of the same rule.
    "http://localhost": true,
    "http://127.8.9.10:1": true,
    "http://[::1]/": true,
    "http://[0:0:0:0:0:0:0:1]:8080": true,
    "http://localhost@example.com": false,
    "http://example.com#localhost": false,
    "http://example.com/.localhost": false,
    "http://localhost.:8080": false,
    "http://localhost:port": false,
    "http://0.0.0.0:8080": false,
    "http://192.168.1.10": false,
    "http://128.0.0.1": false,
    "http://[::2]:8080": false,
    "http://[::ffff:127.0.0.1]:8080": false,
    "http://[::1": false,
    "localhost:8080": false,
    "": false,
  };
  for (const [raw, want] of Object.entries(cases)) {
    assert.equal(isLoopbackUrl(raw), want, `isLoopbackUrl(${JSON.stringify(raw)})`);
  }
});

test("a local dev server needs no key and no project", async () => {
  const client = withEnv({ ...NO_CONFIG, ...homeEnv(devHome), DIFFERENTLY_URL: mock.url }, () => new Differently());
  assert.equal(client.projectId, "prj_default");
  await client.sandboxes.create();
  assert.deepEqual(created(), { body: { projectId: "prj_default" }, authorization: `Bearer ${DEV_TOKEN}` });
});

test("the dev token is not used for another host", () => {
  const unauthenticated = { name: "DifferentlyError", code: "unauthenticated", message: /^missing API key/ };
  for (const url of ["https://differently.example.com", "https://localhost.example.com", "http://10.0.0.5:8080"]) {
    withEnv({ ...NO_CONFIG, ...homeEnv(devHome) }, () => {
      assert.throws(() => new Differently({ baseUrl: url }), unauthenticated, url);
    });
    withEnv({ ...NO_CONFIG, ...homeEnv(devHome), DIFFERENTLY_URL: url }, () => {
      assert.throws(() => new Differently(), unauthenticated, url);
    });
  }
});

test("without a token file a loopback URL still needs a key", () => {
  const blankHome = tempHome(" \n");
  try {
    for (const home of [emptyHome, blankHome]) {
      withEnv({ ...NO_CONFIG, ...homeEnv(home) }, () => {
        assert.throws(() => new Differently({ baseUrl: mock.url }), {
          code: "unauthenticated",
          message:
            'missing API key: pass { apiKey } or set DIFFERENTLY_API_KEY (a local "differently-server --dev" needs neither)',
        });
      });
    }
  } finally {
    rmSync(blankHome, { recursive: true, force: true });
  }
});

test("an explicit key wins over the dev token and adds no project", async () => {
  const client = withEnv({ ...NO_CONFIG, ...homeEnv(devHome) }, () => new Differently({ apiKey: "dly_explicit", baseUrl: mock.url }));
  assert.equal(client.projectId, undefined);
  await client.sandboxes.create();
  assert.deepEqual(created(), { body: {}, authorization: "Bearer dly_explicit" });
});

test("DIFFERENTLY_API_KEY wins over the dev token and adds no project", async () => {
  const env = { ...NO_CONFIG, ...homeEnv(devHome), DIFFERENTLY_API_KEY: "dly_env_key", DIFFERENTLY_URL: mock.url };
  const client = withEnv(env, () => new Differently());
  assert.equal(client.projectId, undefined);
  await client.sandboxes.create({ profile: "small" });
  assert.deepEqual(created(), { body: { profile: "small" }, authorization: "Bearer dly_env_key" });
});

test("DIFFERENTLY_PROJECT is the default project for create", async () => {
  const env = { ...NO_CONFIG, ...homeEnv(emptyHome), DIFFERENTLY_PROJECT: "prj_env" };
  const client = withEnv(env, () => new Differently({ apiKey: "dly_explicit", baseUrl: mock.url }));
  assert.equal(client.projectId, "prj_env");
  await client.sandboxes.create({ profile: "small" });
  assert.deepEqual(created().body, { profile: "small", projectId: "prj_env" });
});

test("the projectId option wins over DIFFERENTLY_PROJECT", async () => {
  const env = { ...NO_CONFIG, ...homeEnv(emptyHome), DIFFERENTLY_PROJECT: "prj_env" };
  const client = withEnv(env, () => new Differently({ apiKey: "dly_explicit", baseUrl: mock.url, projectId: "prj_option" }));
  assert.equal(client.projectId, "prj_option");
  await client.sandboxes.create();
  assert.deepEqual(created().body, { projectId: "prj_option" });
});

test("a project named in the call wins over the default", async () => {
  const client = withEnv({ ...NO_CONFIG, ...homeEnv(emptyHome) }, () => {
    return new Differently({ apiKey: "dly_explicit", baseUrl: mock.url, projectId: "prj_option" });
  });
  await client.sandboxes.create({ projectId: "prj_call", profile: "small" });
  assert.deepEqual(created().body, { projectId: "prj_call", profile: "small" });
});

test("a configured project is kept when the dev token is used", async () => {
  const env = { ...NO_CONFIG, ...homeEnv(devHome), DIFFERENTLY_PROJECT: "prj_env" };
  const client = withEnv(env, () => new Differently({ baseUrl: mock.url }));
  assert.equal(client.projectId, "prj_env");
  await client.sandboxes.create();
  assert.deepEqual(created(), { body: { projectId: "prj_env" }, authorization: `Bearer ${DEV_TOKEN}` });
});

test("only create uses the default project", async () => {
  mock.on("GET /v1/sandboxes", reply(200, { items: [] }));
  mock.on("GET /v1/quotas", reply(200, { orgId: "o", orgQuota: {}, orgUsage: {} }));
  const client = withEnv({ ...NO_CONFIG, ...homeEnv(emptyHome) }, () => {
    return new Differently({ apiKey: "dly_explicit", baseUrl: mock.url, projectId: "prj_option" });
  });
  await client.sandboxes.list();
  await client.quotas.get();
  assert.equal(mock.calls("GET /v1/sandboxes")[0]?.query.has("projectId"), false);
  assert.equal(mock.calls("GET /v1/quotas")[0]?.query.has("projectId"), false);
});
