from __future__ import annotations

from datetime import datetime, timezone
from typing import Any, Generic, Iterator, List, Optional, Sequence, TypeVar, Union

from ._http import HttpClient, path_segment
from ._sandbox import compact
from .types import (
    ApiKey,
    AuditEvent,
    CreatedApiKey,
    Page,
    PlacementPolicy,
    Project,
    Quota,
    QuotaReport,
    Snapshot,
    UsageReport,
)

T = TypeVar("T")
Time = Union[datetime, str]


class Listing(Generic[T]):
    """A read-only collection the server returns in one response."""

    def __init__(self, http: HttpClient, path: str) -> None:
        self._http = http
        self._path = path

    def list(self) -> List[T]:
        return self._http.json("GET", self._path).get("items") or []


class Snapshots:
    def __init__(self, http: HttpClient) -> None:
        self._http = http

    def list(self) -> List[Snapshot]:
        return self._http.json("GET", "/v1/snapshots").get("items") or []

    def get(self, snapshot_id: str) -> Snapshot:
        return self._http.json("GET", f"/v1/snapshots/{path_segment(snapshot_id)}")

    def delete(self, snapshot_id: str) -> None:
        self._http.json("DELETE", f"/v1/snapshots/{path_segment(snapshot_id)}")


class Projects:
    def __init__(self, http: HttpClient) -> None:
        self._http = http

    def list(self, *, org_id: Optional[str] = None) -> List[Project]:
        """``org_id`` is honored for operator keys only."""
        return self._http.json("GET", "/v1/projects", query={"orgId": org_id}).get("items") or []

    def create(self, name: str, *, org_id: Optional[str] = None, quota: Optional[Quota] = None) -> Project:
        """``org_id`` is for operator keys; ``quota`` needs org:admin or an operator key."""
        return self._http.json("POST", "/v1/projects", json=compact(name=name, orgId=org_id, quota=quota))

    def get(self, project_id: str) -> Project:
        return self._http.json("GET", f"/v1/projects/{path_segment(project_id)}")

    def delete(self, project_id: str) -> None:
        self._http.json("DELETE", f"/v1/projects/{path_segment(project_id)}")


class ApiKeys:
    def __init__(self, http: HttpClient) -> None:
        self._http = http

    def list(self, *, org_id: Optional[str] = None) -> List[ApiKey]:
        """``org_id`` is honored for operator keys only."""
        return self._http.json("GET", "/v1/api-keys", query={"orgId": org_id}).get("items") or []

    def create(
        self,
        name: str,
        scopes: Sequence[str],
        *,
        org_id: Optional[str] = None,
        project_id: Optional[str] = None,
        ttl_hours: Optional[int] = None,
    ) -> CreatedApiKey:
        """Creates a key. The returned ``token`` is shown only once."""
        body = compact(name=name, scopes=list(scopes), orgId=org_id, projectId=project_id, ttlHours=ttl_hours)
        return self._http.json("POST", "/v1/api-keys", json=body)

    def revoke(self, key_id: str) -> None:
        self._http.json("DELETE", f"/v1/api-keys/{path_segment(key_id)}")


class Quotas:
    def __init__(self, http: HttpClient) -> None:
        self._http = http

    def get(self, *, org_id: Optional[str] = None, project_id: Optional[str] = None) -> QuotaReport:
        """Limits and current usage. Operator keys must pass ``org_id``."""
        return self._http.json("GET", "/v1/quotas", query={"orgId": org_id, "projectId": project_id})


def _rfc3339(value: Optional[Time]) -> Optional[str]:
    if isinstance(value, datetime):
        return (value if value.tzinfo else value.replace(tzinfo=timezone.utc)).isoformat()
    return value


class Usage:
    def __init__(self, http: HttpClient) -> None:
        self._http = http

    def get(
        self,
        *,
        start: Optional[Time] = None,
        end: Optional[Time] = None,
        group_by: Optional[str] = None,
        org_id: Optional[str] = None,
    ) -> UsageReport:
        """Estimated usage over a window (default: the last 30 days, grouped by pool).

        ``group_by`` is ``pool``, ``project``, ``org`` or ``trust``. Naive
        datetimes are treated as UTC.
        """
        query = {"from": _rfc3339(start), "to": _rfc3339(end), "groupBy": group_by, "orgId": org_id}
        return self._http.json("GET", "/v1/usage", query=query)


class AuditEvents:
    def __init__(self, http: HttpClient) -> None:
        self._http = http

    def list(
        self,
        *,
        resource: Optional[str] = None,
        action: Optional[str] = None,
        after_seq: Optional[int] = None,
        limit: Optional[int] = None,
        org_id: Optional[str] = None,
    ) -> Page[AuditEvent]:
        """Returns one page in ascending sequence order."""
        query = {"resource": resource, "action": action, "afterSeq": after_seq, "limit": limit, "orgId": org_id}
        page = self._http.json("GET", "/v1/audit-events", query=query)
        return Page(items=page.get("items") or [], next_cursor=page.get("nextCursor") or None)

    def iterate(self, **filters: Any) -> Iterator[AuditEvent]:
        """Yields every matching event, following pagination. Takes ``list``'s filters."""
        after_seq = filters.pop("after_seq", None)
        while True:
            page = self.list(after_seq=after_seq, **filters)
            yield from page.items
            if not page.next_cursor:
                return
            after_seq = int(page.next_cursor)


class Policies:
    def __init__(self, http: HttpClient) -> None:
        self._http = http

    def list(self) -> List[PlacementPolicy]:
        """Placement policies visible to this key, including the global one."""
        return self._http.json("GET", "/v1/policies").get("items") or []

    def put(self, scope: str, policy: PlacementPolicy) -> PlacementPolicy:
        """Replaces the policy for a scope: ``global``, ``org:<id>`` or ``project:<id>``."""
        return self._http.json("PUT", f"/v1/policies/{path_segment(scope)}", json=dict(policy))

    def delete(self, scope: str) -> None:
        self._http.json("DELETE", f"/v1/policies/{path_segment(scope)}")
