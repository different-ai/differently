import os
import time
import unittest
from datetime import datetime, timezone
from unittest import mock as patch

from mock_server import MockServer, no_content, reply, sandbox_info, send_error, send_json

from differently import Differently, DifferentlyError


def unavailable(_r, h):
    send_error(h, 503, "provider_unavailable", "store unavailable", headers={"Retry-After": "0"})


class ClientTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.mock = MockServer()
        cls.client = Differently("dly_test_secret", base_url=cls.mock.url + "/")

    @classmethod
    def tearDownClass(cls) -> None:
        cls.mock.close()

    def setUp(self) -> None:
        self.mock.reset()

    def test_reads_environment_defaults(self) -> None:
        self.mock.on("GET /v1/me", reply(200, {"keyId": "env", "orgId": "o", "projectId": "", "scopes": [], "operator": False}))
        env = {"DIFFERENTLY_API_KEY": "dly_env_key", "DIFFERENTLY_URL": self.mock.url}
        with patch.patch.dict(os.environ, env):
            client = Differently()
        self.assertEqual(client.base_url, self.mock.url)
        self.assertEqual(client.me()["keyId"], "env")
        self.assertEqual(self.mock.requests[0].headers["authorization"], "Bearer dly_env_key")
        self.assertTrue(self.mock.requests[0].headers["user-agent"].startswith("differently-python/"))

    def test_missing_api_key_fails_fast(self) -> None:
        with patch.patch.dict(os.environ, {"DIFFERENTLY_API_KEY": ""}):
            with self.assertRaises(DifferentlyError) as ctx:
                Differently()
        self.assertEqual(ctx.exception.code, "unauthenticated")

    def test_api_key_is_not_in_repr(self) -> None:
        self.assertNotIn("dly_test_secret", repr(self.client) + repr(self.client._http))

    def test_error_envelope(self) -> None:
        self.mock.on(
            "GET /v1/snapshots/snp_x",
            lambda _r, h: send_error(h, 404, "not_found", "snapshot snp_x not found", details={"id": "snp_x"}),
        )
        with self.assertRaises(DifferentlyError) as ctx:
            self.client.snapshots.get("snp_x")
        err = ctx.exception
        self.assertEqual((err.code, err.status, err.message), ("not_found", 404, "snapshot snp_x not found"))
        self.assertEqual((err.details, err.request_id), ({"id": "snp_x"}, "req_test"))
        self.assertEqual(str(err), "not_found: snapshot snp_x not found")

    def test_non_json_error_body(self) -> None:
        def bad_gateway(_r, h):
            h.send_response(502)
            h.send_header("Content-Type", "text/plain")
            h.send_header("X-Request-Id", "req_proxy")
            h.send_header("Content-Length", "11")
            h.end_headers()
            h.wfile.write(b"Bad Gateway")

        self.mock.on("GET /v1/profiles", bad_gateway)
        with self.assertRaises(DifferentlyError) as ctx:
            self.client.profiles.list()
        self.assertEqual((ctx.exception.code, ctx.exception.status, ctx.exception.request_id), ("internal", 502, "req_proxy"))

    def test_redirects_are_not_followed(self) -> None:
        def redirect(_r, h):
            h.send_response(302)
            h.send_header("Location", self.mock.url + "/elsewhere")
            h.send_header("Content-Length", "0")
            h.end_headers()

        self.mock.on("GET /v1/profiles", redirect)
        self.mock.on("GET /elsewhere", reply(200, {"items": []}))
        with self.assertRaises(DifferentlyError) as ctx:
            self.client.profiles.list()
        self.assertEqual(ctx.exception.status, 302)
        self.assertEqual(self.mock.calls("GET /elsewhere"), [])

    def test_get_is_retried_on_503(self) -> None:
        self.mock.on("GET /v1/templates", unavailable).on("GET /v1/templates", unavailable)
        self.mock.on("GET /v1/templates", reply(200, {"items": [{"name": "ubuntu-24.04"}]}))
        self.assertEqual(self.client.templates.list(), [{"name": "ubuntu-24.04"}])
        self.assertEqual(len(self.mock.calls("GET /v1/templates")), 3)

    def test_retries_stop_after_max_retries(self) -> None:
        limited = Differently("dly_x_y", base_url=self.mock.url, max_retries=1)
        self.mock.on(
            "GET /v1/classifications",
            lambda _r, h: send_error(h, 429, "rate_limited", "slow down", headers={"Retry-After": "0"}),
        )
        with self.assertRaises(DifferentlyError) as ctx:
            limited.classifications.list()
        self.assertEqual((ctx.exception.code, ctx.exception.status), ("rate_limited", 429))
        self.assertEqual(len(self.mock.calls("GET /v1/classifications")), 2)

    def test_post_is_not_retried(self) -> None:
        self.mock.on("POST /v1/projects", unavailable)
        with self.assertRaises(DifferentlyError):
            self.client.projects.create("web")
        self.assertEqual(len(self.mock.calls("POST /v1/projects")), 1)

    def test_connection_error(self) -> None:
        closed = MockServer()
        closed.close()
        offline = Differently("dly_x_y", base_url=closed.url, max_retries=0)
        with self.assertRaises(DifferentlyError) as ctx:
            offline.version()
        self.assertEqual((ctx.exception.code, ctx.exception.status), ("connection_error", 0))

    def test_timeout(self) -> None:
        def slow(_r, h):
            time.sleep(0.3)
            send_json(h, 200, {"items": []})

        self.mock.on("GET /v1/capabilities", slow)
        impatient = Differently("dly_x_y", base_url=self.mock.url, timeout=0.05, max_retries=0)
        with self.assertRaises(DifferentlyError) as ctx:
            impatient.capabilities.list()
        self.assertEqual(ctx.exception.code, "timeout")

    def test_tenant_resources(self) -> None:
        project = {"id": "prj_1", "orgId": "o", "name": "web", "quota": {}, "createdAt": "t"}
        key = {"id": "key_1", "name": "ci", "scopes": ["sandboxes:exec"], "createdBy": "k", "createdAt": "t", "revoked": False, "token": "dly_key_1_s"}
        usage = {"activeSandboxes": 1, "queued": 0, "cpu": 1, "memoryMiB": 512, "diskGiB": 1, "snapshots": 0}
        self.mock.on("GET /v1/projects", reply(200, {"items": [project]}))
        self.mock.on("POST /v1/api-keys", reply(201, key))
        self.mock.on("DELETE /v1/api-keys/key_1", no_content)
        self.mock.on("GET /v1/quotas", reply(200, {"orgId": "o", "orgQuota": {}, "orgUsage": usage}))
        self.mock.on("GET /v1/usage", reply(200, {"from": "a", "to": "b", "groupBy": "trust", "lines": [], "total": {}, "note": ""}))
        self.mock.on("PUT /v1/policies/org:o", reply(200, {"scope": "org:o", "external": "never", "updatedAt": "t"}))

        self.assertEqual(self.client.projects.list(org_id="o")[0]["name"], "web")
        self.assertEqual(self.mock.calls("GET /v1/projects")[0].param("orgId"), "o")
        self.assertEqual(self.client.api_keys.create("ci", ["sandboxes:exec"])["token"], "dly_key_1_s")
        self.assertEqual(self.mock.calls("POST /v1/api-keys")[0].json(), {"name": "ci", "scopes": ["sandboxes:exec"]})
        self.assertIsNone(self.client.api_keys.revoke("key_1"))
        self.assertEqual(self.client.quotas.get(project_id="prj_1")["orgUsage"]["activeSandboxes"], 1)
        self.assertEqual(self.mock.calls("GET /v1/quotas")[0].param("projectId"), "prj_1")
        self.client.usage.get(start=datetime(2026, 10, 1, tzinfo=timezone.utc), group_by="trust")
        self.assertEqual(self.mock.calls("GET /v1/usage")[0].param("from"), "2026-10-01T00:00:00+00:00")
        self.assertEqual(self.client.policies.put("org:o", {"external": "never"})["external"], "never")
        self.assertEqual(self.mock.calls("PUT /v1/policies/org:o")[0].json(), {"external": "never"})

    def test_key_rotation_provider_secrets_and_switching(self) -> None:
        key = {"id": "key_2", "name": "ci", "scopes": ["sandboxes:exec"], "createdBy": "k", "createdAt": "t", "revoked": False}
        secret = {"name": "daytona-key", "fingerprint": "abc", "createdAt": "t", "createdBy": "k", "updatedAt": "t", "updatedBy": "k", "usedBy": []}
        self.mock.on("POST /v1/api-keys/key_1/rotate", reply(201, {**key, "token": "dly_key_2_s", "replaced": {**key, "id": "key_1"}}))
        self.mock.on("PUT /v1/admin/secrets/daytona-key", reply(201, secret))
        self.mock.on("POST /v1/admin/pools/daytona/use", reply(200, {"pool": {"name": "daytona"}, "drained": ["opensandbox"]}))

        rotated = self.client.api_keys.rotate("key_1", grace_seconds=3600)
        self.assertEqual((rotated["token"], rotated["replaced"]["id"]), ("dly_key_2_s", "key_1"))
        self.assertEqual(self.mock.calls("POST /v1/api-keys/key_1/rotate")[0].json(), {"graceSeconds": 3600})
        self.assertEqual(self.client.admin.secrets.put("daytona-key", "dtn_value")["fingerprint"], "abc")
        self.assertEqual(self.mock.calls("PUT /v1/admin/secrets/daytona-key")[0].json(), {"value": "dtn_value"})
        self.assertEqual(self.client.admin.pools.use("daytona", approve_external=True)["drained"], ["opensandbox"])
        self.assertEqual(self.mock.calls("POST /v1/admin/pools/daytona/use")[0].json(), {"approveExternal": True})

    def test_audit_events_iterate_with_after_seq(self) -> None:
        def event(seq):
            return {"seq": seq, "time": "t", "actor": "k", "action": "sandbox.create", "resource": "sbx", "outcome": "ok", "prevHash": "", "hash": ""}

        self.mock.on("GET /v1/audit-events", reply(200, {"items": [event(1), event(2)], "nextCursor": "2"}))
        self.mock.on("GET /v1/audit-events", reply(200, {"items": [event(3)]}))
        seqs = [ev["seq"] for ev in self.client.audit_events.iterate(action="sandbox.create", limit=2)]
        self.assertEqual(seqs, [1, 2, 3])
        second = self.mock.calls("GET /v1/audit-events")[1]
        self.assertEqual((second.param("afterSeq"), second.param("action")), ("2", "sandbox.create"))

    def test_admin_namespace(self) -> None:
        org = {"id": "org_1", "name": "Acme", "suspended": True, "quota": {}, "createdAt": "t"}
        self.mock.on("POST /v1/admin/organizations", reply(201, {"organization": org, "project": {"id": "prj_1"}}))
        self.mock.on("POST /v1/admin/organizations/org_1/suspend", reply(200, {"organization": org, "terminated": 2}))
        self.mock.on("POST /v1/admin/pools", reply(201, {"name": "edge", "adapter": "a", "trust": "external"}))
        self.mock.on("POST /v1/admin/pools/edge/drain", reply(200, {"name": "edge", "draining": True}))
        self.mock.on("DELETE /v1/admin/pools/edge", no_content)
        self.mock.on("POST /v1/admin/sandboxes/sbx_1/terminate", reply(202, sandbox_info(status="terminating")))
        self.mock.on("GET /v1/admin/audit/verify", reply(200, {"ok": True, "events": 42}))

        created = self.client.admin.organizations.create("Acme", default_project="default")
        self.assertEqual(created["project"]["id"], "prj_1")
        self.assertEqual(self.mock.calls("POST /v1/admin/organizations")[0].json(), {"name": "Acme", "defaultProject": "default"})
        self.assertEqual(self.client.admin.organizations.suspend("org_1", terminate=True, reason="billing")["terminated"], 2)
        self.assertEqual(
            self.mock.calls("POST /v1/admin/organizations/org_1/suspend")[0].json(), {"terminate": True, "reason": "billing"}
        )
        self.client.admin.pools.create("edge", "remote", trust="external", credentials={"env": "EDGE_TOKEN"}, max_concurrency=4)
        self.assertEqual(
            self.mock.calls("POST /v1/admin/pools")[0].json(),
            {"name": "edge", "adapter": "remote", "trust": "external", "maxConcurrency": 4, "credentials": {"env": "EDGE_TOKEN"}},
        )
        self.assertTrue(self.client.admin.pools.drain("edge")["draining"])
        self.client.admin.pools.delete("edge")
        self.assertEqual(self.client.admin.terminate_sandbox("sbx_1", reason="abuse")["status"], "terminating")
        self.assertTrue(self.client.admin.verify_audit()["ok"])


if __name__ == "__main__":
    unittest.main()
