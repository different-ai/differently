package scheduler

// Capacity: who holds a pool's slots, and how slots move between sandboxes.
//
//   - A pool's slots are its maxConcurrency. Provisioning, running, stopped
//     and terminating sandboxes hold one. A paused sandbox holds one only on
//     a pool whose pause keeps it on the host; on a pool that checkpoints on
//     pause (lifecycle.pause-checkpoint) the pause gives the slot back
//     (SlotReleased).
//   - Resuming takes a slot again, atomically with the concurrency check. On
//     a full pool the resume waits in a queue (ResumeRequestedAt), pinned to
//     the pool that holds its checkpoint, and is served in fair order as
//     slots free up.
//   - A pool with a preemption policy makes room for waiting work (queued
//     resumes, and queued creates waiting for that pool) by pausing running
//     sandboxes, least recently active first and never one that is not
//     preemptible. Mode idle picks only sandboxes idle for idleSeconds (60 by
//     default), ahead of their idle timeout; mode lru may pick a busy one that
//     ran at least minRunSeconds. The victim keeps its memory and wakes on its
//     next call, queued like any other resume. internal/capsim measures the
//     trade-off: idle mode shortens the queue without interrupting work.
//   - maxRunSeconds pauses a sandbox that has run that long without a break;
//     maxPausedSeconds deletes one left paused or stopped that long.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/different-ai/differently/internal/metrics"
	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// CodeInsufficientCapacity is the code of a pool at its concurrency limit.
const CodeInsufficientCapacity = "insufficient_capacity"

// Preemption defaults.
const (
	defaultMinRunSeconds = 600
	defaultIdleSeconds   = 60
)

// MarkPaused records a pause (or stop) on r: when it happened and, for a pause
// on a pool that checkpoints, that the sandbox gave its slot back.
func MarkPaused(r *store.SandboxRecord, now time.Time, releasesSlot bool) {
	stopClock(r, now)
	r.PausedAt = &now
	r.SlotReleased = r.Status == api.StatusPaused && releasesSlot
}

// MarkRunning records a resume or start on r.
func MarkRunning(r *store.SandboxRecord, now time.Time) {
	r.Status, r.UpdatedAt = api.StatusRunning, now
	r.RunningSince, r.LastActivityAt = &now, &now
	r.PausedAt, r.ResumeRequestedAt, r.SlotReleased, r.IdledAt = nil, nil, false, nil
}

// SlotFreed lets waiting work at a freed slot right away instead of after its
// retry delay.
func (s *Scheduler) SlotFreed(ctx context.Context) { s.nudge(ctx) }

// ReleasesSlot reports whether a pause on pool frees the sandbox's slot.
func (s *Scheduler) ReleasesSlot(ctx context.Context, pool string) bool {
	return s.Pools.Supports(ctx, pool, adapter.FeaturePauseCheckpoint)
}

// ReclaimSlot gives a paused sandbox its slot back before it resumes. A full
// pool answers insufficient_capacity.
func (s *Scheduler) ReclaimSlot(ctx context.Context, rec store.SandboxRecord) (store.SandboxRecord, error) {
	if !rec.SlotReleased {
		return rec, nil
	}
	p, err := s.Store.GetPool(ctx, rec.Pool)
	if err != nil {
		return rec, err
	}
	return s.Store.ReclaimSlot(ctx, rec.ID, p.MaxConcurrency, s.Now())
}

// QueueResume puts a paused sandbox in the resume queue of its pool.
func (s *Scheduler) QueueResume(ctx context.Context, sid string) (store.SandboxRecord, error) {
	now := s.Now()
	out, err := s.Store.UpdateSandbox(ctx, sid, func(r *store.SandboxRecord) error {
		if r.Status != api.StatusPaused {
			return api.Errorf(api.CodeInvalidState, "sandbox %s is %s, not paused", sid, r.Status)
		}
		if r.ResumeRequestedAt == nil {
			r.ResumeRequestedAt = &now
			metrics.ResumesQueued.WithLabelValues(r.Pool).Inc()
		}
		r.StatusReason = fmt.Sprintf("waiting for a free slot on pool %s to resume", r.Pool)
		return nil
	})
	s.Wake()
	return out, err
}

// resumeQueued resumes waiting sandboxes, in fair order, as slots free up.
func (s *Scheduler) resumeQueued(ctx context.Context) map[string]int {
	waiting, err := s.Store.ListSandboxes(ctx, store.SandboxFilter{Statuses: []api.SandboxStatus{api.StatusPaused}})
	if err != nil {
		return nil
	}
	var queue []store.SandboxRecord
	for _, r := range waiting {
		if r.ResumeRequestedAt != nil && !s.isInflight(r.ID) {
			queue = append(queue, r)
		}
	}
	sort.SliceStable(queue, func(i, j int) bool {
		if queue[i].Priority != queue[j].Priority {
			return queue[i].Priority > queue[j].Priority
		}
		return queue[i].ResumeRequestedAt.Before(*queue[j].ResumeRequestedAt)
	})
	demand := map[string]int{}
	full := map[string]bool{}
	for _, rec := range queue {
		if full[rec.Pool] {
			demand[rec.Pool]++
			continue
		}
		out, err := s.ReclaimSlot(ctx, rec)
		if api.IsCode(err, CodeInsufficientCapacity) {
			full[rec.Pool] = true
			demand[rec.Pool]++
			continue
		}
		if err != nil || !s.claim(rec.ID) {
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.release(rec.ID)
			s.resume(ctx, out)
		}()
	}
	return demand
}

// resume brings a queued sandbox back once it holds a slot.
func (s *Scheduler) resume(ctx context.Context, rec store.SandboxRecord) {
	a, err := s.Pools.Adapter(rec.Pool)
	if err == nil {
		pz, ok := a.(adapter.Pauser)
		if !ok {
			err = fmt.Errorf("pool %s cannot resume", rec.Pool)
		} else {
			err = pz.Resume(ctx, rec.BackendRef)
		}
	}
	now := s.Now()
	if err != nil {
		// Give the slot back and keep the request queued for the next tick.
		s.Log.Warn("queued resume failed", "sandbox", rec.ID, "pool", rec.Pool, "err", err)
		_, _ = s.Store.UpdateSandbox(ctx, rec.ID, func(r *store.SandboxRecord) error {
			if r.Status == api.StatusPaused {
				r.SlotReleased = s.ReleasesSlot(ctx, r.Pool)
			}
			return nil
		})
		return
	}
	waited := 0.0
	if rec.ResumeRequestedAt != nil {
		waited = now.Sub(*rec.ResumeRequestedAt).Seconds()
	}
	out, err := s.Store.UpdateSandbox(ctx, rec.ID, func(r *store.SandboxRecord) error {
		if r.Status != api.StatusPaused {
			return nil
		}
		MarkRunning(r, now)
		r.StatusReason = ""
		return nil
	})
	if err != nil || out.Status != api.StatusRunning {
		return
	}
	metrics.ResumeWaitSeconds.WithLabelValues(rec.Pool).Observe(waited)
	metrics.Transitions.WithLabelValues(string(api.StatusRunning), rec.Pool).Inc()
	s.Audit.Record(ctx, api.AuditEvent{Actor: "scheduler", OrgID: out.OrgID, ProjectID: out.ProjectID, Action: "sandbox.resumed", Resource: out.ID, Details: map[string]string{"pool": out.Pool, "waitedSeconds": fmt.Sprintf("%.0f", waited)}})
}

// createDemand counts queued creates waiting for a slot on each pool: those
// whose last placement found the pool eligible but at its concurrency limit
// and no pool ready.
func (s *Scheduler) createDemand(ctx context.Context, demand map[string]int) {
	queued, err := s.Store.ListSandboxes(ctx, store.SandboxFilter{Statuses: []api.SandboxStatus{api.StatusQueued}, Limit: 500})
	if err != nil {
		return
	}
	for _, r := range queued {
		if r.Decision == nil {
			continue
		}
		ready, target := false, ""
		for _, c := range r.Decision.Candidates {
			if c.Eligible && c.HasCapacity {
				ready = true
				break
			}
			if c.Eligible && target == "" {
				for _, why := range c.Reasons {
					if strings.HasPrefix(why, "at concurrency limit") {
						target = c.Pool
					}
				}
			}
		}
		if !ready && target != "" {
			demand[target]++
		}
	}
}

// makeRoom preempts running sandboxes on pools with waiting work and a
// preemption policy, at most one victim per waiting sandbox, counting victims
// still pausing.
func (s *Scheduler) makeRoom(ctx context.Context, demand map[string]int) {
	if len(demand) == 0 {
		return
	}
	now := s.Now()
	for pool, want := range demand {
		p, err := s.Store.GetPool(ctx, pool)
		if err != nil || p.Preemption == nil || p.Preemption.Mode == "" || p.Preemption.Mode == api.PreemptOff || !s.ReleasesSlot(ctx, pool) {
			continue
		}
		s.mu.Lock()
		want -= s.preempting[pool]
		s.mu.Unlock()
		if want <= 0 {
			continue
		}
		running, err := s.Store.ListSandboxes(ctx, store.SandboxFilter{Pool: pool, Statuses: []api.SandboxStatus{api.StatusRunning}})
		if err != nil {
			continue
		}
		for _, v := range Victims(running, *p.Preemption, now) {
			if want == 0 {
				break
			}
			if s.isInflight(v.ID) || !s.claim(v.ID) {
				continue
			}
			want--
			s.mu.Lock()
			s.preempting[pool]++
			s.mu.Unlock()
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				defer s.release(v.ID)
				defer func() {
					s.mu.Lock()
					s.preempting[pool]--
					s.mu.Unlock()
				}()
				if s.park(ctx, v, "preempted", fmt.Sprintf("paused to make room on pool %s (preemption %s); the next exec, file, port or keepalive call resumes it", pool, p.Preemption.Mode)) {
					metrics.Preemptions.WithLabelValues(pool, p.Preemption.Mode).Inc()
				}
			}()
		}
	}
}

// Victims orders the running sandboxes a preemption policy may pause, best
// first: least recently active, then lowest priority, then longest running.
func Victims(running []store.SandboxRecord, p api.Preemption, now time.Time) []store.SandboxRecord {
	minRun := time.Duration(p.MinRunSeconds) * time.Second
	if p.MinRunSeconds == 0 {
		minRun = defaultMinRunSeconds * time.Second
	}
	idleFor := time.Duration(p.IdleSeconds) * time.Second
	if p.IdleSeconds == 0 {
		idleFor = defaultIdleSeconds * time.Second
	}
	last := func(r store.SandboxRecord) time.Time {
		if r.LastActivityAt != nil {
			return *r.LastActivityAt
		}
		return r.UpdatedAt
	}
	var out []store.SandboxRecord
	for _, r := range running {
		switch {
		case !r.Preemptible || r.BackendRef == "":
		case p.Mode == api.PreemptLRU && r.RunningSince != nil && now.Sub(*r.RunningSince) < minRun:
		case p.Mode == api.PreemptIdle && now.Sub(last(r)) < idleFor:
		default:
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if !last(a).Equal(last(b)) {
			return last(a).Before(last(b))
		}
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		return a.RunningSince != nil && (b.RunningSince == nil || a.RunningSince.Before(*b.RunningSince))
	})
	return out
}

// EnforceLifecycle applies maxRunSeconds and maxPausedSeconds.
func (s *Scheduler) EnforceLifecycle(ctx context.Context) {
	now := s.Now()
	recs, err := s.Store.ListSandboxes(ctx, store.SandboxFilter{Statuses: []api.SandboxStatus{api.StatusRunning, api.StatusPaused, api.StatusStopped}})
	if err != nil {
		return
	}
	for _, rec := range recs {
		switch {
		case rec.Status == api.StatusRunning && rec.MaxRunSeconds > 0 && rec.RunningSince != nil &&
			now.Sub(*rec.RunningSince) >= time.Duration(rec.MaxRunSeconds)*time.Second && !s.recentlyChanged(rec):
			if !s.claim(rec.ID) {
				continue
			}
			s.park(ctx, rec, "max_run", fmt.Sprintf("paused after running %d s without a break (maxRunSeconds); the next exec, file, port or keepalive call resumes it", rec.MaxRunSeconds))
			s.release(rec.ID)
		case rec.Status != api.StatusRunning && rec.MaxPausedSeconds > 0 && rec.PausedAt != nil &&
			now.Sub(*rec.PausedAt) >= time.Duration(rec.MaxPausedSeconds)*time.Second:
			out, err := s.Store.UpdateSandbox(ctx, rec.ID, func(r *store.SandboxRecord) error {
				if r.Status == api.StatusRunning || r.Status.Terminal() || r.Status == api.StatusTerminating {
					return nil
				}
				r.Status, r.UpdatedAt = api.StatusTerminating, now
				r.StatusReason = fmt.Sprintf("deleted after %d s paused (maxPausedSeconds)", rec.MaxPausedSeconds)
				return nil
			})
			if err == nil && out.Status == api.StatusTerminating {
				metrics.LifecycleActions.WithLabelValues("max_paused", rec.Pool).Inc()
				s.Audit.Record(ctx, api.AuditEvent{Actor: "scheduler", OrgID: out.OrgID, ProjectID: out.ProjectID, Action: "sandbox.expired", Resource: out.ID, Details: map[string]string{"reason": "maxPausedSeconds"}})
			}
		}
	}
}

// park pauses (or, where the pool cannot pause, stops) a running sandbox for
// the scheduler's own reasons and marks it idled, so the next call wakes it.
// It reports whether the sandbox was parked.
func (s *Scheduler) park(ctx context.Context, rec store.SandboxRecord, kind, reason string) bool {
	a, err := s.Pools.Adapter(rec.Pool)
	if err != nil {
		return false
	}
	to := api.StatusPaused
	switch pz, pausable := a.(adapter.Pauser); {
	case pausable && s.Pools.Supports(ctx, rec.Pool, adapter.FeaturePauseResume):
		err = pz.Pause(ctx, rec.BackendRef)
	case s.Pools.Supports(ctx, rec.Pool, adapter.FeatureStopStart):
		l, ok := a.(adapter.Lifecycle)
		if !ok {
			return false
		}
		to = api.StatusStopped
		err = l.Stop(ctx, rec.BackendRef)
	default:
		return false
	}
	if err != nil {
		s.Log.Warn(kind+" pause failed", "sandbox", rec.ID, "pool", rec.Pool, "err", err)
		return false
	}
	now := s.Now()
	releases := s.ReleasesSlot(ctx, rec.Pool)
	out, err := s.Store.UpdateSandbox(ctx, rec.ID, func(r *store.SandboxRecord) error {
		if r.Status != api.StatusRunning {
			return api.Errorf(api.CodeConflict, "sandbox changed to %s during the %s pause", r.Status, kind)
		}
		r.Status, r.UpdatedAt, r.IdledAt, r.StatusReason = to, now, &now, reason
		MarkPaused(r, now, releases)
		return nil
	})
	if err != nil {
		return false
	}
	metrics.LifecycleActions.WithLabelValues(kind, rec.Pool).Inc()
	metrics.Transitions.WithLabelValues(string(to), rec.Pool).Inc()
	s.Audit.Record(ctx, api.AuditEvent{Actor: "scheduler", OrgID: out.OrgID, ProjectID: out.ProjectID, Action: "sandbox." + kind, Resource: out.ID, Details: map[string]string{"pool": out.Pool, "to": string(to)}})
	if out.SlotReleased {
		s.SlotFreed(ctx)
	}
	return true
}

// observeSlots publishes each pool's slot use.
func (s *Scheduler) observeSlots(ctx context.Context) {
	poolsList, err := s.Store.ListPools(ctx)
	if err != nil {
		return
	}
	recs, err := s.Store.ListSandboxes(ctx, store.SandboxFilter{Statuses: []api.SandboxStatus{api.StatusProvisioning, api.StatusRunning, api.StatusPaused, api.StatusStopped, api.StatusTerminating}})
	if err != nil {
		return
	}
	held, released, waiting := map[string]int{}, map[string]int{}, map[string]int{}
	for _, r := range recs {
		switch {
		case store.HoldsPoolSlot(r):
			held[r.Pool]++
		case r.Status == api.StatusPaused:
			released[r.Pool]++
		}
		if r.ResumeRequestedAt != nil {
			waiting[r.Pool]++
		}
	}
	for _, p := range poolsList {
		metrics.PoolSlots.WithLabelValues(p.Name, "held").Set(float64(held[p.Name]))
		metrics.PoolSlots.WithLabelValues(p.Name, "released").Set(float64(released[p.Name]))
		metrics.PoolSlots.WithLabelValues(p.Name, "waiting_resume").Set(float64(waiting[p.Name]))
		metrics.PoolSlots.WithLabelValues(p.Name, "limit").Set(float64(p.MaxConcurrency))
		if p.MaxConcurrency > 0 && held[p.Name] > p.MaxConcurrency {
			metrics.Overcommitted.WithLabelValues(p.Name).Set(float64(held[p.Name] - p.MaxConcurrency))
		} else {
			metrics.Overcommitted.WithLabelValues(p.Name).Set(0)
		}
	}
}
