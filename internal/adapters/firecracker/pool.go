// Package firecracker is the control-plane side of the direct Firecracker
// backend. A pool is the set of node agents (differently-agent) that
// registered under the pool's name; each agent manages microVMs on one KVM
// host. The pool places a sandbox on the agent with the most free memory that
// fits it and addresses instances as "<agent-id>:<vm-ref>".
package firecracker

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/adapter/adapterhttp"
	"github.com/different-ai/differently/pkg/api"
)

// PartitionGrace is how long a silent agent is presumed partitioned rather
// than lost.
const PartitionGrace = 10 * time.Minute

// Pool aggregates node agents. Each agent is called with its own secret
// (store.AgentRecord.Secret), never a token shared across nodes.
type Pool struct {
	Name    string
	Store   store.Store
	Timeout time.Duration

	mu      sync.Mutex
	clients map[string]*adapterhttp.Client
}

// New returns a pool backed by agents registered in st.
func New(name string, st store.Store, timeout time.Duration) *Pool {
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &Pool{Name: name, Store: st, Timeout: timeout, clients: map[string]*adapterhttp.Client{}}
}

func (p *Pool) agents(ctx context.Context) ([]store.AgentRecord, error) {
	all, err := p.Store.ListAgents(ctx, p.Name)
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-p.Timeout)
	var out []store.AgentRecord
	for _, a := range all {
		if a.LastSeen.After(cutoff) {
			out = append(out, a)
		}
	}
	return out, nil
}

func (p *Pool) client(a store.AgentRecord) *adapterhttp.Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.clients[a.ID]
	if !ok || c.BaseURL != strings.TrimRight(a.Endpoint, "/") || c.Token != a.Secret {
		c = adapterhttp.NewClient(a.Endpoint, a.Secret)
		p.clients[a.ID] = c
	}
	return c
}

func (p *Pool) route(ctx context.Context, ref string) (*adapterhttp.Client, string, error) {
	agentID, vm, ok := strings.Cut(ref, ":")
	if !ok {
		return nil, "", adapter.NotFound(ref)
	}
	agents, err := p.Store.ListAgents(ctx, p.Name)
	if err != nil {
		return nil, "", err
	}
	for _, a := range agents {
		if a.ID == agentID {
			return p.client(a), vm, nil
		}
	}
	return nil, "", adapter.Unavailable(fmt.Errorf("agent %s is not registered", agentID))
}

func (p *Pool) Info() adapter.Info {
	return adapter.Info{Kind: "firecracker", Version: "1", Trust: api.TrustPrivate, Isolation: "firecracker microVM (KVM)", Protocol: adapter.ProtocolVersion}
}

// Capabilities returns the features every live agent supports and the union
// of their templates.
func (p *Pool) Capabilities(ctx context.Context) (adapter.Capabilities, error) {
	agents, err := p.agents(ctx)
	if err != nil {
		return adapter.Capabilities{}, err
	}
	var caps adapter.Capabilities
	for i, a := range agents {
		if i == 0 {
			caps.Features = slices.Clone(a.Capabilities)
		} else {
			caps.Features = slices.DeleteFunc(caps.Features, func(f string) bool { return !slices.Contains(a.Capabilities, f) })
		}
		for _, t := range a.Templates {
			if !slices.Contains(caps.Templates, t) {
				caps.Templates = append(caps.Templates, t)
			}
		}
		caps.MaxCPU = max(caps.MaxCPU, a.TotalCPU)
		caps.MaxMemoryMiB = max(caps.MaxMemoryMiB, a.TotalMemoryMiB)
	}
	sort.Strings(caps.Templates)
	return caps, nil
}

func (p *Pool) Capacity(ctx context.Context) (adapter.Capacity, error) {
	agents, err := p.agents(ctx)
	if err != nil {
		return adapter.Capacity{}, err
	}
	c := adapter.Capacity{Bounded: true, Nodes: len(agents)}
	for _, a := range agents {
		c.TotalCPU += a.TotalCPU
		c.FreeCPU += a.FreeCPU
		c.TotalMemoryMiB += a.TotalMemoryMiB
		c.FreeMemoryMiB += a.FreeMemoryMiB
		if a.FreeMemoryMiB > c.LargestFreeMemoryMiB {
			c.LargestFreeMemoryMiB, c.LargestFreeCPU = a.FreeMemoryMiB, a.FreeCPU
		}
	}
	return c, nil
}

func (p *Pool) Health(ctx context.Context) error {
	agents, err := p.agents(ctx)
	if err != nil {
		return err
	}
	if len(agents) == 0 {
		return errors.New("no node agents have reported in; check the differently-agent DaemonSet and /dev/kvm on nodes")
	}
	return nil
}

func (p *Pool) Create(ctx context.Context, req adapter.CreateRequest) (adapter.Instance, error) {
	agents, err := p.agents(ctx)
	if err != nil {
		return adapter.Instance{}, err
	}
	// Idempotency: an agent may already hold this sandbox from an earlier
	// attempt whose response was lost.
	for _, a := range agents {
		if inst, err := p.client(a).Get(ctx, req.ID); err == nil {
			inst.Ref = a.ID + ":" + inst.Ref
			return inst, nil
		}
	}
	// Snapshots live on one agent's disk, so a restore must land there.
	pinned := ""
	if req.FromSnapshot != "" {
		var snap string
		pinned, snap, _ = strings.Cut(req.FromSnapshot, ":")
		req.FromSnapshot = snap
	}
	var fits []store.AgentRecord
	for _, a := range agents {
		if pinned != "" && a.ID != pinned {
			continue
		}
		if a.FreeCPU >= req.Resources.CPU && a.FreeMemoryMiB >= req.Resources.MemoryMiB &&
			(req.Template == "" || len(a.Templates) == 0 || slices.Contains(a.Templates, req.Template)) {
			fits = append(fits, a)
		}
	}
	if len(fits) == 0 {
		return adapter.Instance{}, adapter.InsufficientCapacity("no node agent in pool " + p.Name + " can fit the sandbox")
	}
	sort.Slice(fits, func(i, j int) bool {
		if fits[i].FreeMemoryMiB != fits[j].FreeMemoryMiB {
			return fits[i].FreeMemoryMiB > fits[j].FreeMemoryMiB
		}
		return fits[i].ID < fits[j].ID
	})
	var lastErr error
	for _, a := range fits {
		inst, err := p.client(a).Create(ctx, req)
		if err == nil {
			inst.Ref = a.ID + ":" + inst.Ref
			return inst, nil
		}
		lastErr = err
		if !api.IsCode(err, adapter.CodeInsufficientCapacity) {
			break
		}
	}
	return adapter.Instance{}, lastErr
}

func (p *Pool) Get(ctx context.Context, ref string) (adapter.Instance, error) {
	c, vm, err := p.route(ctx, ref)
	if err != nil {
		return adapter.Instance{}, err
	}
	inst, err := c.Get(ctx, vm)
	inst.Ref = ref
	return inst, err
}

func (p *Pool) List(ctx context.Context, installation string) ([]adapter.Instance, error) {
	// An agent that recently stopped heartbeating may only be partitioned.
	// Refuse to report a partial view so the reconciler neither fails its
	// sandboxes nor later deletes its VMs as orphans; after PartitionGrace
	// the agent is treated as gone.
	all, err := p.Store.ListAgents(ctx, p.Name)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for _, a := range all {
		if age := now.Sub(a.LastSeen); age > p.Timeout && age < PartitionGrace {
			return nil, adapter.Unavailable(fmt.Errorf("agent %s missed heartbeats for %s; reconciliation of pool %s is paused", a.ID, age.Round(time.Second), p.Name))
		}
	}
	agents, err := p.agents(ctx)
	if err != nil {
		return nil, err
	}
	var out []adapter.Instance
	for _, a := range agents {
		items, err := p.client(a).List(ctx, installation)
		if err != nil {
			return nil, err // a partial list would make the reconciler think VMs vanished
		}
		for _, it := range items {
			it.Ref = a.ID + ":" + it.Ref
			out = append(out, it)
		}
	}
	return out, nil
}

func (p *Pool) Delete(ctx context.Context, ref string) error {
	c, vm, err := p.route(ctx, ref)
	if api.IsCode(err, api.CodeNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return c.Delete(ctx, vm)
}

func (p *Pool) Exec(ctx context.Context, ref string, req api.ExecRequest) (api.ExecResult, error) {
	c, vm, err := p.route(ctx, ref)
	if err != nil {
		return api.ExecResult{}, err
	}
	return c.Exec(ctx, vm, req)
}

func (p *Pool) ExecStream(ctx context.Context, ref string, req api.ExecRequest, emit func(api.ExecEvent) error) (int, error) {
	c, vm, err := p.route(ctx, ref)
	if err != nil {
		return -1, err
	}
	return c.ExecStream(ctx, vm, req, emit)
}

func (p *Pool) ReadFile(ctx context.Context, ref, path string) ([]byte, error) {
	c, vm, err := p.route(ctx, ref)
	if err != nil {
		return nil, err
	}
	return c.ReadFile(ctx, vm, path)
}

func (p *Pool) WriteFile(ctx context.Context, ref, path string, data []byte) error {
	c, vm, err := p.route(ctx, ref)
	if err != nil {
		return err
	}
	return c.WriteFile(ctx, vm, path, data)
}

func (p *Pool) ListFiles(ctx context.Context, ref, path string) ([]api.FileInfo, error) {
	c, vm, err := p.route(ctx, ref)
	if err != nil {
		return nil, err
	}
	return c.ListFiles(ctx, vm, path)
}

func (p *Pool) call(ctx context.Context, ref string, fn func(*adapterhttp.Client, string) error) error {
	c, vm, err := p.route(ctx, ref)
	if err != nil {
		return err
	}
	return fn(c, vm)
}

func (p *Pool) Stop(ctx context.Context, ref string) error {
	return p.call(ctx, ref, func(c *adapterhttp.Client, vm string) error { return c.Stop(ctx, vm) })
}

func (p *Pool) Start(ctx context.Context, ref string) error {
	return p.call(ctx, ref, func(c *adapterhttp.Client, vm string) error { return c.Start(ctx, vm) })
}

func (p *Pool) Pause(ctx context.Context, ref string) error {
	return p.call(ctx, ref, func(c *adapterhttp.Client, vm string) error { return c.Pause(ctx, vm) })
}

func (p *Pool) Resume(ctx context.Context, ref string) error {
	return p.call(ctx, ref, func(c *adapterhttp.Client, vm string) error { return c.Resume(ctx, vm) })
}

// Snapshot captures on the VM's agent. Snapshot refs are agent-scoped
// ("<agent-id>:<snap>"); restoring places the new VM on the same agent.
func (p *Pool) Snapshot(ctx context.Context, ref, kind string) (adapter.Snapshot, error) {
	agentID, _, _ := strings.Cut(ref, ":")
	c, vm, err := p.route(ctx, ref)
	if err != nil {
		return adapter.Snapshot{}, err
	}
	snap, err := c.Snapshot(ctx, vm, kind)
	snap.Ref = agentID + ":" + snap.Ref
	return snap, err
}

func (p *Pool) DeleteSnapshot(ctx context.Context, ref string) error {
	c, snap, err := p.route(ctx, ref)
	if err != nil {
		return err
	}
	return c.DeleteSnapshot(ctx, snap)
}

var (
	_ adapter.ExecutionAdapter = (*Pool)(nil)
	_ adapter.Streamer         = (*Pool)(nil)
	_ adapter.FileSystem       = (*Pool)(nil)
	_ adapter.Lifecycle        = (*Pool)(nil)
	_ adapter.Pauser           = (*Pool)(nil)
	_ adapter.Snapshotter      = (*Pool)(nil)
)
