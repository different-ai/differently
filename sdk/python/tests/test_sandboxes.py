import re
import time
import unittest

from mock_server import MockServer, no_content, reply, sandbox_info, send_error, send_json, stream_events

from differently import Differently, DifferentlyError, ExecEvent


class SandboxTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.mock = MockServer()
        cls.client = Differently("dly_test_secret", base_url=cls.mock.url)

    @classmethod
    def tearDownClass(cls) -> None:
        cls.mock.close()

    def setUp(self) -> None:
        self.mock.reset()

    def running(self, **overrides):
        self.mock.on("GET /v1/sandboxes/sbx_1", reply(200, sandbox_info(**overrides)))
        return self.client.sandboxes.get("sbx_1")

    def test_create_waits_until_running_and_sends_idempotency_key(self) -> None:
        self.mock.on("POST /v1/sandboxes", reply(202, sandbox_info(status="queued")))
        self.mock.on("GET /v1/sandboxes/sbx_1/wait", reply(200, sandbox_info(status="running")))

        sandbox = self.client.sandboxes.create(
            profile="small", placement={"strategy": "private-first", "external": "never"}
        )

        self.assertEqual(sandbox.id, "sbx_1")
        self.assertEqual(sandbox.status, "running")
        create = self.mock.calls("POST /v1/sandboxes")[0]
        self.assertEqual(create.json(), {"profile": "small", "placement": {"strategy": "private-first", "external": "never"}})
        self.assertEqual(create.headers["authorization"], "Bearer dly_test_secret")
        self.assertEqual(create.headers["content-type"], "application/json")
        self.assertRegex(create.headers["idempotency-key"], re.compile(r"^[0-9a-f-]{36}$"))
        wait = self.mock.calls("GET /v1/sandboxes/sbx_1/wait")[0]
        self.assertEqual(wait.param("status"), "running")
        self.assertEqual(wait.param("timeoutSeconds"), "300")

    def test_create_without_wait(self) -> None:
        self.mock.on("POST /v1/sandboxes", reply(202, sandbox_info(status="queued")))
        sandbox = self.client.sandboxes.create(wait=False, idempotency_key="my-key", labels={"team": "ml"})
        self.assertEqual(sandbox.status, "queued")
        call = self.mock.calls("POST /v1/sandboxes")[0]
        self.assertEqual(call.headers["idempotency-key"], "my-key")
        self.assertEqual(call.json(), {"labels": {"team": "ml"}})
        self.assertEqual(self.mock.calls("GET /v1/sandboxes/sbx_1/wait"), [])

    def test_create_is_retried_with_the_same_idempotency_key(self) -> None:
        self.mock.on(
            "POST /v1/sandboxes",
            lambda _r, h: send_error(h, 503, "provider_unavailable", "busy", headers={"Retry-After": "0"}),
        )
        self.mock.on("POST /v1/sandboxes", reply(202, sandbox_info(status="queued")))
        self.client.sandboxes.create(wait=False)
        calls = self.mock.calls("POST /v1/sandboxes")
        self.assertEqual(len(calls), 2)
        self.assertEqual(calls[0].headers["idempotency-key"], calls[1].headers["idempotency-key"])

    def test_quota_exceeded_is_not_retried(self) -> None:
        self.mock.on(
            "POST /v1/sandboxes",
            lambda _r, h: send_error(h, 429, "quota_exceeded", "org cpu limit reached", details={"dimension": "cpu"}),
        )
        with self.assertRaises(DifferentlyError) as ctx:
            self.client.sandboxes.create()
        err = ctx.exception
        self.assertEqual((err.code, err.status, err.details, err.request_id), ("quota_exceeded", 429, {"dimension": "cpu"}, "req_test"))
        self.assertEqual(len(self.mock.calls("POST /v1/sandboxes")), 1)

    def test_wait_raises_invalid_state_when_the_sandbox_fails(self) -> None:
        self.mock.on("GET /v1/sandboxes/sbx_1/wait", reply(200, sandbox_info(status="failed", statusReason="no capacity")))
        with self.assertRaises(DifferentlyError) as ctx:
            self.client.sandboxes.wait("sbx_1")
        self.assertEqual(ctx.exception.code, "invalid_state")
        self.assertEqual(ctx.exception.details["statusReason"], "no capacity")

    def test_wait_raises_timeout_after_its_deadline(self) -> None:
        def slow(_r, h):
            time.sleep(0.3)
            send_json(h, 200, sandbox_info(status="provisioning"))

        self.mock.on("GET /v1/sandboxes/sbx_1/wait", slow)
        with self.assertRaises(DifferentlyError) as ctx:
            self.client.sandboxes.wait("sbx_1", timeout=0.2)
        self.assertEqual(ctx.exception.code, "timeout")
        self.assertEqual(self.mock.calls("GET /v1/sandboxes/sbx_1/wait")[0].param("timeoutSeconds"), "1")

    def test_exec_sends_shell_command_or_argv(self) -> None:
        sandbox = self.running()
        self.mock.on(
            "POST /v1/sandboxes/sbx_1/exec",
            reply(200, {"exitCode": 0, "stdout": "v20.0.0\n", "stderr": "", "durationMs": 12}),
        )
        result = sandbox.exec("node --version")
        self.assertEqual((result.exit_code, result.stdout, result.duration_ms), (0, "v20.0.0\n", 12))
        sandbox.exec(["ls", "-la"], cwd="/work", env={"A": "1"}, timeout_seconds=5)
        first, second = self.mock.calls("POST /v1/sandboxes/sbx_1/exec")
        self.assertEqual(first.json(), {"command": "node --version"})
        self.assertEqual(second.json(), {"args": ["ls", "-la"], "cwd": "/work", "env": {"A": "1"}, "timeoutSeconds": 5})

    def test_exec_is_not_retried(self) -> None:
        sandbox = self.running()
        self.mock.on(
            "POST /v1/sandboxes/sbx_1/exec",
            lambda _r, h: send_error(h, 503, "provider_unavailable", "down", headers={"Retry-After": "0"}),
        )
        with self.assertRaises(DifferentlyError) as ctx:
            sandbox.exec("true")
        self.assertEqual((ctx.exception.code, ctx.exception.status), ("provider_unavailable", 503))
        self.assertEqual(len(self.mock.calls("POST /v1/sandboxes/sbx_1/exec")), 1)

    def test_exec_stream_yields_events(self) -> None:
        sandbox = self.running()
        wire = (
            'event: stdout\ndata: {"stream":"stdout","data":"building\\n"}\n\n'
            ": keep-alive\n\n"
            'event: stderr\ndata: {"stream":"stderr","data":"warn ünïcode"}\n\n'
            'event: exit\ndata: {"exitCode":2}\n\n'
        ).encode("utf-8")
        chunks = [wire[i : i + 7] for i in range(0, len(wire), 7)]
        self.mock.on("POST /v1/sandboxes/sbx_1/exec", stream_events(chunks))

        events = list(sandbox.exec_stream("make test"))

        self.assertEqual(
            events,
            [
                ExecEvent(type="stdout", data="building\n"),
                ExecEvent(type="stderr", data="warn ünïcode"),
                ExecEvent(type="exit", exit_code=2),
            ],
        )
        call = self.mock.calls("POST /v1/sandboxes/sbx_1/exec")[0]
        self.assertEqual(call.param("stream"), "1")
        self.assertEqual(call.headers["accept"], "text/event-stream")
        self.assertEqual(call.json(), {"command": "make test"})

    def test_exec_stream_reports_backend_errors_and_truncation(self) -> None:
        sandbox = self.running()
        self.mock.on("POST /v1/sandboxes/sbx_1/exec", stream_events([b'event: error\ndata: {"error":"agent lost"}\n\n']))
        self.mock.on(
            "POST /v1/sandboxes/sbx_1/exec",
            stream_events([b'event: stdout\ndata: {"stream":"stdout","data":"partial"}\n\n']),
        )
        self.assertEqual(list(sandbox.exec_stream("x")), [ExecEvent(type="error", error="agent lost")])

        seen = []
        with self.assertRaises(DifferentlyError) as ctx:
            for event in sandbox.exec_stream("x"):
                seen.append(event)
        self.assertEqual(ctx.exception.code, "connection_error")
        self.assertEqual(seen, [ExecEvent(type="stdout", data="partial")])

    def test_exec_stream_raises_errors_before_streaming(self) -> None:
        sandbox = self.running(status="stopped")
        self.mock.on(
            "POST /v1/sandboxes/sbx_1/exec",
            lambda _r, h: send_error(h, 409, "invalid_state", "sandbox sbx_1 is stopped, not running"),
        )
        with self.assertRaises(DifferentlyError) as ctx:
            sandbox.exec_stream("x")
        self.assertEqual((ctx.exception.code, ctx.exception.status), ("invalid_state", 409))

    def test_files_round_trip(self) -> None:
        sandbox = self.running()
        files = {}

        def put(req, h):
            files[req.param("path")] = req.body
            no_content(req, h)

        def get(req, h):
            data = files.get(req.param("path"))
            if data is None:
                return send_error(h, 404, "not_found", "no such file")
            h.send_response(200)
            h.send_header("Content-Type", "application/octet-stream")
            h.send_header("Content-Length", str(len(data)))
            h.end_headers()
            h.wfile.write(data)

        listing = [{"path": "/work/a.txt", "name": "a.txt", "size": 2, "mode": "-rw-r--r--", "isDir": False, "modTime": "t"}]
        self.mock.on("PUT /v1/sandboxes/sbx_1/files/content", put)
        self.mock.on("GET /v1/sandboxes/sbx_1/files/content", get)
        self.mock.on("GET /v1/sandboxes/sbx_1/files", reply(200, {"items": listing}))

        sandbox.files.write("/work/a.txt", "hi")
        sandbox.files.write("/work/b.bin", bytes([0, 255, 7]))
        self.assertEqual(sandbox.files.read_text("/work/a.txt"), "hi")
        self.assertEqual(sandbox.files.read("/work/b.bin"), bytes([0, 255, 7]))
        self.assertEqual(sandbox.files.list("/work")[0]["name"], "a.txt")
        put_call = self.mock.calls("PUT /v1/sandboxes/sbx_1/files/content")[0]
        self.assertEqual(put_call.headers["content-type"], "application/octet-stream")
        self.assertEqual(self.mock.calls("GET /v1/sandboxes/sbx_1/files")[0].param("path"), "/work")
        with self.assertRaises(DifferentlyError) as ctx:
            sandbox.files.read("/missing")
        self.assertEqual(ctx.exception.code, "not_found")

    def test_lifecycle_snapshot_fork_ports_placement(self) -> None:
        sandbox = self.running()
        snap = {"id": "snp_1", "sandboxId": "sbx_1", "orgId": "o", "projectId": "p", "pool": "local", "kind": "memory", "status": "ready", "createdAt": "t", "createdBy": "k"}
        self.mock.on("POST /v1/sandboxes/sbx_1/stop", reply(200, sandbox_info(status="stopped")))
        self.mock.on("POST /v1/sandboxes/sbx_1/start", reply(200, sandbox_info(status="running")))
        self.mock.on("POST /v1/sandboxes/sbx_1/snapshots", reply(201, snap))
        self.mock.on("POST /v1/sandboxes/sbx_1/fork", reply(202, sandbox_info(id="sbx_2", status="queued")))
        self.mock.on("GET /v1/sandboxes/sbx_2/wait", reply(200, sandbox_info(id="sbx_2", status="running")))
        self.mock.on("POST /v1/sandboxes/sbx_1/ports", reply(200, {"port": 3000, "url": "https://p3000.example.test"}))
        self.mock.on(
            "GET /v1/sandboxes/sbx_1/placement",
            reply(200, {"sandboxId": "sbx_1", "status": "running", "pool": "local", "trust": "private", "decision": None}),
        )
        self.mock.on("DELETE /v1/sandboxes/sbx_1", reply(202, sandbox_info(status="terminating")))

        self.assertEqual(sandbox.stop().status, "stopped")
        self.assertEqual(sandbox.start().status, "running")
        self.assertEqual(sandbox.snapshot(kind="memory")["id"], "snp_1")
        self.assertEqual(self.mock.calls("POST /v1/sandboxes/sbx_1/snapshots")[0].json(), {"kind": "memory"})
        fork = sandbox.fork()
        self.assertEqual((fork.id, fork.status), ("sbx_2", "running"))
        self.assertEqual(sandbox.expose_port(3000)["url"], "https://p3000.example.test")
        self.assertEqual(self.mock.calls("POST /v1/sandboxes/sbx_1/ports")[0].json(), {"port": 3000})
        self.assertEqual(sandbox.explain_placement()["trust"], "private")
        with sandbox:
            pass
        self.assertEqual(sandbox.status, "terminating", "the context manager deletes on exit")

    def test_iterate_follows_cursors_and_encodes_filters(self) -> None:
        self.mock.on("GET /v1/sandboxes", reply(200, {"items": [sandbox_info(id="a"), sandbox_info(id="b")], "nextCursor": "b"}))
        self.mock.on("GET /v1/sandboxes", reply(200, {"items": [sandbox_info(id="c")]}))
        ids = [sb.id for sb in self.client.sandboxes.iterate(status=["running", "paused"], labels={"team": "ml"}, limit=2)]
        self.assertEqual(ids, ["a", "b", "c"])
        first, second = self.mock.calls("GET /v1/sandboxes")
        self.assertEqual(first.param("status"), "running,paused")
        self.assertEqual(first.query["label"], ["team=ml"])
        self.assertEqual(first.param("limit"), "2")
        self.assertIsNone(first.param("cursor"))
        self.assertEqual(second.param("cursor"), "b")

    def test_update_sends_only_given_fields(self) -> None:
        self.mock.on("PATCH /v1/sandboxes/sbx_1", reply(200, sandbox_info(labels={"team": "ml"})))
        sandbox = self.client.sandboxes.update("sbx_1", labels={"team": "ml"})
        self.assertEqual(sandbox.labels, {"team": "ml"})
        self.assertEqual(self.mock.calls("PATCH /v1/sandboxes/sbx_1")[0].json(), {"labels": {"team": "ml"}})


if __name__ == "__main__":
    unittest.main()
