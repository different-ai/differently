from __future__ import annotations

import os
from typing import Mapping, Optional

from ._admin import Admin
from ._errors import DifferentlyError
from ._http import HttpClient
from ._resources import ApiKeys, AuditEvents, Listing, Policies, Projects, Quotas, Snapshots, Usage
from ._sandbox import Sandboxes
from .types import Classification, PoolCapabilities, Principal, Profile, Template, VersionInfo

DEFAULT_BASE_URL = "http://localhost:8080"


class Differently:
    """Client for the Differently /v1 API.

    ``api_key`` defaults to ``DIFFERENTLY_API_KEY`` and ``base_url`` to
    ``DIFFERENTLY_URL``, then http://localhost:8080. ``timeout`` is the socket
    timeout in seconds; long polls and streams size their own. Idempotent
    requests are retried up to ``max_retries`` times on 429 (rate limited),
    503 and connection errors.
    """

    def __init__(
        self,
        api_key: Optional[str] = None,
        *,
        base_url: Optional[str] = None,
        timeout: float = 120.0,
        max_retries: int = 3,
        headers: Optional[Mapping[str, str]] = None,
    ) -> None:
        api_key = api_key or os.environ.get("DIFFERENTLY_API_KEY")
        if not api_key:
            raise DifferentlyError("unauthenticated", "missing API key: pass api_key or set DIFFERENTLY_API_KEY")
        self._http = HttpClient(
            api_key=api_key,
            base_url=base_url or os.environ.get("DIFFERENTLY_URL") or DEFAULT_BASE_URL,
            timeout=timeout,
            max_retries=max_retries,
            headers=headers or {},
        )
        self.base_url = self._http.base_url
        self.sandboxes = Sandboxes(self._http)
        self.snapshots = Snapshots(self._http)
        self.projects = Projects(self._http)
        self.api_keys = ApiKeys(self._http)
        self.quotas = Quotas(self._http)
        self.usage = Usage(self._http)
        self.audit_events = AuditEvents(self._http)
        self.policies = Policies(self._http)
        #: Pools as tenants see them: trust, region, health and supported features.
        self.capabilities: Listing[PoolCapabilities] = Listing(self._http, "/v1/capabilities")
        self.profiles: Listing[Profile] = Listing(self._http, "/v1/profiles")
        self.templates: Listing[Template] = Listing(self._http, "/v1/templates")
        self.classifications: Listing[Classification] = Listing(self._http, "/v1/classifications")
        #: Operator-only endpoints.
        self.admin = Admin(self._http)

    def __repr__(self) -> str:
        return f"Differently(base_url={self.base_url!r})"

    def me(self) -> Principal:
        """Describes the calling key: organization, project, scopes and operator status."""
        return self._http.json("GET", "/v1/me")

    def version(self) -> VersionInfo:
        """Server version and installation ID."""
        return self._http.json("GET", "/version")
