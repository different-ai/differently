"""Wire types for the Differently /v1 API.

Records are plain dicts keyed exactly as the server's JSON, typed with
TypedDict so editors can complete them. Timestamps are RFC 3339 strings.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Dict, Generic, List, Literal, Optional, TypedDict, TypeVar

T = TypeVar("T")

SandboxStatus = Literal[
    "queued", "provisioning", "running", "stopped", "paused", "terminating", "terminated", "failed"
]
ExternalMode = Literal["never", "overflow", "allowed"]
PlacementStrategy = Literal["private-first", "cost", "spread"]
ExecEventType = Literal["stdout", "stderr", "exit", "error"]


class Resources(TypedDict, total=False):
    cpu: int
    memoryMiB: int
    diskGiB: int


class Placement(TypedDict, total=False):
    """A request-level placement preference. It can only narrow what policy allows."""

    strategy: PlacementStrategy
    allowedPools: List[str]
    regions: List[str]
    external: ExternalMode


class _CandidateRequired(TypedDict):
    pool: str
    trust: str
    eligible: bool
    hasCapacity: bool
    estimatedCostPerHour: float


class CandidateEvaluation(_CandidateRequired, total=False):
    rank: int
    reasons: List[str]


class _DecisionRequired(TypedDict):
    strategy: str
    external: str
    outcome: str
    reason: str
    policies: Optional[List[str]]
    candidates: Optional[List[CandidateEvaluation]]
    evaluatedAt: str


class PlacementDecision(_DecisionRequired, total=False):
    pool: str


class _SandboxRequired(TypedDict):
    id: str
    orgId: str
    projectId: str
    status: SandboxStatus
    resources: Resources
    classification: str
    priority: int
    attempts: int
    createdBy: str
    createdAt: str
    updatedAt: str


class SandboxInfo(_SandboxRequired, total=False):
    statusReason: str
    template: str
    profile: str
    capabilities: List[str]
    placement: Placement
    pool: str
    adapter: str
    trust: str
    region: str
    labels: Dict[str, str]
    envKeys: List[str]
    ttlSeconds: int
    expiresAt: str
    idleTimeoutSeconds: int
    lastActivityAt: str
    idledAt: str
    fromSnapshot: str
    startedAt: str
    endedAt: str
    decision: PlacementDecision


class _PlacementExplanationRequired(TypedDict):
    sandboxId: str
    status: SandboxStatus
    pool: str
    trust: str
    decision: Optional[PlacementDecision]


class PlacementExplanation(_PlacementExplanationRequired, total=False):
    current: PlacementDecision


class FileInfo(TypedDict):
    path: str
    name: str
    size: int
    mode: str
    isDir: bool
    modTime: str


class _SnapshotRequired(TypedDict):
    id: str
    sandboxId: str
    orgId: str
    projectId: str
    pool: str
    kind: str
    status: str
    createdAt: str
    createdBy: str


class Snapshot(_SnapshotRequired, total=False):
    sizeBytes: int


class PortUrl(TypedDict):
    port: int
    url: str


class Quota(TypedDict, total=False):
    """Quota limits. Zero or absent means unlimited at that level."""

    maxActiveSandboxes: int
    maxQueued: int
    maxCPU: int
    maxMemoryMiB: int
    maxDiskGiB: int
    maxTTLSeconds: int
    maxSnapshots: int
    poolBudgetsPerHour: Dict[str, float]


class QuotaUsage(TypedDict):
    activeSandboxes: int
    queued: int
    cpu: int
    memoryMiB: int
    diskGiB: int
    snapshots: int


class _QuotaReportRequired(TypedDict):
    orgId: str
    orgQuota: Quota
    orgUsage: QuotaUsage


class QuotaReport(_QuotaReportRequired, total=False):
    projectId: str
    projectQuota: Quota
    projectUsage: QuotaUsage


class Organization(TypedDict):
    id: str
    name: str
    suspended: bool
    quota: Quota
    createdAt: str


class Project(TypedDict):
    id: str
    orgId: str
    name: str
    quota: Quota
    createdAt: str


class _ApiKeyRequired(TypedDict):
    id: str
    name: str
    scopes: List[str]
    createdBy: str
    createdAt: str
    revoked: bool


class ApiKey(_ApiKeyRequired, total=False):
    orgId: str
    projectId: str
    expiresAt: str
    lastUsedAt: str
    replacedBy: str  # the key a rotation issued in place of this one


class _CreatedApiKeyOptional(TypedDict, total=False):
    replaced: ApiKey  # on a rotation, the old key's new state


class CreatedApiKey(ApiKey, _CreatedApiKeyOptional):
    """A newly created key. ``token`` is shown only once."""

    token: str


class CostModel(TypedDict, total=False):
    currency: str
    cpuHour: float
    memoryGiBHour: float
    diskGiBHour: float
    sandboxHour: float


class CredentialRef(TypedDict, total=False):
    """Where a pool's credential lives: a server env var, a mounted file, or a credential
    stored in Differently with ``admin.secrets.put``. Never the value itself."""

    env: str
    file: str
    secret: str


class Secret(TypedDict):
    """A provider credential stored in Differently. Its value is never returned."""

    name: str
    fingerprint: str
    createdAt: str
    createdBy: str
    updatedAt: str
    updatedBy: str
    usedBy: List[str]


class UsePoolResult(TypedDict, total=False):
    """What sending new sandboxes to a pool changed."""

    pool: "Pool"
    drained: List[str]
    policy: "PlacementPolicy"


class _PoolRequired(TypedDict):
    name: str
    adapter: str
    trust: str
    enabled: bool
    draining: bool
    costModel: CostModel
    source: str
    active: int


class Pool(_PoolRequired, total=False):
    region: str
    labels: Dict[str, str]
    maxConcurrency: int
    config: Dict[str, str]
    credentialRef: str
    capabilities: List[str]
    health: Dict[str, str]
    capacity: Dict[str, object]


class _PoolCapabilitiesRequired(TypedDict):
    pool: str
    adapter: str
    trust: str
    enabled: bool
    health: str
    capabilities: Optional[List[str]]


class PoolCapabilities(_PoolCapabilitiesRequired, total=False):
    """A pool as tenants see it: no config or credentials."""

    region: str


class PlacementPolicy(TypedDict, total=False):
    """Scope is "global", "org:<id>" or "project:<id>"."""

    scope: str
    allowedPools: List[str]
    deniedPools: List[str]
    allowedRegions: List[str]
    external: ExternalMode
    externalPools: List[str]
    strategy: PlacementStrategy
    maxCostPerHour: float
    maxQueueSeconds: int
    updatedAt: str
    updatedBy: str


class _ClassificationRequired(TypedDict):
    name: str
    external: ExternalMode


class Classification(_ClassificationRequired, total=False):
    description: str


class _ProfileRequired(TypedDict):
    name: str
    resources: Resources


class Profile(_ProfileRequired, total=False):
    template: str
    capabilities: List[str]


class _TemplateRequired(TypedDict):
    name: str


class Template(_TemplateRequired, total=False):
    description: str
    pools: List[str]
    images: Dict[str, str]  # each pool's own image, by pool name


class _AuditEventRequired(TypedDict):
    seq: int
    time: str
    actor: str
    action: str
    resource: str
    outcome: str
    prevHash: str
    hash: str


class AuditEvent(_AuditEventRequired, total=False):
    orgId: str
    projectId: str
    details: Dict[str, str]
    requestId: str


class _AuditVerifyRequired(TypedDict):
    ok: bool
    events: int


class AuditVerifyResult(_AuditVerifyRequired, total=False):
    brokenAt: int
    problem: str
    headHash: str


class UsageLine(TypedDict):
    key: str
    sandboxes: int
    cpuSeconds: float
    memoryGiBSeconds: float
    runSeconds: float
    queueSeconds: float
    estimatedCost: float


# Functional form because "from" is a Python keyword.
UsageReport = TypedDict(
    "UsageReport",
    {"from": str, "to": str, "groupBy": str, "lines": Optional[List[UsageLine]], "total": UsageLine, "note": str},
)


class _OrgResultRequired(TypedDict):
    organization: Organization


class CreateOrganizationResult(_OrgResultRequired, total=False):
    project: Project


class SuspendOrganizationResult(TypedDict):
    organization: Organization
    terminated: int


class Principal(TypedDict):
    """Response of GET /v1/me."""

    keyId: str
    orgId: str
    projectId: str
    scopes: List[str]
    operator: bool


class VersionInfo(TypedDict):
    version: str
    commit: str
    apiVersion: str
    installation: str


TrustReport = Dict[str, object]
"""The operator trust report; see the server's TrustReport for its fields."""


@dataclass(frozen=True)
class ExecResult:
    """The outcome of a finished command. A non-zero exit code is not an error."""

    exit_code: int
    stdout: str
    stderr: str
    duration_ms: int
    truncated: bool = False


@dataclass(frozen=True)
class ExecEvent:
    """One event of a streamed command."""

    type: ExecEventType
    data: str = ""
    exit_code: Optional[int] = None
    error: Optional[str] = None


@dataclass
class Page(Generic[T]):
    """One page of a list. Pass ``next_cursor`` back to get the next page."""

    items: List[T] = field(default_factory=list)
    next_cursor: Optional[str] = None
