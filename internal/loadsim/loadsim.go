// Package loadsim runs a whole Differently control plane in one process and
// drives it with simulated users, so the scheduler can be watched under load
// without any infrastructure. The website's /simulator/ page compiles it to
// WebAssembly (cmd/differently-loadsim).
//
// What is real: the HTTP API, admission, placement policy, the scheduler, the
// capacity rules (slots, the resume queue, preemption), the reconciler and
// the in-memory store. Every simulated user acts through the HTTP handler
// with a project key, as a client would.
//
// What is not: the backends. Both pools use the fake adapter, wrapped so that
// a create, a pause and a resume take as long as the configuration says. The
// numbers therefore describe how work is scheduled, never how fast a backend
// is.
//
// The world keeps real time. The simulator page speeds it up by scaling the
// clock the WebAssembly runtime reads; nothing here knows about that.
package loadsim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/different-ai/differently/internal/adapters/fake"
	"github.com/different-ai/differently/internal/auditlog"
	"github.com/different-ai/differently/internal/config"
	"github.com/different-ai/differently/internal/pools"
	"github.com/different-ai/differently/internal/scheduler"
	"github.com/different-ai/differently/internal/secrets"
	"github.com/different-ai/differently/internal/server"
	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// Pool names.
const (
	PrivatePool  = "own-cluster"
	ExternalPool = "burst-provider"
)

// operatorToken bootstraps the in-process installation. It guards nothing:
// the installation lives in this process and is reachable only from it.
const operatorToken = "dly_simulator_0123456789abcdefghjkmnpqrstvwxyz"

// Config describes the installation. It is fixed for a world's life.
type Config struct {
	// Slots is the private pool's maxConcurrency.
	Slots int `json:"slots"`
	// Checkpoint makes a pause on the private pool write the sandbox out and
	// free its slot (lifecycle.pause-checkpoint). Without it a paused sandbox
	// keeps its slot.
	Checkpoint bool `json:"checkpoint"`
	// Preemption is the private pool's preemption mode: off, idle or lru.
	Preemption string `json:"preemption"`
	// Overflow lets creates that find the private pool full run on the
	// external pool (policy external: overflow). Without it they queue.
	Overflow bool `json:"overflow"`
	// How long the private backend takes, in seconds.
	CreateSeconds float64 `json:"createSeconds"`
	PauseSeconds  float64 `json:"pauseSeconds"`
	ResumeSeconds float64 `json:"resumeSeconds"`
	// IdleTimeoutSeconds pauses a computer nobody has used for this long (at
	// least 60).
	IdleTimeoutSeconds int    `json:"idleTimeoutSeconds"`
	Seed               uint64 `json:"seed"`

	// The users' clockwork. Zero values take the defaults; tests shorten them.
	ThinkStep time.Duration `json:"-"` // how often an idle user decides whether to start (5 s)
	Heartbeat time.Duration `json:"-"` // keepalive interval during a session (20 s)
	Retry     time.Duration `json:"-"` // pause between attempts to reach a computer (1 s)
}

// Load is what the users do. It can change while the world runs.
type Load struct {
	// Users is how many people are active. Each owns one long-lived computer.
	Users int `json:"users"`
	// SessionsPerHour is how often an idle user starts working.
	SessionsPerHour float64 `json:"sessionsPerHour"`
	// SessionMinutes is the median session length (log-normal, sigma 0.9).
	SessionMinutes float64 `json:"sessionMinutes"`
}

// Box is one sandbox as the page draws it.
type Box struct {
	ID            string `json:"id"`
	User          int    `json:"user"`
	Status        string `json:"status"`
	Pool          string `json:"pool,omitempty"`
	Busy          bool   `json:"busy"`                   // its user is in a session
	Waiting       bool   `json:"waiting"`                // its user is waiting for it
	SlotReleased  bool   `json:"slotReleased,omitempty"` // paused without a slot
	ResumeQueued  bool   `json:"resumeQueued,omitempty"` // waiting for a slot to resume
	Idled         bool   `json:"idled,omitempty"`
	Reason        string `json:"reason,omitempty"`
	WaitedSeconds int    `json:"waitedSeconds,omitempty"` // how long its user has waited so far
}

// PoolView is one pool's state.
type PoolView struct {
	Name    string `json:"name"`
	Trust   string `json:"trust"`
	Slots   int    `json:"slots"` // 0: no limit
	Healthy bool   `json:"healthy"`
	Detail  string `json:"detail,omitempty"`
}

// Event is one audit event, or one thing a user saw.
type Event struct {
	Seconds float64 `json:"t"` // since the world started
	Action  string  `json:"action"`
	Sandbox string  `json:"sandbox,omitempty"`
	User    int     `json:"user"` // -1 when unknown
	Text    string  `json:"text"`
}

// Stats are totals since the world started.
type Stats struct {
	Sessions int `json:"sessions"`
	// Waits, in seconds, from a session's start until its computer answered.
	WaitP50 float64 `json:"waitP50"`
	WaitP95 float64 `json:"waitP95"`
	WaitMax float64 `json:"waitMax"`
	// Instant is the share of sessions whose computer answered within 3 s.
	Instant float64 `json:"instant"`
	// PrivateSessions is the share of sessions served on the private pool.
	PrivateSessions float64 `json:"privateSessions"`
	// ExternalHours is computer-hours run on the external pool.
	ExternalHours float64 `json:"externalHours"`
	// PrivateBusyHours is session time served by the private pool.
	PrivateBusyHours float64 `json:"privateBusyHours"`
	Overflowed       int     `json:"overflowed"`    // creates placed on the external pool
	Queued           int     `json:"queuedResumes"` // resumes that waited for a slot, or for a pause to finish
	Preemptions      int     `json:"preemptions"`
	Interruptions    int     `json:"interruptions"` // sessions that lost their computer midway
	Retries          int     `json:"retries"`       // provisioning attempts that failed and were retried
}

// Snapshot is everything the page shows.
type Snapshot struct {
	Seconds float64    `json:"seconds"` // since the world started
	Pools   []PoolView `json:"pools"`
	Boxes   []Box      `json:"boxes"`
	// What the users are doing.
	Working int   `json:"working"`
	Waiting int   `json:"waiting"`
	Idle    int   `json:"idle"`
	Stats   Stats `json:"stats"`
	// Events since the previous snapshot.
	Events []Event `json:"events"`
	Outage bool    `json:"outage"`
}

// backend is the fake adapter with latency and an off switch.
type backend struct {
	*fake.Adapter
	create, pause, resume time.Duration
	down                  atomic.Bool
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

var errOffline = errors.New("backend offline")

func (b *backend) Health(context.Context) error {
	if b.down.Load() {
		return errOffline
	}
	return nil
}

func (b *backend) Create(ctx context.Context, req adapter.CreateRequest) (adapter.Instance, error) {
	if b.down.Load() {
		return adapter.Instance{}, adapter.Unavailable(errOffline)
	}
	if err := sleep(ctx, b.create); err != nil {
		return adapter.Instance{}, err
	}
	return b.Adapter.Create(ctx, req)
}

func (b *backend) Pause(ctx context.Context, ref string) error {
	if err := sleep(ctx, b.pause); err != nil {
		return err
	}
	return b.Adapter.Pause(ctx, ref)
}

func (b *backend) Resume(ctx context.Context, ref string) error {
	if err := sleep(ctx, b.resume); err != nil {
		return err
	}
	return b.Adapter.Resume(ctx, ref)
}

type userState int

const (
	userIdle userState = iota
	userWaiting
	userWorking
)

type user struct {
	n         int
	sandbox   string
	state     userState
	waitSince time.Time
	kick      chan struct{}
}

// World is one installation and its users.
type World struct {
	cfg     Config
	store   store.Store
	pools   *pools.Manager
	sched   *scheduler.Scheduler
	handler http.Handler
	token   string
	private *backend
	started time.Time

	mu       sync.Mutex
	rng      *rand.Rand
	load     Load
	users    []*user
	byBox    map[string]int // sandbox ID -> user
	waits    []float64
	sessions int
	onPriv   int
	events   []Event
	stats    Stats
	auditSeq int64
	lastTick time.Time
	ctx      context.Context
}

func (c *Config) defaults() {
	c.Slots = max(c.Slots, 1)
	if c.Preemption == "" {
		c.Preemption = api.PreemptOff
	}
	c.IdleTimeoutSeconds = max(c.IdleTimeoutSeconds, 60)
	if c.ThinkStep <= 0 {
		c.ThinkStep = 5 * time.Second
	}
	if c.Heartbeat <= 0 {
		c.Heartbeat = 20 * time.Second
	}
	if c.Retry <= 0 {
		c.Retry = time.Second
	}
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// New builds the installation: two pools, a placement policy, one tenant.
func New(cfg Config) (*World, error) {
	cfg.defaults()
	ctx := context.Background()
	c := config.Default()
	c.RateLimit.RequestsPerSecond = 0
	c.Scheduler.IntervalMs = 1000 // requests wake the scheduler; the tick only serves timers
	c.Policy = api.PlacementPolicy{Scope: "global", External: api.ExternalNever}
	if cfg.Overflow {
		c.Policy.External, c.Policy.ExternalPools = api.ExternalOverflow, []string{ExternalPool}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := store.NewMemory()
	rec, err := auditlog.New(st, log, "")
	if err != nil {
		return nil, err
	}
	box, err := secrets.New(make([]byte, secrets.KeySize))
	if err != nil {
		return nil, err
	}
	pm := pools.NewManager(pools.Deps{Store: st, Logger: log, Resolve: server.CredentialResolver(st, box)})

	features := slices.Clone(fake.DefaultFeatures)
	if cfg.Checkpoint {
		features = append(features, adapter.FeaturePauseCheckpoint)
	}
	private := &backend{
		Adapter: fake.New(fake.Options{Trust: api.TrustPrivate, Features: features, Region: "dc1"}),
		create:  seconds(cfg.CreateSeconds), pause: seconds(cfg.PauseSeconds), resume: seconds(cfg.ResumeSeconds),
	}
	external := &backend{
		Adapter: fake.New(fake.Options{Trust: api.TrustExternal, Region: "us"}),
		create:  seconds(cfg.CreateSeconds), pause: 2 * time.Second, resume: seconds(cfg.ResumeSeconds),
	}
	var pre *api.Preemption
	if cfg.Preemption != api.PreemptOff {
		pre = &api.Preemption{Mode: cfg.Preemption, IdleSeconds: 60, MinRunSeconds: 600}
	}
	for _, p := range []struct {
		pool api.Pool
		a    adapter.ExecutionAdapter
	}{
		{api.Pool{Name: PrivatePool, Adapter: "fake", Trust: api.TrustPrivate, Enabled: true, Region: "dc1", MaxConcurrency: cfg.Slots, Source: "config", Preemption: pre}, private},
		{api.Pool{Name: ExternalPool, Adapter: "fake", Trust: api.TrustExternal, Enabled: true, Region: "us", Source: "config"}, external},
	} {
		if err := st.UpsertPool(ctx, p.pool); err != nil {
			return nil, err
		}
		pm.Set(p.pool, p.a)
	}
	inst, err := st.Installation(ctx)
	if err != nil {
		return nil, err
	}
	sched := scheduler.New(st, pm, c, inst, rec, log)
	srv := server.New(c, st, pm, sched, rec, log, inst)
	srv.Secrets = box
	if err := os.Setenv(c.Auth.BootstrapTokenEnv, operatorToken); err != nil {
		return nil, err
	}
	if err := srv.Bootstrap(ctx); err != nil {
		return nil, err
	}
	pm.Refresh(ctx)

	w := &World{
		cfg: cfg, store: st, pools: pm, sched: sched, handler: srv.Handler(), private: private,
		rng: rand.New(rand.NewPCG(cfg.Seed, 7)), byBox: map[string]int{},
	}
	var created struct {
		Organization api.Organization `json:"organization"`
		Project      api.Project      `json:"project"`
	}
	if code := w.call(ctx, "POST", "/v1/admin/organizations", operatorToken, map[string]any{"name": "simulated", "defaultProject": "default"}, &created); code != 201 {
		return nil, fmt.Errorf("create organization: status %d", code)
	}
	var key api.CreateAPIKeyResponse
	if code := w.call(ctx, "POST", "/v1/api-keys", operatorToken, api.CreateAPIKeyRequest{
		Name: "users", OrgID: created.Organization.ID, ProjectID: created.Project.ID,
		Scopes: []string{"sandboxes:read", "sandboxes:write", "sandboxes:exec"},
	}, &key); code != 201 {
		return nil, fmt.Errorf("create key: status %d", code)
	}
	w.token = key.Token
	if evs, err := st.ListAudit(ctx, store.AuditFilter{Desc: true, Limit: 1}); err == nil && len(evs) > 0 {
		w.auditSeq = evs[0].Seq // the page starts after the setup
	}
	return w, nil
}

// call sends one request through the server's handler, as a client would.
func (w *World) call(ctx context.Context, method, path, token string, body, out any) int {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, r).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rw := httptest.NewRecorder()
	w.handler.ServeHTTP(rw, req)
	if out != nil && rw.Body.Len() > 0 {
		_ = json.Unmarshal(rw.Body.Bytes(), out)
	}
	return rw.Code
}

// Start runs the scheduler and the users until ctx ends.
func (w *World) Start(ctx context.Context) {
	w.mu.Lock()
	w.ctx, w.started, w.lastTick = ctx, time.Now(), time.Now()
	w.mu.Unlock()
	go w.sched.Run(ctx)
}

// SetLoad changes what the users do. New users start idle.
func (w *World) SetLoad(l Load) {
	l.Users = max(l.Users, 0)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.load = l
	for len(w.users) < l.Users {
		u := &user{n: len(w.users), kick: make(chan struct{}, 1)}
		w.users = append(w.users, u)
		if w.ctx != nil {
			go w.run(w.ctx, u)
		}
	}
}

// Spike makes this share of the active users who are idle start a session now.
func (w *World) Spike(share float64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, u := range w.users[:min(w.load.Users, len(w.users))] {
		if u.state == userIdle && w.rng.Float64() < share {
			select {
			case u.kick <- struct{}{}:
			default:
			}
		}
	}
}

// SetOutage takes the private backend offline or brings it back: it refuses
// creates and fails its health check. Sandboxes already on it keep running.
func (w *World) SetOutage(down bool) {
	w.private.down.Store(down)
	w.pools.Refresh(context.Background())
	w.sched.Wake()
}

func (w *World) since() float64 { return time.Since(w.started).Seconds() }

func (w *World) note(u *user, action, text string) {
	w.events = append(w.events, Event{Seconds: w.since(), Action: action, Sandbox: u.sandbox, User: u.n, Text: text})
}

// run is one user's life: idle, then a session, again.
func (w *World) run(ctx context.Context, u *user) {
	for ctx.Err() == nil {
		// A memoryless start: every step, start with the probability that
		// gives SessionsPerHour, so a change of rate applies at once.
		kicked := false
		t := time.NewTimer(w.cfg.ThinkStep)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-u.kick:
			t.Stop()
			kicked = true
		case <-t.C:
		}
		w.mu.Lock()
		active := u.n < w.load.Users
		p := 1 - math.Exp(-w.load.SessionsPerHour*w.cfg.ThinkStep.Hours())
		start := active && (kicked || w.rng.Float64() < p)
		median := w.load.SessionMinutes * 60
		length := seconds(max(median*math.Exp(0.9*w.rng.NormFloat64()), 30))
		w.mu.Unlock()
		if start {
			w.session(ctx, u, length)
		}
	}
}

// session is one stretch of work: reach the computer, keep it busy, leave.
func (w *World) session(ctx context.Context, u *user, length time.Duration) {
	began := time.Now()
	if !w.reach(ctx, u, began) {
		return
	}
	w.mu.Lock()
	w.sessions++
	wait := time.Since(began).Seconds()
	w.waits = append(w.waits, wait)
	if rec, err := w.store.GetSandbox(ctx, u.sandbox); err == nil && rec.Pool == PrivatePool {
		w.onPriv++
	}
	w.mu.Unlock()

	end := time.Now().Add(length)
	for time.Now().Before(end) {
		if sleep(ctx, min(w.cfg.Heartbeat, time.Until(end))) != nil {
			return
		}
		asked := time.Now()
		code := w.call(ctx, "POST", "/v1/sandboxes/"+u.sandbox+"/keepalive", w.token, nil, nil)
		if code == http.StatusOK && time.Since(asked) < 3*w.cfg.Retry {
			continue
		}
		// The computer was taken away mid-session (preempted, or lost).
		w.mu.Lock()
		w.stats.Interruptions++
		w.note(u, "user.interrupted", "lost its computer in the middle of a session")
		w.mu.Unlock()
		if !w.reach(ctx, u, asked) {
			return
		}
	}
	w.mu.Lock()
	u.state = userIdle
	w.mu.Unlock()
}

// reach creates the user's computer if there is none and calls it until it
// answers. It reports false when the world ended first.
func (w *World) reach(ctx context.Context, u *user, since time.Time) bool {
	w.mu.Lock()
	u.state, u.waitSince = userWaiting, since
	w.mu.Unlock()
	for ctx.Err() == nil {
		if u.sandbox == "" {
			var sb api.Sandbox
			code := w.call(ctx, "POST", "/v1/sandboxes", w.token, api.CreateSandboxRequest{
				Profile: "small", IdleTimeoutSeconds: w.cfg.IdleTimeoutSeconds,
				Labels: map[string]string{"user": fmt.Sprint(u.n)},
			}, &sb)
			if code == http.StatusAccepted && sb.ID != "" {
				w.mu.Lock()
				u.sandbox = sb.ID
				w.byBox[sb.ID] = u.n
				w.mu.Unlock()
			}
		} else {
			code := w.call(ctx, "POST", "/v1/sandboxes/"+u.sandbox+"/exec", w.token, api.ExecRequest{Command: "true"}, nil)
			if code == http.StatusOK {
				w.mu.Lock()
				u.state = userWorking
				w.mu.Unlock()
				return true
			}
			// Gone for good (failed, expired): the user starts a new one.
			var sb api.Sandbox
			got := w.call(ctx, "GET", "/v1/sandboxes/"+u.sandbox, w.token, nil, &sb)
			if got == http.StatusNotFound || sb.Status == api.StatusFailed || sb.Status == api.StatusTerminated || sb.Status == api.StatusTerminating {
				w.mu.Lock()
				w.note(u, "user.replaced", "its computer is gone ("+string(sb.Status)+"); starting a new one")
				u.sandbox = ""
				w.mu.Unlock()
				continue
			}
		}
		if sleep(ctx, w.cfg.Retry) != nil {
			break
		}
	}
	return false
}

func pct(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(q*float64(len(sorted)))) - 1
	return sorted[max(0, min(i, len(sorted)-1))]
}

var live = []api.SandboxStatus{
	api.StatusQueued, api.StatusProvisioning, api.StatusRunning, api.StatusStopped, api.StatusPausing, api.StatusPaused,
}

// Snapshot reads the installation's state and what happened since the last
// call.
func (w *World) Snapshot() Snapshot {
	ctx := context.Background()
	recs, _ := w.store.ListSandboxes(ctx, store.SandboxFilter{Statuses: live})
	audit, _ := w.store.ListAudit(ctx, store.AuditFilter{AfterSeq: w.auditSeq})

	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	snap := Snapshot{Seconds: w.since(), Outage: w.private.down.Load(), Boxes: []Box{}, Events: []Event{}}

	for _, v := range w.pools.Views(nil) {
		snap.Pools = append(snap.Pools, PoolView{Name: v.Pool.Name, Trust: v.Pool.Trust, Slots: v.Pool.MaxConcurrency, Healthy: v.Healthy, Detail: v.HealthDetail})
	}
	sort.Slice(snap.Pools, func(i, j int) bool { return snap.Pools[i].Trust > snap.Pools[j].Trust }) // private first

	dt := now.Sub(w.lastTick).Hours()
	w.lastTick = now
	for _, r := range recs {
		b := Box{
			ID: r.ID, User: -1, Status: string(r.Status), Pool: r.Pool, SlotReleased: r.SlotReleased,
			ResumeQueued: r.ResumeRequestedAt != nil, Idled: r.IdledAt != nil, Reason: r.StatusReason,
		}
		if n, ok := w.byBox[r.ID]; ok {
			u := w.users[n]
			b.User, b.Busy, b.Waiting = n, u.state == userWorking, u.state == userWaiting
			if b.Waiting {
				b.WaitedSeconds = int(now.Sub(u.waitSince).Seconds())
			}
		}
		snap.Boxes = append(snap.Boxes, b)
		// Hours accrue between snapshots, which the page takes many times a
		// simulated minute.
		holding := r.Status == api.StatusRunning || r.Status == api.StatusProvisioning || r.Status == api.StatusPausing
		switch {
		case holding && r.Pool == ExternalPool:
			w.stats.ExternalHours += dt
		case r.Status == api.StatusRunning && r.Pool == PrivatePool && b.Busy:
			w.stats.PrivateBusyHours += dt
		}
	}
	sort.Slice(snap.Boxes, func(i, j int) bool { return snap.Boxes[i].User < snap.Boxes[j].User })

	for _, u := range w.users[:min(w.load.Users, len(w.users))] {
		switch u.state {
		case userWorking:
			snap.Working++
		case userWaiting:
			snap.Waiting++
		default:
			snap.Idle++
		}
	}

	for _, ev := range audit {
		w.auditSeq = max(w.auditSeq, ev.Seq)
		text, ok := describe(ev)
		queued := false
		switch ev.Action {
		case "sandbox.placed":
			if ev.Details["pool"] == ExternalPool {
				w.stats.Overflowed++
			}
		case "sandbox.resumed":
			if w := ev.Details["waitedSeconds"]; w != "" && w != "0" {
				queued = true
			}
		case "sandbox.preempted":
			w.stats.Preemptions++
		case "sandbox.provision_retry":
			w.stats.Retries++
		}
		if queued {
			w.stats.Queued++
		}
		if !ok {
			continue
		}
		n, known := w.byBox[ev.Resource]
		if !known {
			n = -1
		}
		w.events = append(w.events, Event{Seconds: ev.Time.Sub(w.started).Seconds(), Action: ev.Action, Sandbox: ev.Resource, User: n, Text: text})
	}
	sort.SliceStable(w.events, func(i, j int) bool { return w.events[i].Seconds < w.events[j].Seconds })
	snap.Events, w.events = append(snap.Events, w.events...), nil

	sorted := slices.Clone(w.waits)
	sort.Float64s(sorted)
	instant := sort.SearchFloat64s(sorted, 3.0001)
	s := w.stats
	s.Sessions = w.sessions
	s.WaitP50, s.WaitP95, s.WaitMax = pct(sorted, 0.5), pct(sorted, 0.95), pct(sorted, 1)
	if w.sessions > 0 {
		s.Instant = float64(instant) / float64(len(sorted))
		s.PrivateSessions = float64(w.onPriv) / float64(w.sessions)
	}
	snap.Stats = s
	return snap
}

// describe turns an audit event into a line for the page; false for events
// that are not about scheduling (every exec and keepalive is audited too).
func describe(ev api.AuditEvent) (string, bool) {
	d := ev.Details
	switch ev.Action {
	case "sandbox.placed":
		if d["reason"] != "" {
			return "placed on " + d["pool"] + ": " + d["reason"], true
		}
		return "placed on " + d["pool"], true
	case "sandbox.running":
		return "running on " + d["pool"], true
	case "sandbox.idled":
		return "idle past its timeout: " + d["action"] + " on " + d["pool"], true
	case "sandbox.woke":
		return "woken by its user's call", true
	case "sandbox.resume_queued":
		return "resume queued: " + d["pool"] + " has no free slot", true
	case "sandbox.resumed":
		if d["waitedSeconds"] != "" && d["waitedSeconds"] != "0" {
			return "resumed on " + d["pool"] + " after waiting " + d["waitedSeconds"] + " s for a slot", true
		}
		return "resumed on " + d["pool"], true
	case "sandbox.preempted":
		return "paused to make room on " + d["pool"] + " for waiting work", true
	case "sandbox.provision_retry":
		return "create failed on " + d["pool"] + " (" + d["error"] + "); queued again", true
	case "sandbox.failed":
		return "failed: " + d["reason"], true
	case "sandbox.rejected":
		return "rejected: " + d["reason"], true
	}
	return "", false
}
