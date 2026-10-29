// Package fake is an in-memory execution adapter for tests and demos. It
// implements every optional interface and supports failure injection.
package fake

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/different-ai/differently/internal/id"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// Options configures a fake adapter.
type Options struct {
	Trust    string
	Features []string
	// Capacity bounds total CPU and memory when non-zero.
	TotalCPU       int
	TotalMemoryMiB int
	Region         string
	// ExecFunc overrides command results.
	ExecFunc func(req api.ExecRequest) api.ExecResult
}

// DefaultFeatures is everything the fake implements.
var DefaultFeatures = []string{
	adapter.FeatureExec, adapter.FeatureExecStream, adapter.FeatureFiles, adapter.FeatureStopStart,
	adapter.FeaturePersistDisk, adapter.FeaturePauseResume, adapter.FeatureSnapshotDisk,
	adapter.FeatureSnapshotMem, adapter.FeatureFork, adapter.FeaturePorts, adapter.FeatureMicroVM,
}

type instance struct {
	adapter.Instance
	installation string
	template     string
	expiry       time.Time
	resources    api.Resources
	files        map[string][]byte
}

// Adapter is the fake backend.
type Adapter struct {
	opts Options

	mu        sync.Mutex
	instances map[string]*instance // by ref
	byID      map[string]string    // sandbox ID -> ref
	snapshots map[string]map[string][]byte
	// FailCreates makes the next N Create calls fail, with FailWith or, when
	// that is nil, as unavailable.
	FailCreates int
	FailWith    error
	// HangCreates makes the next N Create calls block until their context
	// ends, like a backend that never answers.
	HangCreates int
	creates     int
}

// New returns a fake adapter.
func New(opts Options) *Adapter {
	if opts.Trust == "" {
		opts.Trust = api.TrustPrivate
	}
	if opts.Features == nil {
		opts.Features = DefaultFeatures
	}
	return &Adapter{opts: opts, instances: map[string]*instance{}, byID: map[string]string{}, snapshots: map[string]map[string][]byte{}}
}

func (a *Adapter) Info() adapter.Info {
	return adapter.Info{Kind: "fake", Version: "1", Trust: a.opts.Trust, Isolation: "none (simulated)", Protocol: adapter.ProtocolVersion}
}

func (a *Adapter) Capabilities(context.Context) (adapter.Capabilities, error) {
	return adapter.Capabilities{Features: a.opts.Features, MaxCPU: a.opts.TotalCPU, MaxMemoryMiB: a.opts.TotalMemoryMiB}, nil
}

func (a *Adapter) Capacity(context.Context) (adapter.Capacity, error) {
	if a.opts.TotalCPU == 0 {
		return adapter.Capacity{}, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	c := adapter.Capacity{Bounded: true, TotalCPU: a.opts.TotalCPU, TotalMemoryMiB: a.opts.TotalMemoryMiB, Nodes: 1}
	c.FreeCPU, c.FreeMemoryMiB = c.TotalCPU, c.TotalMemoryMiB
	for _, in := range a.instances {
		if in.State == adapter.StateRunning || in.State == adapter.StatePaused {
			c.FreeCPU -= in.resources.CPU
			c.FreeMemoryMiB -= in.resources.MemoryMiB
		}
	}
	return c, nil
}

func (a *Adapter) Health(context.Context) error { return nil }

// Count returns how many instances exist.
func (a *Adapter) Count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.instances)
}

// SetExpiry records the backend deadline the control plane asked for.
func (a *Adapter) SetExpiry(_ context.Context, ref string, at time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	in, err := a.get(ref)
	if err != nil {
		return err
	}
	in.expiry = at
	return nil
}

// ExpiryOf returns the backend deadline last set for the sandbox's instance.
func (a *Adapter) ExpiryOf(sandboxID string) time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	if in, ok := a.instances[a.byID[sandboxID]]; ok {
		return in.expiry
	}
	return time.Time{}
}

// TemplateOf returns the template the sandbox's instance was created with.
func (a *Adapter) TemplateOf(sandboxID string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if in, ok := a.instances[a.byID[sandboxID]]; ok {
		return in.template
	}
	return ""
}

// Forget deletes an instance behind the control plane's back.
func (a *Adapter) Forget(ref string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if in, ok := a.instances[ref]; ok {
		delete(a.byID, in.SandboxID)
		delete(a.instances, ref)
	}
}

// Inject creates an instance the control plane does not know (an orphan).
func (a *Adapter) Inject(sandboxID, installation string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	ref := id.New("fake")
	a.instances[ref] = &instance{Instance: adapter.Instance{Ref: ref, SandboxID: sandboxID, State: adapter.StateRunning}, installation: installation, files: map[string][]byte{}}
	a.byID[sandboxID] = ref
	return ref
}

func (a *Adapter) Create(ctx context.Context, req adapter.CreateRequest) (adapter.Instance, error) {
	a.mu.Lock()
	if a.HangCreates > 0 {
		a.HangCreates--
		a.mu.Unlock()
		<-ctx.Done()
		return adapter.Instance{}, ctx.Err()
	}
	defer a.mu.Unlock()
	if ref, ok := a.byID[req.ID]; ok {
		return a.instances[ref].Instance, nil
	}
	if a.FailCreates > 0 {
		a.FailCreates--
		if a.FailWith != nil {
			return adapter.Instance{}, a.FailWith
		}
		return adapter.Instance{}, adapter.Unavailable(fmt.Errorf("injected create failure"))
	}
	if a.opts.TotalCPU > 0 {
		used := 0
		for _, in := range a.instances {
			used += in.resources.CPU
		}
		if used+req.Resources.CPU > a.opts.TotalCPU {
			return adapter.Instance{}, adapter.InsufficientCapacity("fake pool full")
		}
	}
	files := map[string][]byte{}
	if req.FromSnapshot != "" {
		snap, ok := a.snapshots[req.FromSnapshot]
		if !ok {
			return adapter.Instance{}, adapter.NotFound(req.FromSnapshot)
		}
		for k, v := range snap {
			files[k] = append([]byte(nil), v...)
		}
	}
	a.creates++
	ref := id.New("fake")
	in := &instance{Instance: adapter.Instance{Ref: ref, SandboxID: req.ID, State: adapter.StateRunning, Region: a.opts.Region}, installation: req.Installation, template: req.Template, resources: req.Resources, files: files}
	a.instances[ref] = in
	a.byID[req.ID] = ref
	return in.Instance, nil
}

func (a *Adapter) get(ref string) (*instance, error) {
	in, ok := a.instances[ref]
	if !ok {
		return nil, adapter.NotFound(ref)
	}
	return in, nil
}

func (a *Adapter) Get(_ context.Context, ref string) (adapter.Instance, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	in, err := a.get(ref)
	if err != nil {
		return adapter.Instance{}, err
	}
	return in.Instance, nil
}

func (a *Adapter) List(_ context.Context, installation string) ([]adapter.Instance, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []adapter.Instance
	for _, in := range a.instances {
		if installation == "" || in.installation == installation {
			out = append(out, in.Instance)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

func (a *Adapter) Delete(_ context.Context, ref string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if in, ok := a.instances[ref]; ok {
		delete(a.byID, in.SandboxID)
		delete(a.instances, ref)
	}
	return nil
}

func (a *Adapter) running(ref string) (*instance, error) {
	in, err := a.get(ref)
	if err != nil {
		return nil, err
	}
	if in.State != adapter.StateRunning {
		return nil, api.Errorf(api.CodeInvalidState, "instance is %s", in.State)
	}
	return in, nil
}

func (a *Adapter) Exec(_ context.Context, ref string, req api.ExecRequest) (api.ExecResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.running(ref); err != nil {
		return api.ExecResult{}, err
	}
	if a.opts.ExecFunc != nil {
		return a.opts.ExecFunc(req), nil
	}
	cmd := req.Command
	if cmd == "" {
		cmd = fmt.Sprint(req.Args)
	}
	return api.ExecResult{Stdout: "fake: " + cmd + "\n"}, nil
}

func (a *Adapter) ExecStream(ctx context.Context, ref string, req api.ExecRequest, emit func(api.ExecEvent) error) (int, error) {
	res, err := a.Exec(ctx, ref, req)
	if err != nil {
		return -1, err
	}
	if err := emit(api.ExecEvent{Stream: "stdout", Data: res.Stdout}); err != nil {
		return -1, err
	}
	return res.ExitCode, nil
}

func (a *Adapter) ReadFile(_ context.Context, ref, path string) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	in, err := a.running(ref)
	if err != nil {
		return nil, err
	}
	data, ok := in.files[path]
	if !ok {
		return nil, api.Errorf(api.CodeNotFound, "%s does not exist", path)
	}
	return append([]byte(nil), data...), nil
}

func (a *Adapter) WriteFile(_ context.Context, ref, path string, data []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	in, err := a.running(ref)
	if err != nil {
		return err
	}
	in.files[path] = append([]byte(nil), data...)
	return nil
}

func (a *Adapter) ListFiles(_ context.Context, ref, _ string) ([]api.FileInfo, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	in, err := a.running(ref)
	if err != nil {
		return nil, err
	}
	var out []api.FileInfo
	for p, d := range in.files {
		out = append(out, api.FileInfo{Path: p, Name: p, Size: int64(len(d)), Mode: "-rw-r--r--"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (a *Adapter) setState(ref, from, to string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	in, err := a.get(ref)
	if err != nil {
		return err
	}
	if in.State != from && in.State != to {
		return api.Errorf(api.CodeInvalidState, "instance is %s", in.State)
	}
	in.State = to
	return nil
}

func (a *Adapter) Stop(_ context.Context, ref string) error {
	return a.setState(ref, adapter.StateRunning, adapter.StateStopped)
}
func (a *Adapter) Start(_ context.Context, ref string) error {
	return a.setState(ref, adapter.StateStopped, adapter.StateRunning)
}
func (a *Adapter) Pause(_ context.Context, ref string) error {
	return a.setState(ref, adapter.StateRunning, adapter.StatePaused)
}
func (a *Adapter) Resume(_ context.Context, ref string) error {
	return a.setState(ref, adapter.StatePaused, adapter.StateRunning)
}

func (a *Adapter) Snapshot(_ context.Context, ref, kind string) (adapter.Snapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	in, err := a.get(ref)
	if err != nil {
		return adapter.Snapshot{}, err
	}
	snapRef := id.New("fakesnap")
	files := map[string][]byte{}
	size := int64(0)
	for k, v := range in.files {
		files[k] = append([]byte(nil), v...)
		size += int64(len(v))
	}
	a.snapshots[snapRef] = files
	if kind == "" {
		kind = "disk"
	}
	return adapter.Snapshot{Ref: snapRef, Kind: kind, SizeBytes: size}, nil
}

func (a *Adapter) DeleteSnapshot(_ context.Context, ref string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.snapshots, ref)
	return nil
}

func (a *Adapter) ExposePort(_ context.Context, ref string, port int) (string, error) {
	return fmt.Sprintf("https://%d-%s.fake.invalid", port, ref), nil
}

var (
	_ adapter.ExecutionAdapter = (*Adapter)(nil)
	_ adapter.Streamer         = (*Adapter)(nil)
	_ adapter.FileSystem       = (*Adapter)(nil)
	_ adapter.Lifecycle        = (*Adapter)(nil)
	_ adapter.Pauser           = (*Adapter)(nil)
	_ adapter.Snapshotter      = (*Adapter)(nil)
	_ adapter.PortExposer      = (*Adapter)(nil)
)
