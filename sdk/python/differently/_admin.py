from __future__ import annotations

from typing import Any, List, Mapping, Optional

from ._http import HttpClient, path_segment
from ._sandbox import compact
from .types import (
    Agent,
    AuditVerifyResult,
    CostModel,
    CreateOrganizationResult,
    CredentialRef,
    Organization,
    Pool,
    Project,
    Quota,
    SandboxInfo,
    SuspendOrganizationResult,
    TrustReport,
)


class AdminOrganizations:
    def __init__(self, http: HttpClient) -> None:
        self._http = http

    def list(self) -> List[Organization]:
        return self._http.json("GET", "/v1/admin/organizations").get("items") or []

    def create(
        self, name: str, *, quota: Optional[Quota] = None, default_project: Optional[str] = None
    ) -> CreateOrganizationResult:
        """Creates an organization, plus a project named ``default_project`` if given."""
        body = compact(name=name, quota=quota, defaultProject=default_project)
        return self._http.json("POST", "/v1/admin/organizations", json=body)

    def get(self, org_id: str) -> Organization:
        return self._http.json("GET", f"/v1/admin/organizations/{path_segment(org_id)}")

    def update(self, org_id: str, *, name: Optional[str] = None, quota: Optional[Quota] = None) -> Organization:
        body = compact(name=name, quota=quota)
        return self._http.json("PATCH", f"/v1/admin/organizations/{path_segment(org_id)}", json=body)

    def suspend(
        self, org_id: str, *, terminate: bool = False, reason: Optional[str] = None
    ) -> SuspendOrganizationResult:
        """Blocks new work for the organization; ``terminate=True`` also ends its sandboxes."""
        body = compact(terminate=terminate or None, reason=reason)
        return self._http.json("POST", f"/v1/admin/organizations/{path_segment(org_id)}/suspend", json=body)

    def resume(self, org_id: str, *, reason: Optional[str] = None) -> SuspendOrganizationResult:
        body = compact(reason=reason)
        return self._http.json("POST", f"/v1/admin/organizations/{path_segment(org_id)}/resume", json=body)


class AdminProjects:
    def __init__(self, http: HttpClient) -> None:
        self._http = http

    def set_quota(self, project_id: str, quota: Quota) -> Project:
        """Replaces a project's quota."""
        return self._http.json("PATCH", f"/v1/admin/projects/{path_segment(project_id)}", json={"quota": quota})


class AdminPools:
    def __init__(self, http: HttpClient) -> None:
        self._http = http

    def list(self) -> List[Pool]:
        return self._http.json("GET", "/v1/admin/pools").get("items") or []

    def get(self, name: str) -> Pool:
        return self._http.json("GET", f"/v1/admin/pools/{path_segment(name)}")

    def create(
        self,
        name: str,
        adapter: str,
        *,
        trust: Optional[str] = None,
        region: Optional[str] = None,
        labels: Optional[Mapping[str, str]] = None,
        enabled: Optional[bool] = None,
        max_concurrency: Optional[int] = None,
        cost_model: Optional[CostModel] = None,
        config: Optional[Mapping[str, str]] = None,
        credentials: Optional[CredentialRef] = None,
    ) -> Pool:
        """Registers a pool at runtime. ``credentials`` names a server-side env var or file, never a value."""
        body = compact(
            name=name,
            adapter=adapter,
            trust=trust,
            region=region,
            labels=dict(labels) if labels is not None else None,
            enabled=enabled,
            maxConcurrency=max_concurrency,
            costModel=cost_model,
            config=dict(config) if config is not None else None,
            credentials=credentials,
        )
        return self._http.json("POST", "/v1/admin/pools", json=body)

    def update(
        self,
        name: str,
        *,
        enabled: Optional[bool] = None,
        max_concurrency: Optional[int] = None,
        cost_model: Optional[CostModel] = None,
        region: Optional[str] = None,
    ) -> Pool:
        body = compact(enabled=enabled, maxConcurrency=max_concurrency, costModel=cost_model, region=region)
        return self._http.json("PATCH", f"/v1/admin/pools/{path_segment(name)}", json=body)

    def drain(self, name: str) -> Pool:
        """Stops new placements on the pool; running sandboxes continue."""
        return self._http.json("POST", f"/v1/admin/pools/{path_segment(name)}/drain")

    def undrain(self, name: str) -> Pool:
        return self._http.json("POST", f"/v1/admin/pools/{path_segment(name)}/undrain")

    def delete(self, name: str) -> None:
        """Removes an API-registered pool. Pools from the config file cannot be deleted here."""
        self._http.json("DELETE", f"/v1/admin/pools/{path_segment(name)}")


class AdminAgents:
    def __init__(self, http: HttpClient) -> None:
        self._http = http

    def list(self, *, pool: Optional[str] = None) -> List[Agent]:
        return self._http.json("GET", "/v1/admin/agents", query={"pool": pool}).get("items") or []


class Admin:
    """Operator-only endpoints. Every call needs a key with the ``admin`` scope."""

    def __init__(self, http: HttpClient) -> None:
        self._http = http
        self.organizations = AdminOrganizations(http)
        self.projects = AdminProjects(http)
        self.pools = AdminPools(http)
        self.agents = AdminAgents(http)

    def trust_report(self) -> TrustReport:
        """Where workloads can go, where they went, and what Differently cannot prove."""
        return self._http.json("GET", "/v1/admin/trust-report")

    def verify_audit(self) -> AuditVerifyResult:
        """Verifies the hash chain of the whole audit log."""
        return self._http.json("GET", "/v1/admin/audit/verify")

    def terminate_sandbox(self, sandbox_id: str, *, reason: Optional[str] = None) -> SandboxInfo:
        """Terminates any tenant's sandbox."""
        body: Any = compact(reason=reason)
        return self._http.json("POST", f"/v1/admin/sandboxes/{path_segment(sandbox_id)}/terminate", json=body)

    def reconcile(self) -> None:
        """Runs one scheduler reconcile pass now."""
        self._http.json("POST", "/v1/admin/reconcile")
