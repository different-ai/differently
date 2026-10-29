// Package api defines the public, vendor-neutral Differently API resources.
//
// These types are the wire contract for /v1. They deliberately contain no
// Kubernetes, runtime or provider-specific fields: backends are reached
// through pools and capabilities, never named in application code.
package api

import (
	"slices"
	"time"
)

// SandboxStatus is the lifecycle state of a sandbox.
type SandboxStatus string

const (
	StatusQueued       SandboxStatus = "queued"
	StatusProvisioning SandboxStatus = "provisioning"
	StatusRunning      SandboxStatus = "running"
	StatusStopped      SandboxStatus = "stopped"
	StatusPaused       SandboxStatus = "paused"
	StatusTerminating  SandboxStatus = "terminating"
	StatusTerminated   SandboxStatus = "terminated"
	StatusFailed       SandboxStatus = "failed"
)

// Terminal reports whether no further transitions are possible.
func (s SandboxStatus) Terminal() bool {
	return s == StatusTerminated || s == StatusFailed
}

// ReservesCompute reports whether the sandbox counts against CPU and memory
// quota. Paused microVMs keep their memory resident, so they still reserve.
func (s SandboxStatus) ReservesCompute() bool {
	switch s {
	case StatusQueued, StatusProvisioning, StatusRunning, StatusPaused, StatusTerminating:
		return true
	}
	return false
}

// Workload trust boundaries for pools.
const (
	TrustPrivate  = "private"  // infrastructure the operator controls
	TrustExternal = "external" // a third-party provider runs the workload
)

// External placement modes, from most to least restrictive.
const (
	ExternalNever    = "never"    // never place outside private pools
	ExternalOverflow = "overflow" // only when no private pool has capacity
	ExternalAllowed  = "allowed"  // external pools rank alongside private ones
)

// Placement strategies.
const (
	StrategyPrivateFirst = "private-first"
	StrategyCost         = "cost"
	StrategySpread       = "spread"
)

// Resources is the compute envelope of a sandbox.
type Resources struct {
	CPU       int `json:"cpu"`
	MemoryMiB int `json:"memoryMiB"`
	DiskGiB   int `json:"diskGiB"`
}

// Placement is a request-level placement preference. A request can only
// narrow what operator, organization and project policies allow.
type Placement struct {
	Strategy     string   `json:"strategy,omitempty"`
	AllowedPools []string `json:"allowedPools,omitempty"`
	Regions      []string `json:"regions,omitempty"`
	// External can lower the effective external mode (e.g. "never"), never raise it.
	External string `json:"external,omitempty"`
}

// CreateSandboxRequest is the body of POST /v1/sandboxes.
type CreateSandboxRequest struct {
	ProjectID      string            `json:"projectId,omitempty"`
	Template       string            `json:"template,omitempty"`
	Profile        string            `json:"profile,omitempty"`
	Resources      *Resources        `json:"resources,omitempty"`
	Capabilities   []string          `json:"capabilities,omitempty"`
	Classification string            `json:"classification,omitempty"`
	Placement      *Placement        `json:"placement,omitempty"`
	TTLSeconds     int               `json:"ttlSeconds,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	Priority       int               `json:"priority,omitempty"`
	FromSnapshot   string            `json:"fromSnapshot,omitempty"`
	// IdleTimeoutSeconds stops (or pauses) the sandbox after this long with
	// no exec, file, port or keepalive call; the next such call wakes it.
	// 0 uses the operator default; -1 turns it off.
	IdleTimeoutSeconds int `json:"idleTimeoutSeconds,omitempty"`
}

// UpdateSandboxRequest is the body of PATCH /v1/sandboxes/{id}.
type UpdateSandboxRequest struct {
	Labels             map[string]string `json:"labels,omitempty"`
	TTLSeconds         *int              `json:"ttlSeconds,omitempty"`
	IdleTimeoutSeconds *int              `json:"idleTimeoutSeconds,omitempty"`
}

// Sandbox is an isolated compute environment.
type Sandbox struct {
	ID             string            `json:"id"`
	OrgID          string            `json:"orgId"`
	ProjectID      string            `json:"projectId"`
	Status         SandboxStatus     `json:"status"`
	StatusReason   string            `json:"statusReason,omitempty"`
	Template       string            `json:"template,omitempty"`
	Profile        string            `json:"profile,omitempty"`
	Resources      Resources         `json:"resources"`
	Capabilities   []string          `json:"capabilities,omitempty"`
	Classification string            `json:"classification"`
	Placement      *Placement        `json:"placement,omitempty"`
	Pool           string            `json:"pool,omitempty"`
	Adapter        string            `json:"adapter,omitempty"`
	Trust          string            `json:"trust,omitempty"`
	Region         string            `json:"region,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	EnvKeys        []string          `json:"envKeys,omitempty"`
	Priority       int               `json:"priority"`
	TTLSeconds     int               `json:"ttlSeconds,omitempty"`
	ExpiresAt      *time.Time        `json:"expiresAt,omitempty"`
	// IdleTimeoutSeconds is the idle auto-stop threshold (0: off).
	IdleTimeoutSeconds int `json:"idleTimeoutSeconds,omitempty"`
	// LastActivityAt is the last exec, file, port or keepalive call.
	LastActivityAt *time.Time `json:"lastActivityAt,omitempty"`
	// IdledAt is set while the sandbox is stopped or paused for being idle;
	// the next exec, file, port or keepalive call wakes it.
	IdledAt      *time.Time         `json:"idledAt,omitempty"`
	FromSnapshot string             `json:"fromSnapshot,omitempty"`
	Attempts     int                `json:"attempts"`
	CreatedBy    string             `json:"createdBy"`
	CreatedAt    time.Time          `json:"createdAt"`
	UpdatedAt    time.Time          `json:"updatedAt"`
	StartedAt    *time.Time         `json:"startedAt,omitempty"`
	EndedAt      *time.Time         `json:"endedAt,omitempty"`
	Decision     *PlacementDecision `json:"decision,omitempty"`
}

// PlacementDecision explains where a sandbox was placed and why.
type PlacementDecision struct {
	Pool        string                `json:"pool,omitempty"`
	Strategy    string                `json:"strategy"`
	External    string                `json:"external"`
	Outcome     string                `json:"outcome"` // placed | waiting | rejected
	Reason      string                `json:"reason"`
	Policies    []string              `json:"policies"`
	Candidates  []CandidateEvaluation `json:"candidates"`
	EvaluatedAt time.Time             `json:"evaluatedAt"`
}

// CandidateEvaluation is the verdict for one pool during placement.
type CandidateEvaluation struct {
	Pool          string   `json:"pool"`
	Trust         string   `json:"trust"`
	Eligible      bool     `json:"eligible"`
	HasCapacity   bool     `json:"hasCapacity"`
	EstimatedCost float64  `json:"estimatedCostPerHour"`
	Rank          int      `json:"rank,omitempty"`
	Reasons       []string `json:"reasons,omitempty"`
}

// ExecRequest runs a command in a sandbox. Command is run by /bin/sh -c;
// Args, when set, is executed directly without a shell.
type ExecRequest struct {
	Command        string            `json:"command,omitempty"`
	Args           []string          `json:"args,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	Cwd            string            `json:"cwd,omitempty"`
	Stdin          string            `json:"stdin,omitempty"`
	TimeoutSeconds int               `json:"timeoutSeconds,omitempty"`
}

// ExecResult is the outcome of a completed command.
type ExecResult struct {
	ExitCode   int    `json:"exitCode"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	DurationMs int64  `json:"durationMs"`
	Truncated  bool   `json:"truncated,omitempty"`
}

// ExecEvent is one server-sent event of a streamed command.
type ExecEvent struct {
	Stream   string `json:"stream,omitempty"` // stdout | stderr
	Data     string `json:"data,omitempty"`
	ExitCode *int   `json:"exitCode,omitempty"`
	Error    string `json:"error,omitempty"`
}

// FileInfo describes a file inside a sandbox.
type FileInfo struct {
	Path    string    `json:"path"`
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Mode    string    `json:"mode"`
	IsDir   bool      `json:"isDir"`
	ModTime time.Time `json:"modTime"`
}

// Snapshot captures sandbox state for restore or fork.
type Snapshot struct {
	ID        string    `json:"id"`
	SandboxID string    `json:"sandboxId"`
	OrgID     string    `json:"orgId"`
	ProjectID string    `json:"projectId"`
	Pool      string    `json:"pool"`
	Kind      string    `json:"kind"` // disk | memory
	Status    string    `json:"status"`
	SizeBytes int64     `json:"sizeBytes,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	CreatedBy string    `json:"createdBy"`
}

// CreateSnapshotRequest is the body of POST /v1/sandboxes/{id}/snapshots.
type CreateSnapshotRequest struct {
	Kind string `json:"kind,omitempty"` // disk (default) | memory
}

// PortURL is a reachable address for a sandbox service port.
type PortURL struct {
	Port int    `json:"port"`
	URL  string `json:"url"`
}

// Quota limits. Zero means unlimited at that level.
type Quota struct {
	MaxActiveSandboxes int                `json:"maxActiveSandboxes,omitempty"`
	MaxQueued          int                `json:"maxQueued,omitempty"`
	MaxCPU             int                `json:"maxCPU,omitempty"`
	MaxMemoryMiB       int                `json:"maxMemoryMiB,omitempty"`
	MaxDiskGiB         int                `json:"maxDiskGiB,omitempty"`
	MaxTTLSeconds      int                `json:"maxTTLSeconds,omitempty"`
	MaxSnapshots       int                `json:"maxSnapshots,omitempty"`
	PoolBudgetsPerHour map[string]float64 `json:"poolBudgetsPerHour,omitempty"`
}

// QuotaUsage is the current consumption counted against a quota.
type QuotaUsage struct {
	ActiveSandboxes int `json:"activeSandboxes"`
	Queued          int `json:"queued"`
	CPU             int `json:"cpu"`
	MemoryMiB       int `json:"memoryMiB"`
	DiskGiB         int `json:"diskGiB"`
	Snapshots       int `json:"snapshots"`
}

// QuotaReport pairs limits with usage for an organization and project.
type QuotaReport struct {
	OrgID        string      `json:"orgId"`
	ProjectID    string      `json:"projectId,omitempty"`
	OrgQuota     Quota       `json:"orgQuota"`
	OrgUsage     QuotaUsage  `json:"orgUsage"`
	ProjectQuota *Quota      `json:"projectQuota,omitempty"`
	ProjectUsage *QuotaUsage `json:"projectUsage,omitempty"`
}

// Organization is a tenant.
type Organization struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Suspended bool      `json:"suspended"`
	Quota     Quota     `json:"quota"`
	CreatedAt time.Time `json:"createdAt"`
}

// Project groups sandboxes and keys within an organization.
type Project struct {
	ID        string    `json:"id"`
	OrgID     string    `json:"orgId"`
	Name      string    `json:"name"`
	Quota     Quota     `json:"quota"`
	CreatedAt time.Time `json:"createdAt"`
}

// APIKey is a scoped credential. The secret is returned only once on creation.
type APIKey struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	OrgID      string     `json:"orgId,omitempty"`
	ProjectID  string     `json:"projectId,omitempty"`
	Scopes     []string   `json:"scopes"`
	CreatedBy  string     `json:"createdBy"`
	CreatedAt  time.Time  `json:"createdAt"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	Revoked    bool       `json:"revoked"`
	// ReplacedBy names the key a rotation issued in place of this one.
	ReplacedBy string `json:"replacedBy,omitempty"`
}

// CreateAPIKeyRequest is the body of POST /v1/api-keys.
type CreateAPIKeyRequest struct {
	Name      string   `json:"name"`
	OrgID     string   `json:"orgId,omitempty"`
	ProjectID string   `json:"projectId,omitempty"`
	Scopes    []string `json:"scopes"`
	TTLHours  int      `json:"ttlHours,omitempty"`
}

// CreateAPIKeyResponse includes the one-time token.
type CreateAPIKeyResponse struct {
	APIKey
	Token string `json:"token"`
	// Replaced is the rotated key's new state, on a rotation.
	Replaced *APIKey `json:"replaced,omitempty"`
}

// UsePoolRequest makes a pool the one new sandboxes go to.
type UsePoolRequest struct {
	// ApproveExternal adds an external pool to the global policy's approved
	// pools (raising its external mode to overflow if it was never).
	ApproveExternal bool `json:"approveExternal,omitempty"`
}

// UsePoolResponse reports what a switch changed.
type UsePoolResponse struct {
	Pool    Pool             `json:"pool"`
	Drained []string         `json:"drained"`
	Policy  *PlacementPolicy `json:"policy,omitempty"` // the global policy, when the switch approved the pool
}

// RotateAPIKeyRequest replaces a key. GraceSeconds keeps the old key valid
// that long (at most a week) so callers can switch without downtime; zero
// revokes it at once.
type RotateAPIKeyRequest struct {
	GraceSeconds int `json:"graceSeconds,omitempty"`
}

// CostModel prices pool consumption for estimates and chargeback.
type CostModel struct {
	Currency      string  `json:"currency,omitempty"`
	CPUHour       float64 `json:"cpuHour,omitempty"`
	MemoryGiBHour float64 `json:"memoryGiBHour,omitempty"`
	DiskGiBHour   float64 `json:"diskGiBHour,omitempty"`
	SandboxHour   float64 `json:"sandboxHour,omitempty"`
}

// HourlyCost estimates the hourly price of a resource envelope.
func (c CostModel) HourlyCost(r Resources) float64 {
	return c.SandboxHour + float64(r.CPU)*c.CPUHour +
		float64(r.MemoryMiB)/1024*c.MemoryGiBHour + float64(r.DiskGiB)*c.DiskGiBHour
}

// Pool is a registered execution backend instance.
type Pool struct {
	Name           string            `json:"name"`
	Adapter        string            `json:"adapter"`
	Trust          string            `json:"trust"`
	Region         string            `json:"region,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	Enabled        bool              `json:"enabled"`
	Draining       bool              `json:"draining"`
	MaxConcurrency int               `json:"maxConcurrency,omitempty"`
	CostModel      CostModel         `json:"costModel"`
	Config         map[string]string `json:"config,omitempty"`
	CredentialRef  string            `json:"credentialRef,omitempty"`
	Source         string            `json:"source"` // config | api
	Capabilities   []string          `json:"capabilities,omitempty"`
	Health         *PoolHealth       `json:"health,omitempty"`
	Capacity       *PoolCapacity     `json:"capacity,omitempty"`
	Active         int               `json:"active"`
}

// PoolHealth is the last observed health of a pool.
type PoolHealth struct {
	Status    string    `json:"status"` // healthy | degraded | unavailable | unknown
	Message   string    `json:"message,omitempty"`
	CheckedAt time.Time `json:"checkedAt"`
}

// PoolCapacity is the last observed free capacity of a pool.
type PoolCapacity struct {
	Bounded        bool `json:"bounded"`
	TotalCPU       int  `json:"totalCPU,omitempty"`
	FreeCPU        int  `json:"freeCPU,omitempty"`
	TotalMemoryMiB int  `json:"totalMemoryMiB,omitempty"`
	FreeMemoryMiB  int  `json:"freeMemoryMiB,omitempty"`
	Nodes          int  `json:"nodes,omitempty"`
}

// PlacementPolicy constrains where sandboxes in a scope may run.
// Scope is "global", "org:<id>" or "project:<id>". More specific scopes can
// only narrow security constraints; strategy prefers the most specific value.
type PlacementPolicy struct {
	Scope           string    `json:"scope"`
	AllowedPools    []string  `json:"allowedPools,omitempty"`
	DeniedPools     []string  `json:"deniedPools,omitempty"`
	AllowedRegions  []string  `json:"allowedRegions,omitempty"`
	External        string    `json:"external,omitempty"`
	ExternalPools   []string  `json:"externalPools,omitempty"`
	Strategy        string    `json:"strategy,omitempty"`
	MaxCostPerHour  float64   `json:"maxCostPerHour,omitempty"`
	MaxQueueSeconds int       `json:"maxQueueSeconds,omitempty"`
	UpdatedAt       time.Time `json:"updatedAt"`
	UpdatedBy       string    `json:"updatedBy,omitempty"`
}

// Classification is an operator-defined data sensitivity level.
type Classification struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// External is the most permissive external mode this class allows.
	External string `json:"external"`
}

// Profile is a named resource preset.
type Profile struct {
	Name         string    `json:"name"`
	Resources    Resources `json:"resources"`
	Template     string    `json:"template,omitempty"`
	Capabilities []string  `json:"capabilities,omitempty"`
}

// Template is a bootable image known to one or more pools.
type Template struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Pools restricts the template to these pools; empty means any pool.
	Pools []string `json:"pools,omitempty"`
	// Images maps a pool name to that backend's own image (an OpenSandbox
	// image or template, a Daytona or Freestyle snapshot), so one template
	// name can be placed on any pool listed here. When set, only those pools
	// can run the template.
	Images map[string]string `json:"images,omitempty"`
}

// Offers reports whether pool can run the template, with the reason if not.
func (t Template) Offers(pool string) (bool, string) {
	if len(t.Pools) > 0 && !slices.Contains(t.Pools, pool) {
		return false, "template " + t.Name + " is not offered on this pool"
	}
	if len(t.Images) > 0 && t.Images[pool] == "" {
		return false, "template " + t.Name + " has no image for this pool"
	}
	return true, ""
}

// ImageFor returns what the backend of pool should boot for this template:
// the pool's own image when one is mapped, otherwise the template name.
func (t Template) ImageFor(pool string) string {
	if img := t.Images[pool]; img != "" {
		return img
	}
	return t.Name
}

// Secret is a provider credential stored in Differently. Its value is
// write-only: the API returns only a fingerprint and which pools use it.
type Secret struct {
	Name        string    `json:"name"`
	Fingerprint string    `json:"fingerprint"`
	CreatedAt   time.Time `json:"createdAt"`
	CreatedBy   string    `json:"createdBy"`
	UpdatedAt   time.Time `json:"updatedAt"`
	UpdatedBy   string    `json:"updatedBy"`
	UsedBy      []string  `json:"usedBy"`
}

// PutSecretRequest stores or replaces a secret's value.
type PutSecretRequest struct {
	Value string `json:"value"`
}

// AuditEvent is one entry of the tamper-evident audit chain.
type AuditEvent struct {
	Seq       int64             `json:"seq"`
	Time      time.Time         `json:"time"`
	Actor     string            `json:"actor"`
	OrgID     string            `json:"orgId,omitempty"`
	ProjectID string            `json:"projectId,omitempty"`
	Action    string            `json:"action"`
	Resource  string            `json:"resource"`
	Outcome   string            `json:"outcome"`
	Details   map[string]string `json:"details,omitempty"`
	RequestID string            `json:"requestId,omitempty"`
	PrevHash  string            `json:"prevHash"`
	Hash      string            `json:"hash"`
}

// UsageLine is one aggregated usage row.
type UsageLine struct {
	Key              string  `json:"key"`
	Sandboxes        int     `json:"sandboxes"`
	CPUSeconds       float64 `json:"cpuSeconds"`
	MemoryGiBSeconds float64 `json:"memoryGiBSeconds"`
	RunSeconds       float64 `json:"runSeconds"`
	QueueSeconds     float64 `json:"queueSeconds"`
	EstimatedCost    float64 `json:"estimatedCost"`
}

// UsageReport aggregates usage over a time window.
type UsageReport struct {
	From    time.Time   `json:"from"`
	To      time.Time   `json:"to"`
	GroupBy string      `json:"groupBy"`
	Lines   []UsageLine `json:"lines"`
	Total   UsageLine   `json:"total"`
	Note    string      `json:"note"`
}

// List is a paginated collection.
type List[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// VersionInfo is returned by GET /version.
type VersionInfo struct {
	Version      string `json:"version"`
	Commit       string `json:"commit"`
	APIVersion   string `json:"apiVersion"`
	Installation string `json:"installation"`
}
