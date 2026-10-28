// Package pools builds execution adapters for registered pools and keeps a
// cached view of their health, capabilities and capacity for placement.
package pools

import (
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"sync"
	"time"

	"github.com/different-ai/differently/internal/policy"
	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// Deps are shared resources adapter factories may need.
type Deps struct {
	Store   store.Store
	Dev     bool
	DataDir string
	Logger  *slog.Logger
	// Resolve returns the secret behind a pool's credential reference.
	Resolve func(ref string) (string, error)
}

// Factory builds an adapter for a pool.
type Factory func(p api.Pool, deps Deps) (adapter.ExecutionAdapter, error)

var (
	factoriesMu sync.RWMutex
	factories   = map[string]Factory{}
)

// Register makes an adapter kind available. Built-in adapters register from
// the server's wiring; plugins can register at init.
func Register(kind string, f Factory) {
	factoriesMu.Lock()
	defer factoriesMu.Unlock()
	factories[kind] = f
}

// Kinds lists registered adapter kinds.
func Kinds() []string {
	factoriesMu.RLock()
	defer factoriesMu.RUnlock()
	var out []string
	for k := range factories {
		out = append(out, k)
	}
	return out
}

type entry struct {
	pool      api.Pool
	adapter   adapter.ExecutionAdapter
	caps      adapter.Capabilities
	capacity  adapter.Capacity
	healthy   bool
	detail    string
	checkedAt time.Time
	buildErr  error
}

// Manager owns one adapter per pool.
type Manager struct {
	deps    Deps
	mu      sync.RWMutex
	entries map[string]*entry
}

// NewManager returns an empty manager.
func NewManager(deps Deps) *Manager {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Manager{deps: deps, entries: map[string]*entry{}}
}

// Set installs a pre-built adapter (tests, embedding).
func (m *Manager) Set(p api.Pool, a adapter.ExecutionAdapter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[p.Name] = &entry{pool: p, adapter: a}
}

// Sync builds adapters for new or changed pools and drops removed ones.
func (m *Manager) Sync(pools []api.Pool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]bool{}
	for _, p := range pools {
		seen[p.Name] = true
		old, ok := m.entries[p.Name]
		if ok && old.adapter != nil && sameBuild(old.pool, p) {
			old.pool = p
			continue
		}
		if ok && old.adapter != nil && old.buildErr == nil && factoryFor(p.Adapter) == nil {
			old.pool = p // pre-built via Set
			continue
		}
		e := &entry{pool: p}
		if f := factoryFor(p.Adapter); f == nil {
			e.buildErr = fmt.Errorf("adapter kind %q is not available in this build", p.Adapter)
		} else if a, err := f(p, m.deps); err != nil {
			e.buildErr = err
		} else {
			e.adapter = a
		}
		if e.buildErr != nil {
			e.detail = e.buildErr.Error()
			m.deps.Logger.Warn("pool adapter unavailable", "pool", p.Name, "err", e.buildErr)
		}
		m.entries[p.Name] = e
	}
	for name := range m.entries {
		if !seen[name] {
			delete(m.entries, name)
		}
	}
}

func sameBuild(a, b api.Pool) bool {
	return a.Adapter == b.Adapter && a.CredentialRef == b.CredentialRef && reflect.DeepEqual(a.Config, b.Config)
}

func factoryFor(kind string) Factory {
	factoriesMu.RLock()
	defer factoriesMu.RUnlock()
	return factories[kind]
}

// Adapter returns the adapter for a pool.
func (m *Manager) Adapter(name string) (adapter.ExecutionAdapter, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.entries[name]
	switch {
	case !ok:
		return nil, api.Errorf(api.CodeNotFound, "pool %s is not registered", name)
	case e.adapter == nil:
		return nil, api.Errorf(api.CodeProviderUnavailable, "pool %s: %v", name, e.buildErr)
	}
	return e.adapter, nil
}

// Refresh probes every pool's health, capabilities and capacity.
func (m *Manager) Refresh(ctx context.Context) {
	m.mu.RLock()
	list := make([]*entry, 0, len(m.entries))
	for _, e := range m.entries {
		list = append(list, e)
	}
	m.mu.RUnlock()
	var wg sync.WaitGroup
	for _, e := range list {
		if e.adapter == nil {
			continue
		}
		wg.Add(1)
		go func(e *entry) {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			healthy, detail := true, ""
			caps, err := e.adapter.Capabilities(pctx)
			if err == nil {
				caps.Features = adapter.EffectiveFeatures(e.adapter, caps.Features)
				err = e.adapter.Health(pctx)
			}
			var capacity adapter.Capacity
			if err == nil {
				capacity, err = e.adapter.Capacity(pctx)
			}
			if err != nil {
				healthy, detail = false, err.Error()
			}
			m.mu.Lock()
			if err == nil {
				e.caps, e.capacity = caps, capacity
			}
			e.healthy, e.detail, e.checkedAt = healthy, detail, time.Now().UTC()
			m.mu.Unlock()
		}(e)
	}
	wg.Wait()
}

// Supports reports whether a pool advertises feature (declared by the
// adapter and backed by an implemented interface).
func (m *Manager) Supports(ctx context.Context, name, feature string) bool {
	m.mu.RLock()
	e, ok := m.entries[name]
	m.mu.RUnlock()
	if !ok || e.adapter == nil {
		return false
	}
	caps := e.caps
	if caps.Features == nil {
		c, err := e.adapter.Capabilities(ctx)
		if err != nil {
			return false
		}
		c.Features = adapter.EffectiveFeatures(e.adapter, c.Features)
		caps = c
	}
	return caps.Has(feature)
}

// MarkUnhealthy records a provisioning failure until the next good probe.
func (m *Manager) MarkUnhealthy(name string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[name]; ok {
		e.healthy, e.detail = false, err.Error()
	}
}

// Views returns placement inputs for every pool.
func (m *Manager) Views(active map[string]int) []policy.PoolView {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]policy.PoolView, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, policy.PoolView{
			Pool: e.pool, Capabilities: e.caps, Capacity: e.capacity,
			Healthy: e.healthy && e.adapter != nil, HealthDetail: e.detail, Active: active[e.pool.Name],
		})
	}
	return out
}

// Decorate fills runtime fields (health, capacity, capabilities) on a pool.
func (m *Manager) Decorate(p *api.Pool, active int) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p.Active = active
	e, ok := m.entries[p.Name]
	if !ok {
		return
	}
	status := "unknown"
	switch {
	case e.adapter == nil:
		status = "unavailable"
	case !e.checkedAt.IsZero() && e.healthy:
		status = "healthy"
	case !e.checkedAt.IsZero():
		status = "unavailable"
	}
	p.Health = &api.PoolHealth{Status: status, Message: e.detail, CheckedAt: e.checkedAt}
	p.Capabilities = e.caps.Features
	c := e.capacity
	p.Capacity = &api.PoolCapacity{Bounded: c.Bounded, TotalCPU: c.TotalCPU, FreeCPU: c.FreeCPU, TotalMemoryMiB: c.TotalMemoryMiB, FreeMemoryMiB: c.FreeMemoryMiB, Nodes: c.Nodes}
}

// Endpoints lists the external endpoints configured adapters reach, for the
// trust report.
func (m *Manager) Endpoints() map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[string]string{}
	for name, e := range m.entries {
		if ep, ok := e.adapter.(interface{ Endpoint() string }); ok {
			out[name] = ep.Endpoint()
		}
	}
	return out
}
