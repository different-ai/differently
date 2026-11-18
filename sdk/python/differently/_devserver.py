from __future__ import annotations

import ipaddress
from pathlib import Path
from typing import Optional
from urllib.parse import urlsplit

#: The project a local ``differently-server --dev`` creates.
DEV_PROJECT_ID = "prj_default"

_IPV6_LOOPBACK = ipaddress.IPv6Address("::1")


def is_loopback_url(raw: str) -> bool:
    """Reports whether ``raw`` points at this machine: localhost, a *.localhost
    name or a loopback address (127.0.0.0/8 or ::1).

    The dev operator token is sent nowhere else; a host that merely contains
    "localhost" (for example localhost.example.com) is someone else's server.
    """
    try:
        parts = urlsplit(raw)
        host = parts.hostname or ""
        parts.port  # raises on a port that is not a number: not a URL to trust
    except ValueError:
        return False
    if host == "localhost" or host.endswith(".localhost"):
        return True
    try:
        ip = ipaddress.ip_address(host)
    except ValueError:
        return False
    if isinstance(ip, ipaddress.IPv4Address):
        return ip.is_loopback
    # Not is_loopback: what it says about IPv4-mapped addresses depends on the Python version.
    return ip == _IPV6_LOOPBACK


def read_dev_token() -> Optional[str]:
    """The operator token a local ``differently-server --dev`` wrote to
    ~/.differently/dev/token, or None when there is none to read."""
    try:
        token = (Path.home() / ".differently" / "dev" / "token").read_text(encoding="utf-8").strip()
    except (OSError, RuntimeError, KeyError, ValueError):
        # No file, no permission, no home directory, or not text: no token.
        return None
    return token or None
