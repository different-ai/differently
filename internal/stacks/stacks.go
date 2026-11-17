// Package stacks watches application deployments: it probes the health and
// readiness routes of each service of a registered stack and keeps the last
// answer. It only reads; nothing here deploys or changes a service.
package stacks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/different-ai/differently/internal/config"
	"github.com/different-ai/differently/internal/metrics"
	"github.com/different-ai/differently/pkg/api"
)

// MaxComponents bounds one stack.
const MaxComponents = 32

var (
	name = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

	// openWork holds the health and readiness routes of OpenWork's services.
	openWork = map[string][2]string{
		"den-api":         {"/health", "/ready"},
		"den-web":         {"/api/health", "/api/ready"},
		"gateway":         {"/health", "/ready"},
		"den-gateway":     {"/__gw/health", "/__gw/ready"},
		"workbot":         {"/healthz", ""},
		"headless-runner": {"/health", ""},
	}
)

// ValidName reports whether s can name a stack or a component.
func ValidName(s string) bool { return name.MatchString(s) }

// Normalize validates a stack's kind and components and fills the defaults,
// so what is stored is exactly what gets probed.
func Normalize(kind string, in []api.StackComponent) (string, []api.StackComponent, error) {
	if kind == "" {
		kind = api.StackCustom
	}
	if kind != api.StackCustom && kind != api.StackOpenWork {
		return "", nil, api.Errorf(api.CodeInvalidRequest, "kind is %q; use %q or %q", kind, api.StackOpenWork, api.StackCustom)
	}
	switch {
	case len(in) == 0:
		return "", nil, api.Errorf(api.CodeInvalidRequest, "a stack needs at least one component")
	case len(in) > MaxComponents:
		return "", nil, api.Errorf(api.CodeInvalidRequest, "a stack holds at most %d components", MaxComponents)
	}
	out := make([]api.StackComponent, 0, len(in))
	seen := map[string]bool{}
	for _, c := range in {
		c.Health = nil
		if c.Name == "" {
			c.Name = c.Role
		}
		if !ValidName(c.Name) {
			return "", nil, api.Errorf(api.CodeInvalidRequest, "component name %q: use lowercase letters, digits, '.', '_' or '-', up to 63 characters", c.Name)
		}
		if seen[c.Name] {
			return "", nil, api.Errorf(api.CodeInvalidRequest, "component %s is listed twice", c.Name)
		}
		seen[c.Name] = true
		u, err := url.Parse(c.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return "", nil, api.Errorf(api.CodeInvalidRequest, "component %s: url must be an http or https base URL without credentials or a query", c.Name)
		}
		c.URL = strings.TrimRight(c.URL, "/")
		if kind == api.StackOpenWork && c.HealthPath == "" {
			if paths, ok := openWork[c.Role]; ok {
				c.HealthPath = paths[0]
				if c.ReadyPath == "" {
					c.ReadyPath = paths[1]
				}
			}
		}
		if c.HealthPath == "" {
			c.HealthPath = "/health"
		}
		for _, p := range []string{c.HealthPath, c.ReadyPath} {
			if p != "" && !strings.HasPrefix(p, "/") {
				return "", nil, api.Errorf(api.CodeInvalidRequest, "component %s: path %q must start with /", c.Name, p)
			}
		}
		if c.CredentialRef != "" {
			if ref := config.ParseCredentialRef(c.CredentialRef); ref.Env == "" && ref.File == "" && ref.Secret == "" {
				return "", nil, api.Errorf(api.CodeInvalidRequest, "component %s: credentialRef must be env:NAME, file:/path or secret:name", c.Name)
			}
		}
		out = append(out, c)
	}
	return kind, out, nil
}

// Lister is the part of the store the manager reads.
type Lister interface {
	ListStacks(ctx context.Context) ([]api.Stack, error)
	TryLeader(ctx context.Context) (bool, error)
}

// Recorder records audit events.
type Recorder interface {
	Record(ctx context.Context, ev api.AuditEvent)
}

// Deps are a Manager's collaborators.
type Deps struct {
	Store Lister
	// Resolve turns a credential reference into its value.
	Resolve func(ref string) (string, error)
	Audit   Recorder
	Log     *slog.Logger
	// Interval is how often every stack is probed. Default 10 s.
	Interval time.Duration
	// Timeout bounds one request of a probe. Default 5 s.
	Timeout time.Duration
	Client  *http.Client
}

// Manager probes registered stacks and holds the last result of each
// component in memory, per replica, as the pool manager does for pools.
type Manager struct {
	deps Deps
	mu   sync.RWMutex
	seen map[string]*observed // by stack/component
}

type observed struct {
	target string // what was probed; a changed component starts over
	health api.ComponentHealth
	streak int    // consecutive probes with health.Status
	logged string // the status last written to the audit log
}

// New builds a manager.
func New(d Deps) *Manager {
	if d.Interval <= 0 {
		d.Interval = 10 * time.Second
	}
	if d.Timeout <= 0 {
		d.Timeout = 5 * time.Second
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Client == nil {
		// A health route that redirects is not answering for itself, and
		// following it could carry the probe's token to another host.
		d.Client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return &Manager{deps: d, seen: map[string]*observed{}}
}

// Run probes every stack until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(m.deps.Interval)
	defer t.Stop()
	for {
		m.Refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Refresh probes every registered stack once and forgets removed ones.
func (m *Manager) Refresh(ctx context.Context) {
	list, err := m.deps.Store.ListStacks(ctx)
	if err != nil {
		if ctx.Err() == nil {
			m.deps.Log.Warn("listing stacks failed", "err", err)
		}
		return
	}
	keep := map[string]bool{}
	var wg sync.WaitGroup
	for _, s := range list {
		for _, c := range s.Components {
			keep[key(s.Name, c.Name)] = true
		}
		wg.Add(1)
		go func(s api.Stack) {
			defer wg.Done()
			m.Check(ctx, s)
		}(s)
	}
	wg.Wait()
	m.mu.Lock()
	for k := range m.seen {
		if !keep[k] {
			delete(m.seen, k)
			stack, component, _ := strings.Cut(k, "/")
			metrics.StackComponentUp.DeleteLabelValues(stack, component)
		}
	}
	m.mu.Unlock()
}

// Check probes one stack's components now, side by side.
func (m *Manager) Check(ctx context.Context, s api.Stack) {
	var wg sync.WaitGroup
	for _, c := range s.Components {
		wg.Add(1)
		go func(c api.StackComponent) {
			defer wg.Done()
			h := m.probe(ctx, c)
			if ctx.Err() != nil {
				return // shutting down, or the caller left: not the service's fault
			}
			m.record(ctx, s.Name, c, h)
		}(c)
	}
	wg.Wait()
}

// record stores a probe result. A status that has held for two probes in a
// row is written to the audit log by the leader, so one dropped request does
// not fill the log and each change is recorded once across replicas.
func (m *Manager) record(ctx context.Context, stack string, c api.StackComponent, h api.ComponentHealth) {
	k := key(stack, c.Name)
	m.mu.Lock()
	o := m.seen[k]
	if o == nil || o.target != target(c) {
		o = &observed{target: target(c)}
		m.seen[k] = o
	}
	if o.health.Status == h.Status {
		h.Since = o.health.Since
		o.streak++
	} else {
		h.Since = h.CheckedAt
		o.streak = 1
	}
	o.health = h
	var from string
	changed := o.streak >= 2 && o.logged != h.Status
	if changed {
		from, o.logged = o.logged, h.Status
	}
	m.mu.Unlock()

	up := 0.0
	if h.Status == api.HealthHealthy {
		up = 1
	}
	metrics.StackComponentUp.WithLabelValues(stack, c.Name).Set(up)

	// The first settled status is the baseline; only a bad one is news.
	if !changed || (from == "" && h.Status == api.HealthHealthy) || m.deps.Audit == nil {
		return
	}
	if leader, err := m.deps.Store.TryLeader(ctx); err != nil || !leader {
		return
	}
	outcome := "success"
	if h.Status != api.HealthHealthy {
		outcome = "failure"
	}
	details := map[string]string{"stack": stack, "component": c.Name, "to": h.Status}
	if from != "" {
		details["from"] = from
	}
	if h.Message != "" {
		details["message"] = h.Message
	}
	m.deps.Audit.Record(ctx, api.AuditEvent{Actor: "stacks", Action: "stack.health", Resource: k, Outcome: outcome, Details: details})
}

// probe asks a component's health route, then its readiness route.
func (m *Manager) probe(ctx context.Context, c api.StackComponent) api.ComponentHealth {
	start := time.Now()
	h := api.ComponentHealth{Status: api.HealthHealthy}
	done := func() api.ComponentHealth {
		h.LatencyMs = time.Since(start).Milliseconds()
		h.CheckedAt = time.Now().UTC()
		return h
	}
	token := ""
	if c.CredentialRef != "" {
		if m.deps.Resolve == nil {
			h.Status, h.Message = api.HealthUnknown, "this server cannot resolve credential references"
			return done()
		}
		v, err := m.deps.Resolve(c.CredentialRef)
		if err != nil {
			h.Status, h.Message = api.HealthUnknown, "the probe's credential could not be read: "+err.Error()
			return done()
		}
		token = v
	}
	status, body, err := m.get(ctx, c.URL+c.HealthPath, token)
	switch {
	case err != nil:
		h.Status, h.Message = api.HealthUnavailable, err.Error()
		return done()
	case status < 200 || status > 299:
		h.Status, h.Message = api.HealthUnavailable, fmt.Sprintf("GET %s answered %d", c.HealthPath, status)
		return done()
	}
	h.Version = reportedVersion(body)
	if c.ReadyPath != "" {
		status, _, err := m.get(ctx, c.URL+c.ReadyPath, token)
		switch {
		case err != nil:
			h.Status, h.Message = api.HealthDegraded, "up, but the readiness check failed: "+err.Error()
		case status < 200 || status > 299:
			h.Status, h.Message = api.HealthDegraded, fmt.Sprintf("up, but not ready: GET %s answered %d", c.ReadyPath, status)
		}
	}
	return done()
}

func (m *Manager) get(ctx context.Context, target, token string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, m.deps.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "differently-stack-probe")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := m.deps.Client.Do(req)
	if err != nil {
		if ue, ok := err.(*url.Error); ok {
			err = ue.Err // the URL is already shown beside the message
		}
		return 0, nil, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	return res.StatusCode, body, nil
}

// reportedVersion reads the version a health route states about itself.
func reportedVersion(body []byte) string {
	var doc map[string]any
	if json.Unmarshal(body, &doc) != nil {
		return ""
	}
	for _, k := range []string{"version", "build"} {
		if v, ok := doc[k].(string); ok && v != "" && len(v) <= 128 {
			return v
		}
	}
	return ""
}

// Decorate fills the health of a stack and its components from the last
// probes of this replica.
func (m *Manager) Decorate(s *api.Stack) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	overall := api.StackHealth{Status: api.HealthHealthy}
	var bad []string
	for i := range s.Components {
		c := &s.Components[i]
		h := api.ComponentHealth{Status: api.HealthUnknown}
		if o := m.seen[key(s.Name, c.Name)]; o != nil && o.target == target(*c) {
			h = o.health
		}
		c.Health = &h
		if h.CheckedAt.After(overall.CheckedAt) {
			overall.CheckedAt = h.CheckedAt
		}
		if c.Optional {
			continue
		}
		if h.Status != api.HealthHealthy {
			bad = append(bad, c.Name+" is "+h.Status)
		}
		if rank(h.Status) > rank(overall.Status) {
			overall.Status = h.Status
		}
	}
	overall.Message = strings.Join(bad, ", ")
	s.Health = &overall
}

// rank orders statuses from best to worst.
func rank(status string) int {
	switch status {
	case api.HealthHealthy:
		return 0
	case api.HealthUnknown:
		return 1
	case api.HealthDegraded:
		return 2
	}
	return 3
}

func key(stack, component string) string { return stack + "/" + component }

func target(c api.StackComponent) string {
	return strings.Join([]string{c.URL, c.HealthPath, c.ReadyPath, c.CredentialRef}, "\n")
}
