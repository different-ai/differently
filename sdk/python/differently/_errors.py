from __future__ import annotations

import json
from typing import Dict, Mapping, Optional

_STATUS_CODES = {
    400: "invalid_request",
    401: "unauthenticated",
    403: "permission_denied",
    404: "not_found",
    409: "conflict",
    422: "no_eligible_pool",
    429: "rate_limited",
    501: "unsupported_operation",
    503: "provider_unavailable",
}


class DifferentlyError(Exception):
    """Every failure the SDK reports.

    Branch on ``code``, never on the message. Server codes include
    ``invalid_request``, ``unauthenticated``, ``permission_denied``,
    ``not_found``, ``conflict``, ``quota_exceeded``, ``no_eligible_pool``,
    ``unsupported_capability``, ``unsupported_operation``,
    ``provider_unavailable``, ``invalid_state``, ``rate_limited``,
    ``suspended`` and ``internal``. The SDK itself raises ``timeout`` and
    ``connection_error`` with ``status`` 0.
    """

    def __init__(
        self,
        code: str,
        message: str,
        *,
        status: int = 0,
        details: Optional[Mapping[str, str]] = None,
        request_id: Optional[str] = None,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.status = status
        self.details: Dict[str, str] = dict(details or {})
        self.request_id = request_id

    def __str__(self) -> str:
        return f"{self.code}: {self.message}"

    def __repr__(self) -> str:
        return f"DifferentlyError(code={self.code!r}, status={self.status}, message={self.message!r})"


def error_from_response(status: int, body: bytes, header_request_id: Optional[str]) -> DifferentlyError:
    """Builds an error from a non-2xx response, tolerating non-JSON bodies from proxies."""
    err: dict = {}
    try:
        envelope = json.loads(body.decode("utf-8"))
        if isinstance(envelope, dict) and isinstance(envelope.get("error"), dict):
            err = envelope["error"]
    except (ValueError, UnicodeDecodeError):
        pass
    message = err.get("message")
    if not isinstance(message, str):
        text = body.decode("utf-8", "replace")[:200]
        message = f"HTTP {status}" + (f": {text}" if text else "")
    details = err.get("details")
    return DifferentlyError(
        err["code"] if isinstance(err.get("code"), str) else _STATUS_CODES.get(status, "internal"),
        message,
        status=status,
        details=details if isinstance(details, dict) else None,
        request_id=err.get("requestId") or header_request_id,
    )
