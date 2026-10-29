// Package daytona adapts Daytona's hosted (or customer-region) sandbox API.
//
// Daytona runs the workload and its control plane, so pools built on this
// adapter are always TrustExternal. The control plane API creates and manages
// sandboxes; each sandbox's toolbox API (at its toolboxProxyUrl) runs commands
// and moves files. Ownership is tracked with Differently labels, which also
// makes Create idempotent.
//
// Written from Daytona's published OpenAPI specs and run against a live
// account on 2026-10-29 (live_test.go; the container class only). Known
// semantic differences: exec output combines stdout and stderr; stdin is not
// supported; snapshots are filesystem-only unless the sandbox is a VM class,
// which comes from the snapshot.
package daytona

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/different-ai/differently/internal/adapters/httpx"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// DefaultAPIURL is Daytona's hosted control plane.
const DefaultAPIURL = "https://app.daytona.io/api"

// Options configures the adapter. Pool config keys: apiURL, target, snapshot,
// class (container|linux-vm). Daytona's own auto-stop is always off:
// Differently owns TTL and lifecycle.
type Options struct {
	APIURL       string
	APIKey       string
	Target       string
	Snapshot     string
	Class        string
	ReadyTimeout time.Duration
}

// FromConfig maps pool config to options.
func FromConfig(cfg map[string]string, apiKey string) Options {
	o := Options{APIURL: cfg["apiURL"], APIKey: apiKey, Target: cfg["target"], Snapshot: cfg["snapshot"], Class: cfg["class"]}
	if o.APIURL == "" {
		o.APIURL = DefaultAPIURL
	}
	if o.Class == "" {
		o.Class = "container"
	}
	return o
}

// Adapter talks to Daytona.
type Adapter struct {
	opts Options
	c    *httpx.Client

	mu      sync.Mutex
	toolbox map[string]string // sandbox id -> toolbox base URL
}

// New returns a Daytona adapter.
func New(o Options) *Adapter {
	if o.ReadyTimeout == 0 {
		o.ReadyTimeout = 3 * time.Minute
	}
	return &Adapter{opts: o, c: httpx.New("daytona", o.APIURL, o.APIKey), toolbox: map[string]string{}}
}

// Endpoint is reported in the trust report.
func (a *Adapter) Endpoint() string { return a.opts.APIURL }

type sandbox struct {
	ID              string            `json:"id"`
	State           string            `json:"state"`
	Target          string            `json:"target"`
	Labels          map[string]string `json:"labels"`
	ToolboxProxyURL string            `json:"toolboxProxyUrl"`
	ErrorReason     string            `json:"errorReason"`
	Snapshot        string            `json:"snapshot"`
}

func (a *Adapter) Info() adapter.Info {
	iso := "container (Daytona-managed)"
	if a.opts.Class == "linux-vm" {
		iso = "virtual machine (Daytona-managed)"
	}
	return adapter.Info{Kind: "daytona", Version: "1", Trust: api.TrustExternal, Isolation: iso, Protocol: adapter.ProtocolVersion, Description: "Daytona sandboxes at " + a.opts.APIURL}
}

func (a *Adapter) Capabilities(context.Context) (adapter.Capabilities, error) {
	f := []string{adapter.FeatureExec, adapter.FeatureFiles, adapter.FeatureStopStart, adapter.FeaturePersistDisk, adapter.FeatureSnapshotDisk, adapter.FeaturePorts, adapter.FeatureArchAMD64}
	if a.opts.Class == "linux-vm" {
		f = append(f, adapter.FeaturePauseResume, adapter.FeatureSnapshotMem, adapter.FeatureMicroVM)
	} else {
		f = append(f, adapter.FeatureContainer)
	}
	var regions []string
	if a.opts.Target != "" {
		regions = []string{a.opts.Target}
	}
	// Daytona's default per-sandbox maximum is 4 vCPU / 8 GiB / 10 GiB.
	return adapter.Capabilities{Features: f, MaxCPU: 4, MaxMemoryMiB: 8192, MaxDiskGiB: 10, Regions: regions}, nil
}

func (a *Adapter) Capacity(context.Context) (adapter.Capacity, error) { return adapter.Capacity{}, nil }

func (a *Adapter) Health(ctx context.Context) error {
	return a.c.JSON(ctx, http.MethodGet, "/sandbox?limit=1", nil, nil)
}

func state(s sandbox) string {
	switch s.State {
	case "started":
		return adapter.StateRunning
	case "stopped", "archived":
		return adapter.StateStopped
	case "paused":
		return adapter.StatePaused
	case "error", "build_failed":
		return adapter.StateError
	}
	return adapter.StatePending
}

func (a *Adapter) instance(s sandbox) adapter.Instance {
	a.mu.Lock()
	if s.ToolboxProxyURL != "" {
		a.toolbox[s.ID] = s.ToolboxProxyURL
	}
	a.mu.Unlock()
	return adapter.Instance{Ref: s.ID, SandboxID: s.Labels[adapter.LabelSandboxID], State: state(s), Region: s.Target, Message: s.ErrorReason}
}

func (a *Adapter) find(ctx context.Context, labels map[string]string) ([]sandbox, error) {
	q, _ := json.Marshal(labels)
	var out []sandbox
	cursor := ""
	for {
		path := "/sandbox?limit=200&labels=" + url.QueryEscape(string(q))
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		var page struct {
			Items      []sandbox `json:"items"`
			NextCursor *string   `json:"nextCursor"`
		}
		if err := a.c.JSON(ctx, http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.NextCursor == nil || *page.NextCursor == "" {
			return out, nil
		}
		cursor = *page.NextCursor
	}
}

func (a *Adapter) Create(ctx context.Context, req adapter.CreateRequest) (adapter.Instance, error) {
	existing, err := a.find(ctx, map[string]string{adapter.LabelSandboxID: req.ID})
	if err != nil {
		return adapter.Instance{}, err
	}
	var s sandbox
	if len(existing) > 0 {
		s = existing[0]
	} else {
		snapshot := a.opts.Snapshot
		if req.Template != "" {
			snapshot = req.Template
		}
		if req.FromSnapshot != "" {
			snapshot = req.FromSnapshot
		}
		body := map[string]any{
			"name":             req.ID,
			"labels":           adapter.OwnershipLabels(req),
			"env":              req.Env,
			"autoStopInterval": 0, // Differently owns TTL and lifecycle
		}
		if snapshot != "" {
			body["snapshot"] = snapshot
		}
		if a.opts.Target != "" {
			body["target"] = a.opts.Target
		}
		if req.TTLSeconds > 0 {
			// Backstop in case this control plane is lost: the provider deletes too.
			body["ttlMinutes"] = (req.TTLSeconds + 59) / 60
		}
		if err := a.c.JSON(ctx, http.MethodPost, "/sandbox", body, &s); err != nil {
			return adapter.Instance{}, err
		}
	}
	err = httpx.Poll(ctx, time.Second, a.opts.ReadyTimeout, func() (bool, error) {
		if state(s) == adapter.StateRunning {
			return true, nil
		}
		if state(s) == adapter.StateError {
			return false, adapter.Unavailable(fmt.Errorf("daytona sandbox %s failed: %s", s.ID, s.ErrorReason))
		}
		return false, a.c.JSON(ctx, http.MethodGet, "/sandbox/"+url.PathEscape(s.ID), nil, &s)
	})
	if err != nil {
		return adapter.Instance{}, err
	}
	return a.instance(s), nil
}

func (a *Adapter) Get(ctx context.Context, ref string) (adapter.Instance, error) {
	var s sandbox
	if err := a.c.JSON(ctx, http.MethodGet, "/sandbox/"+url.PathEscape(ref), nil, &s); err != nil {
		return adapter.Instance{}, err
	}
	if s.State == "destroyed" || s.State == "destroying" {
		return adapter.Instance{}, adapter.NotFound(ref)
	}
	return a.instance(s), nil
}

func (a *Adapter) List(ctx context.Context, installation string) ([]adapter.Instance, error) {
	items, err := a.find(ctx, map[string]string{adapter.LabelInstallation: installation})
	if err != nil {
		return nil, err
	}
	var out []adapter.Instance
	for _, s := range items {
		if s.State != "destroyed" && s.State != "destroying" {
			out = append(out, a.instance(s))
		}
	}
	return out, nil
}

func (a *Adapter) Delete(ctx context.Context, ref string) error {
	del := func() error { return a.c.JSON(ctx, http.MethodDelete, "/sandbox/"+url.PathEscape(ref), nil, nil) }
	err := del()
	if api.IsCode(err, api.CodeInvalidState) {
		// Daytona answers 409 while a state change is in progress: the
		// sandbox is already being destroyed (a repeated delete), or it is
		// starting or stopping. Get reports a destroying sandbox as not found;
		// otherwise wait for the change to settle and delete again.
		err = httpx.Poll(ctx, time.Second, a.opts.ReadyTimeout, func() (bool, error) {
			in, err := a.Get(ctx, ref)
			if err != nil || in.State == adapter.StatePending {
				return false, err
			}
			if err := del(); !api.IsCode(err, api.CodeInvalidState) {
				return true, err
			}
			return false, nil
		})
	}
	if api.IsCode(err, api.CodeNotFound) {
		return nil
	}
	return err
}

func (a *Adapter) toolboxURL(ctx context.Context, ref string) (string, error) {
	a.mu.Lock()
	base, ok := a.toolbox[ref]
	a.mu.Unlock()
	if !ok {
		if _, err := a.Get(ctx, ref); err != nil {
			return "", err
		}
		a.mu.Lock()
		base = a.toolbox[ref]
		a.mu.Unlock()
	}
	if base == "" {
		return "", adapter.Unavailable(fmt.Errorf("daytona did not report a toolbox URL for %s", ref))
	}
	return base + "/" + url.PathEscape(ref), nil
}

func (a *Adapter) Exec(ctx context.Context, ref string, req api.ExecRequest) (api.ExecResult, error) {
	if req.Stdin != "" {
		return api.ExecResult{}, api.Errorf(api.CodeUnsupportedOperation, "the daytona backend does not support exec stdin").With("operation", "exec.stdin")
	}
	base, err := a.toolboxURL(ctx, ref)
	if err != nil {
		return api.ExecResult{}, err
	}
	timeout := req.TimeoutSeconds
	if timeout == 0 {
		timeout = 600 // the provider default is short; always send one
	}
	body := map[string]any{"command": httpx.CommandLine(api.ExecRequest{Command: req.Command, Args: req.Args}), "timeout": timeout}
	if req.Cwd != "" {
		body["cwd"] = req.Cwd
	}
	if len(req.Env) > 0 {
		body["envs"] = req.Env
	}
	var out struct {
		ExitCode int    `json:"exitCode"`
		Result   string `json:"result"`
	}
	start := time.Now()
	if err := a.c.JSON(ctx, http.MethodPost, base+"/process/execute", body, &out); err != nil {
		// A command that outlives its timeout is a 408 without its output,
		// not a provider failure: report it the way a shell's timeout does.
		var e *api.Error
		if errors.As(err, &e) && e.Details["providerCode"] == "PROCESS_EXECUTION_TIMEOUT" {
			return api.ExecResult{ExitCode: 124, Stderr: fmt.Sprintf("command timed out after %ds\n", timeout), DurationMs: time.Since(start).Milliseconds()}, nil
		}
		return api.ExecResult{}, err
	}
	// Daytona combines stdout and stderr into one buffer.
	return api.ExecResult{ExitCode: out.ExitCode, Stdout: out.Result, DurationMs: time.Since(start).Milliseconds()}, nil
}

func (a *Adapter) ReadFile(ctx context.Context, ref, path string) ([]byte, error) {
	base, err := a.toolboxURL(ctx, ref)
	if err != nil {
		return nil, err
	}
	resp, err := a.c.Do(ctx, http.MethodGet, base+"/files/download?path="+url.QueryEscape(path), nil, "", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 100<<20))
}

func (a *Adapter) WriteFile(ctx context.Context, ref, path string, data []byte) error {
	base, err := a.toolboxURL(ctx, ref)
	if err != nil {
		return err
	}
	resp, err := a.c.Do(ctx, http.MethodPost, base+"/files/upload-v2?path="+url.QueryEscape(path), bytes.NewReader(data), "application/octet-stream", nil)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (a *Adapter) ListFiles(ctx context.Context, ref, path string) ([]api.FileInfo, error) {
	base, err := a.toolboxURL(ctx, ref)
	if err != nil {
		return nil, err
	}
	var items []struct {
		Name       string `json:"name"`
		Path       string `json:"path"`
		Size       int64  `json:"size"`
		IsDir      bool   `json:"isDir"`
		Mode       string `json:"mode"`
		ModifiedAt string `json:"modifiedAt"`
	}
	if err := a.c.JSON(ctx, http.MethodGet, base+"/files?path="+url.QueryEscape(path), nil, &items); err != nil {
		return nil, err
	}
	out := make([]api.FileInfo, 0, len(items))
	for _, it := range items {
		mt, _ := time.Parse(time.RFC3339, it.ModifiedAt)
		out = append(out, api.FileInfo{Path: it.Path, Name: it.Name, Size: it.Size, IsDir: it.IsDir, Mode: it.Mode, ModTime: mt})
	}
	return out, nil
}

// action asks for a state change and waits for it: Daytona answers while the
// sandbox is still stopping or starting.
func (a *Adapter) action(ctx context.Context, ref, action, want string) error {
	if err := a.c.JSON(ctx, http.MethodPost, "/sandbox/"+url.PathEscape(ref)+"/"+action, nil, nil); err != nil {
		return err
	}
	return httpx.Poll(ctx, time.Second, a.opts.ReadyTimeout, func() (bool, error) {
		var s sandbox
		if err := a.c.JSON(ctx, http.MethodGet, "/sandbox/"+url.PathEscape(ref), nil, &s); err != nil {
			return false, err
		}
		if state(s) == adapter.StateError {
			return false, adapter.SandboxFailed(fmt.Errorf("daytona sandbox %s failed to %s: %s", ref, action, s.ErrorReason))
		}
		return s.State == want, nil
	})
}

func (a *Adapter) Stop(ctx context.Context, ref string) error {
	return a.action(ctx, ref, "stop", "stopped")
}
func (a *Adapter) Start(ctx context.Context, ref string) error {
	return a.action(ctx, ref, "start", "started")
}

// Pause is only meaningful for VM-class sandboxes (containers answer 422);
// Capabilities hides it for containers so placement never routes
// pause-requiring work here.
func (a *Adapter) Pause(ctx context.Context, ref string) error {
	return a.action(ctx, ref, "pause", "paused")
}
func (a *Adapter) Resume(ctx context.Context, ref string) error {
	return a.action(ctx, ref, "start", "started")
}

func (a *Adapter) Snapshot(ctx context.Context, ref, kind string) (adapter.Snapshot, error) {
	if kind == "memory" && a.opts.Class != "linux-vm" {
		return adapter.Snapshot{}, adapter.Unsupported("snapshots.memory")
	}
	name := "dly-" + ref + "-" + strconv.FormatInt(time.Now().Unix(), 36)
	var s sandbox
	body := map[string]any{"name": name, "includeMemory": kind == "memory"}
	if err := a.c.JSON(ctx, http.MethodPost, "/sandbox/"+url.PathEscape(ref)+"/snapshot", body, &s); err != nil {
		return adapter.Snapshot{}, err
	}
	// Snapshotting is asynchronous; wait until the source settles and the
	// snapshot can be created from.
	err := httpx.Poll(ctx, time.Second, a.opts.ReadyTimeout, func() (bool, error) {
		if s.State == "snapshotting" {
			return false, a.c.JSON(ctx, http.MethodGet, "/sandbox/"+url.PathEscape(ref), nil, &s)
		}
		snap, err := a.snapshot(ctx, name)
		switch {
		case err != nil:
			return false, err
		case snap.State == "error" || snap.State == "build_failed":
			return false, adapter.SandboxFailed(fmt.Errorf("daytona snapshot %s failed: %s", name, snap.ErrorReason))
		}
		return snap.State == "active", nil
	})
	if kind == "" {
		kind = "disk"
	}
	return adapter.Snapshot{Ref: name, Kind: kind}, err
}

type snapshotInfo struct {
	ID          string `json:"id"`
	State       string `json:"state"`
	ErrorReason string `json:"errorReason"`
}

// snapshot looks a snapshot up by name (or id).
func (a *Adapter) snapshot(ctx context.Context, ref string) (snapshotInfo, error) {
	var s snapshotInfo
	return s, a.c.JSON(ctx, http.MethodGet, "/snapshots/"+url.PathEscape(ref), nil, &s)
}

// DeleteSnapshot deletes by id, which Daytona requires; the ref is the name.
// A snapshot already being removed answers 200 again.
func (a *Adapter) DeleteSnapshot(ctx context.Context, ref string) error {
	s, err := a.snapshot(ctx, ref)
	if err == nil {
		err = a.c.JSON(ctx, http.MethodDelete, "/snapshots/"+url.PathEscape(s.ID), nil, nil)
	}
	if api.IsCode(err, api.CodeNotFound) {
		return nil
	}
	return err
}

// ExposePort returns a signed preview URL valid for one hour.
func (a *Adapter) ExposePort(ctx context.Context, ref string, port int) (string, error) {
	var out struct {
		URL string `json:"url"`
	}
	err := a.c.JSON(ctx, http.MethodGet, fmt.Sprintf("/sandbox/%s/ports/%d/signed-preview-url?expiresInSeconds=3600", url.PathEscape(ref), port), nil, &out)
	return out.URL, err
}

var (
	_ adapter.ExecutionAdapter = (*Adapter)(nil)
	_ adapter.FileSystem       = (*Adapter)(nil)
	_ adapter.Lifecycle        = (*Adapter)(nil)
	_ adapter.Pauser           = (*Adapter)(nil)
	_ adapter.Snapshotter      = (*Adapter)(nil)
	_ adapter.PortExposer      = (*Adapter)(nil)
)
