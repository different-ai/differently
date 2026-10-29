from __future__ import annotations

import http.client
import json
import random
import socket
import time
import urllib.error
import urllib.request
from typing import Any, Dict, Iterator, List, Mapping, Optional, Tuple, Union
from urllib.parse import quote, urlencode

from ._errors import DifferentlyError, error_from_response

__version__ = "0.1.0"

Query = Mapping[str, Any]
_IDEMPOTENT = {"GET", "PUT", "DELETE"}
_MAX_RETRY_AFTER = 30.0
_DEFAULT = object()
# URLError, socket timeouts and connection resets are all OSError subclasses.
_TRANSPORT_ERRORS = (OSError, http.client.HTTPException)


def path_segment(value: str) -> str:
    return quote(value, safe="")


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    """The API never redirects; refusing keeps the bearer token on this origin."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):  # type: ignore[override]
        return None


class HttpClient:
    """Sends authenticated requests, retrying idempotent ones on 429 and 503."""

    def __init__(
        self,
        *,
        api_key: str,
        base_url: str,
        timeout: float,
        max_retries: int,
        headers: Mapping[str, str],
    ) -> None:
        self.__api_key = api_key
        self.base_url = base_url.rstrip("/")
        self.timeout = timeout
        self.max_retries = max_retries
        self._headers = dict(headers)
        self._opener = urllib.request.build_opener(_NoRedirect)

    def __repr__(self) -> str:
        return f"<HttpClient {self.base_url}>"

    def json(self, method: str, path: str, **kwargs: Any) -> Any:
        raw = self.bytes(method, path, **kwargs)
        return json.loads(raw) if raw else None

    def bytes(self, method: str, path: str, **kwargs: Any) -> bytes:
        resp = self._send(method, path, **kwargs)
        with resp:
            try:
                return resp.read()
            except _TRANSPORT_ERRORS as exc:
                raise _transport_error(exc, kwargs.get("timeout", self.timeout)) from exc

    def stream(self, method: str, path: str, **kwargs: Any) -> http.client.HTTPResponse:
        """Opens a streaming response without a socket timeout; the caller closes it."""
        return self._send(method, path, timeout=None, **kwargs)

    def url(self, path: str, query: Optional[Query] = None) -> str:
        pairs = []
        for key, value in (query or {}).items():
            if value is None:
                continue
            if isinstance(value, (list, tuple)):
                pairs.extend((key, str(v)) for v in value)
            elif isinstance(value, bool):
                pairs.append((key, "true" if value else "false"))
            else:
                pairs.append((key, str(value)))
        return self.base_url + path + ("?" + urlencode(pairs) if pairs else "")

    def _send(
        self,
        method: str,
        path: str,
        *,
        query: Optional[Query] = None,
        json: Any = None,
        body: Union[bytes, str, None] = None,
        headers: Optional[Mapping[str, str]] = None,
        retry: Optional[bool] = None,
        timeout: Any = _DEFAULT,
    ) -> http.client.HTTPResponse:
        request_headers: Dict[str, str] = {
            "Accept": "application/json",
            "User-Agent": f"differently-python/{__version__}",
            **self._headers,
            **(headers or {}),
            "Authorization": f"Bearer {self.__api_key}",
        }
        data: Optional[bytes] = None
        if json is not None:
            data = _json_dumps(json)
            request_headers["Content-Type"] = "application/json"
        elif body is not None:
            data = body.encode("utf-8") if isinstance(body, str) else bytes(body)
            request_headers["Content-Type"] = "application/octet-stream"
        url = self.url(path, query)
        retryable = method in _IDEMPOTENT if retry is None else retry
        socket_timeout = self.timeout if timeout is _DEFAULT else timeout

        attempt = 0
        while True:
            can_retry = retryable and attempt < self.max_retries
            request = urllib.request.Request(url, data=data, headers=request_headers, method=method)
            try:
                return self._opener.open(request, timeout=socket_timeout)
            except urllib.error.HTTPError as exc:
                try:
                    payload = exc.read()
                except _TRANSPORT_ERRORS:
                    payload = b""
                finally:
                    exc.close()
                error = error_from_response(exc.code, payload, exc.headers.get("X-Request-Id"))
                if can_retry and _should_retry(error):
                    delay = _retry_after(exc.headers.get("Retry-After"))
                    time.sleep(_backoff(attempt) if delay is None else delay)
                    attempt += 1
                    continue
                raise error from None
            except _TRANSPORT_ERRORS as exc:
                error = _transport_error(exc, socket_timeout)
                if can_retry and error.code == "connection_error":
                    time.sleep(_backoff(attempt))
                    attempt += 1
                    continue
                raise error from exc


def _should_retry(error: DifferentlyError) -> bool:
    # 429 is retried for rate limiting only: quota_exceeded also uses 429 but
    # will not clear by waiting a few seconds.
    return error.status == 503 or (error.status == 429 and error.code != "quota_exceeded")


def _retry_after(value: Optional[str]) -> Optional[float]:
    try:
        seconds = float(value) if value is not None else -1.0
    except ValueError:
        return None
    return min(seconds, _MAX_RETRY_AFTER) if seconds >= 0 else None


def _backoff(attempt: int) -> float:
    return min(8.0, 0.25 * 2**attempt) * (0.5 + random.random() / 2)


def _transport_error(exc: BaseException, timeout: Any) -> DifferentlyError:
    reason = exc.reason if isinstance(exc, urllib.error.URLError) else exc
    if isinstance(reason, (socket.timeout, TimeoutError)):
        after = f" after {timeout}s" if timeout is not None else ""
        return DifferentlyError("timeout", f"request timed out{after}")
    return DifferentlyError("connection_error", f"request failed: {reason}")


def _json_dumps(value: Any) -> bytes:
    return json.dumps(value, separators=(",", ":"), ensure_ascii=False).encode("utf-8")


def read_sse(resp: http.client.HTTPResponse) -> Iterator[Tuple[str, str]]:
    """Yields (event, data) pairs from a text/event-stream response."""
    event = ""
    data: List[str] = []
    while True:
        try:
            raw = resp.readline()
        except _TRANSPORT_ERRORS as exc:
            raise _transport_error(exc, None) from exc
        if not raw:
            return
        line = raw.decode("utf-8").rstrip("\r\n")
        if not line:
            if data:
                yield event or "message", "\n".join(data)
            event, data = "", []
        elif not line.startswith(":"):
            name, _, value = line.partition(":")
            if value.startswith(" "):
                value = value[1:]
            if name == "event":
                event = value
            elif name == "data":
                data.append(value)
