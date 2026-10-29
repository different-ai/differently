from __future__ import annotations

import json
import math
import time
import uuid
from typing import Any, Dict, Iterator, List, Mapping, Optional, Sequence, Union

from ._errors import DifferentlyError
from ._http import HttpClient, path_segment, read_sse
from .types import (
    ExecEvent,
    ExecResult,
    FileInfo,
    Page,
    Placement,
    PlacementExplanation,
    PortUrl,
    Resources,
    SandboxInfo,
    Snapshot,
)

# The server holds one wait request open for at most this long.
_MAX_WAIT_CALL_SECONDS = 300
DEFAULT_WAIT_SECONDS = 300.0
_TERMINAL = {"terminated", "failed"}

Command = Union[str, Sequence[str]]


def compact(**fields: Any) -> Dict[str, Any]:
    """Drops unset fields so the server's strict JSON decoder sees only real ones."""
    return {k: v for k, v in fields.items() if v is not None}


def _sandbox_path(sandbox_id: str) -> str:
    return f"/v1/sandboxes/{path_segment(sandbox_id)}"


def wait_for_status(http: HttpClient, sandbox_id: str, status: str, timeout: float) -> SandboxInfo:
    """Long-polls until the sandbox reaches ``status``.

    Raises ``invalid_state`` if it ends in another terminal state and
    ``timeout`` if the deadline passes.
    """
    deadline = time.monotonic() + timeout
    while True:
        seconds = min(max(math.ceil(deadline - time.monotonic()), 1), _MAX_WAIT_CALL_SECONDS)
        info: SandboxInfo = http.json(
            "GET",
            f"{_sandbox_path(sandbox_id)}/wait",
            query={"status": status, "timeoutSeconds": seconds},
            timeout=seconds + 30,
        )
        if info["status"] == status:
            return info
        details = compact(sandboxId=sandbox_id, status=info["status"], statusReason=info.get("statusReason") or None)
        if info["status"] in _TERMINAL:
            reason = f" ({info['statusReason']})" if info.get("statusReason") else ""
            raise DifferentlyError(
                "invalid_state", f"sandbox {sandbox_id} is {info['status']}{reason}, not {status}", details=details
            )
        if time.monotonic() >= deadline:
            raise DifferentlyError(
                "timeout",
                f"sandbox {sandbox_id} is still {info['status']} after {timeout:g}s waiting for {status}",
                details=details,
            )


def _exec_body(
    command: Command,
    env: Optional[Mapping[str, str]],
    cwd: Optional[str],
    stdin: Optional[str],
    timeout_seconds: Optional[int],
) -> Dict[str, Any]:
    target: Dict[str, Any] = {"command": command} if isinstance(command, str) else {"args": list(command)}
    return {**target, **compact(env=dict(env) if env else None, cwd=cwd, stdin=stdin, timeoutSeconds=timeout_seconds)}


def _exec_event(name: str, data: str) -> ExecEvent:
    payload = json.loads(data)
    if name == "exit" or (name not in ("stdout", "stderr", "error") and "exitCode" in payload):
        return ExecEvent(type="exit", exit_code=int(payload.get("exitCode", -1)))
    if name == "error" or (name not in ("stdout", "stderr") and payload.get("error")):
        return ExecEvent(type="error", error=payload.get("error") or "unknown error")
    stream = name if name in ("stdout", "stderr") else ("stderr" if payload.get("stream") == "stderr" else "stdout")
    return ExecEvent(type=stream, data=payload.get("data", ""))  # type: ignore[arg-type]


class SandboxFiles:
    """Files inside a running sandbox."""

    def __init__(self, http: HttpClient, sandbox_id: str) -> None:
        self._http = http
        self._path = f"{_sandbox_path(sandbox_id)}/files"

    def list(self, path: Optional[str] = None) -> List[FileInfo]:
        """Lists a directory. Without a path the backend's default directory is listed."""
        return self._http.json("GET", self._path, query={"path": path}).get("items") or []

    def read(self, path: str) -> bytes:
        return self._http.bytes("GET", f"{self._path}/content", query={"path": path}, headers={"Accept": "*/*"})

    def read_text(self, path: str, encoding: str = "utf-8") -> str:
        return self.read(path).decode(encoding)

    def write(self, path: str, content: Union[str, bytes, bytearray]) -> None:
        """Creates or replaces a file. Strings are written as UTF-8."""
        self._http.json("PUT", f"{self._path}/content", query={"path": path}, body=content)


class Sandbox:
    """A handle to one sandbox.

    ``info`` is the latest state the SDK has seen; every method that returns
    the sandbox refreshes it. Used as a context manager, the sandbox is
    deleted on exit.
    """

    def __init__(self, http: HttpClient, info: SandboxInfo) -> None:
        self._http = http
        self.info = info
        self.files = SandboxFiles(http, info["id"])

    @property
    def id(self) -> str:
        return self.info["id"]

    @property
    def status(self) -> str:
        return self.info["status"]

    @property
    def labels(self) -> Dict[str, str]:
        return dict(self.info.get("labels") or {})

    @property
    def _path(self) -> str:
        return _sandbox_path(self.id)

    def __repr__(self) -> str:
        return f"Sandbox(id={self.id!r}, status={self.status!r})"

    def __enter__(self) -> "Sandbox":
        return self

    def __exit__(self, *exc_info: Any) -> None:
        self.delete()

    def refresh(self) -> "Sandbox":
        return self._set(self._http.json("GET", self._path))

    def wait(self, status: str = "running", *, timeout: float = DEFAULT_WAIT_SECONDS) -> "Sandbox":
        """Waits until the sandbox reaches ``status``."""
        return self._set(wait_for_status(self._http, self.id, status, timeout))

    def update(
        self,
        *,
        labels: Optional[Mapping[str, str]] = None,
        ttl_seconds: Optional[int] = None,
        idle_timeout_seconds: Optional[int] = None,
    ) -> "Sandbox":
        """Merges labels (an empty value removes one), resets the TTL, and/or sets
        the idle timeout (at least 60 seconds, or -1 to turn it off)."""
        body = compact(
            labels=dict(labels) if labels is not None else None,
            ttlSeconds=ttl_seconds,
            idleTimeoutSeconds=idle_timeout_seconds,
        )
        return self._set(self._http.json("PATCH", self._path, json=body))

    def delete(self) -> "Sandbox":
        """Requests deletion. The sandbox moves to ``terminating`` (or ``terminated`` if still queued)."""
        return self._set(self._http.json("DELETE", self._path))

    def exec(
        self,
        command: Command,
        *,
        env: Optional[Mapping[str, str]] = None,
        cwd: Optional[str] = None,
        stdin: Optional[str] = None,
        timeout_seconds: Optional[int] = None,
    ) -> ExecResult:
        """Runs a command and returns its output.

        A string runs through ``/bin/sh -c``; a list runs the program directly.
        A non-zero exit code is not an error.
        """
        socket_timeout = self._http.timeout if timeout_seconds is None else max(self._http.timeout, timeout_seconds + 30)
        raw = self._http.json(
            "POST",
            f"{self._path}/exec",
            json=_exec_body(command, env, cwd, stdin, timeout_seconds),
            timeout=socket_timeout,
        )
        return ExecResult(
            exit_code=raw["exitCode"],
            stdout=raw.get("stdout", ""),
            stderr=raw.get("stderr", ""),
            duration_ms=raw.get("durationMs", 0),
            truncated=bool(raw.get("truncated", False)),
        )

    def exec_stream(
        self,
        command: Command,
        *,
        env: Optional[Mapping[str, str]] = None,
        cwd: Optional[str] = None,
        stdin: Optional[str] = None,
        timeout_seconds: Optional[int] = None,
    ) -> Iterator[ExecEvent]:
        """Starts a command and returns an iterator of its output events.

        The iterator ends after an ``exit`` event, or an ``error`` event if the
        backend failed mid-run. Closing it early closes the connection.
        """
        resp = self._http.stream(
            "POST",
            f"{self._path}/exec",
            query={"stream": "1"},
            json=_exec_body(command, env, cwd, stdin, timeout_seconds),
            headers={"Accept": "text/event-stream"},
        )

        def events() -> Iterator[ExecEvent]:
            with resp:
                for name, data in read_sse(resp):
                    event = _exec_event(name, data)
                    yield event
                    if event.type in ("exit", "error"):
                        return
            raise DifferentlyError("connection_error", "exec stream ended before the command finished")

        return events()

    def stop(self) -> "Sandbox":
        return self._lifecycle("stop")

    def start(self) -> "Sandbox":
        return self._lifecycle("start")

    def pause(self) -> "Sandbox":
        return self._lifecycle("pause")

    def resume(self) -> "Sandbox":
        return self._lifecycle("resume")

    def keepalive(self) -> "Sandbox":
        """Counts as activity for idle auto-stop, and wakes an idled sandbox."""
        return self._lifecycle("keepalive")

    def snapshot(self, *, kind: Optional[str] = None) -> Snapshot:
        """Captures a ``disk`` (default) or ``memory`` snapshot."""
        return self._http.json("POST", f"{self._path}/snapshots", json=compact(kind=kind))

    def fork(self, *, wait: bool = True, timeout: float = DEFAULT_WAIT_SECONDS) -> "Sandbox":
        """Snapshots this sandbox and starts a copy from it, by default waiting until it runs."""
        child = Sandbox(self._http, self._http.json("POST", f"{self._path}/fork"))
        return child.wait("running", timeout=timeout) if wait and child.status != "running" else child

    def expose_port(self, port: int) -> PortUrl:
        """Returns a reachable URL for a port the sandbox listens on."""
        return self._http.json("POST", f"{self._path}/ports", json={"port": port})

    def explain_placement(self) -> PlacementExplanation:
        """Explains where the sandbox was (or would be) placed and why."""
        return self._http.json("GET", f"{self._path}/placement")

    def _lifecycle(self, action: str) -> "Sandbox":
        return self._set(self._http.json("POST", f"{self._path}/{action}"))

    def _set(self, info: SandboxInfo) -> "Sandbox":
        self.info = info
        return self


class Sandboxes:
    def __init__(self, http: HttpClient) -> None:
        self._http = http

    def create(
        self,
        *,
        project_id: Optional[str] = None,
        template: Optional[str] = None,
        profile: Optional[str] = None,
        resources: Optional[Resources] = None,
        capabilities: Optional[Sequence[str]] = None,
        classification: Optional[str] = None,
        placement: Optional[Placement] = None,
        ttl_seconds: Optional[int] = None,
        env: Optional[Mapping[str, str]] = None,
        labels: Optional[Mapping[str, str]] = None,
        priority: Optional[int] = None,
        from_snapshot: Optional[str] = None,
        idle_timeout_seconds: Optional[int] = None,
        wait: bool = True,
        timeout: float = DEFAULT_WAIT_SECONDS,
        idempotency_key: Optional[str] = None,
    ) -> Sandbox:
        """Creates a sandbox and, unless ``wait=False``, waits until it is running.

        Creation is retried safely under a per-call idempotency key.
        """
        body = compact(
            projectId=project_id,
            template=template,
            profile=profile,
            resources=resources,
            capabilities=list(capabilities) if capabilities is not None else None,
            classification=classification,
            placement=placement,
            ttlSeconds=ttl_seconds,
            env=dict(env) if env is not None else None,
            labels=dict(labels) if labels is not None else None,
            priority=priority,
            fromSnapshot=from_snapshot,
            idleTimeoutSeconds=idle_timeout_seconds,
        )
        info = self._http.json(
            "POST",
            "/v1/sandboxes",
            json=body,
            headers={"Idempotency-Key": idempotency_key or str(uuid.uuid4())},
            retry=True,
        )
        sandbox = Sandbox(self._http, info)
        return sandbox.wait("running", timeout=timeout) if wait and sandbox.status != "running" else sandbox

    def get(self, sandbox_id: str) -> Sandbox:
        return Sandbox(self._http, self._http.json("GET", _sandbox_path(sandbox_id)))

    def list(
        self,
        *,
        project_id: Optional[str] = None,
        org_id: Optional[str] = None,
        status: Union[str, Sequence[str], None] = None,
        labels: Optional[Mapping[str, str]] = None,
        pool: Optional[str] = None,
        limit: Optional[int] = None,
        cursor: Optional[str] = None,
    ) -> Page[Sandbox]:
        """Returns one page. Use ``iterate`` to walk every page."""
        query = {
            "projectId": project_id,
            "orgId": org_id,
            "status": status if status is None or isinstance(status, str) else ",".join(status),
            "label": [f"{k}={v}" for k, v in labels.items()] if labels else None,
            "pool": pool,
            "limit": limit,
            "cursor": cursor,
        }
        page = self._http.json("GET", "/v1/sandboxes", query=query)
        items = [Sandbox(self._http, info) for info in page.get("items") or []]
        return Page(items=items, next_cursor=page.get("nextCursor") or None)

    def iterate(self, **filters: Any) -> Iterator[Sandbox]:
        """Yields every matching sandbox, following pagination cursors. Takes ``list``'s filters."""
        cursor = filters.pop("cursor", None)
        while True:
            page = self.list(cursor=cursor, **filters)
            yield from page.items
            cursor = page.next_cursor
            if not cursor:
                return

    def update(
        self, sandbox_id: str, *, labels: Optional[Mapping[str, str]] = None, ttl_seconds: Optional[int] = None
    ) -> Sandbox:
        body = compact(labels=dict(labels) if labels is not None else None, ttlSeconds=ttl_seconds)
        return Sandbox(self._http, self._http.json("PATCH", _sandbox_path(sandbox_id), json=body))

    def delete(self, sandbox_id: str) -> Sandbox:
        return Sandbox(self._http, self._http.json("DELETE", _sandbox_path(sandbox_id)))

    def wait(self, sandbox_id: str, status: str = "running", *, timeout: float = DEFAULT_WAIT_SECONDS) -> Sandbox:
        return Sandbox(self._http, wait_for_status(self._http, sandbox_id, status, timeout))
