// Package morph adapts Morph Cloud's hosted VM API (/api).
//
// Morph Cloud is hosted-only, so pools on this adapter are always
// TrustExternal. Notable differences from other backends, handled here:
//
//   - instances start from snapshots, not images: without a template the
//     adapter builds (once, keyed by digest) a base snapshot of the pool's
//     image at the requested size;
//   - there is no stop that keeps the instance: deleting is stopping, so only
//     pause/resume (memory and disk) is offered;
//   - exec takes no env, cwd, stdin or timeout: env and cwd are written into
//     the command, a timeout runs it under coreutils' timeout, stdin is
//     unsupported;
//   - there is no HTTP files API (the SDKs use SFTP), so files move through
//     exec with base64 and need base64 and GNU find in the image;
//   - snapshots always capture memory and disk.
//
// Verified against Morph Cloud's published OpenAPI spec and SDKs
// (2026-10-28), not yet against a live account.
package morph

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/different-ai/differently/internal/adapters/httpx"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// DefaultAPIURL is Morph Cloud's API.
const DefaultAPIURL = "https://cloud.morph.so/api"

// Metadata keys. Morph metadata is a flat string map.
const (
	metaSandbox      = "differently-sandbox-id"
	metaInstallation = "differently-installation"
	metaOrg          = "differently-org"
	metaProject      = "differently-project"
)

// Exit codes the file scripts use to report a missing or wrong-kind path.
const (
	exitNotFound = 44
	exitNotFile  = 45
	chunkBytes   = 48 << 10 // raw bytes per exec; base64 stays well under the 128 KiB argument limit
)

// Options configures the adapter. Pool config keys: apiURL, snapshot (the
// snapshot to start from), image (for the base snapshot when there is no
// snapshot; default morphvm-minimal) and portAuth (none|api_key).
type Options struct {
	APIURL       string
	APIKey       string
	Snapshot     string
	Image        string
	PortAuth     string
	ReadyTimeout time.Duration
}

// FromConfig maps pool config to options.
func FromConfig(cfg map[string]string, apiKey string) Options {
	o := Options{APIURL: cfg["apiURL"], APIKey: apiKey, Snapshot: cfg["snapshot"], Image: cfg["image"], PortAuth: cfg["portAuth"]}
	if o.APIURL == "" {
		o.APIURL = DefaultAPIURL
	}
	if o.Image == "" {
		o.Image = "morphvm-minimal"
	}
	if o.PortAuth == "" {
		o.PortAuth = "none"
	}
	return o
}

// Adapter talks to Morph Cloud.
type Adapter struct {
	opts Options
	c    *httpx.Client
}

// New returns a Morph Cloud adapter.
func New(o Options) *Adapter {
	if o.ReadyTimeout == 0 {
		o.ReadyTimeout = 3 * time.Minute
	}
	c := httpx.New("morph", o.APIURL, o.APIKey)
	c.HTTP.Transport = errorBodies{next: http.DefaultTransport}
	return &Adapter{opts: o, c: c}
}

// Endpoint is reported in the trust report.
func (a *Adapter) Endpoint() string { return a.opts.APIURL }

// errorBodies rewrites Morph's FastAPI error bodies ({"detail": ..., "error_code": ...})
// into the {"message", "code"} shape httpx reads, so provider messages and
// codes survive into Differently errors.
type errorBodies struct{ next http.RoundTripper }

func (t errorBodies) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(r)
	if err != nil || resp.StatusCode < 300 {
		return resp, err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	var body struct {
		Detail    json.RawMessage `json:"detail"`
		ErrorCode string          `json:"error_code"`
	}
	out := map[string]string{}
	if json.Unmarshal(raw, &body) == nil && len(body.Detail) > 0 {
		var text string
		var items []struct {
			Msg string `json:"msg"`
		}
		switch {
		case json.Unmarshal(body.Detail, &text) == nil:
			out["message"] = text
		case json.Unmarshal(body.Detail, &items) == nil:
			msgs := make([]string, 0, len(items))
			for _, it := range items {
				msgs = append(msgs, it.Msg)
			}
			out["message"] = strings.Join(msgs, "; ")
		}
		if body.ErrorCode != "" {
			out["code"] = body.ErrorCode
		}
	}
	b, _ := json.Marshal(out)
	resp.Body, resp.ContentLength = io.NopCloser(bytes.NewReader(b)), int64(len(b))
	resp.Header.Del("Content-Length")
	return resp, nil
}

type instance struct {
	ID       string            `json:"id"`
	Status   string            `json:"status"`
	Metadata map[string]string `json:"metadata"`
	Refs     struct {
		SnapshotID string `json:"snapshot_id"`
	} `json:"refs"`
	Networking struct {
		HTTPServices []struct {
			Name string `json:"name"`
			Port int    `json:"port"`
			URL  string `json:"url"`
		} `json:"http_services"`
	} `json:"networking"`
}

type snapshot struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

func (a *Adapter) Info() adapter.Info {
	return adapter.Info{Kind: "morph", Version: "1", Trust: api.TrustExternal, Isolation: "virtual machine (Morph Cloud-managed)", Protocol: adapter.ProtocolVersion, Description: "Morph Cloud VMs at " + a.opts.APIURL + "; exec has no stdin; files move through exec"}
}

func (a *Adapter) Capabilities(context.Context) (adapter.Capabilities, error) {
	return adapter.Capabilities{Features: []string{
		adapter.FeatureExec, adapter.FeatureExecStream, adapter.FeatureFiles, adapter.FeaturePauseResume,
		adapter.FeatureSnapshotMem, adapter.FeatureSnapshotDisk, adapter.FeatureFork, adapter.FeaturePorts,
		adapter.FeatureMicroVM, adapter.FeatureArchAMD64,
	}}, nil
}

func (a *Adapter) Capacity(context.Context) (adapter.Capacity, error) { return adapter.Capacity{}, nil }

func (a *Adapter) Health(ctx context.Context) error {
	return a.call(ctx, http.MethodGet, "/instance/list?page=1&limit=1", nil, nil)
}

func instancePath(ref, rest string) string { return "/instance/" + url.PathEscape(ref) + rest }

// call wraps httpx with Morph's 412 (a readiness or paused precondition).
func (a *Adapter) call(ctx context.Context, method, path string, in, out any) error {
	err := a.c.JSON(ctx, method, path, in, out)
	var e *api.Error
	if errors.As(err, &e) && e.Details["providerStatus"] == "412" {
		e.Code = api.CodeInvalidState
	}
	return err
}

func state(i instance) string {
	switch i.Status {
	case "ready", "saving": // saving: a snapshot of a live instance is in progress
		return adapter.StateRunning
	case "paused":
		return adapter.StatePaused
	case "error":
		return adapter.StateError
	}
	return adapter.StatePending
}

func inst(i instance) adapter.Instance {
	return adapter.Instance{Ref: i.ID, SandboxID: i.Metadata[metaSandbox], State: state(i)}
}

func (a *Adapter) get(ctx context.Context, ref string) (instance, error) {
	var i instance
	return i, a.call(ctx, http.MethodGet, instancePath(ref, ""), nil, &i)
}

// find pages through every instance with a metadata value, failing rather
// than returning part of the list.
func (a *Adapter) find(ctx context.Context, key, value string) ([]instance, error) {
	var out []instance
	for page := 1; ; page++ {
		var res struct {
			Instances []instance `json:"instances"`
			HasNext   bool       `json:"has_next"`
		}
		path := fmt.Sprintf("/instance/list?page=%d&limit=100&%s=%s", page, url.QueryEscape("metadata["+key+"]"), url.QueryEscape(value))
		if err := a.call(ctx, http.MethodGet, path, nil, &res); err != nil {
			return nil, err
		}
		out = append(out, res.Instances...)
		if !res.HasNext || len(res.Instances) == 0 {
			return out, nil
		}
	}
}

func (a *Adapter) waitInstance(ctx context.Context, ref, want string) (instance, error) {
	var i instance
	err := httpx.Poll(ctx, time.Second, a.opts.ReadyTimeout, func() (bool, error) {
		var err error
		if i, err = a.get(ctx, ref); err != nil {
			return false, err
		}
		if i.Status == "error" {
			return false, adapter.SandboxFailed(fmt.Errorf("morph instance %s failed", ref))
		}
		return state(i) == want, nil
	})
	return i, err
}

func (a *Adapter) waitSnapshot(ctx context.Context, id string) error {
	return httpx.Poll(ctx, time.Second, a.opts.ReadyTimeout, func() (bool, error) {
		var s snapshot
		if err := a.call(ctx, http.MethodGet, "/snapshot/"+url.PathEscape(id), nil, &s); err != nil {
			return false, err
		}
		if s.Status == "failed" {
			return false, adapter.SandboxFailed(fmt.Errorf("morph snapshot %s failed", id))
		}
		return s.Status == "ready", nil
	})
}

// baseSnapshot returns a snapshot of the pool's image at the requested size,
// creating it once: the digest names the image and size, so later sandboxes
// of the same shape reuse it.
func (a *Adapter) baseSnapshot(ctx context.Context, r api.Resources) (string, error) {
	vcpus, mem, disk := max(r.CPU, 1), r.MemoryMiB, r.DiskGiB*1024
	if mem == 0 {
		mem = 2048
	}
	if disk == 0 {
		disk = 10240
	}
	digest := fmt.Sprintf("differently-%s-%dc-%dm-%dd", a.opts.Image, vcpus, mem, disk)
	var list struct {
		Data []snapshot `json:"data"`
	}
	if err := a.call(ctx, http.MethodGet, "/snapshot?digest="+url.QueryEscape(digest), nil, &list); err != nil {
		return "", err
	}
	var s snapshot
	for _, c := range list.Data {
		if c.Status == "ready" || c.Status == "pending" {
			s = c
			break
		}
	}
	if s.ID == "" {
		body := map[string]any{"image_id": a.opts.Image, "vcpus": vcpus, "memory": mem, "disk_size": disk, "digest": digest}
		err := a.call(ctx, http.MethodPost, "/snapshot", body, &s)
		if api.IsCode(err, api.CodeNotFound) {
			return "", api.Errorf(api.CodeInvalidRequest, "morph: image %q does not exist", a.opts.Image)
		}
		if err != nil {
			return "", err
		}
	}
	if s.Status != "ready" {
		if err := a.waitSnapshot(ctx, s.ID); err != nil {
			return "", err
		}
	}
	return s.ID, nil
}

func (a *Adapter) Create(ctx context.Context, req adapter.CreateRequest) (adapter.Instance, error) {
	existing, err := a.find(ctx, metaSandbox, req.ID)
	if err != nil {
		return adapter.Instance{}, err
	}
	var i instance
	for _, e := range existing {
		if e.Status == "error" {
			// A failed attempt: clear it so this retry starts afresh.
			if err := a.Delete(ctx, e.ID); err != nil {
				return adapter.Instance{}, err
			}
			continue
		}
		i = e
	}
	if i.ID == "" {
		snap := a.opts.Snapshot
		if req.Template != "" {
			snap = req.Template
		}
		if req.FromSnapshot != "" {
			snap = req.FromSnapshot
		}
		if snap == "" {
			if snap, err = a.baseSnapshot(ctx, req.Resources); err != nil {
				return adapter.Instance{}, err
			}
		}
		labels := adapter.OwnershipLabels(req)
		meta := map[string]string{metaSandbox: req.ID, metaInstallation: req.Installation}
		if v := labels[adapter.LabelOrg]; v != "" {
			meta[metaOrg] = v
		}
		if v := labels[adapter.LabelProject]; v != "" {
			meta[metaProject] = v
		}
		body := map[string]any{"snapshot_id": snap, "metadata": meta}
		if req.TTLSeconds > 0 {
			// Backstop in case this control plane is lost: Morph's "stop" deletes.
			body["ttl_seconds"], body["ttl_action"] = req.TTLSeconds, "stop"
		}
		err := a.call(ctx, http.MethodPost, "/instance", body, &i)
		if api.IsCode(err, api.CodeNotFound) {
			return adapter.Instance{}, api.Errorf(api.CodeInvalidRequest, "morph: snapshot %q does not exist", snap)
		}
		if err != nil {
			return adapter.Instance{}, err
		}
	}
	if state(i) != adapter.StateRunning {
		if i, err = a.waitInstance(ctx, i.ID, adapter.StateRunning); err != nil {
			return adapter.Instance{}, err
		}
	}
	return inst(i), nil
}

func (a *Adapter) Get(ctx context.Context, ref string) (adapter.Instance, error) {
	i, err := a.get(ctx, ref)
	if err != nil {
		return adapter.Instance{}, err
	}
	return inst(i), nil
}

func (a *Adapter) List(ctx context.Context, installation string) ([]adapter.Instance, error) {
	items, err := a.find(ctx, metaInstallation, installation)
	if err != nil {
		return nil, err
	}
	out := make([]adapter.Instance, 0, len(items))
	for _, i := range items {
		out = append(out, inst(i))
	}
	return out, nil
}

func (a *Adapter) Delete(ctx context.Context, ref string) error {
	err := a.call(ctx, http.MethodDelete, instancePath(ref, ""), nil, nil)
	if api.IsCode(err, api.CodeNotFound) {
		return nil
	}
	return err
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// script renders req as one shell command: env exported, then cwd, then the
// command, under coreutils' timeout when one is set.
func script(req api.ExecRequest) (string, error) {
	if req.Stdin != "" {
		return "", api.Errorf(api.CodeUnsupportedOperation, "the morph backend does not support exec stdin").With("operation", "exec.stdin")
	}
	keys := make([]string, 0, len(req.Env))
	for k := range req.Env {
		if !envName.MatchString(k) {
			return "", api.Errorf(api.CodeInvalidRequest, "invalid environment variable name %q", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	if len(keys) > 0 {
		b.WriteString("export")
		for _, k := range keys {
			b.WriteString(" " + k + "=" + httpx.ShellQuote([]string{req.Env[k]}))
		}
		b.WriteString("; ")
	}
	b.WriteString(httpx.CommandLine(req))
	if req.TimeoutSeconds > 0 {
		return "timeout " + strconv.Itoa(req.TimeoutSeconds) + " sh -c " + httpx.ShellQuote([]string{b.String()}), nil
	}
	return b.String(), nil
}

// run sends one shell command. The SDKs send the whole command as a single
// element, which Morph runs through the VM's shell.
func (a *Adapter) run(ctx context.Context, ref, cmd string) (api.ExecResult, error) {
	var out struct {
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	start := time.Now()
	if err := a.call(ctx, http.MethodPost, instancePath(ref, "/exec"), map[string]any{"command": []string{cmd}}, &out); err != nil {
		return api.ExecResult{}, err
	}
	return api.ExecResult{ExitCode: out.ExitCode, Stdout: out.Stdout, Stderr: out.Stderr, DurationMs: time.Since(start).Milliseconds()}, nil
}

func (a *Adapter) Exec(ctx context.Context, ref string, req api.ExecRequest) (api.ExecResult, error) {
	cmd, err := script(req)
	if err != nil {
		return api.ExecResult{}, err
	}
	return a.run(ctx, ref, cmd)
}

// ExecStream reads /exec/sse. The OpenAPI spec documents named events with
// plain-text data (event: stdout, data: ...), while both SDKs read JSON data
// ({"type": "stdout", "content": ...} and a final [DONE]); both are accepted.
func (a *Adapter) ExecStream(ctx context.Context, ref string, req api.ExecRequest, emit func(api.ExecEvent) error) (int, error) {
	cmd, err := script(req)
	if err != nil {
		return -1, err
	}
	b, _ := json.Marshal(map[string]any{"command": []string{cmd}})
	resp, err := a.c.Do(ctx, http.MethodPost, instancePath(ref, "/exec/sse"), bytes.NewReader(b), "application/json", http.Header{"Cache-Control": {"no-cache"}})
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()
	exit := -1
	exited := false
	// dispatch handles one SSE event and reports whether the stream is done.
	dispatch := func(event, data string) (bool, error) {
		if event == "" && strings.HasPrefix(data, "{") {
			var m struct {
				Type    string          `json:"type"`
				Content json.RawMessage `json:"content"`
			}
			if err := json.Unmarshal([]byte(data), &m); err != nil {
				return true, adapter.Unavailable(fmt.Errorf("morph: unreadable exec event: %w", err))
			}
			var content string
			if json.Unmarshal(m.Content, &content) != nil {
				content = string(m.Content) // a bare number
			}
			event, data = m.Type, content
		}
		switch event {
		case "stdout", "stderr":
			return false, emit(api.ExecEvent{Stream: event, Data: data})
		case "exit", "exit_code":
			code, err := strconv.Atoi(strings.TrimSpace(data))
			if err != nil {
				return true, adapter.Unavailable(fmt.Errorf("morph: unreadable exit code %q", data))
			}
			exit, exited = code, true
			return false, nil
		case "error":
			return true, adapter.SandboxFailed(fmt.Errorf("morph: exec failed in instance %s: %s", ref, data))
		}
		return data == "[DONE]", nil
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	var event string
	var data []string
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if len(data) > 0 || event != "" {
				done, err := dispatch(event, strings.Join(data, "\n"))
				if err != nil {
					return -1, err
				}
				if done {
					break
				}
			}
			event, data = "", nil
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			data = append(data, value)
		}
	}
	if err := sc.Err(); err != nil {
		return -1, adapter.Unavailable(fmt.Errorf("morph: exec stream: %w", err))
	}
	if len(data) > 0 && !exited {
		if _, err := dispatch(event, strings.Join(data, "\n")); err != nil {
			return -1, err
		}
	}
	if !exited {
		return -1, adapter.Unavailable(errors.New("morph: exec stream ended without an exit code"))
	}
	return exit, nil
}

func quote(s string) string { return httpx.ShellQuote([]string{s}) }

// fileError turns a file script's exit status into an error.
func fileError(path string, res api.ExecResult) error {
	switch res.ExitCode {
	case 0:
		return nil
	case exitNotFound:
		return api.Errorf(api.CodeNotFound, "%s does not exist", path)
	case exitNotFile:
		return api.Errorf(api.CodeInvalidRequest, "%s is not a regular file", path)
	}
	return api.Errorf(api.CodeInvalidRequest, "morph: file operation on %s failed: %s", path, strings.TrimSpace(res.Stderr))
}

func (a *Adapter) ReadFile(ctx context.Context, ref, path string) ([]byte, error) {
	p := quote(path)
	res, err := a.run(ctx, ref, fmt.Sprintf("[ -e %s ] || exit %d; [ -f %s ] || exit %d; base64 < %s", p, exitNotFound, p, exitNotFile, p))
	if err != nil {
		return nil, err
	}
	if err := fileError(path, res); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(strings.Join(strings.Fields(res.Stdout), ""))
}

// WriteFile sends the file in chunks small enough for one shell argument.
func (a *Adapter) WriteFile(ctx context.Context, ref, path string, data []byte) error {
	p := quote(path)
	first := "mkdir -p -- \"$(dirname -- " + p + ")\" && : > " + p
	for off := 0; ; off += chunkBytes {
		cmd := "printf %s " + quote(base64.StdEncoding.EncodeToString(data[off:min(off+chunkBytes, len(data))])) + " | base64 -d >> " + p
		if off == 0 {
			cmd = first + " && " + cmd
		}
		res, err := a.run(ctx, ref, cmd)
		if err != nil {
			return err
		}
		if err := fileError(path, res); err != nil {
			return err
		}
		if off+chunkBytes >= len(data) {
			return nil
		}
	}
}

func (a *Adapter) ListFiles(ctx context.Context, ref, path string) ([]api.FileInfo, error) {
	p := quote(path)
	res, err := a.run(ctx, ref, fmt.Sprintf(`[ -d %s ] || exit %d; find %s -mindepth 1 -maxdepth 1 -printf '%%y\t%%s\t%%m\t%%T@\t%%p\n'`, p, exitNotFound, p))
	if err != nil {
		return nil, err
	}
	if err := fileError(path, res); err != nil {
		return nil, err
	}
	var items []api.FileInfo
	for _, line := range strings.Split(strings.TrimRight(res.Stdout, "\n"), "\n") {
		f := strings.SplitN(line, "\t", 5)
		if len(f) != 5 {
			continue
		}
		size, _ := strconv.ParseInt(f[1], 10, 64)
		secs, _ := strconv.ParseFloat(f[3], 64)
		items = append(items, api.FileInfo{
			Path: f[4], Name: f[4][strings.LastIndex(f[4], "/")+1:], Size: size, IsDir: f[0] == "d",
			Mode: "0" + f[2], ModTime: time.Unix(0, int64(secs*1e9)).UTC(),
		})
	}
	return items, nil
}

// Pause snapshots memory and disk and releases compute; the instance bills
// storage only while paused.
func (a *Adapter) Pause(ctx context.Context, ref string) error {
	var i instance
	if err := a.call(ctx, http.MethodPost, instancePath(ref, "/pause"), nil, &i); err != nil {
		return err
	}
	if i.Status == "paused" {
		return nil
	}
	_, err := a.waitInstance(ctx, ref, adapter.StatePaused)
	return err
}

func (a *Adapter) Resume(ctx context.Context, ref string) error {
	if err := a.call(ctx, http.MethodPost, instancePath(ref, "/resume"), nil, nil); err != nil {
		return err
	}
	_, err := a.waitInstance(ctx, ref, adapter.StateRunning)
	return err
}

// SetExpiry moves Morph's TTL backstop; the TTL counts from now.
func (a *Adapter) SetExpiry(ctx context.Context, ref string, at time.Time) error {
	secs := max(int(time.Until(at).Seconds()+0.999), 1)
	return a.call(ctx, http.MethodPost, instancePath(ref, "/ttl"), map[string]any{"ttl_seconds": secs, "ttl_action": "stop"}, nil)
}

// Snapshot always captures memory and disk on Morph Cloud.
func (a *Adapter) Snapshot(ctx context.Context, ref, _ string) (adapter.Snapshot, error) {
	var s snapshot
	body := map[string]any{"metadata": map[string]string{"differently-source-instance": ref}}
	if err := a.call(ctx, http.MethodPost, instancePath(ref, "/snapshot"), body, &s); err != nil {
		return adapter.Snapshot{}, err
	}
	if s.Status != "ready" {
		if err := a.waitSnapshot(ctx, s.ID); err != nil {
			return adapter.Snapshot{}, err
		}
	}
	return adapter.Snapshot{Ref: s.ID, Kind: "memory"}, nil
}

func (a *Adapter) DeleteSnapshot(ctx context.Context, ref string) error {
	err := a.call(ctx, http.MethodDelete, "/snapshot/"+url.PathEscape(ref), nil, nil)
	if api.IsCode(err, api.CodeNotFound) {
		return nil
	}
	return err
}

// ExposePort publishes a port as an HTTP service. With portAuth none (the
// default) the URL is public: anyone with it can connect. With api_key,
// callers must send the pool's Morph API key.
func (a *Adapter) ExposePort(ctx context.Context, ref string, port int) (string, error) {
	name := "port-" + strconv.Itoa(port)
	var i instance
	if err := a.call(ctx, http.MethodPost, instancePath(ref, "/http"), map[string]any{"name": name, "port": port, "auth_mode": a.opts.PortAuth}, &i); err != nil {
		return "", err
	}
	for _, s := range i.Networking.HTTPServices {
		if s.Name == name {
			return s.URL, nil
		}
	}
	return "", adapter.Unavailable(fmt.Errorf("morph did not report a URL for port %d of %s", port, ref))
}

var (
	_ adapter.ExecutionAdapter = (*Adapter)(nil)
	_ adapter.Streamer         = (*Adapter)(nil)
	_ adapter.FileSystem       = (*Adapter)(nil)
	_ adapter.Pauser           = (*Adapter)(nil)
	_ adapter.Expirer          = (*Adapter)(nil)
	_ adapter.Snapshotter      = (*Adapter)(nil)
	_ adapter.PortExposer      = (*Adapter)(nil)
)
