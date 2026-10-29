// Wire types for the Differently /v1 API. They mirror pkg/api in the server.
// Timestamps are RFC 3339 strings exactly as the server sends them.

/** Lifecycle state of a sandbox. `terminated` and `failed` are terminal. */
export type SandboxStatus =
  | "queued"
  | "provisioning"
  | "running"
  | "stopped"
  | "paused"
  | "terminating"
  | "terminated"
  | "failed";

/** How far a sandbox may be placed outside private pools, most restrictive first. */
export type ExternalMode = "never" | "overflow" | "allowed";

export type PlacementStrategy = "private-first" | "cost" | "spread";

/** Feature names a sandbox can require. Unknown names are rejected by the server. */
export type SandboxCapability =
  | "exec"
  | "exec.stream"
  | "files"
  | "lifecycle.stop-start"
  | "lifecycle.pause-resume"
  | "snapshots.disk"
  | "snapshots.memory"
  | "fork"
  | "persistence.disk"
  | "ports.expose"
  | "ssh"
  | "pty"
  | "network.egress-control"
  | "gpu"
  | "arch.amd64"
  | "arch.arm64"
  | "isolation.microvm"
  | "isolation.container"
  | (string & {});

export interface Resources {
  cpu: number;
  memoryMiB: number;
  diskGiB: number;
}

/** A request-level placement preference. It can only narrow what policy allows. */
export interface Placement {
  strategy?: PlacementStrategy;
  allowedPools?: string[];
  regions?: string[];
  /** Lowers the effective external mode (for example to "never"); it never raises it. */
  external?: ExternalMode;
}

export interface CreateSandboxParams {
  projectId?: string;
  template?: string;
  profile?: string;
  resources?: Partial<Resources>;
  capabilities?: SandboxCapability[];
  classification?: string;
  placement?: Placement;
  ttlSeconds?: number;
  env?: Record<string, string>;
  labels?: Record<string, string>;
  priority?: number;
  fromSnapshot?: string;
  /** Stop or pause after this many seconds unused (at least 60); the next call wakes it. -1 turns it off. */
  idleTimeoutSeconds?: number;
}

export interface UpdateSandboxParams {
  /** Labels to merge in. An empty string value removes that label. */
  labels?: Record<string, string>;
  ttlSeconds?: number;
  /** At least 60, or -1 to turn idle auto-stop off. */
  idleTimeoutSeconds?: number;
}

export interface ListSandboxesParams {
  projectId?: string;
  /** Operator keys only. */
  orgId?: string;
  status?: SandboxStatus | SandboxStatus[];
  /** Every label must match. */
  labels?: Record<string, string>;
  pool?: string;
  /** Page size, 1-500 (server default 50). */
  limit?: number;
  cursor?: string;
}

export interface SandboxInfo {
  id: string;
  orgId: string;
  projectId: string;
  status: SandboxStatus;
  statusReason?: string;
  template?: string;
  profile?: string;
  resources: Resources;
  capabilities?: string[];
  classification: string;
  placement?: Placement;
  pool?: string;
  adapter?: string;
  trust?: string;
  region?: string;
  labels?: Record<string, string>;
  envKeys?: string[];
  priority: number;
  ttlSeconds?: number;
  expiresAt?: string;
  idleTimeoutSeconds?: number;
  lastActivityAt?: string;
  /** Set while stopped or paused for being idle; the next exec, file, port or keepalive call wakes it. */
  idledAt?: string;
  fromSnapshot?: string;
  attempts: number;
  createdBy: string;
  createdAt: string;
  updatedAt: string;
  startedAt?: string;
  endedAt?: string;
  decision?: PlacementDecision;
}

export interface PlacementDecision {
  pool?: string;
  strategy: string;
  external: string;
  outcome: "placed" | "waiting" | "rejected" | (string & {});
  reason: string;
  policies: string[] | null;
  candidates: CandidateEvaluation[] | null;
  evaluatedAt: string;
}

export interface CandidateEvaluation {
  pool: string;
  trust: string;
  eligible: boolean;
  hasCapacity: boolean;
  estimatedCostPerHour: number;
  rank?: number;
  reasons?: string[];
}

/** Response of GET /v1/sandboxes/{id}/placement. */
export interface PlacementExplanation {
  sandboxId: string;
  status: SandboxStatus;
  pool: string;
  trust: string;
  /** The decision recorded when the sandbox was placed, if any. */
  decision: PlacementDecision | null;
  /** For queued sandboxes, what placement would decide right now. */
  current?: PlacementDecision;
}

export interface ExecOptions {
  env?: Record<string, string>;
  cwd?: string;
  stdin?: string;
  timeoutSeconds?: number;
  signal?: AbortSignal;
}

export interface ExecResult {
  exitCode: number;
  stdout: string;
  stderr: string;
  durationMs: number;
  truncated?: boolean;
}

/** One event of a streamed command. */
export type ExecEvent =
  | { type: "stdout" | "stderr"; data: string }
  | { type: "exit"; exitCode: number }
  | { type: "error"; error: string };

export interface FileInfo {
  path: string;
  name: string;
  size: number;
  mode: string;
  isDir: boolean;
  modTime: string;
}

export interface Snapshot {
  id: string;
  sandboxId: string;
  orgId: string;
  projectId: string;
  pool: string;
  kind: "disk" | "memory" | (string & {});
  status: string;
  sizeBytes?: number;
  createdAt: string;
  createdBy: string;
}

export interface PortUrl {
  port: number;
  url: string;
}

/** Quota limits. Zero or absent means unlimited at that level. */
export interface Quota {
  maxActiveSandboxes?: number;
  maxQueued?: number;
  maxCPU?: number;
  maxMemoryMiB?: number;
  maxDiskGiB?: number;
  maxTTLSeconds?: number;
  maxSnapshots?: number;
  poolBudgetsPerHour?: Record<string, number>;
}

export interface QuotaUsage {
  activeSandboxes: number;
  queued: number;
  cpu: number;
  memoryMiB: number;
  diskGiB: number;
  snapshots: number;
}

export interface QuotaReport {
  orgId: string;
  projectId?: string;
  orgQuota: Quota;
  orgUsage: QuotaUsage;
  projectQuota?: Quota;
  projectUsage?: QuotaUsage;
}

export interface Organization {
  id: string;
  name: string;
  suspended: boolean;
  quota: Quota;
  createdAt: string;
}

export interface Project {
  id: string;
  orgId: string;
  name: string;
  quota: Quota;
  createdAt: string;
}

export interface CreateProjectParams {
  name: string;
  /** Operator keys only. */
  orgId?: string;
  /** Requires org:admin or an operator key. */
  quota?: Quota;
}

export type ApiKeyScope =
  | "admin"
  | "org:admin"
  | "sandboxes:read"
  | "sandboxes:write"
  | "sandboxes:exec"
  | "projects:write"
  | "keys:write"
  | "usage:read"
  | "audit:read"
  | "policies:write"
  | (string & {});

export interface ApiKey {
  id: string;
  name: string;
  orgId?: string;
  projectId?: string;
  scopes: ApiKeyScope[];
  createdBy: string;
  createdAt: string;
  expiresAt?: string;
  lastUsedAt?: string;
  revoked: boolean;
  /** The key a rotation issued in place of this one. */
  replacedBy?: string;
}

/** A newly created key. `token` is shown only once. */
export interface CreatedApiKey extends ApiKey {
  token: string;
  /** On a rotation, the old key's new state. */
  replaced?: ApiKey;
}

export interface CreateApiKeyParams {
  name: string;
  scopes: ApiKeyScope[];
  orgId?: string;
  projectId?: string;
  ttlHours?: number;
}

export interface CostModel {
  currency?: string;
  cpuHour?: number;
  memoryGiBHour?: number;
  diskGiBHour?: number;
  sandboxHour?: number;
}

export interface PoolHealth {
  status: "healthy" | "degraded" | "unavailable" | "unknown" | (string & {});
  message?: string;
  checkedAt: string;
}

export interface PoolCapacity {
  bounded: boolean;
  totalCPU?: number;
  freeCPU?: number;
  totalMemoryMiB?: number;
  freeMemoryMiB?: number;
  nodes?: number;
}

export interface Pool {
  name: string;
  adapter: string;
  trust: string;
  region?: string;
  labels?: Record<string, string>;
  enabled: boolean;
  draining: boolean;
  maxConcurrency?: number;
  costModel: CostModel;
  config?: Record<string, string>;
  credentialRef?: string;
  source: "config" | "api" | (string & {});
  capabilities?: string[];
  health?: PoolHealth;
  capacity?: PoolCapacity;
  active: number;
}

/** Body of POST /v1/admin/pools. Credentials are references, never values. */
export interface CreatePoolParams {
  name: string;
  adapter: string;
  trust?: "private" | "external" | (string & {});
  region?: string;
  labels?: Record<string, string>;
  enabled?: boolean;
  maxConcurrency?: number;
  costModel?: CostModel;
  config?: Record<string, string>;
  /**
   * Where the credential lives: a server environment variable, a mounted file, or a
   * credential stored in Differently with `admin.secrets.put` (`secret`).
   */
  credentials?: { env?: string; file?: string; secret?: string };
}

export interface UpdatePoolParams {
  enabled?: boolean;
  maxConcurrency?: number;
  costModel?: CostModel;
  region?: string;
  /** API-registered pools only: `env:NAME`, `file:/path` or `secret:name`. */
  credentialRef?: string;
}

/** A provider credential stored in Differently. Its value is never returned. */
export interface Secret {
  name: string;
  fingerprint: string;
  createdAt: string;
  createdBy: string;
  updatedAt: string;
  updatedBy: string;
  /** Pools whose credential is this secret. */
  usedBy: string[];
}

/** The result of sending new sandboxes to a pool. */
export interface UsePoolResult {
  pool: Pool;
  /** Pools drained by the switch; their running sandboxes stay. */
  drained: string[];
  /** The global policy, when the switch approved an external pool. */
  policy?: PlacementPolicy;
}

/** A pool as tenants see it: no config or credentials. */
export interface PoolCapabilities {
  pool: string;
  adapter: string;
  trust: string;
  region?: string;
  enabled: boolean;
  health: string;
  capabilities: string[] | null;
}

/** Scope is "global", "org:<id>" or "project:<id>". */
export interface PlacementPolicy {
  scope: string;
  allowedPools?: string[];
  deniedPools?: string[];
  allowedRegions?: string[];
  external?: ExternalMode;
  externalPools?: string[];
  strategy?: PlacementStrategy;
  maxCostPerHour?: number;
  maxQueueSeconds?: number;
  updatedAt: string;
  updatedBy?: string;
}

export type PlacementPolicyParams = Omit<PlacementPolicy, "scope" | "updatedAt" | "updatedBy">;

export interface Classification {
  name: string;
  description?: string;
  /** The most permissive external mode this class allows. */
  external: ExternalMode;
}

export interface Profile {
  name: string;
  resources: Resources;
  template?: string;
  capabilities?: string[];
}

export interface Template {
  name: string;
  description?: string;
  pools?: string[];
  /** Each pool's own image for this template, by pool name. */
  images?: Record<string, string>;
}

export interface AuditEvent {
  seq: number;
  time: string;
  actor: string;
  orgId?: string;
  projectId?: string;
  action: string;
  resource: string;
  outcome: string;
  details?: Record<string, string>;
  requestId?: string;
  prevHash: string;
  hash: string;
}

export interface ListAuditEventsParams {
  resource?: string;
  action?: string;
  /** Return events with a sequence number greater than this. */
  afterSeq?: number;
  /** Page size, 1-1000 (server default 100). */
  limit?: number;
  /** Operator keys only. */
  orgId?: string;
}

export interface AuditVerifyResult {
  ok: boolean;
  events: number;
  brokenAt?: number;
  problem?: string;
  headHash?: string;
}

export interface UsageLine {
  key: string;
  sandboxes: number;
  cpuSeconds: number;
  memoryGiBSeconds: number;
  runSeconds: number;
  queueSeconds: number;
  estimatedCost: number;
}

export interface UsageReport {
  from: string;
  to: string;
  groupBy: string;
  lines: UsageLine[] | null;
  total: UsageLine;
  note: string;
}

export interface UsageParams {
  from?: Date | string;
  to?: Date | string;
  groupBy?: "pool" | "project" | "org" | "trust";
  /** Operator keys only. */
  orgId?: string;
}

export interface TrustPool {
  name: string;
  adapter: string;
  trust: string;
  enabled: boolean;
  draining: boolean;
  region?: string;
  /** The backend runtime's isolation as its adapter reports it. */
  isolation?: string;
  endpoint?: string;
  credentialRef?: string;
  approvedForExternalPlacement: boolean;
  health: string;
}

export interface TrustPlacement {
  sandboxId: string;
  orgId: string;
  pool: string;
  classification: string;
  createdBy: string;
  placedAt: string;
  reason: string;
}

export interface TrustReport {
  generatedAt: string;
  installation: string;
  version: string;
  telemetry: string;
  controlPlane: Record<string, string>;
  pools: TrustPool[] | null;
  globalPolicy: PlacementPolicy;
  scopedPolicies: PlacementPolicy[] | null;
  classifications: Classification[] | null;
  placementsByPool: UsageLine[] | null;
  recentExternalPlacements: TrustPlacement[] | null;
  recentAdminActions: AuditEvent[] | null;
  auditChain: AuditVerifyResult;
  limitations: string[];
}

export interface CreateOrganizationParams {
  name: string;
  quota?: Quota;
  /** Also create a project with this name. */
  defaultProject?: string;
}

export interface CreateOrganizationResult {
  organization: Organization;
  project?: Project;
}

export interface UpdateOrganizationParams {
  name?: string;
  quota?: Quota;
}

export interface SuspendOrganizationParams {
  /** Also terminate the organization's live sandboxes. */
  terminate?: boolean;
  reason?: string;
}

export interface SuspendOrganizationResult {
  organization: Organization;
  terminated: number;
}

/** Response of GET /v1/me. */
export interface Principal {
  keyId: string;
  orgId: string;
  projectId: string;
  scopes: ApiKeyScope[];
  operator: boolean;
}

export interface VersionInfo {
  version: string;
  commit: string;
  apiVersion: string;
  installation: string;
}

/** One page of a paginated list. Pass `nextCursor` back to get the next page. */
export interface Page<T> {
  items: T[];
  nextCursor?: string;
}
