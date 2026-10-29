package store

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/different-ai/differently/internal/audit"
	"github.com/different-ai/differently/internal/id"
	"github.com/different-ai/differently/pkg/api"
)

// Memory is an in-process Store for development and tests. State is lost on
// restart; use postgres for anything durable.
type Memory struct {
	mu           sync.Mutex
	installation string
	orgs         map[string]api.Organization
	projects     map[string]api.Project
	keys         map[string]KeyRecord
	pools        map[string]api.Pool
	secrets      map[string]SecretRecord
	policies     map[string]api.PlacementPolicy
	sandboxes    map[string]SandboxRecord
	snapshots    map[string]SnapshotRecord
	audit        []api.AuditEvent
}

// NewMemory returns an empty memory store.
func NewMemory() *Memory {
	return &Memory{
		orgs:      map[string]api.Organization{},
		projects:  map[string]api.Project{},
		keys:      map[string]KeyRecord{},
		pools:     map[string]api.Pool{},
		secrets:   map[string]SecretRecord{},
		policies:  map[string]api.PlacementPolicy{},
		sandboxes: map[string]SandboxRecord{},
		snapshots: map[string]SnapshotRecord{},
	}
}

// clone deep-copies v so callers never alias stored state.
func clone[T any](v T) T {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var out T
	if err := json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	return out
}

func (m *Memory) Installation(context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.installation == "" {
		m.installation = id.New("inst")
	}
	return m.installation, nil
}

func (m *Memory) CreateOrg(_ context.Context, o api.Organization) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.orgs[o.ID]; ok {
		return api.Errorf(api.CodeConflict, "organization %s exists", o.ID)
	}
	m.orgs[o.ID] = clone(o)
	return nil
}

func (m *Memory) GetOrg(_ context.Context, id string) (api.Organization, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.orgs[id]
	if !ok {
		return o, NotFound("organization", id)
	}
	return clone(o), nil
}

func (m *Memory) ListOrgs(context.Context) ([]api.Organization, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]api.Organization, 0, len(m.orgs))
	for _, o := range m.orgs {
		out = append(out, clone(o))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *Memory) UpdateOrg(_ context.Context, id string, fn func(*api.Organization) error) (api.Organization, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.orgs[id]
	if !ok {
		return o, NotFound("organization", id)
	}
	o = clone(o)
	if err := fn(&o); err != nil {
		return o, err
	}
	m.orgs[id] = clone(o)
	return o, nil
}

func (m *Memory) CreateProject(_ context.Context, p api.Project) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[p.ID]; ok {
		return api.Errorf(api.CodeConflict, "project %s exists", p.ID)
	}
	m.projects[p.ID] = clone(p)
	return nil
}

func (m *Memory) GetProject(_ context.Context, id string) (api.Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.projects[id]
	if !ok {
		return p, NotFound("project", id)
	}
	return clone(p), nil
}

func (m *Memory) ListProjects(_ context.Context, orgID string) ([]api.Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []api.Project
	for _, p := range m.projects {
		if orgID == "" || p.OrgID == orgID {
			out = append(out, clone(p))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *Memory) UpdateProject(_ context.Context, id string, fn func(*api.Project) error) (api.Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.projects[id]
	if !ok {
		return p, NotFound("project", id)
	}
	p = clone(p)
	if err := fn(&p); err != nil {
		return p, err
	}
	m.projects[id] = clone(p)
	return p, nil
}

func (m *Memory) DeleteProject(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[id]; !ok {
		return NotFound("project", id)
	}
	for _, s := range m.sandboxes {
		if s.ProjectID == id && !s.Status.Terminal() {
			return api.Errorf(api.CodeConflict, "project %s still has sandbox %s", id, s.ID)
		}
	}
	delete(m.projects, id)
	return nil
}

func (m *Memory) CreateKey(_ context.Context, k KeyRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.keys[k.ID]; ok {
		return api.Errorf(api.CodeConflict, "key %s exists", k.ID)
	}
	m.keys[k.ID] = clone(k)
	return nil
}

func (m *Memory) GetKey(_ context.Context, id string) (KeyRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.keys[id]
	if !ok {
		return k, NotFound("api key", id)
	}
	return clone(k), nil
}

func (m *Memory) ListKeys(_ context.Context, orgID, projectID string) ([]KeyRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []KeyRecord
	for _, k := range m.keys {
		if (orgID == "" || k.OrgID == orgID) && (projectID == "" || k.ProjectID == projectID) {
			out = append(out, clone(k))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *Memory) UpdateKey(_ context.Context, id string, fn func(*KeyRecord) error) (KeyRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.keys[id]
	if !ok {
		return k, NotFound("api key", id)
	}
	k = clone(k)
	if err := fn(&k); err != nil {
		return k, err
	}
	m.keys[id] = clone(k)
	return k, nil
}

func (m *Memory) PutSecret(_ context.Context, s SecretRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.secrets[s.Name] = clone(s)
	return nil
}

func (m *Memory) GetSecret(_ context.Context, name string) (SecretRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.secrets[name]
	if !ok {
		return s, NotFound("secret", name)
	}
	return clone(s), nil
}

func (m *Memory) ListSecrets(context.Context) ([]SecretRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SecretRecord, 0, len(m.secrets))
	for _, s := range m.secrets {
		out = append(out, clone(s))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *Memory) DeleteSecret(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.pools {
		if p.CredentialRef == "secret:"+name {
			return api.Errorf(api.CodeConflict, "secret %s is used by pool %s", name, p.Name)
		}
	}
	if _, ok := m.secrets[name]; !ok {
		return NotFound("secret", name)
	}
	delete(m.secrets, name)
	return nil
}

func (m *Memory) UpsertPool(_ context.Context, p api.Pool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p.Health, p.Capacity, p.Active = nil, nil, 0
	m.pools[p.Name] = clone(p)
	return nil
}

func (m *Memory) GetPool(_ context.Context, name string) (api.Pool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.pools[name]
	if !ok {
		return p, NotFound("pool", name)
	}
	return clone(p), nil
}

func (m *Memory) ListPools(context.Context) ([]api.Pool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]api.Pool, 0, len(m.pools))
	for _, p := range m.pools {
		out = append(out, clone(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *Memory) DeletePool(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sandboxes {
		if s.Pool == name && !s.Status.Terminal() {
			return api.Errorf(api.CodeConflict, "pool %s still hosts sandbox %s; drain it first", name, s.ID)
		}
	}
	delete(m.pools, name)
	return nil
}

func (m *Memory) PutPolicy(_ context.Context, p api.PlacementPolicy) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.policies[p.Scope] = clone(p)
	return nil
}

func (m *Memory) GetPolicy(_ context.Context, scope string) (api.PlacementPolicy, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.policies[scope]
	return clone(p), ok, nil
}

func (m *Memory) ListPolicies(context.Context) ([]api.PlacementPolicy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]api.PlacementPolicy, 0, len(m.policies))
	for _, p := range m.policies {
		out = append(out, clone(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Scope < out[j].Scope })
	return out, nil
}

func (m *Memory) DeletePolicy(_ context.Context, scope string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.policies, scope)
	return nil
}

func (m *Memory) tally(orgID, projectID string) api.QuotaUsage {
	var recs []SandboxRecord
	for _, s := range m.sandboxes {
		if s.OrgID == orgID && (projectID == "" || s.ProjectID == projectID) {
			recs = append(recs, s)
		}
	}
	snaps := 0
	for _, s := range m.snapshots {
		if s.OrgID == orgID && (projectID == "" || s.ProjectID == projectID) {
			snaps++
		}
	}
	return Tally(recs, snaps)
}

func (m *Memory) AdmitSandbox(_ context.Context, rec SandboxRecord, adm Admission) (SandboxRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rec.IdempotencyKey != "" {
		for _, s := range m.sandboxes {
			if s.ProjectID == rec.ProjectID && s.IdempotencyKey == rec.IdempotencyKey && !s.Status.Terminal() {
				return clone(s), true, nil
			}
		}
	}
	if err := CheckAdmission("organization", m.tally(rec.OrgID, ""), adm.OrgQuota, rec.Resources, rec.TTLSeconds); err != nil {
		return rec, false, err
	}
	if err := CheckAdmission("project", m.tally(rec.OrgID, rec.ProjectID), adm.ProjectQuota, rec.Resources, rec.TTLSeconds); err != nil {
		return rec, false, err
	}
	rec.Status = api.StatusQueued
	m.sandboxes[rec.ID] = clone(rec)
	return clone(rec), false, nil
}

func (m *Memory) GetSandbox(_ context.Context, id string) (SandboxRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sandboxes[id]
	if !ok {
		return s, NotFound("sandbox", id)
	}
	return clone(s), nil
}

func (m *Memory) ListSandboxes(_ context.Context, f SandboxFilter) ([]SandboxRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []SandboxRecord
	for _, s := range m.sandboxes {
		if f.Matches(s) && (f.Cursor == "" || s.ID < f.Cursor) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return clone(out), nil
}

func (m *Memory) UpdateSandbox(_ context.Context, id string, fn func(*SandboxRecord) error) (SandboxRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sandboxes[id]
	if !ok {
		return s, NotFound("sandbox", id)
	}
	s = clone(s)
	if err := fn(&s); err != nil {
		return s, err
	}
	m.sandboxes[id] = clone(s)
	return s, nil
}

func (m *Memory) QueuedCandidates(_ context.Context, now time.Time, limit int) ([]SandboxRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var queued []SandboxRecord
	activeCPU := map[string]int{}
	for _, s := range m.sandboxes {
		switch {
		case s.Status == api.StatusQueued && !s.NextAttemptAt.After(now):
			queued = append(queued, s)
		case s.Status.ReservesCompute() && s.Status != api.StatusQueued:
			activeCPU[s.OrgID] += s.Resources.CPU
		}
	}
	out := FairOrder(queued, activeCPU)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return clone(out), nil
}

func (m *Memory) ActivateSandbox(_ context.Context, sid string, act Activation) (SandboxRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sandboxes[sid]
	if !ok {
		return s, NotFound("sandbox", sid)
	}
	if s.Status != api.StatusQueued {
		return s, api.Errorf(api.CodeInvalidState, "sandbox %s is %s, not queued", sid, s.Status)
	}
	if err := CheckActivation("organization", m.tally(s.OrgID, ""), act.OrgQuota, s.Resources); err != nil {
		return s, err
	}
	if err := CheckActivation("project", m.tally(s.OrgID, s.ProjectID), act.ProjectQuota, s.Resources); err != nil {
		return s, err
	}
	if act.MaxConcurrency > 0 {
		n := 0
		for _, o := range m.sandboxes {
			if o.Pool == act.Pool && HoldsPoolSlot(o) {
				n++
			}
		}
		if n >= act.MaxConcurrency {
			return s, InsufficientCapacity(act.Pool)
		}
	}
	s = clone(s)
	applyActivation(&s, act)
	m.sandboxes[sid] = clone(s)
	return s, nil
}

func applyActivation(s *SandboxRecord, act Activation) {
	now := act.Now
	s.Status = api.StatusProvisioning
	s.StatusReason = ""
	s.Pool, s.Adapter, s.Trust, s.Region = act.Pool, act.Adapter, act.Trust, act.Region
	s.Decision = act.Decision
	s.Attempts++
	s.QueueSeconds = now.Sub(s.CreatedAt).Seconds() - s.RunSeconds
	s.ProvisionStartedAt = &now
	s.UpdatedAt = now
}

func (m *Memory) ReserveCompute(_ context.Context, sid string, from, to api.SandboxStatus, oq, pq api.Quota, now time.Time) (SandboxRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sandboxes[sid]
	if !ok {
		return s, NotFound("sandbox", sid)
	}
	if s.Status != from {
		return s, api.Errorf(api.CodeInvalidState, "sandbox %s is %s, not %s", sid, s.Status, from)
	}
	if err := CheckCompute("organization", m.tally(s.OrgID, ""), oq, s.Resources); err != nil {
		return s, err
	}
	if err := CheckCompute("project", m.tally(s.OrgID, s.ProjectID), pq, s.Resources); err != nil {
		return s, err
	}
	s = clone(s)
	s.Status, s.UpdatedAt = to, now
	m.sandboxes[sid] = clone(s)
	return s, nil
}

func (m *Memory) ReclaimSlot(_ context.Context, sid string, maxConcurrency int, now time.Time) (SandboxRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sandboxes[sid]
	if !ok {
		return s, NotFound("sandbox", sid)
	}
	if !s.SlotReleased {
		return clone(s), nil
	}
	if maxConcurrency > 0 {
		n := 0
		for _, o := range m.sandboxes {
			if o.Pool == s.Pool && HoldsPoolSlot(o) {
				n++
			}
		}
		if n >= maxConcurrency {
			return clone(s), InsufficientCapacity(s.Pool)
		}
	}
	s = clone(s)
	s.SlotReleased, s.UpdatedAt = false, now
	m.sandboxes[sid] = clone(s)
	return s, nil
}

func (m *Memory) Usage(_ context.Context, orgID, projectID string) (api.QuotaUsage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tally(orgID, projectID), nil
}

func (m *Memory) PoolActive(context.Context) (map[string]int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int{}
	for _, s := range m.sandboxes {
		if s.Pool != "" && HoldsPoolSlot(s) {
			out[s.Pool]++
		}
	}
	return out, nil
}

func (m *Memory) CreateSnapshot(_ context.Context, s SnapshotRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snapshots[s.ID] = clone(s)
	return nil
}

func (m *Memory) GetSnapshot(_ context.Context, id string) (SnapshotRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.snapshots[id]
	if !ok {
		return s, NotFound("snapshot", id)
	}
	return clone(s), nil
}

func (m *Memory) ListSnapshots(_ context.Context, orgID, projectID string) ([]SnapshotRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []SnapshotRecord
	for _, s := range m.snapshots {
		if (orgID == "" || s.OrgID == orgID) && (projectID == "" || s.ProjectID == projectID) {
			out = append(out, clone(s))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, nil
}

func (m *Memory) DeleteSnapshot(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.snapshots, id)
	return nil
}

func (m *Memory) AppendAudit(_ context.Context, ev api.AuditEvent) (api.AuditEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prev := ""
	if n := len(m.audit); n > 0 {
		prev = m.audit[n-1].Hash
	}
	ev.Seq = int64(len(m.audit) + 1)
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	ev = audit.Seal(prev, clone(ev))
	m.audit = append(m.audit, ev)
	return ev, nil
}

func (m *Memory) ListAudit(_ context.Context, f AuditFilter) ([]api.AuditEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []api.AuditEvent
	for _, ev := range m.audit {
		if ev.Seq <= f.AfterSeq ||
			(f.OrgID != "" && ev.OrgID != f.OrgID) ||
			(f.Resource != "" && ev.Resource != f.Resource) ||
			(f.Action != "" && ev.Action != f.Action) {
			continue
		}
		out = append(out, clone(ev))
		if f.Limit > 0 && len(out) >= f.Limit {
			break
		}
	}
	return out, nil
}

func (m *Memory) TryLeader(context.Context) (bool, error) { return true, nil }

func (m *Memory) Close() error { return nil }

var _ Store = (*Memory)(nil)
