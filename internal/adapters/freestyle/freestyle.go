// Package freestyle adapts Freestyle's hosted microVM API (/v5).
//
// Freestyle is hosted-only, so pools on this adapter are always
// TrustExternal. Notable differences from other backends, handled here:
//
//   - create has no resource or env fields: VMs boot at their snapshot's size
//     and are grown with /resize; sandbox env is written to a 0600 file inside
//     the VM and sourced by every exec;
//   - there is no stop endpoint: stop runs poweroff and waits for "stopped";
//   - snapshots always capture memory and disk;
//   - networking is deny-all unless the pool sets config.egress=public.
//
// Verified against Freestyle's published OpenAPI spec (2026-10-28), not yet
// against a live account.
package freestyle

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/different-ai/differently/internal/adapters/httpx"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// DefaultAPIURL is Freestyle's API.
const DefaultAPIURL = "https://api.freestyle.sh"

// Metadata keys. Freestyle metadata keys are short and unprefixed.
const (
	metaSandbox      = "dly-sandbox"
	metaInstallation = "dly-installation"
	envFile          = "/etc/differently/env"
)

// Options configures the adapter. Pool config keys: apiURL, snapshot,
// egress (none|public), domain (for exposed ports, default style.dev).
type Options struct {
	APIURL       string
	APIKey       string
	Snapshot     string
	Egress       string
	Domain       string
	ReadyTimeout time.Duration
}

// FromConfig maps pool config to options.
func FromConfig(cfg map[string]string, apiKey string) Options {
	o := Options{APIURL: cfg["apiURL"], APIKey: apiKey, Snapshot: cfg["snapshot"], Egress: cfg["egress"], Domain: cfg["domain"]}
	if o.APIURL == "" {
		o.APIURL = DefaultAPIURL
	}
	if o.Domain == "" {
		o.Domain = "style.dev"
	}
	return o
}

// Adapter talks to Freestyle.
type Adapter struct {
	opts Options
	c    *httpx.Client
}

// New returns a Freestyle adapter.
func New(o Options) *Adapter {
	if o.ReadyTimeout == 0 {
		o.ReadyTimeout = 3 * time.Minute
	}
	return &Adapter{opts: o, c: httpx.New("freestyle", o.APIURL, o.APIKey)}
}

// Endpoint is reported in the trust report.
func (a *Adapter) Endpoint() string { return a.opts.APIURL }

type vm struct {
	ID        string            `json:"id"`
	State     string            `json:"state"`
	Slug      string            `json:"slug"`
	Metadata  map[string]string `json:"metadata"`
	Resources struct {
		CPU     int `json:"cpu"`
		Memory  int `json:"memory"`
		Storage int `json:"storage"`
	} `json:"resources"`
}

func (a *Adapter) Info() adapter.Info {
	return adapter.Info{Kind: "freestyle", Version: "1", Trust: api.TrustExternal, Isolation: "hardware-virtualized microVM (Freestyle-managed)", Protocol: adapter.ProtocolVersion, Description: "Freestyle VMs at " + a.opts.APIURL}
}

func (a *Adapter) Capabilities(context.Context) (adapter.Capabilities, error) {
	return adapter.Capabilities{Features: []string{
		adapter.FeatureExec, adapter.FeatureFiles, adapter.FeatureStopStart, adapter.FeaturePersistDisk,
		adapter.FeaturePauseResume, adapter.FeatureSnapshotMem, adapter.FeatureSnapshotDisk, adapter.FeatureFork,
		adapter.FeaturePorts, adapter.FeatureEgressControl, adapter.FeatureMicroVM, adapter.FeatureArchAMD64,
	}, MaxCPU: 32, MaxMemoryMiB: 65536, MaxDiskGiB: 256}, nil
}

func (a *Adapter) Capacity(context.Context) (adapter.Capacity, error) { return adapter.Capacity{}, nil }

func (a *Adapter) Health(ctx context.Context) error {
	return a.c.JSON(ctx, http.MethodGet, "/v5/vms?limit=1", nil, nil)
}

// slug derives Freestyle's unique per-account name from the sandbox ID, which
// makes create idempotent: a replay conflicts and resolves to the same VM.
func slug(sandboxID string) string { return strings.ReplaceAll(strings.ToLower(sandboxID), "_", "-") }

func state(v vm) string {
	switch v.State {
	case "running":
		return adapter.StateRunning
	case "paused":
		return adapter.StatePaused
	case "stopped":
		return adapter.StateStopped
	}
	return adapter.StatePending
}

func inst(v vm) adapter.Instance {
	return adapter.Instance{Ref: v.ID, SandboxID: v.Metadata[metaSandbox], State: state(v)}
}

func (a *Adapter) get(ctx context.Context, ref string) (vm, error) {
	var v vm
	return v, a.c.JSON(ctx, http.MethodGet, "/v5/vms/"+url.PathEscape(ref), nil, &v)
}

func (a *Adapter) waitState(ctx context.Context, ref, want string) (vm, error) {
	var v vm
	err := httpx.Poll(ctx, time.Second, a.opts.ReadyTimeout, func() (bool, error) {
		var err error
		v, err = a.get(ctx, ref)
		return err == nil && v.State == want, err
	})
	return v, err
}

func (a *Adapter) Create(ctx context.Context, req adapter.CreateRequest) (adapter.Instance, error) {
	v, err := a.get(ctx, slug(req.ID))
	if api.IsCode(err, api.CodeNotFound) {
		rules := []map[string]any{}
		if a.opts.Egress == "public" {
			rules = append(rules, map[string]any{"action": "allow", "source": map[string]any{}, "destination": map[string]any{"public": true}, "description": "egress allowed by Differently pool config"})
		}
		body := map[string]any{
			"slug":     slug(req.ID),
			"firewall": map[string]any{"rules": rules},
			"metadata": map[string]string{metaSandbox: req.ID, metaInstallation: req.Installation},
		}
		snapshot := a.opts.Snapshot
		if req.Template != "" {
			snapshot = req.Template
		}
		if req.FromSnapshot != "" {
			snapshot = req.FromSnapshot
		}
		if snapshot != "" {
			body["snapshotId"] = snapshot
		}
		if req.TTLSeconds > 0 {
			body["ttlSeconds"] = req.TTLSeconds // provider-side backstop
		}
		err = a.c.JSON(ctx, http.MethodPost, "/v5/vms", body, &v)
		if api.IsCode(err, api.CodeInvalidState) {
			v, err = a.get(ctx, slug(req.ID)) // lost the race with a replay
		}
	}
	if err != nil {
		return adapter.Instance{}, err
	}
	if v, err = a.waitState(ctx, v.ID, "running"); err != nil {
		return adapter.Instance{}, err
	}
	// Grow to the requested envelope; resize never shrinks.
	need := map[string]int{}
	if req.Resources.CPU > v.Resources.CPU {
		need["cpu"] = req.Resources.CPU
	}
	if req.Resources.MemoryMiB > v.Resources.Memory {
		need["memory"] = req.Resources.MemoryMiB
	}
	if d := req.Resources.DiskGiB * 1024; d > v.Resources.Storage {
		need["storage"] = d
	}
	if len(need) > 0 {
		if err := a.c.JSON(ctx, http.MethodPost, "/v5/vms/"+url.PathEscape(v.ID)+"/resize", need, &v); err != nil {
			return adapter.Instance{}, err
		}
	}
	if len(req.Env) > 0 {
		if err := a.WriteFile(ctx, v.ID, envFile, envFileContent(req.Env)); err != nil {
			return adapter.Instance{}, err
		}
	}
	return inst(v), nil
}

func envFileContent(env map[string]string) []byte {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + httpx.ShellQuote([]string{env[k]}) + "\n")
	}
	return []byte(b.String())
}

func (a *Adapter) Get(ctx context.Context, ref string) (adapter.Instance, error) {
	v, err := a.get(ctx, ref)
	return inst(v), err
}

func (a *Adapter) List(ctx context.Context, installation string) ([]adapter.Instance, error) {
	var out []adapter.Instance
	for offset := 0; ; {
		var page struct {
			VMs        []vm `json:"vms"`
			TotalCount int  `json:"totalCount"`
		}
		path := fmt.Sprintf("/v5/vms?limit=100&offset=%d&metadata=%s", offset, url.QueryEscape(metaInstallation+":"+installation))
		if err := a.c.JSON(ctx, http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		for _, v := range page.VMs {
			out = append(out, inst(v))
		}
		offset += len(page.VMs)
		if len(page.VMs) == 0 || offset >= page.TotalCount {
			return out, nil
		}
	}
}

func (a *Adapter) Delete(ctx context.Context, ref string) error {
	err := a.c.JSON(ctx, http.MethodDelete, "/v5/vms/"+url.PathEscape(ref), nil, nil)
	if api.IsCode(err, api.CodeNotFound) {
		return nil
	}
	return err
}

func (a *Adapter) Exec(ctx context.Context, ref string, req api.ExecRequest) (api.ExecResult, error) {
	timeout := req.TimeoutSeconds * 1000
	if timeout <= 0 || timeout > 300_000 {
		timeout = 300_000 // inline exec cap
	}
	body := map[string]any{
		"command":   "set -a; [ -r " + envFile + " ] && . " + envFile + "; set +a; " + httpx.CommandLine(req),
		"timeoutMs": timeout,
	}
	if len(req.Env) > 0 {
		body["env"] = req.Env
	}
	if req.Stdin != "" {
		body["stdin"] = base64.StdEncoding.EncodeToString([]byte(req.Stdin))
	}
	var out struct {
		StatusCode *int   `json:"statusCode"`
		Stdout     string `json:"stdout"`
		Stderr     string `json:"stderr"`
	}
	start := time.Now()
	if err := a.c.JSON(ctx, http.MethodPost, "/v5/vms/"+url.PathEscape(ref)+"/exec-await", body, &out); err != nil {
		return api.ExecResult{}, err
	}
	code := 124 // statusCode is absent when the command was killed by timeout
	if out.StatusCode != nil {
		code = *out.StatusCode
	}
	return api.ExecResult{ExitCode: code, Stdout: out.Stdout, Stderr: out.Stderr, DurationMs: time.Since(start).Milliseconds()}, nil
}

func absPath(p string) string {
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

func (a *Adapter) ReadFile(ctx context.Context, ref, path string) ([]byte, error) {
	resp, err := a.c.Do(ctx, http.MethodGet, "/v5/vms/"+url.PathEscape(ref)+"/fs/read?path="+url.QueryEscape(absPath(path)), nil, "", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 100<<20))
}

func (a *Adapter) WriteFile(ctx context.Context, ref, path string, data []byte) error {
	dir := absPath(path)
	if i := strings.LastIndex(dir, "/"); i > 0 {
		if err := a.c.JSON(ctx, http.MethodPost, "/v5/vms/"+url.PathEscape(ref)+"/fs/mkdir", map[string]string{"path": dir[:i]}, nil); err != nil {
			return err
		}
	}
	resp, err := a.c.Do(ctx, http.MethodPut, "/v5/vms/"+url.PathEscape(ref)+"/fs/write?mode=0600&path="+url.QueryEscape(absPath(path)), bytes.NewReader(data), "application/octet-stream", nil)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (a *Adapter) ListFiles(ctx context.Context, ref, path string) ([]api.FileInfo, error) {
	var out struct {
		Entries []struct {
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"entries"`
	}
	dir := absPath(path)
	if err := a.c.JSON(ctx, http.MethodGet, "/v5/vms/"+url.PathEscape(ref)+"/fs/dir?path="+url.QueryEscape(dir), nil, &out); err != nil {
		return nil, err
	}
	items := make([]api.FileInfo, 0, len(out.Entries))
	for _, e := range out.Entries {
		items = append(items, api.FileInfo{Path: strings.TrimRight(dir, "/") + "/" + e.Name, Name: e.Name, IsDir: e.Kind == "directory"})
	}
	return items, nil
}

// Stop powers the VM off from inside (Freestyle has no stop endpoint).
func (a *Adapter) Stop(ctx context.Context, ref string) error {
	// The exec usually fails when the guest disconnects; the state poll decides.
	_, _ = a.Exec(ctx, ref, api.ExecRequest{Command: "poweroff", TimeoutSeconds: 10})
	_, err := a.waitState(ctx, ref, "stopped")
	return err
}

func (a *Adapter) Start(ctx context.Context, ref string) error {
	if err := a.c.JSON(ctx, http.MethodPost, "/v5/vms/"+url.PathEscape(ref)+"/start", nil, nil); err != nil {
		return err
	}
	_, err := a.waitState(ctx, ref, "running")
	return err
}

func (a *Adapter) Pause(ctx context.Context, ref string) error {
	if err := a.c.JSON(ctx, http.MethodPost, "/v5/vms/"+url.PathEscape(ref)+"/pause", nil, nil); err != nil {
		return err
	}
	_, err := a.waitState(ctx, ref, "paused")
	return err
}

func (a *Adapter) Resume(ctx context.Context, ref string) error { return a.Start(ctx, ref) }

// Snapshot always captures memory and disk on Freestyle.
func (a *Adapter) Snapshot(ctx context.Context, ref, _ string) (adapter.Snapshot, error) {
	var out struct {
		SnapshotID string `json:"snapshotId"`
	}
	if err := a.c.JSON(ctx, http.MethodPost, "/v5/vms/"+url.PathEscape(ref)+"/snapshot", map[string]any{}, &out); err != nil {
		return adapter.Snapshot{}, err
	}
	return adapter.Snapshot{Ref: out.SnapshotID, Kind: "memory"}, nil
}

func (a *Adapter) DeleteSnapshot(ctx context.Context, ref string) error {
	err := a.c.JSON(ctx, http.MethodDelete, "/v5/snapshots/"+url.PathEscape(ref), nil, nil)
	if api.IsCode(err, api.CodeNotFound) {
		return nil
	}
	return err
}

// ExposePort publishes a port on a *.style.dev hostname. The URL is public:
// anyone with it can reach the port.
func (a *Adapter) ExposePort(ctx context.Context, ref string, port int) (string, error) {
	v, err := a.get(ctx, ref)
	if err != nil {
		return "", err
	}
	domain := fmt.Sprintf("%d-%s.%s", port, v.Slug, a.opts.Domain)
	body := map[string]any{"action": "allow", "domain": domain, "source": map[string]any{"public": true}, "destination": map[string]any{"vmId": v.ID, "port": port}}
	if err := a.c.JSON(ctx, http.MethodPost, "/v5/tls", body, nil); err != nil && !api.IsCode(err, api.CodeInvalidState) {
		return "", err
	}
	return "https://" + domain, nil
}

var (
	_ adapter.ExecutionAdapter = (*Adapter)(nil)
	_ adapter.FileSystem       = (*Adapter)(nil)
	_ adapter.Lifecycle        = (*Adapter)(nil)
	_ adapter.Pauser           = (*Adapter)(nil)
	_ adapter.Snapshotter      = (*Adapter)(nil)
	_ adapter.PortExposer      = (*Adapter)(nil)
)
