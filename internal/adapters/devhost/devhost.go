// Package devhost runs sandboxes as plain directories on the control-plane
// host. Commands execute as the server's user with NO isolation. It exists so
// the full API, CLI and SDK flow can be exercised on a laptop; the server only
// enables it in --dev mode and its pools are labelled isolation.none.
package devhost

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/different-ai/differently/internal/guest"
	"github.com/different-ai/differently/internal/id"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// Adapter manages sandbox directories under Root.
type Adapter struct {
	Root string
	mu   sync.Mutex
}

type meta struct {
	adapter.Instance
	Installation string            `json:"installation"`
	Env          map[string]string `json:"env,omitempty"`
}

// New returns a devhost adapter rooted at root.
func New(root string) (*Adapter, error) {
	for _, d := range []string{"sandboxes", "snapshots"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return nil, err
		}
	}
	return &Adapter{Root: root}, nil
}

func (a *Adapter) dir(ref string) string {
	return filepath.Join(a.Root, "sandboxes", filepath.Base(ref))
}
func (a *Adapter) fsDir(ref string) string { return filepath.Join(a.dir(ref), "fs") }

func (a *Adapter) load(ref string) (meta, error) {
	var m meta
	b, err := os.ReadFile(filepath.Join(a.dir(ref), "meta.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return m, adapter.NotFound(ref)
	}
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

func (a *Adapter) save(m meta) error {
	b, _ := json.Marshal(m)
	return os.WriteFile(filepath.Join(a.dir(m.Ref), "meta.json"), b, 0o600)
}

func (a *Adapter) Info() adapter.Info {
	return adapter.Info{Kind: "devhost", Version: "1", Trust: api.TrustPrivate, Isolation: "none", Description: "development only: runs commands on the host", Protocol: adapter.ProtocolVersion}
}

func (a *Adapter) Capabilities(context.Context) (adapter.Capabilities, error) {
	return adapter.Capabilities{Features: []string{
		adapter.FeatureExec, adapter.FeatureExecStream, adapter.FeatureFiles, adapter.FeatureStopStart,
		adapter.FeaturePersistDisk, adapter.FeatureSnapshotDisk, adapter.FeatureFork, adapter.FeatureNoIsolation,
	}}, nil
}

func (a *Adapter) Capacity(context.Context) (adapter.Capacity, error) { return adapter.Capacity{}, nil }

func (a *Adapter) Health(context.Context) error {
	_, err := os.Stat(a.Root)
	return err
}

func (a *Adapter) Create(_ context.Context, req adapter.CreateRequest) (adapter.Instance, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ref := req.ID // the sandbox ID is the idempotency key and the ref
	if m, err := a.load(ref); err == nil {
		return m.Instance, nil
	}
	if err := os.MkdirAll(a.fsDir(ref), 0o755); err != nil {
		return adapter.Instance{}, err
	}
	if req.FromSnapshot != "" {
		src := filepath.Join(a.Root, "snapshots", filepath.Base(req.FromSnapshot))
		if err := copyTree(src, a.fsDir(ref)); err != nil {
			_ = os.RemoveAll(a.dir(ref))
			return adapter.Instance{}, err
		}
	}
	m := meta{Instance: adapter.Instance{Ref: ref, SandboxID: req.ID, State: adapter.StateRunning}, Installation: req.Installation, Env: req.Env}
	return m.Instance, a.save(m)
}

func (a *Adapter) Get(_ context.Context, ref string) (adapter.Instance, error) {
	m, err := a.load(ref)
	return m.Instance, err
}

func (a *Adapter) List(_ context.Context, installation string) ([]adapter.Instance, error) {
	entries, err := os.ReadDir(filepath.Join(a.Root, "sandboxes"))
	if err != nil {
		return nil, err
	}
	var out []adapter.Instance
	for _, e := range entries {
		m, err := a.load(e.Name())
		if err == nil && (installation == "" || m.Installation == installation) {
			out = append(out, m.Instance)
		}
	}
	return out, nil
}

func (a *Adapter) Delete(_ context.Context, ref string) error {
	return os.RemoveAll(a.dir(ref))
}

func (a *Adapter) runner(ref string) (*guest.Runner, error) {
	m, err := a.load(ref)
	if err != nil {
		return nil, err
	}
	if m.State != adapter.StateRunning {
		return nil, api.Errorf(api.CodeInvalidState, "sandbox is %s", m.State)
	}
	return &guest.Runner{Root: a.fsDir(ref), BaseEnv: m.Env}, nil
}

func (a *Adapter) Exec(ctx context.Context, ref string, req api.ExecRequest) (api.ExecResult, error) {
	r, err := a.runner(ref)
	if err != nil {
		return api.ExecResult{}, err
	}
	return r.Exec(ctx, req)
}

func (a *Adapter) ExecStream(ctx context.Context, ref string, req api.ExecRequest, emit func(api.ExecEvent) error) (int, error) {
	r, err := a.runner(ref)
	if err != nil {
		return -1, err
	}
	return r.ExecStream(ctx, req, emit)
}

func (a *Adapter) ReadFile(_ context.Context, ref, p string) ([]byte, error) {
	r, err := a.runner(ref)
	if err != nil {
		return nil, err
	}
	return r.ReadFile(p)
}

func (a *Adapter) WriteFile(_ context.Context, ref, p string, data []byte) error {
	r, err := a.runner(ref)
	if err != nil {
		return err
	}
	return r.WriteFile(p, data)
}

func (a *Adapter) ListFiles(_ context.Context, ref, p string) ([]api.FileInfo, error) {
	r, err := a.runner(ref)
	if err != nil {
		return nil, err
	}
	return r.ListFiles(p)
}

func (a *Adapter) setState(ref, state string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	m, err := a.load(ref)
	if err != nil {
		return err
	}
	m.State = state
	return a.save(m)
}

func (a *Adapter) Stop(_ context.Context, ref string) error {
	return a.setState(ref, adapter.StateStopped)
}

func (a *Adapter) Start(_ context.Context, ref string) error {
	return a.setState(ref, adapter.StateRunning)
}

func (a *Adapter) Snapshot(_ context.Context, ref, kind string) (adapter.Snapshot, error) {
	if kind == "memory" {
		return adapter.Snapshot{}, adapter.Unsupported("snapshots.memory")
	}
	if _, err := a.load(ref); err != nil {
		return adapter.Snapshot{}, err
	}
	snap := id.New("snap")
	dst := filepath.Join(a.Root, "snapshots", snap)
	if err := copyTree(a.fsDir(ref), dst); err != nil {
		return adapter.Snapshot{}, err
	}
	return adapter.Snapshot{Ref: snap, Kind: "disk", SizeBytes: treeSize(dst)}, nil
}

func (a *Adapter) DeleteSnapshot(_ context.Context, ref string) error {
	return os.RemoveAll(filepath.Join(a.Root, "snapshots", filepath.Base(ref)))
}

func copyTree(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	// cp -a preserves modes and symlinks without following them.
	return exec.Command("cp", "-a", src+"/.", dst).Run()
}

func treeSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

var (
	_ adapter.ExecutionAdapter = (*Adapter)(nil)
	_ adapter.Streamer         = (*Adapter)(nil)
	_ adapter.FileSystem       = (*Adapter)(nil)
	_ adapter.Lifecycle        = (*Adapter)(nil)
	_ adapter.Snapshotter      = (*Adapter)(nil)
)
