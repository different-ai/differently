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
// What is not: the backends. Every pool uses the fake adapter, wrapped so
// that a create, a pause and a resume take as long as the configuration says,
// whatever provider the page names it after. The numbers therefore describe
// how work is routed and scheduled, never how fast a backend is.
//
// Routing can change while the world runs, through the admin API: pools are
// enabled and disabled, their slots and the placement policy change. A split
// by percentage is the calling application's doing, not a policy feature:
// each create is pinned to a pool with placement.allowedPools.
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

// Provider is one pool of the installation. The page names them after real
// providers; every one is the fake adapter.
type Provider struct {
	Name  string `json:"name"`
	Trust string `json:"trust"` // private or external
	// Slots is the pool's maxConcurrency when the world starts.
	Slots int `json:"slots"`
	// Checkpoint makes a pause write the sandbox out and free its slot
	// (lifecycle.pause-checkpoint). Without it a paused sandbox keeps its slot.
	Checkpoint bool `json:"checkpoint"`
	// CostPerHour is the pool's cost model: one sandbox for an hour.
	CostPerHour float64 `json:"costPerHour"`
}

// operatorToken bootstraps the in-process installation. It guards nothing:
// the installation lives in this process and is reachable only from it.
const operatorToken = "dly_simulator_0123456789abcdefghjkmnpqrstvwxyz"

// Config describes the installation. It is fixed for a world's life.
type Config struct {
	Providers []Provider `json:"providers"`
	// Preemption is the preemption mode of private pools that checkpoint:
	// off, idle or lru.
	Preemption string `json:"preemption"`
	// How long a backend takes, in seconds. PauseSeconds is a checkpoint; a
	// pause that stays on the host takes a second.
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
	// Users is how many people (or agents) are active.
	Users int `json:"users"`
	// SessionsPerHour is how often an idle user starts working.
	SessionsPerHour float64 `json:"sessionsPerHour"`
	// SessionMinutes is the median session length (log-normal, sigma 0.9).
	SessionMinutes float64 `json:"sessionMinutes"`
	// Ephemeral makes every session a task: a new sandbox, deleted when the
	// session ends. Otherwise each user owns one long-lived computer that
	// pauses when idle and resumes at the next session.
	Ephemeral bool `json:"ephemeral"`
}

// Routing modes.
const (
	RoutePolicy = "policy" // creates name no pool; placement policy decides
	RouteSplit  = "split"  // each create is pinned to a pool drawn by share
)

// ProviderRouting is what an operator, or the calling application, can change
// about a pool while the world runs.
type ProviderRouting struct {
	Enabled bool `json:"enabled"`
	Slots   int  `json:"slots"`
	// Share is the pool's weight in split mode.
	Share float64 `json:"share"`
}

// Routing is where work may go. It can change while the world runs; every
// change goes through the admin API, as an operator's would.
type Routing struct {
	Mode string `json:"mode"`
	// Strategy and External are the global placement policy.
	Strategy  string                     `json:"strategy"`
	External  string                     `json:"external"`
	Providers map[string]ProviderRouting `json:"providers"`
}

// Box is one sandbox as the page draws it.
type Box struct {
	ID   string `json:"id"`
	User int    `json:"user"`
	// Status is the sandbox's status.
	Status string `json:"status"`
	Pool   string `json:"pool,omitempty"`
	// Want is the pool a queued create is pinned to, when it is pinned.
	Want          string `json:"want,omitempty"`
	Busy          bool   `json:"busy"`                   // its user is in a session
	Waiting       bool   `json:"waiting"`                // its user is waiting for it
	SlotReleased  bool   `json:"slotReleased,omitempty"` // paused without a slot
	ResumeQueued  bool   `json:"resumeQueued,omitempty"` // waiting for a slot to resume
	Reason        string `json:"reason,omitempty"`
	WaitedSeconds int    `json:"waitedSeconds,omitempty"` // how long its user has waited so far
}

// PoolView is one pool's state.
type PoolView struct {
	Name       string  `json:"name"`
	Trust      string  `json:"trust"`
	Slots      int     `json:"slots"` // 0: no limit
	Enabled    bool    `json:"enabled"`
	Healthy    bool    `json:"healthy"`
	Offline    bool    `json:"offline"` // its backend was taken offline
	Detail     string  `json:"detail,omitempty"`
	Checkpoint bool    `json:"checkpoint"`
	Cost       float64 `json:"costPerHour"`
	// Now.
	Held         int `json:"held"`         // sandboxes holding a slot
	Busy         int `json:"busy"`         // of those, in a session
	Checkpointed int `json:"checkpointed"` // paused, holding no slot
	Waiting      int `json:"waiting"`      // creates pinned here and resumes, waiting for a slot
	// Since the world started.
	Placed   int     `json:"placed"`   // creates placed here
	Sessions int     `json:"sessions"` // sessions served here
	Hours    float64 `json:"hours"`    // sandbox-hours holding a slot
}

// Event is one audit event, or one thing a user saw.
type Event struct {
	Seconds float64 `json:"t"` // since the world started
	Action  string  `json:"action"`
	Sandbox string  `json:"sandbox,omitempty"`
	Pool    string  `json:"pool,omitempty"`
	User    int     `json:"user"` // -1 when unknown
	Text    string  `json:"text"`
}

// Stats are totals since the world started.
type Stats struct {
	Sessions int `json:"sessions"`
	// Waits, in seconds, from a session's start until its sandbox answered.
	WaitP50 float64 `json:"waitP50"`
	WaitP95 float64 `json:"waitP95"`
	WaitMax float64 `json:"waitMax"`
	// PrivateSessions is the share of sessions served on private pools.
	PrivateSessions float64 `json:"privateSessions"`
	// ExternalHours is sandbox-hours run on external pools; Spend is every
	// pool's hours at its cost model.
	ExternalHours float64 `json:"externalHours"`
	Spend         float64 `json:"spend"`
	Placed        int     `json:"placed"`
	Refused       int     `json:"refused"`       // creates the control plane turned away
	Queued        int     `json:"queuedResumes"` // resumes that waited for a slot, or for a pause to finish
	Preemptions   int     `json:"preemptions"`
	Interruptions int     `json:"interruptions"` // sessions that lost their sandbox midway
	Retries       int     `json:"retries"`       // provisioning attempts that failed and were retried
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
	Queued  int   `json:"queued"` // creates no pool has taken yet
	Stats   Stats `json:"stats"`
	// Events since the previous snapshot.
	Events []Event `json:"events"`
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
	cfg      Config
	store    store.Store
	pools    *pools.Manager
	sched    *scheduler.Scheduler
	handler  http.Handler
	token    string
	backends map[string]*backend
	byName   map[string]Provider
	started  time.Time

	// applying serializes changes of routing; applied is what the admin API
	// was last told.
	applying sync.Mutex
	applied  Routing

	mu       sync.Mutex
	rng      *rand.Rand
	load     Load
	routing  Routing
	users    []*user
	byBox    map[string]int // sandbox ID -> user
	waits    []float64
	sessions int
	onPriv   int
	served   map[string]int     // sessions, by pool
	placed   map[string]int     // creates placed, by pool
	hours    map[string]float64 // sandbox-hours holding a slot, by pool
	events   []Event
	stats    Stats
	auditSeq int64
	lastTick time.Time
	ctx      context.Context
}

// DefaultProviders is a private cluster of eight slots and an external pool
// without a limit.
func DefaultProviders() []Provider {
	return []Provider{
		{Name: "own-cluster", Trust: api.TrustPrivate, Slots: 8, Checkpoint: true},
		{Name: "burst-provider", Trust: api.TrustExternal, CostPerHour: 0.08},
	}
}

func (c *Config) defaults() {
	if len(c.Providers) == 0 {
		c.Providers = DefaultProviders()
	}
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

// New builds the installation: its pools, every one enabled, a placement
// policy that keeps work private, and one tenant. SetRouting changes the rest.
func New(cfg Config) (*World, error) {
	cfg.defaults()
	ctx := context.Background()
	c := config.Default()
	c.RateLimit.RequestsPerSecond = 0
	c.Scheduler.IntervalMs = 1000 // requests wake the scheduler; the tick only serves timers
	c.Policy = api.PlacementPolicy{Scope: "global", External: api.ExternalNever}
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

	w := &World{
		cfg: cfg, store: st, pools: pm, backends: map[string]*backend{}, byName: map[string]Provider{},
		rng: rand.New(rand.NewPCG(cfg.Seed, 7)), byBox: map[string]int{},
		served: map[string]int{}, placed: map[string]int{}, hours: map[string]float64{},
		routing: Routing{Mode: RoutePolicy, Strategy: api.StrategyPrivateFirst, External: api.ExternalNever, Providers: map[string]ProviderRouting{}},
	}
	for _, p := range cfg.Providers {
		if _, dup := w.byName[p.Name]; dup || p.Name == "" {
			return nil, fmt.Errorf("provider %q: names must be set and differ", p.Name)
		}
		if p.Trust != api.TrustPrivate {
			p.Trust = api.TrustExternal
		}
		features := slices.Clone(fake.DefaultFeatures)
		pause := time.Second
		if p.Checkpoint {
			features = append(features, adapter.FeaturePauseCheckpoint)
			pause = seconds(cfg.PauseSeconds)
		}
		b := &backend{
			Adapter: fake.New(fake.Options{Trust: p.Trust, Features: features, Region: "sim"}),
			create:  seconds(cfg.CreateSeconds), pause: pause, resume: seconds(cfg.ResumeSeconds),
		}
		pool := api.Pool{
			Name: p.Name, Adapter: "fake", Trust: p.Trust, Enabled: true, Region: "sim", MaxConcurrency: p.Slots,
			Source: "config", CostModel: api.CostModel{SandboxHour: p.CostPerHour},
		}
		if p.Checkpoint && p.Trust == api.TrustPrivate && cfg.Preemption != api.PreemptOff {
			pool.Preemption = &api.Preemption{Mode: cfg.Preemption, IdleSeconds: 60, MinRunSeconds: 600}
		}
		if err := st.UpsertPool(ctx, pool); err != nil {
			return nil, err
		}
		pm.Set(pool, b)
		w.backends[p.Name], w.byName[p.Name] = b, p
		w.routing.Providers[p.Name] = ProviderRouting{Enabled: true, Slots: p.Slots, Share: 1}
	}
	w.applied = cloneRouting(w.routing)

	inst, err := st.Installation(ctx)
	if err != nil {
		return nil, err
	}
	w.sched = scheduler.New(st, pm, c, inst, rec, log)
	srv := server.New(c, st, pm, w.sched, rec, log, inst)
	srv.Secrets = box
	if err := os.Setenv(c.Auth.BootstrapTokenEnv, operatorToken); err != nil {
		return nil, err
	}
	if err := srv.Bootstrap(ctx); err != nil {
		return nil, err
	}
	pm.Refresh(ctx)
	w.handler = srv.Handler()

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
	w.skipAudit(ctx)
	return w, nil
}

// skipAudit moves the page's reading of the audit log past what is there now.
func (w *World) skipAudit(ctx context.Context) {
	if evs, err := w.store.ListAudit(ctx, store.AuditFilter{Desc: true, Limit: 1}); err == nil && len(evs) > 0 {
		w.mu.Lock()
		w.auditSeq = max(w.auditSeq, evs[0].Seq)
		w.mu.Unlock()
	}
}

func cloneRouting(r Routing) Routing {
	out := r
	out.Providers = make(map[string]ProviderRouting, len(r.Providers))
	for k, v := range r.Providers {
		out.Providers[k] = v
	}
	return out
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
	for _, u := range w.users {
		go w.run(ctx, u)
	}
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

// SetRouting changes where work may go, through the admin API: which pools
// are enabled and how many slots each has (PATCH /v1/admin/pools/{name}), and
// the global placement policy (PUT /v1/policies/global). Shares and the mode
// are the calling application's own: in split mode each create is pinned to a
// pool with placement.allowedPools, as the console's launcher does.
func (w *World) SetRouting(r Routing) error {
	w.applying.Lock()
	defer w.applying.Unlock()
	ctx := context.Background()
	if r.Mode != RouteSplit {
		r.Mode = RoutePolicy
	}
	if r.Strategy == "" {
		r.Strategy = api.StrategyPrivateFirst
	}
	if r.External == "" {
		r.External = api.ExternalNever
	}
	next := cloneRouting(r)
	var external []string
	for name, p := range w.byName {
		if p.Trust == api.TrustExternal {
			external = append(external, name)
		}
		want, ok := next.Providers[name]
		if !ok {
			want = w.applied.Providers[name]
		}
		want.Slots, want.Share = max(want.Slots, 0), max(want.Share, 0)
		next.Providers[name] = want
		if was := w.applied.Providers[name]; was.Enabled != want.Enabled || was.Slots != want.Slots {
			if code := w.call(ctx, "PATCH", "/v1/admin/pools/"+name, operatorToken, map[string]any{"enabled": want.Enabled, "maxConcurrency": want.Slots}, nil); code != http.StatusOK {
				return fmt.Errorf("update pool %s: status %d", name, code)
			}
		}
	}
	sort.Strings(external)
	if next.Strategy != w.applied.Strategy || next.External != w.applied.External || w.applied.Mode == "" {
		pol := api.PlacementPolicy{External: next.External, Strategy: next.Strategy}
		if next.External != api.ExternalNever {
			pol.ExternalPools = external
		}
		if code := w.call(ctx, "PUT", "/v1/policies/global", operatorToken, pol, nil); code != http.StatusOK {
			return fmt.Errorf("update policy: status %d", code)
		}
	}
	w.applied = cloneRouting(next)
	w.pools.Refresh(ctx)
	w.mu.Lock()
	w.routing = next
	unstarted := w.ctx == nil
	w.mu.Unlock()
	if unstarted {
		w.skipAudit(ctx) // the setup is not part of the story
	}
	w.sched.Wake()
	return nil
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

// SetOutage takes a pool's backend offline or brings it back: it refuses
// creates and fails its health check. Sandboxes already on it keep running.
func (w *World) SetOutage(pool string, down bool) {
	b, ok := w.backends[pool]
	if !ok {
		return
	}
	b.down.Store(down)
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
		length := max(seconds(median*math.Exp(0.9*w.rng.NormFloat64())), w.cfg.Heartbeat*3/2) // 30 s at least
		w.mu.Unlock()
		if start {
			w.session(ctx, u, length)
			w.mu.Lock()
			u.state = userIdle
			w.mu.Unlock()
		}
	}
}

// session is one stretch of work: reach the sandbox, keep it busy, leave.
func (w *World) session(ctx context.Context, u *user, length time.Duration) {
	began := time.Now()
	if !w.reach(ctx, u, began) {
		return
	}
	w.mu.Lock()
	w.sessions++
	w.waits = append(w.waits, time.Since(began).Seconds())
	if rec, err := w.store.GetSandbox(ctx, u.sandbox); err == nil {
		w.served[rec.Pool]++
		if w.byName[rec.Pool].Trust == api.TrustPrivate {
			w.onPriv++
		}
	}
	ephemeral := w.load.Ephemeral
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
		// The sandbox was taken away mid-session (preempted, or lost).
		w.mu.Lock()
		w.stats.Interruptions++
		w.note(u, "user.interrupted", "lost its sandbox in the middle of a session")
		w.mu.Unlock()
		if !w.reach(ctx, u, asked) {
			return
		}
	}
	if ephemeral {
		// A task's sandbox ends with the task.
		w.call(ctx, "DELETE", "/v1/sandboxes/"+u.sandbox, w.token, nil, nil)
		w.mu.Lock()
		u.sandbox = ""
		w.mu.Unlock()
	}
}

// pick draws the pool a new sandbox is pinned to in split mode: an enabled
// pool, by share. Empty in policy mode, or when no pool has a share.
func (w *World) pick() string {
	if w.routing.Mode != RouteSplit {
		return ""
	}
	names := make([]string, 0, len(w.routing.Providers))
	total := 0.0
	for name, p := range w.routing.Providers {
		if p.Enabled && p.Share > 0 {
			names = append(names, name)
			total += p.Share
		}
	}
	sort.Strings(names)
	at := w.rng.Float64() * total
	for _, name := range names {
		if at -= w.routing.Providers[name].Share; at < 0 {
			return name
		}
	}
	return ""
}

// reach creates the user's sandbox if there is none and calls it until it
// answers. It reports false when the create was refused, or the world ended.
func (w *World) reach(ctx context.Context, u *user, since time.Time) bool {
	w.mu.Lock()
	u.state, u.waitSince = userWaiting, since
	w.mu.Unlock()
	for ctx.Err() == nil {
		if u.sandbox == "" {
			w.mu.Lock()
			pin := w.pick()
			w.mu.Unlock()
			req := api.CreateSandboxRequest{
				Profile: "small", IdleTimeoutSeconds: w.cfg.IdleTimeoutSeconds,
				Labels: map[string]string{"user": fmt.Sprint(u.n)},
			}
			if pin != "" {
				req.Placement = &api.Placement{AllowedPools: []string{pin}}
			}
			var sb api.Sandbox
			code := w.call(ctx, "POST", "/v1/sandboxes", w.token, req, &sb)
			if code != http.StatusAccepted || sb.ID == "" {
				// Turned away: the audit log says why (sandbox.rejected).
				w.mu.Lock()
				w.stats.Refused++
				w.mu.Unlock()
				return false
			}
			w.mu.Lock()
			u.sandbox = sb.ID
			w.byBox[sb.ID] = u.n
			w.mu.Unlock()
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
				w.note(u, "user.replaced", "its sandbox is gone ("+string(sb.Status)+"); starting a new one")
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
	w.mu.Lock()
	after := w.auditSeq
	w.mu.Unlock()
	audit, _ := w.store.ListAudit(ctx, store.AuditFilter{AfterSeq: after})

	w.mu.Lock()
	defer w.mu.Unlock()
	now := time.Now()
	snap := Snapshot{Seconds: w.since(), Boxes: []Box{}, Events: []Event{}}
	views := map[string]*PoolView{}
	for _, v := range w.pools.Views(nil) {
		p := w.byName[v.Pool.Name]
		views[v.Pool.Name] = &PoolView{
			Name: v.Pool.Name, Trust: v.Pool.Trust, Slots: v.Pool.MaxConcurrency, Enabled: v.Pool.Enabled,
			Healthy: v.Healthy, Offline: w.backends[v.Pool.Name].down.Load(), Detail: v.HealthDetail,
			Checkpoint: p.Checkpoint, Cost: p.CostPerHour,
		}
	}

	dt := now.Sub(w.lastTick).Hours()
	w.lastTick = now
	for _, r := range recs {
		b := Box{
			ID: r.ID, User: -1, Status: string(r.Status), Pool: r.Pool, SlotReleased: r.SlotReleased,
			ResumeQueued: r.ResumeRequestedAt != nil, Reason: r.StatusReason,
		}
		if r.Status == api.StatusQueued && r.Placement != nil && len(r.Placement.AllowedPools) == 1 {
			b.Want = r.Placement.AllowedPools[0]
		}
		if n, ok := w.byBox[r.ID]; ok {
			u := w.users[n]
			mine := u.sandbox == r.ID
			b.User, b.Busy, b.Waiting = n, mine && u.state == userWorking, mine && u.state == userWaiting
			if b.Waiting {
				b.WaitedSeconds = int(now.Sub(u.waitSince).Seconds())
			}
		}
		snap.Boxes = append(snap.Boxes, b)
		v := views[r.Pool]
		switch {
		case r.Status == api.StatusQueued:
			if pinned := views[b.Want]; pinned != nil {
				pinned.Waiting++
			} else {
				snap.Queued++
			}
		case v == nil:
		case r.Status == api.StatusPaused && r.SlotReleased:
			v.Checkpointed++
			if b.ResumeQueued {
				v.Waiting++
			}
		default:
			v.Held++
			if b.Busy {
				v.Busy++
			}
			// Hours accrue between snapshots, which the page takes many
			// times a simulated minute.
			if r.Status != api.StatusPaused && r.Status != api.StatusStopped {
				w.hours[r.Pool] += dt
			}
		}
	}
	sort.Slice(snap.Boxes, func(i, j int) bool { return snap.Boxes[i].User < snap.Boxes[j].User })
	if len(w.byBox) > 4*len(recs)+2000 { // tasks come and go; forget the ones that are gone
		keep := make(map[string]int, len(recs))
		for _, r := range recs {
			if n, ok := w.byBox[r.ID]; ok {
				keep[r.ID] = n
			}
		}
		w.byBox = keep
	}

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
		pool := ev.Details["pool"]
		switch ev.Action {
		case "sandbox.placed":
			w.placed[pool]++
			w.stats.Placed++
		case "sandbox.resumed":
			if waited := ev.Details["waitedSeconds"]; waited != "" && waited != "0" {
				w.stats.Queued++
			}
		case "sandbox.preempted":
			w.stats.Preemptions++
		case "sandbox.provision_retry":
			w.stats.Retries++
		}
		if !ok {
			continue
		}
		n, known := w.byBox[ev.Resource]
		if !known {
			n = -1
		}
		w.events = append(w.events, Event{Seconds: ev.Time.Sub(w.started).Seconds(), Action: ev.Action, Sandbox: ev.Resource, Pool: pool, User: n, Text: text})
	}
	sort.SliceStable(w.events, func(i, j int) bool { return w.events[i].Seconds < w.events[j].Seconds })
	snap.Events, w.events = append(snap.Events, w.events...), nil

	s := w.stats
	for _, p := range w.cfg.Providers {
		v := views[p.Name]
		if v == nil {
			continue
		}
		v.Placed, v.Sessions, v.Hours = w.placed[p.Name], w.served[p.Name], w.hours[p.Name]
		s.Spend += v.Hours * p.CostPerHour
		if v.Trust == api.TrustExternal {
			s.ExternalHours += v.Hours
		}
		snap.Pools = append(snap.Pools, *v)
	}
	sorted := slices.Clone(w.waits)
	sort.Float64s(sorted)
	s.Sessions = w.sessions
	s.WaitP50, s.WaitP95, s.WaitMax = pct(sorted, 0.5), pct(sorted, 0.95), pct(sorted, 1)
	if w.sessions > 0 {
		s.PrivateSessions = float64(w.onPriv) / float64(w.sessions)
	}
	snap.Stats = s
	return snap
}

// describe turns an audit event into a line for the page; false for events
// that are not about scheduling (every exec is audited too).
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
		return "refused: " + d["reason"], true
	case "pool.update":
		return "operator changed pool " + ev.Resource, true
	case "policy.update":
		return "operator changed the placement policy", true
	}
	return "", false
}
