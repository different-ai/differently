"""An in-process HTTP server for tests. Routes are "METHOD /path"; registering a
route several times queues responses, and the last one repeats."""

from __future__ import annotations

import json
import threading
import time
from dataclasses import dataclass
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Callable, Dict, List, Mapping, Optional
from urllib.parse import parse_qs, unquote, urlsplit


@dataclass
class Recorded:
    method: str
    path: str
    query: Dict[str, List[str]]
    headers: Mapping[str, str]
    body: bytes

    def json(self) -> Any:
        return json.loads(self.body.decode("utf-8"))

    def param(self, name: str) -> Optional[str]:
        values = self.query.get(name)
        return values[0] if values else None


Handler = Callable[[Recorded, BaseHTTPRequestHandler], None]


def send_json(h: BaseHTTPRequestHandler, status: int, body: Any, headers: Optional[Mapping[str, str]] = None) -> None:
    data = json.dumps(body).encode("utf-8")
    h.send_response(status)
    h.send_header("Content-Type", "application/json")
    h.send_header("Content-Length", str(len(data)))
    for k, v in (headers or {}).items():
        h.send_header(k, v)
    h.end_headers()
    h.wfile.write(data)


def send_error(
    h: BaseHTTPRequestHandler,
    status: int,
    code: str,
    message: str,
    details: Optional[Mapping[str, str]] = None,
    headers: Optional[Mapping[str, str]] = None,
) -> None:
    error = {"code": code, "message": message, "requestId": "req_test"}
    if details:
        error["details"] = dict(details)
    send_json(h, status, {"error": error}, headers)


def reply(status: int, body: Any, headers: Optional[Mapping[str, str]] = None) -> Handler:
    return lambda _req, h: send_json(h, status, body, headers)


def no_content(_req: Recorded, h: BaseHTTPRequestHandler) -> None:
    h.send_response(204)
    h.end_headers()


def sandbox_info(**overrides: Any) -> Dict[str, Any]:
    info = {
        "id": "sbx_1",
        "orgId": "org_default",
        "projectId": "prj_default",
        "status": "running",
        "profile": "small",
        "resources": {"cpu": 1, "memoryMiB": 512, "diskGiB": 1},
        "classification": "internal",
        "priority": 0,
        "attempts": 1,
        "createdBy": "key:k1",
        "createdAt": "2026-10-28T12:00:00Z",
        "updatedAt": "2026-10-28T12:00:00Z",
    }
    info.update(overrides)
    return info


class MockServer:
    def __init__(self) -> None:
        self.requests: List[Recorded] = []
        self._routes: Dict[str, List[Handler]] = {}
        self._lock = threading.Lock()
        mock = self

        class _Handler(BaseHTTPRequestHandler):
            def _dispatch(self) -> None:
                url = urlsplit(self.path)
                length = int(self.headers.get("Content-Length") or 0)
                recorded = Recorded(
                    method=self.command,
                    path=unquote(url.path),
                    query=parse_qs(url.query),
                    headers={k.lower(): v for k, v in self.headers.items()},
                    body=self.rfile.read(length) if length else b"",
                )
                with mock._lock:
                    mock.requests.append(recorded)
                    queue = mock._routes.get(f"{recorded.method} {recorded.path}")
                    handler = (queue.pop(0) if len(queue) > 1 else queue[0]) if queue else None
                if handler is None:
                    send_error(self, 404, "not_found", f"no mock for {recorded.method} {recorded.path}")
                else:
                    handler(recorded, self)

            do_GET = do_POST = do_PUT = do_PATCH = do_DELETE = _dispatch

            def log_message(self, *args: Any) -> None:
                pass

        class _Server(ThreadingHTTPServer):
            daemon_threads = True

            def handle_error(self, request: Any, client_address: Any) -> None:
                pass  # clients that time out early break the pipe; that is expected

        self._server = _Server(("127.0.0.1", 0), _Handler)
        self.url = f"http://127.0.0.1:{self._server.server_address[1]}"
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)
        self._thread.start()

    def on(self, route: str, handler: Handler) -> "MockServer":
        with self._lock:
            self._routes.setdefault(route, []).append(handler)
        return self

    def reset(self) -> None:
        with self._lock:
            self._routes.clear()
            self.requests.clear()

    def calls(self, route: str) -> List[Recorded]:
        return [r for r in self.requests if f"{r.method} {r.path}" == route]

    def close(self) -> None:
        self._server.shutdown()
        self._server.server_close()


def stream_events(chunks: List[bytes], pause: float = 0.001) -> Handler:
    """Writes a text/event-stream body in pieces, then closes the connection."""

    def handler(_req: Recorded, h: BaseHTTPRequestHandler) -> None:
        h.send_response(200)
        h.send_header("Content-Type", "text/event-stream")
        h.end_headers()
        for chunk in chunks:
            h.wfile.write(chunk)
            h.wfile.flush()
            time.sleep(pause)

    return handler
