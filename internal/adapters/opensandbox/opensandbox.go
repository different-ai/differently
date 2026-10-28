// Package opensandbox adapts an OpenSandbox lifecycle server
// (github.com/opensandbox-group/OpenSandbox, Apache-2.0).
//
// OpenSandbox can be self-hosted, so unlike Daytona and Freestyle the pool's
// trust boundary is whatever the operator declares: private when they run
// the OpenSandbox server and its runtime, external when someone else does.
// Its isolation depends on the runtime behind the server (Docker or
// Kubernetes containers, or Firecracker microVMs with Fast Sandbox), so the
// pool declares that too.
//
// Two APIs are involved:
//
//   - the lifecycle API (sandbox-lifecycle.yml): create, get, list, delete,
//     pause, resume, snapshots and port endpoints, authenticated with the
//     OPEN-SANDBOX-API-KEY header;
//   - execd (execd-api.yaml), the agent inside each sandbox: commands and
//     files. Its address and per-sandbox headers come from the lifecycle
//     endpoint for port 44772, by default through the server's proxy.
//
// Written against the published specs of release 1.2.0 (2026-10-28). Exec
// output arrives as a stream of typed, line-based events (newline-delimited
// JSON in practice, SSE "data:" framing also accepted); stdin is not
// supported.
// Checked live against the release 1.2.0 server with the Docker runtime and
// execd v1.1.0 (see test/opensandbox).
package opensandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/different-ai/differently/internal/adapters/httpx"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

const (
	// ExecdPort is execd's port inside every OpenSandbox sandbox.
	ExecdPort = 44772
	// APIKeyHeader authenticates lifecycle (and server-proxied) requests.
	APIKeyHeader = "OPEN-SANDBOX-API-KEY"

	// Metadata keys. Plain keys keep the server's metadata filter simple.
	metaSandbox      = "differently-sandbox-id"
	metaInstallation = "differently-installation"
)

// DefaultFeatures are advertised unless the pool overrides config.features.
var DefaultFeatures = []string{
	adapter.FeatureExec, adapter.FeatureExecStream, adapter.FeatureFiles,
	adapter.FeaturePauseResume, adapter.FeatureSnapshotDisk, adapter.FeatureFork, adapter.FeaturePorts,
}

// Options configures the adapter. Pool config keys: apiURL (required, the
// lifecycle base URL ending in /v1), image (default image URI), templateId,
// isolation (microvm|container), features (comma list), useServerProxy
// (true|false, default true).
type Options struct {
	APIURL         string
	APIKey         string
	Trust          string
	Image          string
	TemplateID     string
	Isolation      string
	Features       []string
	UseServerProxy bool
	ReadyTimeout   time.Duration
}

// FromConfig maps pool config to options.
func FromConfig(cfg map[string]string, trust, apiKey string) (Options, error) {
	o := Options{
		APIURL: strings.TrimRight(cfg["apiURL"], "/"), APIKey: apiKey, Trust: trust,
		Image: cfg["image"], TemplateID: cfg["templateId"], Isolation: cfg["isolation"],
		UseServerProxy: cfg["useServerProxy"] != "false", Features: slices.Clone(DefaultFeatures),
	}
	if o.APIURL == "" {
		return o, errors.New("opensandbox pools need config.apiURL (the lifecycle API, e.g. http://opensandbox:8090/v1)")
	}
	if o.Image == "" && o.TemplateID == "" {
		o.Image = "ubuntu:24.04"
	}
	if f := strings.TrimSpace(cfg["features"]); f != "" {
		o.Features = nil
		for _, v := range strings.Split(f, ",") {
			if v = strings.TrimSpace(v); v != "" {
				o.Features = append(o.Features, v)
			}
		}
	}
	switch o.Isolation {
	case "":
	case "microvm":
		o.Features = append(o.Features, adapter.FeatureMicroVM)
	case "container":
		o.Features = append(o.Features, adapter.FeatureContainer)
	default:
		return o, fmt.Errorf("opensandbox config.isolation must be microvm or container, not %q", o.Isolation)
	}
	return o, nil
}

// Adapter talks to one OpenSandbox server.
type Adapter struct {
	opts Options
	c    *httpx.Client

	mu    sync.Mutex
	execd map[string]endpoint // sandbox id -> resolved execd endpoint
}

type endpoint struct {
	URL     string
	Headers map[string]string
}

// New returns an adapter.
func New(o Options) *Adapter {
	if o.ReadyTimeout == 0 {
		o.ReadyTimeout = 3 * time.Minute
	}
	c := httpx.New("opensandbox", o.APIURL, "")
	return &Adapter{opts: o, c: c, execd: map[string]endpoint{}}
}

// Endpoint is reported in the trust report.
func (a *Adapter) Endpoint() string { return a.opts.APIURL }

func (a *Adapter) lifecycle() http.Header {
	h := http.Header{}
	if a.opts.APIKey != "" {
		h.Set(APIKeyHeader, a.opts.APIKey)
	}
	return h
}

// call sends a lifecycle request with the API key header.
func (a *Adapter) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ct = bytes.NewReader(b), "application/json"
	}
	resp, err := a.c.Do(ctx, method, path, body, ct, a.lifecycle())
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusAccepted && resp.ContentLength == 0 {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

type status struct {
	State   string `json:"state"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type sandbox struct {
	ID       string            `json:"id"`
	Status   status            `json:"status"`
	Metadata map[string]string `json:"metadata"`
}

func (a *Adapter) Info() adapter.Info {
	iso := "depends on the OpenSandbox runtime"
	switch a.opts.Isolation {
	case "microvm":
		iso = "Firecracker microVM (OpenSandbox Fast Sandbox)"
	case "container":
		iso = "container (OpenSandbox Docker or Kubernetes runtime)"
	}
	return adapter.Info{Kind: "opensandbox", Version: "1", Trust: a.opts.Trust, Isolation: iso, Protocol: adapter.ProtocolVersion, Description: "OpenSandbox at " + a.opts.APIURL}
}

func (a *Adapter) Capabilities(context.Context) (adapter.Capabilities, error) {
	var templates []string
	if a.opts.TemplateID != "" {
		templates = []string{a.opts.TemplateID}
	}
	return adapter.Capabilities{Features: a.opts.Features, Templates: templates}, nil
}

func (a *Adapter) Capacity(context.Context) (adapter.Capacity, error) { return adapter.Capacity{}, nil }

func (a *Adapter) Health(ctx context.Context) error {
	return a.call(ctx, http.MethodGet, "/sandboxes?page=1&pageSize=1", nil, nil)
}

func state(s sandbox) string {
	switch s.Status.State {
	case "Running":
		return adapter.StateRunning
	case "Paused":
		return adapter.StatePaused
	case "Failed":
		return adapter.StateError
	}
	return adapter.StatePending
}

func gone(s sandbox) bool { return s.Status.State == "Terminated" }

func inst(s sandbox) adapter.Instance {
	return adapter.Instance{Ref: s.ID, SandboxID: s.Metadata[metaSandbox], State: state(s), Message: strings.TrimSpace(s.Status.Reason + " " + s.Status.Message)}
}

// metadataFilter encodes the server's metadata query: url-encoded k=v pairs.
func metadataFilter(k, v string) string { return url.QueryEscape(k + "=" + v) }

func (a *Adapter) find(ctx context.Context, key, value string) ([]sandbox, error) {
	var out []sandbox
	for page := 1; page < 1000; page++ {
		var resp struct {
			Items      []sandbox `json:"items"`
			Pagination struct {
				HasNextPage bool `json:"hasNextPage"`
			} `json:"pagination"`
		}
		path := fmt.Sprintf("/sandboxes?metadata=%s&page=%d&pageSize=100", metadataFilter(key, value), page)
		if err := a.call(ctx, http.MethodGet, path, nil, &resp); err != nil {
			return nil, err
		}
		for _, s := range resp.Items {
			if s.Metadata[key] == value && !gone(s) { // never trust a filter we did not write
				out = append(out, s)
			}
		}
		if !resp.Pagination.HasNextPage {
			break
		}
	}
	return out, nil
}

func (a *Adapter) get(ctx context.Context, ref string) (sandbox, error) {
	var s sandbox
	if err := a.call(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(ref), nil, &s); err != nil {
		return s, err
	}
	if gone(s) {
		return s, adapter.NotFound(ref)
	}
	return s, nil
}

func (a *Adapter) waitFor(ctx context.Context, ref, want string) (sandbox, error) {
	var s sandbox
	err := httpx.Poll(ctx, 500*time.Millisecond, a.opts.ReadyTimeout, func() (bool, error) {
		var err error
		if s, err = a.get(ctx, ref); err != nil {
			return false, err
		}
		if s.Status.State == "Failed" {
			return false, adapter.Unavailable(fmt.Errorf("opensandbox sandbox %s failed: %s %s", ref, s.Status.Reason, s.Status.Message))
		}
		return s.Status.State == want, nil
	})
	return s, err
}

// Create implements adapter.ExecutionAdapter. It is idempotent on req.ID:
// a sandbox carrying the same Differently ID in its metadata is reused.
func (a *Adapter) Create(ctx context.Context, req adapter.CreateRequest) (adapter.Instance, error) {
	existing, err := a.find(ctx, metaSandbox, req.ID)
	if err != nil {
		return adapter.Instance{}, err
	}
	var s sandbox
	if len(existing) > 0 {
		s = existing[0]
	} else {
		body := map[string]any{
			"env":      req.Env,
			"metadata": map[string]string{metaSandbox: req.ID, metaInstallation: req.Installation, "name": req.ID},
			"resourceLimits": map[string]string{
				"cpu": strconv.Itoa(max(req.Resources.CPU, 1)), "memory": strconv.Itoa(max(req.Resources.MemoryMiB, 128)) + "Mi",
			},
		}
		switch {
		case req.FromSnapshot != "":
			body["snapshotId"] = req.FromSnapshot
		case a.opts.TemplateID != "" && (req.Template == "" || req.Template == a.opts.TemplateID):
			body["templateId"] = a.opts.TemplateID
		default:
			image := a.opts.Image
			if req.Template != "" {
				image = req.Template
			}
			body["image"] = map[string]string{"uri": image}
			body["entrypoint"] = []string{"tail", "-f", "/dev/null"}
		}
		if req.TTLSeconds > 0 {
			body["timeout"] = max(req.TTLSeconds, 60) // provider-side backstop; minimum 60 s
		}
		if err := a.call(ctx, http.MethodPost, "/sandboxes", body, &s); err != nil {
			return adapter.Instance{}, err
		}
	}
	if s, err = a.waitFor(ctx, s.ID, "Running"); err != nil {
		return adapter.Instance{}, err
	}
	if err := a.waitExecd(ctx, s.ID); err != nil {
		return adapter.Instance{}, err
	}
	return inst(s), nil
}

// waitExecd pings execd until the sandbox's agent answers.
func (a *Adapter) waitExecd(ctx context.Context, ref string) error {
	return httpx.Poll(ctx, 200*time.Millisecond, a.opts.ReadyTimeout, func() (bool, error) {
		resp, err := a.execdDo(ctx, ref, http.MethodGet, "/ping", nil, "")
		if err != nil {
			return false, nil // not ready yet
		}
		resp.Body.Close()
		return true, nil
	})
}

func (a *Adapter) Get(ctx context.Context, ref string) (adapter.Instance, error) {
	s, err := a.get(ctx, ref)
	return inst(s), err
}

func (a *Adapter) List(ctx context.Context, installation string) ([]adapter.Instance, error) {
	items, err := a.find(ctx, metaInstallation, installation)
	if err != nil {
		return nil, err
	}
	out := make([]adapter.Instance, 0, len(items))
	for _, s := range items {
		out = append(out, inst(s))
	}
	return out, nil
}

func (a *Adapter) Delete(ctx context.Context, ref string) error {
	err := a.call(ctx, http.MethodDelete, "/sandboxes/"+url.PathEscape(ref), nil, nil)
	a.mu.Lock()
	delete(a.execd, ref)
	a.mu.Unlock()
	if api.IsCode(err, api.CodeNotFound) {
		return nil
	}
	return err
}

// resolve returns execd's address and headers for a sandbox. Through the
// server proxy the tenant-facing credential is our API key, sent in the
// proxy's own header; the server never forwards it to the sandbox.
func (a *Adapter) resolve(ctx context.Context, ref string, refresh bool) (endpoint, error) {
	a.mu.Lock()
	ep, ok := a.execd[ref]
	a.mu.Unlock()
	if ok && !refresh {
		return ep, nil
	}
	var out struct {
		Endpoint string            `json:"endpoint"`
		Headers  map[string]string `json:"headers"`
	}
	path := fmt.Sprintf("/sandboxes/%s/endpoints/%d?use_server_proxy=%t", url.PathEscape(ref), ExecdPort, a.opts.UseServerProxy)
	if err := a.call(ctx, http.MethodGet, path, nil, &out); err != nil {
		return ep, err
	}
	ep = endpoint{URL: out.Endpoint, Headers: out.Headers}
	if !strings.HasPrefix(ep.URL, "http://") && !strings.HasPrefix(ep.URL, "https://") {
		scheme := "http"
		if strings.HasPrefix(a.opts.APIURL, "https://") {
			scheme = "https"
		}
		ep.URL = scheme + "://" + ep.URL
	}
	ep.URL = strings.TrimRight(ep.URL, "/")
	if ep.Headers == nil {
		ep.Headers = map[string]string{}
	}
	if a.opts.UseServerProxy && a.opts.APIKey != "" {
		if _, set := ep.Headers[APIKeyHeader]; !set {
			ep.Headers[APIKeyHeader] = a.opts.APIKey
		}
	}
	a.mu.Lock()
	a.execd[ref] = ep
	a.mu.Unlock()
	return ep, nil
}

// execdDo calls execd. A failed GET re-resolves the endpoint once in case it
// went stale; other methods are never repeated, so a command never runs twice.
func (a *Adapter) execdDo(ctx context.Context, ref, method, path string, body []byte, contentType string) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		ep, err := a.resolve(ctx, ref, attempt > 0)
		if err != nil {
			return nil, err
		}
		h := http.Header{}
		for k, v := range ep.Headers {
			h.Set(k, v)
		}
		var r io.Reader
		if body != nil {
			r = bytes.NewReader(body)
		}
		resp, err := a.c.Do(ctx, method, ep.URL+path, r, contentType, h)
		if err != nil && attempt == 0 && method == http.MethodGet && (api.IsCode(err, api.CodeNotFound) || api.IsCode(err, api.CodeProviderUnavailable)) {
			continue
		}
		return resp, err
	}
}

type event struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ExitCode *int   `json:"exit_code"`
	Error    *struct {
		Ename    string `json:"ename"`
		Evalue   string `json:"evalue"`
		ExitCode *int   `json:"exit_code"`
	} `json:"error"`
}

// run executes a command and feeds stdout/stderr chunks to emit. execd
// reports success with an execution_complete event (exit code 0) and a
// non-zero exit as an error event named CommandExecError.
func (a *Adapter) run(ctx context.Context, ref string, req api.ExecRequest, emit func(api.ExecEvent) error) (int, error) {
	if req.Stdin != "" {
		return -1, api.Errorf(api.CodeUnsupportedOperation, "the opensandbox backend does not support exec stdin").With("operation", "exec.stdin")
	}
	// execd's argv field is newer than the execd images servers deploy
	// (v1.1.0 requires command), so argument lists go as a quoted command.
	body := map[string]any{"command": req.Command}
	if len(req.Args) > 0 {
		body["command"] = httpx.ShellQuote(req.Args)
	}
	if req.Cwd != "" {
		body["cwd"] = req.Cwd
	}
	if len(req.Env) > 0 {
		body["envs"] = req.Env
	}
	if req.TimeoutSeconds > 0 {
		body["timeout"] = req.TimeoutSeconds * 1000
	}
	b, _ := json.Marshal(body)
	resp, err := a.execdDo(ctx, ref, http.MethodPost, "/command", b, "application/json")
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if line == "" || !strings.HasPrefix(line, "{") {
			continue // SSE comments, event names, keep-alives
		}
		var ev event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return -1, fmt.Errorf("opensandbox: decode exec event: %w", err)
		}
		switch ev.Type {
		case "stdout", "stderr":
			// Events are line-based. execd 1.2 keeps each line's terminator;
			// execd 1.1 (still the default image) strips it, so restore one.
			// A final fragment without a newline therefore gains one.
			text := ev.Text
			if !strings.HasSuffix(text, "\n") && !strings.HasSuffix(text, "\r") {
				text += "\n"
			}
			if err := emit(api.ExecEvent{Stream: ev.Type, Data: text}); err != nil {
				return -1, err
			}
		case "execution_complete":
			code := 0
			if ev.ExitCode != nil {
				code = *ev.ExitCode
			}
			return code, nil
		case "error":
			if ev.Error != nil && ev.Error.Ename == "CommandExecError" {
				if ev.Error.ExitCode != nil {
					return *ev.Error.ExitCode, nil
				}
				if n, err := strconv.Atoi(strings.TrimSpace(ev.Error.Evalue)); err == nil {
					return n, nil
				}
			}
			msg := "command failed"
			if ev.Error != nil {
				msg = ev.Error.Ename + ": " + ev.Error.Evalue
			}
			return -1, api.Errorf(api.CodeInvalidRequest, "opensandbox: %s", msg)
		}
	}
	if err := sc.Err(); err != nil {
		return -1, err
	}
	return -1, adapter.Unavailable(errors.New("opensandbox: exec stream ended without a completion event"))
}

// Exec implements adapter.ExecutionAdapter.
func (a *Adapter) Exec(ctx context.Context, ref string, req api.ExecRequest) (api.ExecResult, error) {
	var stdout, stderr strings.Builder
	start := time.Now()
	code, err := a.run(ctx, ref, req, func(ev api.ExecEvent) error {
		if ev.Stream == "stderr" {
			stderr.WriteString(ev.Data)
		} else {
			stdout.WriteString(ev.Data)
		}
		return nil
	})
	if err != nil {
		return api.ExecResult{}, err
	}
	return api.ExecResult{ExitCode: code, Stdout: stdout.String(), Stderr: stderr.String(), DurationMs: time.Since(start).Milliseconds()}, nil
}

// ExecStream implements adapter.Streamer.
func (a *Adapter) ExecStream(ctx context.Context, ref string, req api.ExecRequest, emit func(api.ExecEvent) error) (int, error) {
	return a.run(ctx, ref, req, emit)
}

func (a *Adapter) ReadFile(ctx context.Context, ref, path string) ([]byte, error) {
	resp, err := a.execdDo(ctx, ref, http.MethodGet, "/files/download?path="+url.QueryEscape(path), nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 100<<20))
}

func (a *Adapter) WriteFile(ctx context.Context, ref, path string, data []byte) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	// execd reads the metadata as a JSON file part (as the official SDKs send
	// it), not a plain form field.
	meta, _ := json.Marshal(map[string]any{"path": path, "mode": 644})
	mh := textproto.MIMEHeader{}
	mh.Set("Content-Disposition", `form-data; name="metadata"; filename="metadata"`)
	mh.Set("Content-Type", "application/json")
	mp, err := w.CreatePart(mh)
	if err != nil {
		return err
	}
	if _, err := mp.Write(meta); err != nil {
		return err
	}
	part, err := w.CreateFormFile("file", path[strings.LastIndex(path, "/")+1:])
	if err != nil {
		return err
	}
	if _, err := part.Write(data); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	resp, err := a.execdDo(ctx, ref, http.MethodPost, "/files/upload", buf.Bytes(), w.FormDataContentType())
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (a *Adapter) ListFiles(ctx context.Context, ref, path string) ([]api.FileInfo, error) {
	resp, err := a.execdDo(ctx, ref, http.MethodGet, "/directories/list?depth=1&path="+url.QueryEscape(path), nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var items []struct {
		Path       string `json:"path"`
		Type       string `json:"type"`
		Size       int64  `json:"size"`
		ModifiedAt string `json:"modified_at"`
		Mode       int    `json:"mode"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&items); err != nil {
		return nil, err
	}
	dir := strings.TrimRight(path, "/")
	out := make([]api.FileInfo, 0, len(items))
	for _, it := range items {
		if strings.TrimRight(it.Path, "/") == dir {
			continue // some runtimes include the directory itself
		}
		mt, _ := time.Parse(time.RFC3339, it.ModifiedAt)
		out = append(out, api.FileInfo{Path: it.Path, Name: it.Path[strings.LastIndex(it.Path, "/")+1:], Size: it.Size,
			IsDir: it.Type == "directory" || it.Type == "dir", Mode: strconv.Itoa(it.Mode), ModTime: mt})
	}
	return out, nil
}

func (a *Adapter) Pause(ctx context.Context, ref string) error {
	if err := a.call(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(ref)+"/pause", nil, nil); err != nil {
		return err
	}
	_, err := a.waitFor(ctx, ref, "Paused")
	return err
}

func (a *Adapter) Resume(ctx context.Context, ref string) error {
	if err := a.call(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(ref)+"/resume", nil, nil); err != nil {
		return err
	}
	if _, err := a.waitFor(ctx, ref, "Running"); err != nil {
		return err
	}
	return a.waitExecd(ctx, ref)
}

// Snapshot captures a sandbox; what is captured (filesystem, or memory too)
// depends on the OpenSandbox runtime, so it is reported as the pool declares.
func (a *Adapter) Snapshot(ctx context.Context, ref, kind string) (adapter.Snapshot, error) {
	if kind == "memory" && !slices.Contains(a.opts.Features, adapter.FeatureSnapshotMem) {
		return adapter.Snapshot{}, adapter.Unsupported("snapshots.memory")
	}
	var snap struct {
		ID     string `json:"id"`
		Status status `json:"status"`
	}
	if err := a.call(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(ref)+"/snapshots", map[string]string{"name": "differently-" + ref}, &snap); err != nil {
		return adapter.Snapshot{}, err
	}
	err := httpx.Poll(ctx, 500*time.Millisecond, a.opts.ReadyTimeout, func() (bool, error) {
		switch snap.Status.State {
		case "Ready":
			return true, nil
		case "Failed":
			return false, adapter.Unavailable(fmt.Errorf("opensandbox snapshot failed: %s %s", snap.Status.Reason, snap.Status.Message))
		}
		return false, a.call(ctx, http.MethodGet, "/snapshots/"+url.PathEscape(snap.ID), nil, &snap)
	})
	if kind == "" {
		kind = "disk"
	}
	return adapter.Snapshot{Ref: snap.ID, Kind: kind}, err
}

func (a *Adapter) DeleteSnapshot(ctx context.Context, ref string) error {
	err := a.call(ctx, http.MethodDelete, "/snapshots/"+url.PathEscape(ref), nil, nil)
	if api.IsCode(err, api.CodeNotFound) {
		return nil
	}
	return err
}

// ExposePort returns a direct endpoint for a sandbox port. Endpoints that
// need headers are refused: the only header a proxied endpoint needs is the
// pool's own API key, which must never be handed to tenants.
func (a *Adapter) ExposePort(ctx context.Context, ref string, port int) (string, error) {
	var out struct {
		Endpoint string            `json:"endpoint"`
		Headers  map[string]string `json:"headers"`
	}
	if err := a.call(ctx, http.MethodGet, fmt.Sprintf("/sandboxes/%s/endpoints/%d?use_server_proxy=false", url.PathEscape(ref), port), nil, &out); err != nil {
		return "", err
	}
	if len(out.Headers) > 0 {
		return "", api.Errorf(api.CodeUnsupportedOperation, "this OpenSandbox deployment only serves port %d with request headers, which Differently cannot hand out", port).With("operation", adapter.FeaturePorts)
	}
	if !strings.HasPrefix(out.Endpoint, "http") {
		out.Endpoint = "http://" + out.Endpoint
	}
	return out.Endpoint, nil
}

var (
	_ adapter.ExecutionAdapter = (*Adapter)(nil)
	_ adapter.Streamer         = (*Adapter)(nil)
	_ adapter.FileSystem       = (*Adapter)(nil)
	_ adapter.Pauser           = (*Adapter)(nil)
	_ adapter.Snapshotter      = (*Adapter)(nil)
	_ adapter.PortExposer      = (*Adapter)(nil)
)
