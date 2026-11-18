import os
import tempfile
import unittest
from pathlib import Path
from typing import Any, Dict, Optional
from unittest import mock as patch

from mock_server import MockServer, reply, sandbox_info

from differently import Differently, DifferentlyError
from differently._devserver import is_loopback_url

DEV_TOKEN = "dly_dev_operator_token"
CONFIG = ("DIFFERENTLY_API_KEY", "DIFFERENTLY_PROJECT", "DIFFERENTLY_URL")


def make_home(dev_token: Optional[str] = None) -> "tempfile.TemporaryDirectory[str]":
    """A fresh home directory, with a dev server's token file when one is given.
    Tests never read the real one, which may hold a real token."""
    home = tempfile.TemporaryDirectory(prefix="differently-sdk-")
    if dev_token is not None:
        token_dir = Path(home.name, ".differently", "dev")
        token_dir.mkdir(parents=True)
        (token_dir / "token").write_text(dev_token, encoding="utf-8")
    return home


class LoopbackTests(unittest.TestCase):
    def test_only_this_machine_counts_as_loopback(self) -> None:
        cases = {
            # The table of TestDevTokenOnlyForLoopback in cmd/differently/main_test.go.
            "http://localhost:8080": True,
            "http://LOCALHOST:8080/": True,
            "http://api.localhost:8080": True,
            "http://127.0.0.1:18180": True,
            "http://[::1]:8080": True,
            "https://localhost.example.com": False,
            "https://differently.example.com": False,
            "https://example.com/localhost": False,
            "https://example.com/?next=localhost": False,
            "https://notlocalhost": False,
            "http://10.0.0.5:8080": False,
            "://bad": False,
            # More of the same rule.
            "http://localhost": True,
            "http://127.8.9.10:1": True,
            "http://[::1]/": True,
            "http://[0:0:0:0:0:0:0:1]:8080": True,
            "http://localhost@example.com": False,
            "http://example.com#localhost": False,
            "http://example.com/.localhost": False,
            "http://localhost.:8080": False,
            "http://localhost:port": False,
            "http://0.0.0.0:8080": False,
            "http://192.168.1.10": False,
            "http://128.0.0.1": False,
            "http://[::2]:8080": False,
            "http://[::ffff:127.0.0.1]:8080": False,
            "http://[::1": False,
            "localhost:8080": False,
            "": False,
        }
        for raw, want in cases.items():
            with self.subTest(raw):
                self.assertEqual(is_loopback_url(raw), want)


class DevServerTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.mock = MockServer()
        cls.dev_home = make_home(DEV_TOKEN + "\n")
        cls.empty_home = make_home()
        cls.addClassCleanup(cls.dev_home.cleanup)
        cls.addClassCleanup(cls.empty_home.cleanup)

    @classmethod
    def tearDownClass(cls) -> None:
        cls.mock.close()

    def setUp(self) -> None:
        self.mock.reset()
        self.mock.on("POST /v1/sandboxes", reply(201, sandbox_info()))

    def client(self, home: "tempfile.TemporaryDirectory[str]", env: Optional[Dict[str, str]] = None, **options: Any) -> Differently:
        """Constructs a client that sees only ``env`` and ``home``. The client reads
        its environment when it is constructed; patch.dict restores it afterwards."""
        with patch.patch.dict(os.environ, {"HOME": home.name, "USERPROFILE": home.name, **(env or {})}):
            for name in CONFIG:
                if name not in (env or {}):
                    os.environ.pop(name, None)
            return Differently(**options)

    def created(self):
        calls = self.mock.calls("POST /v1/sandboxes")
        self.assertEqual(len(calls), 1)
        return calls[0].json(), calls[0].headers["authorization"]

    def test_local_dev_server_needs_no_key_and_no_project(self) -> None:
        client = self.client(self.dev_home, {"DIFFERENTLY_URL": self.mock.url})
        self.assertEqual(client.project_id, "prj_default")
        client.sandboxes.create()
        self.assertEqual(self.created(), ({"projectId": "prj_default"}, f"Bearer {DEV_TOKEN}"))

    def test_dev_token_is_not_used_for_another_host(self) -> None:
        for url in ("https://differently.example.com", "https://localhost.example.com", "http://10.0.0.5:8080"):
            for env, options in (({}, {"base_url": url}), ({"DIFFERENTLY_URL": url}, {})):
                with self.subTest(url=url, env=env):
                    with self.assertRaises(DifferentlyError) as ctx:
                        self.client(self.dev_home, env, **options)
                    self.assertEqual(ctx.exception.code, "unauthenticated")
                    self.assertTrue(ctx.exception.message.startswith("missing API key"))

    def test_without_a_token_file_a_loopback_url_still_needs_a_key(self) -> None:
        blank_home = make_home(" \n")
        self.addCleanup(blank_home.cleanup)
        for home in (self.empty_home, blank_home):
            with self.assertRaises(DifferentlyError) as ctx:
                self.client(home, base_url=self.mock.url)
            self.assertEqual(ctx.exception.code, "unauthenticated")
            self.assertEqual(
                ctx.exception.message,
                'missing API key: pass api_key or set DIFFERENTLY_API_KEY (a local "differently-server --dev" needs neither)',
            )

    def test_explicit_key_wins_over_the_dev_token_and_adds_no_project(self) -> None:
        client = self.client(self.dev_home, api_key="dly_explicit", base_url=self.mock.url)
        self.assertIsNone(client.project_id)
        client.sandboxes.create()
        self.assertEqual(self.created(), ({}, "Bearer dly_explicit"))

    def test_env_key_wins_over_the_dev_token_and_adds_no_project(self) -> None:
        client = self.client(self.dev_home, {"DIFFERENTLY_API_KEY": "dly_env_key", "DIFFERENTLY_URL": self.mock.url})
        self.assertIsNone(client.project_id)
        client.sandboxes.create(profile="small")
        self.assertEqual(self.created(), ({"profile": "small"}, "Bearer dly_env_key"))

    def test_environment_project_is_the_default_for_create(self) -> None:
        client = self.client(self.empty_home, {"DIFFERENTLY_PROJECT": "prj_env"}, api_key="dly_explicit", base_url=self.mock.url)
        self.assertEqual(client.project_id, "prj_env")
        client.sandboxes.create(profile="small")
        self.assertEqual(self.created()[0], {"profile": "small", "projectId": "prj_env"})

    def test_project_id_option_wins_over_the_environment(self) -> None:
        client = self.client(
            self.empty_home,
            {"DIFFERENTLY_PROJECT": "prj_env"},
            api_key="dly_explicit",
            base_url=self.mock.url,
            project_id="prj_option",
        )
        self.assertEqual(client.project_id, "prj_option")
        client.sandboxes.create()
        self.assertEqual(self.created()[0], {"projectId": "prj_option"})

    def test_project_named_in_the_call_wins_over_the_default(self) -> None:
        client = self.client(self.empty_home, api_key="dly_explicit", base_url=self.mock.url, project_id="prj_option")
        client.sandboxes.create(project_id="prj_call", profile="small")
        self.assertEqual(self.created()[0], {"projectId": "prj_call", "profile": "small"})

    def test_configured_project_is_kept_when_the_dev_token_is_used(self) -> None:
        client = self.client(self.dev_home, {"DIFFERENTLY_PROJECT": "prj_env"}, base_url=self.mock.url)
        self.assertEqual(client.project_id, "prj_env")
        client.sandboxes.create()
        self.assertEqual(self.created(), ({"projectId": "prj_env"}, f"Bearer {DEV_TOKEN}"))

    def test_only_create_uses_the_default_project(self) -> None:
        self.mock.on("GET /v1/sandboxes", reply(200, {"items": []}))
        self.mock.on("GET /v1/quotas", reply(200, {"orgId": "o", "orgQuota": {}, "orgUsage": {}}))
        client = self.client(self.empty_home, api_key="dly_explicit", base_url=self.mock.url, project_id="prj_option")
        client.sandboxes.list()
        client.quotas.get()
        self.assertIsNone(self.mock.calls("GET /v1/sandboxes")[0].param("projectId"))
        self.assertIsNone(self.mock.calls("GET /v1/quotas")[0].param("projectId"))

    def test_project_id_is_read_only(self) -> None:
        client = self.client(self.empty_home, api_key="dly_explicit", base_url=self.mock.url)
        with self.assertRaises(AttributeError):
            client.project_id = "prj_other"  # type: ignore[misc]


if __name__ == "__main__":
    unittest.main()
