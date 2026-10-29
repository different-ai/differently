// Package scheduler moves sandboxes through their lifecycle.
//
// One leader runs the loops (the postgres store elects it with an advisory
// lock); API replicas only write intent to the store. Each tick:
//
//  1. dispatch: queued sandboxes, in fair order, are placed by the policy
//     engine and atomically activated on a pool (re-checking tenant quota and
//     pool concurrency);
//  2. provision: activated sandboxes are created through their pool adapter,
//     using the sandbox ID as idempotency key so retries never double-create;
//  3. terminate: sandboxes marked terminating are deleted on the backend;
//  4. expire: sandboxes past their TTL are marked terminating.
//
// The reconciler separately compares each pool's instances with the store,
// deleting orphans, failing sandboxes whose instance vanished, and recovering
// provisioning that was interrupted by a crash.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/different-ai/differently/internal/auditlog"
	"github.com/different-ai/differently/internal/config"
	"github.com/different-ai/differently/internal/metrics"
	"github.com/different-ai/differently/internal/policy"
	"github.com/different-ai/differently/internal/pools"
	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// RetryDelay is the wait before a sandbox is reconsidered for placement.
const RetryDelay = 2 * time.Second

// Scheduler runs dispatch, provisioning, termination and reconciliation.
type Scheduler struct {
	sweeping atomic.Bool // an idle sweep is running

	Store        store.Store
	Pools        *pools.Manager
	Config       config.Config
	Installation string
	Audit        *auditlog.Recorder
	Log          *slog.Logger
	Now          func() time.Time
	// ReconcileGrace is how long the reconciler leaves a sandbox alone after
	// it changed, so it never races a lifecycle operation.
	ReconcileGrace time.Duration

	wake     chan struct{}
	mu       sync.Mutex
	inflight map[string]bool
	wg       sync.WaitGroup
}

// New returns a scheduler.
func New(st store.Store, pm *pools.Manager, cfg config.Config, installation string, rec *auditlog.Recorder, log *slog.Logger) *Scheduler {
	return &Scheduler{
		Store: st, Pools: pm, Config: cfg, Installation: installation, Audit: rec, Log: log,
		Now: func() time.Time { return time.Now().UTC() }, ReconcileGrace: 30 * time.Second,
		wake: make(chan struct{}, 1), inflight: map[string]bool{},
	}
}

// Wake asks the loop to run a tick soon.
func (s *Scheduler) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run loops until ctx ends.
func (s *Scheduler) Run(ctx context.Context) {
	tick := time.NewTicker(time.Duration(s.Config.Scheduler.IntervalMs) * time.Millisecond)
	refresh := time.NewTicker(5 * time.Second)
	reconcile := time.NewTicker(time.Duration(s.Config.Scheduler.ReconcileIntervalSecs) * time.Second)
	defer tick.Stop()
	defer refresh.Stop()
	defer reconcile.Stop()
	s.Pools.Refresh(ctx)
	for {
		select {
		case <-ctx.Done():
			s.wg.Wait()
			return
		case <-refresh.C:
			s.Pools.Refresh(ctx)
			continue
		case <-reconcile.C:
			if s.leader(ctx) {
				s.Reconcile(ctx)
				if s.sweeping.CompareAndSwap(false, true) {
					// Stops can take seconds each: never hold up the loop.
					go func() {
						defer s.sweeping.Store(false)
						s.SweepIdle(ctx)
					}()
				}
			}
			continue
		case <-tick.C:
		case <-s.wake:
		}
		if s.leader(ctx) {
			s.Tick(ctx)
		}
	}
}

func (s *Scheduler) leader(ctx context.Context) bool {
	ok, err := s.Store.TryLeader(ctx)
	if err != nil {
		s.Log.Warn("leader check failed", "err", err)
	}
	return ok
}

// Tick runs one dispatch/provision/terminate/expire pass.
func (s *Scheduler) Tick(ctx context.Context) {
	s.dispatch(ctx)
	s.startProvisioning(ctx)
	s.terminate(ctx)
	s.expire(ctx)
}

// Drain waits for in-flight provisioning and termination.
func (s *Scheduler) Drain() { s.wg.Wait() }

func (s *Scheduler) policies(ctx context.Context, orgID, projectID string) []api.PlacementPolicy {
	global := s.Config.Policy
	if p, ok, _ := s.Store.GetPolicy(ctx, "global"); ok {
		global = p
	}
	out := []api.PlacementPolicy{global}
	for _, scope := range []string{"org:" + orgID, "project:" + projectID} {
		if p, ok, _ := s.Store.GetPolicy(ctx, scope); ok {
			out = append(out, p)
		}
	}
	return out
}

// PlacementInput assembles the policy input for a sandbox.
func (s *Scheduler) PlacementInput(ctx context.Context, rec store.SandboxRecord, active map[string]int) policy.Input {
	class, _ := s.Config.Classification(rec.Classification)
	in := policy.Input{
		Sandbox: rec.Sandbox, Policies: s.policies(ctx, rec.OrgID, rec.ProjectID), Classification: class,
		Pools: s.Pools.Views(active), ExcludedPools: rec.ExcludedPools, Now: s.Now(),
	}
	if rec.FromSnapshot != "" {
		if snap, err := s.Store.GetSnapshot(ctx, rec.FromSnapshot); err == nil {
			in.PinnedPool = snap.Pool
		}
	}
	if t, ok := s.Config.Template(rec.Template); ok && rec.Template != "" {
		in.Template = &t
	}
	return in
}

func (s *Scheduler) dispatch(ctx context.Context) {
	now := s.Now()
	cands, err := s.Store.QueuedCandidates(ctx, now, 100)
	if err != nil || len(cands) == 0 {
		return
	}
	active, err := s.Store.PoolActive(ctx)
	if err != nil {
		return
	}
	orgs := map[string]api.Organization{}
	for _, rec := range cands {
		org, ok := orgs[rec.OrgID]
		if !ok {
			if org, err = s.Store.GetOrg(ctx, rec.OrgID); err != nil {
				continue
			}
			orgs[rec.OrgID] = org
		}
		if org.Suspended {
			s.wait(ctx, rec.ID, "organization suspended", nil)
			continue
		}
		project, err := s.Store.GetProject(ctx, rec.ProjectID)
		if err != nil {
			continue
		}
		in := s.PlacementInput(ctx, rec, active)
		res := policy.Decide(in)
		if (res.Err != nil || len(res.Ranked) == 0) && len(in.ExcludedPools) > 0 {
			// Every other pool is out: retry the pools that failed before.
			in.ExcludedPools = nil
			res = policy.Decide(in)
		}
		if res.Err != nil {
			metrics.Placements.WithLabelValues("rejected", "").Inc()
			s.fail(ctx, rec.ID, res.Err.Message, res.Decision)
			continue
		}
		if len(res.Ranked) == 0 {
			if policy.QueueExpired(in) {
				s.fail(ctx, rec.ID, "queue timeout: no eligible pool had capacity in time", res.Decision)
				continue
			}
			metrics.Placements.WithLabelValues("waiting", "").Inc()
			s.wait(ctx, rec.ID, "waiting for capacity: "+summarize(res.Decision), res.Decision)
			continue
		}
		s.activate(ctx, rec, res, org, project, active)
	}
}

func (s *Scheduler) activate(ctx context.Context, rec store.SandboxRecord, res policy.Result, org api.Organization, project api.Project, active map[string]int) {
	for _, name := range res.Ranked {
		pool, err := s.Store.GetPool(ctx, name)
		if err != nil {
			continue
		}
		dec := *res.Decision
		dec.Pool, dec.Outcome = name, "placed"
		if name != res.Decision.Pool {
			dec.Reason += "; " + res.Decision.Pool + " filled concurrently, used next ranked pool " + name
		}
		out, err := s.Store.ActivateSandbox(ctx, rec.ID, store.Activation{
			Pool: name, Adapter: pool.Adapter, Trust: pool.Trust, Region: pool.Region, MaxConcurrency: pool.MaxConcurrency,
			OrgQuota: org.Quota, ProjectQuota: project.Quota, Decision: &dec, Now: s.Now(),
		})
		switch {
		case err == nil:
			active[name]++
			metrics.Placements.WithLabelValues("placed", name).Inc()
			metrics.QueueSeconds.Observe(s.Now().Sub(rec.CreatedAt).Seconds())
			s.Audit.Record(ctx, api.AuditEvent{
				Actor: "scheduler", OrgID: out.OrgID, ProjectID: out.ProjectID, Action: "sandbox.placed", Resource: out.ID,
				Details: map[string]string{"pool": name, "trust": pool.Trust, "strategy": dec.Strategy, "external": dec.External, "reason": dec.Reason},
			})
			return
		case api.IsCode(err, api.CodeQuotaExceeded):
			s.wait(ctx, rec.ID, "waiting for tenant quota: "+err.Error(), res.Decision)
			return
		case api.IsCode(err, adapter.CodeInsufficientCapacity):
			continue
		default:
			s.Log.Warn("activation failed", "sandbox", rec.ID, "pool", name, "err", err)
			return
		}
	}
	s.wait(ctx, rec.ID, "waiting for capacity: ranked pools filled concurrently", res.Decision)
}

func summarize(d *api.PlacementDecision) string {
	var parts []string
	for _, c := range d.Candidates {
		if c.Eligible && len(c.Reasons) > 0 {
			parts = append(parts, c.Pool+" ("+strings.Join(c.Reasons, "; ")+")")
		}
	}
	return strings.Join(parts, ", ")
}

func (s *Scheduler) wait(ctx context.Context, sid, reason string, dec *api.PlacementDecision) {
	_, _ = s.Store.UpdateSandbox(ctx, sid, func(r *store.SandboxRecord) error {
		if r.Status != api.StatusQueued {
			return nil
		}
		r.StatusReason = reason
		if dec != nil {
			r.Decision = dec
		}
		r.NextAttemptAt = s.Now().Add(RetryDelay)
		return nil
	})
}

func (s *Scheduler) fail(ctx context.Context, sid, reason string, dec *api.PlacementDecision) {
	rec, err := s.Store.UpdateSandbox(ctx, sid, func(r *store.SandboxRecord) error {
		now := s.Now()
		stopClock(r, now)
		r.Status, r.StatusReason, r.EndedAt, r.UpdatedAt = api.StatusFailed, reason, &now, now
		if dec != nil {
			r.Decision = dec
		}
		return nil
	})
	if err == nil {
		metrics.Transitions.WithLabelValues(string(api.StatusFailed), rec.Pool).Inc()
		s.Audit.Record(ctx, api.AuditEvent{Actor: "scheduler", OrgID: rec.OrgID, ProjectID: rec.ProjectID, Action: "sandbox.failed", Resource: sid, Outcome: "failure", Details: map[string]string{"reason": reason}})
	}
}

func (s *Scheduler) claim(sid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight[sid] {
		return false
	}
	s.inflight[sid] = true
	return true
}

func (s *Scheduler) release(sid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.inflight, sid)
}

func (s *Scheduler) startProvisioning(ctx context.Context) {
	recs, err := s.Store.ListSandboxes(ctx, store.SandboxFilter{Statuses: []api.SandboxStatus{api.StatusProvisioning}})
	if err != nil {
		return
	}
	for _, rec := range recs {
		if !s.claim(rec.ID) {
			continue
		}
		s.wg.Add(1)
		go func(rec store.SandboxRecord) {
			defer s.wg.Done()
			defer s.release(rec.ID)
			s.provision(context.WithoutCancel(ctx), rec)
		}(rec)
	}
}

func (s *Scheduler) provision(ctx context.Context, rec store.SandboxRecord) {
	a, err := s.Pools.Adapter(rec.Pool)
	if err != nil {
		s.retry(ctx, rec, err)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(s.Config.Scheduler.ProvisionTimeoutSeconds)*time.Second)
	defer cancel()
	labels := map[string]string{adapter.LabelOrg: rec.OrgID, adapter.LabelProject: rec.ProjectID}
	for k, v := range rec.Labels {
		labels[k] = v
	}
	start := s.Now()
	inst, err := a.Create(ctx, adapter.CreateRequest{
		ID: rec.ID, Installation: s.Installation, Template: s.Config.BackendTemplate(rec.Template, rec.Pool), Resources: rec.Resources,
		Env: rec.Env, Labels: labels, TTLSeconds: rec.TTLSeconds, FromSnapshot: rec.SnapshotBackendRef,
		IdleTimeoutSeconds: rec.IdleTimeoutSeconds,
	})
	if err != nil {
		s.retry(ctx, rec, err)
		return
	}
	metrics.ProvisionSeconds.WithLabelValues(rec.Pool).Observe(s.Now().Sub(start).Seconds())
	out, err := s.Store.UpdateSandbox(ctx, rec.ID, func(r *store.SandboxRecord) error {
		now := s.Now()
		r.BackendRef = inst.Ref
		if inst.Region != "" {
			r.Region = inst.Region
		}
		if r.Status != api.StatusProvisioning {
			return nil // deleted while provisioning: the terminator now has the ref
		}
		r.Status, r.StatusReason, r.UpdatedAt = api.StatusRunning, "", now
		if r.StartedAt == nil {
			r.StartedAt = &now
		}
		r.RunningSince = &now
		if r.TTLSeconds > 0 {
			exp := now.Add(time.Duration(r.TTLSeconds) * time.Second)
			r.ExpiresAt = &exp
		}
		return nil
	})
	if err == nil && out.Status == api.StatusRunning {
		metrics.Transitions.WithLabelValues(string(api.StatusRunning), rec.Pool).Inc()
		s.Audit.Record(ctx, api.AuditEvent{Actor: "scheduler", OrgID: out.OrgID, ProjectID: out.ProjectID, Action: "sandbox.running", Resource: out.ID, Details: map[string]string{"pool": out.Pool, "backendRef": inst.Ref}})
	}
}

// retry requeues after a provisioning failure or fails the sandbox once
// attempts are exhausted. Transient capacity errors and failures of this one
// sandbox do not count against pool health.
func (s *Scheduler) retry(ctx context.Context, rec store.SandboxRecord, cause error) {
	capacity := api.IsCode(cause, adapter.CodeInsufficientCapacity)
	if api.IsCode(cause, api.CodeInvalidRequest) {
		// The backend rejected the request itself; retrying cannot help and
		// the pool is not unhealthy.
		s.fail(ctx, rec.ID, "backend rejected the sandbox: "+cause.Error(), nil)
		return
	}
	if !capacity && !api.IsCode(cause, adapter.CodeSandboxFailed) {
		s.Pools.MarkUnhealthy(rec.Pool, cause)
		s.Log.Warn("provisioning failed", "sandbox", rec.ID, "pool", rec.Pool, "attempt", rec.Attempts, "err", cause)
	}
	if !capacity && rec.Attempts >= s.Config.Scheduler.MaxAttempts {
		s.fail(ctx, rec.ID, "provisioning failed after "+itoa(rec.Attempts)+" attempts: "+cause.Error(), nil)
		return
	}
	_, _ = s.Store.UpdateSandbox(ctx, rec.ID, func(r *store.SandboxRecord) error {
		if r.Status != api.StatusProvisioning {
			return nil
		}
		now := s.Now()
		if !capacity && !slices.Contains(r.ExcludedPools, r.Pool) {
			r.ExcludedPools = append(r.ExcludedPools, r.Pool)
		}
		r.Status, r.Pool, r.Adapter, r.Trust, r.Region = api.StatusQueued, "", "", "", ""
		r.StatusReason = "retrying after provisioning error: " + cause.Error()
		backoff := RetryDelay * time.Duration(1<<min(r.Attempts, 5))
		if capacity {
			backoff = RetryDelay
			if r.Attempts > 0 {
				r.Attempts-- // capacity misses are not failed attempts
			}
		}
		r.NextAttemptAt, r.UpdatedAt = now.Add(backoff), now
		return nil
	})
	s.Audit.Record(ctx, api.AuditEvent{Actor: "scheduler", OrgID: rec.OrgID, ProjectID: rec.ProjectID, Action: "sandbox.provision_retry", Resource: rec.ID, Outcome: "failure", Details: map[string]string{"pool": rec.Pool, "error": cause.Error()}})
}

func (s *Scheduler) terminate(ctx context.Context) {
	recs, err := s.Store.ListSandboxes(ctx, store.SandboxFilter{Statuses: []api.SandboxStatus{api.StatusTerminating}})
	if err != nil {
		return
	}
	for _, rec := range recs {
		if !s.claim(rec.ID) {
			continue // provisioning still running: it will record the ref first
		}
		s.wg.Add(1)
		go func(rec store.SandboxRecord) {
			defer s.wg.Done()
			defer s.release(rec.ID)
			s.destroy(context.WithoutCancel(ctx), rec)
		}(rec)
	}
}

func (s *Scheduler) destroy(ctx context.Context, rec store.SandboxRecord) {
	if rec.BackendRef != "" {
		a, err := s.Pools.Adapter(rec.Pool)
		if err == nil {
			dctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			err = a.Delete(dctx, rec.BackendRef)
			cancel()
		}
		if err != nil {
			s.Log.Warn("terminate failed; will retry", "sandbox", rec.ID, "pool", rec.Pool, "err", err)
			return
		}
	}
	out, err := s.Store.UpdateSandbox(ctx, rec.ID, func(r *store.SandboxRecord) error {
		now := s.Now()
		stopClock(r, now)
		r.Status, r.EndedAt, r.UpdatedAt = api.StatusTerminated, &now, now
		return nil
	})
	if err == nil {
		s.nudge(ctx)
		metrics.Transitions.WithLabelValues(string(api.StatusTerminated), out.Pool).Inc()
		s.Audit.Record(ctx, api.AuditEvent{Actor: "scheduler", OrgID: out.OrgID, ProjectID: out.ProjectID, Action: "sandbox.terminated", Resource: out.ID, Details: map[string]string{"pool": out.Pool, "reason": out.StatusReason}})
	}
}

func (s *Scheduler) expire(ctx context.Context) {
	now := s.Now()
	recs, err := s.Store.ListSandboxes(ctx, store.SandboxFilter{Statuses: []api.SandboxStatus{api.StatusRunning, api.StatusPaused, api.StatusStopped}})
	if err != nil {
		return
	}
	for _, rec := range recs {
		if rec.ExpiresAt == nil || rec.ExpiresAt.After(now) {
			continue
		}
		_, _ = s.Store.UpdateSandbox(ctx, rec.ID, func(r *store.SandboxRecord) error {
			r.Status, r.StatusReason, r.UpdatedAt = api.StatusTerminating, "ttl expired", now
			return nil
		})
		s.Audit.Record(ctx, api.AuditEvent{Actor: "scheduler", OrgID: rec.OrgID, ProjectID: rec.ProjectID, Action: "sandbox.expired", Resource: rec.ID})
	}
}

// SweepIdle stops or pauses running sandboxes with an idle timeout that have
// had no exec, file, port or keepalive call for that long, and marks them
// idled so the next such call wakes them. Pools that keep memory pause
// (cheaper to wake); others stop with the disk kept. Pools that can do
// neither are left alone, and so are pools whose backend idles natively
// (adapter.NativeIdler): the reconciler follows those pauses instead.
func (s *Scheduler) SweepIdle(ctx context.Context) {
	now := s.Now()
	recs, err := s.Store.ListSandboxes(ctx, store.SandboxFilter{Statuses: []api.SandboxStatus{api.StatusRunning}})
	if err != nil {
		return
	}
	for _, rec := range recs {
		if rec.IdleTimeoutSeconds <= 0 || rec.BackendRef == "" || s.recentlyChanged(rec) {
			continue
		}
		last := rec.UpdatedAt
		if rec.LastActivityAt != nil && rec.LastActivityAt.After(last) {
			last = *rec.LastActivityAt
		}
		if now.Sub(last) < time.Duration(rec.IdleTimeoutSeconds)*time.Second || s.idlesNatively(rec.Pool) || !s.claim(rec.ID) {
			continue
		}
		s.idle(ctx, rec)
		s.release(rec.ID)
	}
}

func (s *Scheduler) idlesNatively(pool string) bool {
	a, err := s.Pools.Adapter(pool)
	if err != nil {
		return false
	}
	n, ok := a.(adapter.NativeIdler)
	return ok && n.IdlesNatively()
}

func (s *Scheduler) idle(ctx context.Context, rec store.SandboxRecord) {
	a, err := s.Pools.Adapter(rec.Pool)
	if err != nil {
		return
	}
	to, action := api.StatusPaused, "pause"
	switch pz, pausable := a.(adapter.Pauser); {
	case pausable && s.Pools.Supports(ctx, rec.Pool, adapter.FeaturePauseResume):
		err = pz.Pause(ctx, rec.BackendRef)
	case s.Pools.Supports(ctx, rec.Pool, adapter.FeatureStopStart):
		l, ok := a.(adapter.Lifecycle)
		if !ok {
			return
		}
		to, action = api.StatusStopped, "stop"
		err = l.Stop(ctx, rec.BackendRef)
	default:
		return
	}
	if err != nil {
		s.Log.Warn("idle "+action+" failed", "sandbox", rec.ID, "pool", rec.Pool, "err", err)
		return
	}
	now := s.Now()
	out, err := s.Store.UpdateSandbox(ctx, rec.ID, func(r *store.SandboxRecord) error {
		if r.Status != api.StatusRunning {
			return api.Errorf(api.CodeConflict, "sandbox changed to %s during the idle %s", r.Status, action)
		}
		stopClock(r, now)
		r.Status, r.UpdatedAt, r.IdledAt = to, now, &now
		r.StatusReason = fmt.Sprintf("%s after %d s idle; the next exec, file, port or keepalive call wakes it", action, rec.IdleTimeoutSeconds)
		return nil
	})
	if err != nil {
		return
	}
	metrics.Reconciled.WithLabelValues("idle_"+action, rec.Pool).Inc()
	s.Audit.Record(ctx, api.AuditEvent{Actor: "scheduler", OrgID: out.OrgID, ProjectID: out.ProjectID, Action: "sandbox.idled", Resource: out.ID, Details: map[string]string{"pool": out.Pool, "action": action}})
}

// nudge makes waiting sandboxes eligible for the next tick after quota or
// capacity was released, instead of waiting out their retry delay.
func (s *Scheduler) nudge(ctx context.Context) {
	now := s.Now()
	waiting, err := s.Store.ListSandboxes(ctx, store.SandboxFilter{Statuses: []api.SandboxStatus{api.StatusQueued}, Limit: 200})
	if err != nil {
		return
	}
	for _, rec := range waiting {
		if rec.NextAttemptAt.After(now) && rec.Attempts == 0 {
			_, _ = s.Store.UpdateSandbox(ctx, rec.ID, func(r *store.SandboxRecord) error {
				if r.Status == api.StatusQueued {
					r.NextAttemptAt = now
				}
				return nil
			})
		}
	}
	s.Wake()
}

// Reconcile corrects drift between the store and every healthy pool.
func (s *Scheduler) Reconcile(ctx context.Context) {
	poolsList, err := s.Store.ListPools(ctx)
	if err != nil {
		return
	}
	for _, p := range poolsList {
		a, err := s.Pools.Adapter(p.Name)
		if err != nil {
			continue
		}
		lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		instances, err := a.List(lctx, s.Installation)
		cancel()
		if err != nil {
			// Never act on a partial view: a failed list is not "no instances".
			s.Log.Warn("reconcile list failed", "pool", p.Name, "err", err)
			continue
		}
		s.reconcilePool(ctx, p.Name, a, instances)
	}
}

func (s *Scheduler) reconcilePool(ctx context.Context, pool string, a adapter.ExecutionAdapter, instances []adapter.Instance) {
	byRef := map[string]adapter.Instance{}
	bySandbox := map[string]adapter.Instance{}
	for _, in := range instances {
		byRef[in.Ref] = in
		bySandbox[in.SandboxID] = in
	}
	// Orphans: backend instances whose sandbox is gone, terminal or elsewhere.
	// "Elsewhere" means placed on another pool with a different instance: two
	// pools can front the same backend (one OpenSandbox server registered
	// twice), so each sees the other's live instances and must leave them be.
	for _, in := range instances {
		rec, err := s.Store.GetSandbox(ctx, in.SandboxID)
		elsewhere := rec.Pool != pool && in.Ref != rec.BackendRef && rec.Status != api.StatusQueued && rec.Status != api.StatusProvisioning
		orphan := api.IsCode(err, api.CodeNotFound) || (err == nil && (rec.Status.Terminal() || elsewhere))
		if !orphan {
			continue
		}
		if err := a.Delete(ctx, in.Ref); err != nil {
			s.Log.Warn("orphan delete failed", "pool", pool, "ref", in.Ref, "err", err)
			continue
		}
		metrics.Reconciled.WithLabelValues("orphan_deleted", pool).Inc()
		s.Audit.Record(ctx, api.AuditEvent{Actor: "reconciler", OrgID: rec.OrgID, ProjectID: rec.ProjectID, Action: "reconcile.orphan_deleted", Resource: in.SandboxID, Details: map[string]string{"pool": pool, "ref": in.Ref}})
	}
	recs, err := s.Store.ListSandboxes(ctx, store.SandboxFilter{Pool: pool, Statuses: []api.SandboxStatus{api.StatusProvisioning, api.StatusRunning, api.StatusPaused, api.StatusStopped}})
	if err != nil {
		return
	}
	now := s.Now()
	timeout := time.Duration(s.Config.Scheduler.ProvisionTimeoutSeconds) * time.Second
	for _, rec := range recs {
		switch rec.Status {
		case api.StatusProvisioning:
			if !s.claim(rec.ID) {
				continue // provisioning is in flight in this process
			}
			stale := rec.ProvisionStartedAt == nil || now.Sub(*rec.ProvisionStartedAt) > timeout
			if in, ok := bySandbox[rec.ID]; ok && stale {
				// Created before a crash: adopt it.
				_, _ = s.Store.UpdateSandbox(ctx, rec.ID, func(r *store.SandboxRecord) error {
					r.BackendRef, r.Status, r.RunningSince, r.UpdatedAt = in.Ref, api.StatusRunning, &now, now
					if r.StartedAt == nil {
						r.StartedAt = &now
					}
					if r.TTLSeconds > 0 && r.ExpiresAt == nil {
						exp := now.Add(time.Duration(r.TTLSeconds) * time.Second)
						r.ExpiresAt = &exp
					}
					return nil
				})
				metrics.Reconciled.WithLabelValues("adopted", pool).Inc()
			} else if stale {
				s.release(rec.ID)
				s.retry(ctx, rec, adapter.Unavailable(errString("provisioning interrupted")))
				continue
			}
			s.release(rec.ID)
		default:
			if rec.BackendRef == "" || s.recentlyChanged(rec) {
				continue
			}
			if in, ok := byRef[rec.BackendRef]; ok {
				s.reconcileState(ctx, pool, rec, in)
				continue
			}
			s.fail(ctx, rec.ID, "backend instance disappeared", nil)
			metrics.Reconciled.WithLabelValues("lost", pool).Inc()
		}
	}
}

// reconcileState follows a backend instance that changed state behind the
// control plane's back, for example sandboxes that stopped when their
// runtime restarted. A stopped instance makes the sandbox stopped (so it can
// be started again from its disk); an instance in error fails it. On pools
// that idle natively, a backend pause of a sandbox with an idle timeout marks
// it idled (so the next call wakes it), and a backend wake of an idled
// sandbox (traffic through the backend's gateway) marks it running again.
func (s *Scheduler) reconcileState(ctx context.Context, pool string, rec store.SandboxRecord, in adapter.Instance) {
	if (rec.Status != api.StatusRunning && rec.Status != api.StatusPaused) || s.isInflight(rec.ID) || s.recentlyChanged(rec) {
		return
	}
	switch in.State {
	case adapter.StateStopped:
		out, err := s.Store.UpdateSandbox(ctx, rec.ID, func(r *store.SandboxRecord) error {
			if r.Status != api.StatusRunning && r.Status != api.StatusPaused {
				return nil
			}
			now := s.Now()
			stopClock(r, now)
			r.Status, r.StatusReason, r.UpdatedAt = api.StatusStopped, "backend instance stopped unexpectedly; start it to boot from its disk", now
			return nil
		})
		if err == nil && out.Status == api.StatusStopped {
			metrics.Reconciled.WithLabelValues("stopped", pool).Inc()
			s.Audit.Record(ctx, api.AuditEvent{Actor: "reconciler", OrgID: rec.OrgID, ProjectID: rec.ProjectID, Action: "reconcile.instance_stopped", Resource: rec.ID, Details: map[string]string{"pool": pool, "ref": in.Ref}})
		}
	case adapter.StatePaused:
		if rec.Status != api.StatusRunning || rec.IdleTimeoutSeconds <= 0 || !s.idlesNatively(pool) {
			return
		}
		out, err := s.Store.UpdateSandbox(ctx, rec.ID, func(r *store.SandboxRecord) error {
			if r.Status != api.StatusRunning {
				return nil
			}
			now := s.Now()
			stopClock(r, now)
			r.Status, r.UpdatedAt, r.IdledAt = api.StatusPaused, now, &now
			r.StatusReason = fmt.Sprintf("paused by the backend after %d s idle; the next exec, file, port or keepalive call wakes it", rec.IdleTimeoutSeconds)
			return nil
		})
		if err == nil && out.Status == api.StatusPaused {
			metrics.Reconciled.WithLabelValues("idle_pause", pool).Inc()
			s.Audit.Record(ctx, api.AuditEvent{Actor: "reconciler", OrgID: rec.OrgID, ProjectID: rec.ProjectID, Action: "sandbox.idled", Resource: rec.ID, Details: map[string]string{"pool": pool, "action": "pause", "by": "backend"}})
		}
	case adapter.StateRunning:
		if rec.Status != api.StatusPaused || rec.IdledAt == nil || !s.idlesNatively(pool) {
			return
		}
		out, err := s.Store.UpdateSandbox(ctx, rec.ID, func(r *store.SandboxRecord) error {
			if r.Status != api.StatusPaused || r.IdledAt == nil {
				return nil
			}
			now := s.Now()
			r.Status, r.UpdatedAt, r.IdledAt, r.StatusReason = api.StatusRunning, now, nil, ""
			r.RunningSince, r.LastActivityAt = &now, &now
			return nil
		})
		if err == nil && out.Status == api.StatusRunning {
			metrics.Reconciled.WithLabelValues("woke", pool).Inc()
			s.Audit.Record(ctx, api.AuditEvent{Actor: "reconciler", OrgID: rec.OrgID, ProjectID: rec.ProjectID, Action: "sandbox.woke", Resource: rec.ID, Details: map[string]string{"pool": pool, "by": "backend"}})
		}
	case adapter.StateError:
		s.fail(ctx, rec.ID, "backend instance failed: "+in.Message, nil)
		metrics.Reconciled.WithLabelValues("failed", pool).Inc()
	}
}

// recentlyChanged reports whether a lifecycle operation (stop, start, pause,
// resume) may still be in progress on another replica. The reconciler leaves
// such sandboxes alone instead of racing the operation.
func (s *Scheduler) recentlyChanged(rec store.SandboxRecord) bool {
	return s.Now().Sub(rec.UpdatedAt) < s.ReconcileGrace
}

func (s *Scheduler) isInflight(sid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inflight[sid]
}

// stopClock folds the current running interval into RunSeconds.
func stopClock(r *store.SandboxRecord, now time.Time) {
	if r.RunningSince != nil {
		r.RunSeconds += now.Sub(*r.RunningSince).Seconds()
		r.RunningSince = nil
	}
}

// StopClock is exported for lifecycle handlers.
func StopClock(r *store.SandboxRecord, now time.Time) { stopClock(r, now) }

type errString string

func (e errString) Error() string { return string(e) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
