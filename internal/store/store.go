// Package store persists control-plane state.
//
// Two implementations share this contract: memory (development and tests) and
// postgres (production, HA). Quota-sensitive transitions — admitting a request
// and activating a queued sandbox onto a pool — are single atomic store
// operations so concurrent requests cannot double-allocate or bypass quota.
// Quota usage is always derived from sandbox state rather than kept in
// separate counters, so a crash cannot leave usage out of sync.
package store

import (
	"context"
	"slices"
	"sort"
	"time"

	"github.com/different-ai/differently/pkg/api"
)

// SandboxRecord is a sandbox plus fields that never leave the control plane.
type SandboxRecord struct {
	api.Sandbox
	BackendRef         string            `json:"backendRef,omitempty"`
	Env                map[string]string `json:"env,omitempty"`
	IdempotencyKey     string            `json:"idempotencyKey,omitempty"`
	NextAttemptAt      time.Time         `json:"nextAttemptAt"`
	RunningSince       *time.Time        `json:"runningSince,omitempty"`
	RunSeconds         float64           `json:"runSeconds"`
	QueueSeconds       float64           `json:"queueSeconds"`
	ExcludedPools      []string          `json:"excludedPools,omitempty"`
	SnapshotBackendRef string            `json:"snapshotBackendRef,omitempty"`
	ProvisionStartedAt *time.Time        `json:"provisionStartedAt,omitempty"`
}

// KeyRecord is an API key plus its secret hash.
type KeyRecord struct {
	api.APIKey
	Hash string `json:"hash"`
}

// SecretRecord is a stored provider credential, sealed by internal/secrets.
type SecretRecord struct {
	api.Secret
	Sealed []byte `json:"sealed"`
}

// SnapshotRecord is a snapshot plus its backend reference.
type SnapshotRecord struct {
	api.Snapshot
	BackendRef string `json:"backendRef"`
}

// SandboxFilter selects sandboxes for listing.
type SandboxFilter struct {
	OrgID     string
	ProjectID string
	Pool      string
	Statuses  []api.SandboxStatus
	Label     map[string]string
	Cursor    string // return IDs strictly older than this
	Limit     int
}

// Matches reports whether rec passes the filter (ignoring cursor and limit).
func (f SandboxFilter) Matches(rec SandboxRecord) bool {
	if f.OrgID != "" && rec.OrgID != f.OrgID {
		return false
	}
	if f.ProjectID != "" && rec.ProjectID != f.ProjectID {
		return false
	}
	if f.Pool != "" && rec.Pool != f.Pool {
		return false
	}
	if len(f.Statuses) > 0 && !slices.Contains(f.Statuses, rec.Status) {
		return false
	}
	for k, v := range f.Label {
		if rec.Labels[k] != v {
			return false
		}
	}
	return true
}

// AuditFilter selects audit events.
type AuditFilter struct {
	OrgID    string
	Resource string
	Action   string
	AfterSeq int64
	Limit    int
}

// Admission is the input to AdmitSandbox.
type Admission struct {
	OrgQuota     api.Quota
	ProjectQuota api.Quota
}

// Activation is the input to ActivateSandbox: placing a queued sandbox on a
// pool after re-checking tenant quota and pool concurrency atomically.
type Activation struct {
	Pool           string
	Adapter        string
	Trust          string
	Region         string
	MaxConcurrency int
	OrgQuota       api.Quota
	ProjectQuota   api.Quota
	Decision       *api.PlacementDecision
	Now            time.Time
}

// Store is the persistence contract.
type Store interface {
	// Installation returns the stable installation ID, creating it once.
	Installation(ctx context.Context) (string, error)

	CreateOrg(ctx context.Context, org api.Organization) error
	GetOrg(ctx context.Context, id string) (api.Organization, error)
	ListOrgs(ctx context.Context) ([]api.Organization, error)
	UpdateOrg(ctx context.Context, id string, fn func(*api.Organization) error) (api.Organization, error)

	CreateProject(ctx context.Context, p api.Project) error
	GetProject(ctx context.Context, id string) (api.Project, error)
	ListProjects(ctx context.Context, orgID string) ([]api.Project, error)
	UpdateProject(ctx context.Context, id string, fn func(*api.Project) error) (api.Project, error)
	// DeleteProject fails with conflict while non-terminal sandboxes exist.
	DeleteProject(ctx context.Context, id string) error

	CreateKey(ctx context.Context, k KeyRecord) error
	GetKey(ctx context.Context, id string) (KeyRecord, error)
	ListKeys(ctx context.Context, orgID, projectID string) ([]KeyRecord, error)
	UpdateKey(ctx context.Context, id string, fn func(*KeyRecord) error) (KeyRecord, error)

	// PutSecret creates or replaces a sealed secret.
	PutSecret(ctx context.Context, s SecretRecord) error
	GetSecret(ctx context.Context, name string) (SecretRecord, error)
	ListSecrets(ctx context.Context) ([]SecretRecord, error)
	// DeleteSecret refuses while a pool's credential refers to the secret.
	DeleteSecret(ctx context.Context, name string) error

	UpsertPool(ctx context.Context, p api.Pool) error
	GetPool(ctx context.Context, name string) (api.Pool, error)
	ListPools(ctx context.Context) ([]api.Pool, error)
	DeletePool(ctx context.Context, name string) error

	PutPolicy(ctx context.Context, p api.PlacementPolicy) error
	GetPolicy(ctx context.Context, scope string) (api.PlacementPolicy, bool, error)
	ListPolicies(ctx context.Context) ([]api.PlacementPolicy, error)
	DeletePolicy(ctx context.Context, scope string) error

	// AdmitSandbox atomically checks admission limits and inserts rec as
	// queued. A repeated IdempotencyKey within a project returns the existing
	// record and existed=true.
	AdmitSandbox(ctx context.Context, rec SandboxRecord, adm Admission) (out SandboxRecord, existed bool, err error)
	GetSandbox(ctx context.Context, id string) (SandboxRecord, error)
	ListSandboxes(ctx context.Context, f SandboxFilter) ([]SandboxRecord, error)
	UpdateSandbox(ctx context.Context, id string, fn func(*SandboxRecord) error) (SandboxRecord, error)
	// QueuedCandidates returns runnable queued sandboxes in fair order.
	QueuedCandidates(ctx context.Context, now time.Time, limit int) ([]SandboxRecord, error)
	// ActivateSandbox moves a queued sandbox to provisioning on a pool.
	ActivateSandbox(ctx context.Context, id string, act Activation) (SandboxRecord, error)
	// ReserveCompute atomically moves a sandbox from one status to another
	// when the move re-reserves CPU and memory (for example stopped to
	// running), re-checking tenant compute quota under the same lock as
	// ActivateSandbox.
	ReserveCompute(ctx context.Context, id string, from, to api.SandboxStatus, orgQuota, projectQuota api.Quota, now time.Time) (SandboxRecord, error)
	// Usage tallies quota usage for an organization or project.
	Usage(ctx context.Context, orgID, projectID string) (api.QuotaUsage, error)
	// PoolActive counts sandboxes holding a slot on each pool.
	PoolActive(ctx context.Context) (map[string]int, error)

	CreateSnapshot(ctx context.Context, s SnapshotRecord) error
	GetSnapshot(ctx context.Context, id string) (SnapshotRecord, error)
	ListSnapshots(ctx context.Context, orgID, projectID string) ([]SnapshotRecord, error)
	DeleteSnapshot(ctx context.Context, id string) error

	// AppendAudit seals ev into the chain and stores it.
	AppendAudit(ctx context.Context, ev api.AuditEvent) (api.AuditEvent, error)
	ListAudit(ctx context.Context, f AuditFilter) ([]api.AuditEvent, error)

	// TryLeader reports whether this process holds scheduler leadership.
	TryLeader(ctx context.Context) (bool, error)
	Close() error
}

// HoldsPoolSlot reports whether a sandbox occupies pool concurrency.
func HoldsPoolSlot(s api.SandboxStatus) bool {
	switch s {
	case api.StatusProvisioning, api.StatusRunning, api.StatusPaused, api.StatusStopped, api.StatusTerminating:
		return true
	}
	return false
}

// Tally computes quota usage from records.
func Tally(recs []SandboxRecord, snapshots int) api.QuotaUsage {
	var u api.QuotaUsage
	for _, r := range recs {
		switch {
		case r.Status.Terminal():
			continue
		case r.Status == api.StatusQueued:
			u.Queued++
			continue
		}
		u.ActiveSandboxes++
		u.DiskGiB += r.Resources.DiskGiB
		if r.Status.ReservesCompute() {
			u.CPU += r.Resources.CPU
			u.MemoryMiB += r.Resources.MemoryMiB
		}
	}
	u.Snapshots = snapshots
	return u
}

// CheckActivation returns a quota_exceeded error if activating r would push
// usage over q.
func CheckActivation(scope string, u api.QuotaUsage, q api.Quota, r api.Resources) error {
	over := func(limit, used, add int) bool { return limit > 0 && used+add > limit }
	switch {
	case over(q.MaxActiveSandboxes, u.ActiveSandboxes, 1):
		return api.Errorf(api.CodeQuotaExceeded, "%s active sandbox limit %d reached", scope, q.MaxActiveSandboxes).With("dimension", "activeSandboxes")
	case over(q.MaxCPU, u.CPU, r.CPU):
		return api.Errorf(api.CodeQuotaExceeded, "%s CPU limit %d reached", scope, q.MaxCPU).With("dimension", "cpu")
	case over(q.MaxMemoryMiB, u.MemoryMiB, r.MemoryMiB):
		return api.Errorf(api.CodeQuotaExceeded, "%s memory limit %d MiB reached", scope, q.MaxMemoryMiB).With("dimension", "memoryMiB")
	case over(q.MaxDiskGiB, u.DiskGiB, r.DiskGiB):
		return api.Errorf(api.CodeQuotaExceeded, "%s disk limit %d GiB reached", scope, q.MaxDiskGiB).With("dimension", "diskGiB")
	}
	return nil
}

// CheckCompute returns a quota_exceeded error if re-reserving r's CPU and
// memory would push usage over q. The sandbox already counts as active.
func CheckCompute(scope string, u api.QuotaUsage, q api.Quota, r api.Resources) error {
	switch {
	case q.MaxCPU > 0 && u.CPU+r.CPU > q.MaxCPU:
		return api.Errorf(api.CodeQuotaExceeded, "%s CPU limit %d reached", scope, q.MaxCPU).With("dimension", "cpu")
	case q.MaxMemoryMiB > 0 && u.MemoryMiB+r.MemoryMiB > q.MaxMemoryMiB:
		return api.Errorf(api.CodeQuotaExceeded, "%s memory limit %d MiB reached", scope, q.MaxMemoryMiB).With("dimension", "memoryMiB")
	}
	return nil
}

// CheckAdmission rejects requests that can never fit or would overflow the
// queue. Concurrency limits are enforced at activation, so a tenant at its
// limit queues instead of failing.
func CheckAdmission(scope string, u api.QuotaUsage, q api.Quota, r api.Resources, ttl int) error {
	switch {
	case q.MaxCPU > 0 && r.CPU > q.MaxCPU:
		return api.Errorf(api.CodeQuotaExceeded, "request CPU %d exceeds %s limit %d", r.CPU, scope, q.MaxCPU).With("dimension", "cpu")
	case q.MaxMemoryMiB > 0 && r.MemoryMiB > q.MaxMemoryMiB:
		return api.Errorf(api.CodeQuotaExceeded, "request memory exceeds %s limit %d MiB", scope, q.MaxMemoryMiB).With("dimension", "memoryMiB")
	case q.MaxDiskGiB > 0 && r.DiskGiB > q.MaxDiskGiB:
		return api.Errorf(api.CodeQuotaExceeded, "request disk exceeds %s limit %d GiB", scope, q.MaxDiskGiB).With("dimension", "diskGiB")
	case q.MaxTTLSeconds > 0 && ttl > q.MaxTTLSeconds:
		return api.Errorf(api.CodeQuotaExceeded, "ttl %ds exceeds %s limit %ds", ttl, scope, q.MaxTTLSeconds).With("dimension", "ttlSeconds")
	case q.MaxQueued > 0 && u.Queued+1 > q.MaxQueued:
		return api.Errorf(api.CodeQuotaExceeded, "%s queue limit %d reached", scope, q.MaxQueued).With("dimension", "queued")
	}
	return nil
}

// FairOrder orders queued sandboxes for dispatch. Organizations with less
// active CPU go first and organizations are interleaved round-robin, so one
// tenant's burst cannot starve others; within an organization higher priority
// then older requests go first.
func FairOrder(queued []SandboxRecord, activeCPUByOrg map[string]int) []SandboxRecord {
	byOrg := map[string][]SandboxRecord{}
	var orgs []string
	for _, r := range queued {
		if _, ok := byOrg[r.OrgID]; !ok {
			orgs = append(orgs, r.OrgID)
		}
		byOrg[r.OrgID] = append(byOrg[r.OrgID], r)
	}
	for _, list := range byOrg {
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].Priority != list[j].Priority {
				return list[i].Priority > list[j].Priority
			}
			return list[i].ID < list[j].ID
		})
	}
	sort.SliceStable(orgs, func(i, j int) bool {
		a, b := activeCPUByOrg[orgs[i]], activeCPUByOrg[orgs[j]]
		if a != b {
			return a < b
		}
		return orgs[i] < orgs[j]
	})
	out := make([]SandboxRecord, 0, len(queued))
	for i := 0; len(out) < len(queued); i++ {
		for _, o := range orgs {
			if i < len(byOrg[o]) {
				out = append(out, byOrg[o][i])
			}
		}
	}
	return out
}

// NotFound builds a not_found error for a resource kind.
func NotFound(kind, id string) error {
	return api.Errorf(api.CodeNotFound, "%s %s not found", kind, id)
}
