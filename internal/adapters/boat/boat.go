// Package boat adapts Boat's hosted sandbox API (/api/v1).
//
// Boat is hosted-only, so pools on this adapter are always TrustExternal.
// Notable differences from other backends, handled here:
//
//   - sizes are fixed (small, default, large, xlarge): the smallest one that
//     fits the requested resources is chosen;
//   - there is no image: every sandbox runs Boat's one Ubuntu image, and a
//     template or snapshot is a saved snapshot name;
//   - commands are one shell string with no env or stdin field, so per-exec
//     env is exported in the command, stdin is unsupported and commands are
//     limited to 600 s;
//   - files are reachable only under /home/user and /tmp, up to 5 MiB each;
//   - there is no pause: stop keeps the disk only, and snapshots are disk-only
//     saved snapshots;
//   - a sandbox's TTL stops it (keeping the disk); it does not delete it.
//
// Verified against Boat's published OpenAPI spec (2026-10-28), not yet
// against a live account.
package boat

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

// DefaultAPIURL is Boat's API.
const DefaultAPIURL = "https://boat.dev/api/v1"

// Limits Boat documents.
const (
	maxExecSeconds = 600
	maxFileBytes   = 5 << 20
	maxTTLSeconds  = 2592000
	maxLabels      = 20
)

// liveStates are every state but cancelled, which Boat reports once and then
// forgets. List passes them explicitly so stopped sandboxes are never left out.
const liveStates = "init,provisioning,provisioned,cloning,ready,idle,running,archiving,archived,error"

// size is one of Boat's fixed sandbox types.
type size struct {
	name             string
	cpu, mem, diskGB int
}

var sizes = []size{
	{"small", 2, 4096, 51},
	{"default", 4, 8192, 51},
	{"large", 8, 16384, 111},
	{"xlarge", 16, 32768, 251},
}

// Options configures the adapter. Pool config keys: apiURL, type (the
// smallest size to use), snapshot (a saved snapshot to start from),
// environment, org, autoStopSeconds (the provider auto-stop for sandboxes
// without a TTL; default off) and portAccess (token|public).
type Options struct {
	APIURL          string
	APIKey          string
	Type            string
	Snapshot        string
	Environment     string
	Org             string
	AutoStopSeconds int
	PortAccess      string
	ReadyTimeout    time.Duration
}

// FromConfig maps pool config to options.
func FromConfig(cfg map[string]string, apiKey string) Options {
	o := Options{APIURL: cfg["apiURL"], APIKey: apiKey, Type: cfg["type"], Snapshot: cfg["snapshot"], Environment: cfg["environment"], Org: cfg["org"], PortAccess: cfg["portAccess"]}
	o.AutoStopSeconds, _ = strconv.Atoi(cfg["autoStopSeconds"])
	if o.APIURL == "" {
		o.APIURL = DefaultAPIURL
	}
	return o
}

// Adapter talks to Boat.
type Adapter struct {
	opts Options
	c    *httpx.Client
}

// New returns a Boat adapter.
func New(o Options) *Adapter {
	if o.ReadyTimeout == 0 {
		o.ReadyTimeout = 5 * time.Minute // a stop saves the disk first and can take minutes
	}
	return &Adapter{opts: o, c: httpx.New("boat", o.APIURL, o.APIKey)}
}

// Endpoint is reported in the trust report.
func (a *Adapter) Endpoint() string { return a.opts.APIURL }

type sandbox struct {
	ID     string            `json:"id"`
	State  string            `json:"state"`
	Error  string            `json:"error"`
	Labels map[string]string `json:"labels"`
}

func (a *Adapter) Info() adapter.Info {
	return adapter.Info{Kind: "boat", Version: "1", Trust: api.TrustExternal, Isolation: "virtual machine with its own kernel (Boat-managed)", Protocol: adapter.ProtocolVersion, Description: "Boat sandboxes at " + a.opts.APIURL + "; exec has no stdin and runs at most 600 s; files only under /home/user and /tmp"}
}

func (a *Adapter) Capabilities(context.Context) (adapter.Capabilities, error) {
	largest := sizes[len(sizes)-1]
	return adapter.Capabilities{Features: []string{
		adapter.FeatureExec, adapter.FeatureExecStream, adapter.FeatureFiles, adapter.FeatureStopStart, adapter.FeaturePersistDisk,
		adapter.FeatureSnapshotDisk, adapter.FeatureFork, adapter.FeaturePorts, adapter.FeatureMicroVM, adapter.FeatureArchAMD64,
	}, MaxCPU: largest.cpu, MaxMemoryMiB: largest.mem, MaxDiskGiB: largest.diskGB}, nil
}

func (a *Adapter) Capacity(context.Context) (adapter.Capacity, error) { return adapter.Capacity{}, nil }

func (a *Adapter) Health(ctx context.Context) error {
	return a.call(ctx, http.MethodGet, a.path("/sandboxes", "limit=1"), nil, nil, nil)
}

// path adds the pool's org, when set, to a request path.
func (a *Adapter) path(p, query string) string {
	if a.opts.Org != "" {
		if query != "" {
			query += "&"
		}
		query += "org=" + url.QueryEscape(a.opts.Org)
	}
	if query == "" {
		return p
	}
	return p + "?" + query
}

func sandboxPath(ref, rest string) string { return "/sandboxes/" + url.PathEscape(ref) + rest }

// call sends JSON and decodes JSON, classifying Boat's own error codes.
func (a *Adapter) call(ctx context.Context, method, path string, header http.Header, in, out any) error {
	var body io.Reader
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ct = bytes.NewReader(b), "application/json"
	}
	resp, err := a.c.Do(ctx, method, path, body, ct, header)
	if err != nil {
		return classify(err)
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// classify refines httpx's status mapping with Boat's error codes.
func classify(err error) error {
	var e *api.Error
	if !errors.As(err, &e) {
		return err
	}
	switch e.Details["providerCode"] {
	case "no_ready_machine", "out_of_capacity":
		e.Code = adapter.CodeInsufficientCapacity // Boat's 503: no machine right now
	case "sandbox_direct_failed", "boat_direct_failed":
		e.Code = adapter.CodeSandboxFailed // this sandbox's machine did not answer
	case "machine_class_plan_required", "trial_machine_class_not_allowed":
		e.Code = api.CodeInvalidRequest
		e.Message = "boat: the account's plan does not allow this sandbox size; set a smaller config.type or reduce the requested resources (" + e.Details["providerCode"] + ")"
	}
	return e
}

func state(s sandbox) string {
	switch s.State {
	case "ready", "idle", "running":
		return adapter.StateRunning
	case "archived":
		return adapter.StateStopped
	case "error", "cancelled":
		return adapter.StateError
	}
	return adapter.StatePending
}

func inst(s sandbox) adapter.Instance {
	return adapter.Instance{Ref: s.ID, SandboxID: s.Labels[adapter.LabelSandboxID], State: state(s), Message: s.Error}
}

func (a *Adapter) get(ctx context.Context, ref string) (sandbox, error) {
	var out struct {
		Sandbox sandbox `json:"sandbox"`
	}
	err := a.call(ctx, http.MethodGet, a.path(sandboxPath(ref, ""), ""), nil, nil, &out)
	return out.Sandbox, err
}

// find pages through every sandbox carrying a label, failing rather than
// returning part of the list.
func (a *Adapter) find(ctx context.Context, key, value string) ([]sandbox, error) {
	var out []sandbox
	cursor := ""
	for {
		q := "limit=200&state=" + url.QueryEscape(liveStates) + "&label=" + url.QueryEscape(key+"="+value)
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		var page struct {
			Sandboxes []sandbox `json:"sandboxes"`
			PageInfo  struct {
				NextCursor *string `json:"nextCursor"`
				HasMore    bool    `json:"hasMore"`
			} `json:"pageInfo"`
		}
		if err := a.call(ctx, http.MethodGet, a.path("/sandboxes", q), nil, nil, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Sandboxes...)
		if !page.PageInfo.HasMore || page.PageInfo.NextCursor == nil || *page.PageInfo.NextCursor == "" {
			return out, nil
		}
		cursor = *page.PageInfo.NextCursor
	}
}

// sizeFor picks the smallest Boat size that fits r, starting at the pool's
// configured floor.
func (a *Adapter) sizeFor(r api.Resources) (string, error) {
	start := 0
	if a.opts.Type != "" {
		start = -1
		for i, s := range sizes {
			if s.name == a.opts.Type {
				start = i
			}
		}
		if start < 0 {
			return "", api.Errorf(api.CodeInvalidRequest, "boat: unknown config.type %q (small, default, large or xlarge)", a.opts.Type)
		}
	}
	for _, s := range sizes[start:] {
		if r.CPU <= s.cpu && r.MemoryMiB <= s.mem && r.DiskGiB <= s.diskGB {
			return s.name, nil
		}
	}
	return "", api.Errorf(api.CodeInvalidRequest, "boat: no sandbox size fits %d vCPU, %d MiB, %d GiB (the largest is 16 vCPU, 32 GiB, 251 GB)", r.CPU, r.MemoryMiB, r.DiskGiB)
}

var labelKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,62}$`)

// labels keeps the ownership labels and as many other valid labels as Boat
// accepts (20 at most), dropping any Boat would refuse.
func labels(req adapter.CreateRequest) map[string]string {
	all := adapter.OwnershipLabels(req)
	keys := make([]string, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}
	owned := func(k string) bool { return strings.HasPrefix(k, "differently.dev/") }
	sort.Slice(keys, func(i, j int) bool {
		if owned(keys[i]) != owned(keys[j]) {
			return owned(keys[i])
		}
		return keys[i] < keys[j]
	})
	out := map[string]string{}
	for _, k := range keys {
		if len(out) < maxLabels && labelKey.MatchString(k) && len(all[k]) <= 256 {
			out[k] = all[k]
		}
	}
	return out
}

func (a *Adapter) Create(ctx context.Context, req adapter.CreateRequest) (adapter.Instance, error) {
	existing, err := a.find(ctx, adapter.LabelSandboxID, req.ID)
	if err != nil {
		return adapter.Instance{}, err
	}
	var s sandbox
	for _, e := range existing {
		if state(e) == adapter.StateError {
			// A failed attempt: clear it so this retry starts afresh.
			if err := a.Delete(ctx, e.ID); err != nil {
				return adapter.Instance{}, err
			}
			continue
		}
		s = e
	}
	if s.ID == "" {
		if s, err = a.create(ctx, req); err != nil {
			return adapter.Instance{}, err
		}
	}
	err = httpx.Poll(ctx, time.Second, a.opts.ReadyTimeout, func() (bool, error) {
		if state(s) == adapter.StatePending {
			next, err := a.get(ctx, s.ID)
			if api.IsCode(err, api.CodeNotFound) {
				return false, adapter.SandboxFailed(fmt.Errorf("boat sandbox %s disappeared while starting", s.ID))
			}
			if err != nil {
				return false, err
			}
			if next.Labels == nil {
				next.Labels = s.Labels // a cancelled sandbox is reported with id, state and error only
			}
			s = next
		}
		switch {
		case s.State == "cancelled":
			return false, adapter.InsufficientCapacity("boat could not get a machine for sandbox " + s.ID)
		case state(s) == adapter.StateError:
			return false, adapter.SandboxFailed(fmt.Errorf("boat sandbox %s failed: %s", s.ID, s.Error))
		}
		return state(s) == adapter.StateRunning, nil
	})
	if err != nil {
		return adapter.Instance{}, err
	}
	return inst(s), nil
}

func (a *Adapter) create(ctx context.Context, req adapter.CreateRequest) (sandbox, error) {
	typ, err := a.sizeFor(req.Resources)
	if err != nil {
		return sandbox{}, err
	}
	body := map[string]any{"type": typ, "labels": labels(req)}
	if len(req.Env) > 0 {
		body["env"] = req.Env
	}
	from := a.opts.Snapshot
	if req.Template != "" {
		from = req.Template
	}
	if req.FromSnapshot != "" {
		from = req.FromSnapshot
	}
	if from != "" {
		body["from"] = from
	}
	if a.opts.Environment != "" {
		body["environment"] = a.opts.Environment
	}
	// Boat's TTL stops the sandbox (keeping its disk). Differently owns the
	// lifecycle, so it is only a backstop: the sandbox's TTL, else the pool's
	// autoStopSeconds, else off.
	switch {
	case req.TTLSeconds > 0:
		body["ttlSeconds"] = min(req.TTLSeconds, maxTTLSeconds)
	case a.opts.AutoStopSeconds > 0:
		body["ttlSeconds"] = min(a.opts.AutoStopSeconds, maxTTLSeconds)
	default:
		body["ttlSeconds"] = nil
	}
	var out struct {
		Sandbox sandbox `json:"sandbox"`
	}
	err = a.call(ctx, http.MethodPost, a.path("/sandboxes", ""), nil, body, &out)
	if api.IsCode(err, api.CodeNotFound) {
		// Boat answers 404 snapshot_not_found for an unknown "from".
		return sandbox{}, api.Errorf(api.CodeInvalidRequest, "boat: saved snapshot %q does not exist", from)
	}
	if out.Sandbox.Labels == nil {
		out.Sandbox.Labels = map[string]string{adapter.LabelSandboxID: req.ID}
	}
	return out.Sandbox, err
}

func (a *Adapter) Get(ctx context.Context, ref string) (adapter.Instance, error) {
	s, err := a.get(ctx, ref)
	if err != nil {
		return adapter.Instance{}, err
	}
	return inst(s), nil
}

func (a *Adapter) List(ctx context.Context, installation string) ([]adapter.Instance, error) {
	items, err := a.find(ctx, adapter.LabelInstallation, installation)
	if err != nil {
		return nil, err
	}
	out := make([]adapter.Instance, 0, len(items))
	for _, s := range items {
		if s.State != "cancelled" {
			out = append(out, inst(s))
		}
	}
	return out, nil
}

// Delete permanently deletes the sandbox and its disk in the background.
func (a *Adapter) Delete(ctx context.Context, ref string) error {
	h := http.Header{"X-Ascii-Confirm-Delete": {ref}}
	err := a.call(ctx, http.MethodDelete, a.path(sandboxPath(ref, ""), ""), h, nil, nil)
	if api.IsCode(err, api.CodeNotFound) {
		return nil
	}
	return err
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// commandLine renders req as Boat's single shell string: per-exec env is
// exported first, then cwd and the command.
func commandLine(req api.ExecRequest) (string, error) {
	if req.Stdin != "" {
		return "", api.Errorf(api.CodeUnsupportedOperation, "the boat backend does not support exec stdin").With("operation", "exec.stdin")
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
	return b.String(), nil
}

func (a *Adapter) Exec(ctx context.Context, ref string, req api.ExecRequest) (api.ExecResult, error) {
	var stdout, stderr strings.Builder
	start := time.Now()
	code, err := a.ExecStream(ctx, ref, req, func(e api.ExecEvent) error {
		if e.Stream == "stderr" {
			stderr.WriteString(e.Data)
		} else {
			stdout.WriteString(e.Data)
		}
		return nil
	})
	if err != nil {
		return api.ExecResult{}, err
	}
	return api.ExecResult{ExitCode: code, Stdout: stdout.String(), Stderr: stderr.String(), DurationMs: time.Since(start).Milliseconds()}, nil
}

// ExecStream runs the command with stream: true and relays Boat's NDJSON
// frames (started, stdout, stderr, then exit or error).
func (a *Adapter) ExecStream(ctx context.Context, ref string, req api.ExecRequest, emit func(api.ExecEvent) error) (int, error) {
	cmd, err := commandLine(req)
	if err != nil {
		return -1, err
	}
	timeout := req.TimeoutSeconds
	if timeout <= 0 || timeout > maxExecSeconds {
		timeout = maxExecSeconds // Boat's own default is 30 s; always send one
	}
	b, _ := json.Marshal(map[string]any{"command": cmd, "timeoutSeconds": timeout, "stream": true})
	resp, err := a.c.Do(ctx, http.MethodPost, a.path(sandboxPath(ref, "/commands"), ""), bytes.NewReader(b), "application/json", nil)
	if err != nil {
		return -1, classify(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var f struct {
			Type      string `json:"type"`
			Data      string `json:"data"`
			ExitCode  *int   `json:"exitCode"`
			TimedOut  bool   `json:"timedOut"`
			Error     string `json:"error"`
			Message   string `json:"message"`
			Retryable bool   `json:"retryable"`
		}
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			return -1, adapter.Unavailable(fmt.Errorf("boat: unreadable command stream: %w", err))
		}
		switch f.Type {
		case "stdout", "stderr":
			if err := emit(api.ExecEvent{Stream: f.Type, Data: f.Data}); err != nil {
				return -1, err
			}
		case "exit":
			switch {
			case f.ExitCode != nil:
				return *f.ExitCode, nil
			case f.TimedOut:
				return 124, nil
			}
			return -1, nil // killed by a signal
		case "error":
			if f.Retryable {
				return -1, api.Errorf(api.CodeInvalidState, "boat: sandbox %s is still restoring its disk; the command did not run (%s)", ref, f.Error)
			}
			return -1, adapter.SandboxFailed(fmt.Errorf("boat: command failed in sandbox %s: %s %s", ref, f.Error, f.Message))
		}
	}
	if err := sc.Err(); err != nil {
		return -1, adapter.Unavailable(fmt.Errorf("boat: command stream: %w", err))
	}
	return -1, adapter.Unavailable(errors.New("boat: command stream ended without an exit frame"))
}

func (a *Adapter) ReadFile(ctx context.Context, ref, path string) ([]byte, error) {
	var out struct {
		Content string `json:"content"`
	}
	q := "encoding=base64&path=" + url.QueryEscape(path)
	if err := a.call(ctx, http.MethodGet, a.path(sandboxPath(ref, "/files"), q), nil, nil, &out); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(out.Content)
}

func (a *Adapter) WriteFile(ctx context.Context, ref, path string, data []byte) error {
	if len(data) > maxFileBytes {
		return api.Errorf(api.CodeInvalidRequest, "boat: files are limited to 5 MiB through this backend (got %d bytes)", len(data))
	}
	if i := strings.LastIndex(path, "/"); i > 0 {
		if err := a.call(ctx, http.MethodPost, a.path(sandboxPath(ref, "/files/folder"), ""), nil, map[string]string{"path": path[:i]}, nil); err != nil {
			return err
		}
	}
	body := map[string]string{"path": path, "content": base64.StdEncoding.EncodeToString(data), "encoding": "base64"}
	return a.call(ctx, http.MethodPut, a.path(sandboxPath(ref, "/files"), ""), nil, body, nil)
}

func (a *Adapter) ListFiles(ctx context.Context, ref, path string) ([]api.FileInfo, error) {
	var out struct {
		Files []struct {
			Name       string `json:"name"`
			Path       string `json:"path"`
			IsDir      bool   `json:"isDir"`
			Size       int64  `json:"size"`
			Mode       string `json:"mode"`
			ModifiedAt string `json:"modifiedAt"`
		} `json:"files"`
	}
	if err := a.call(ctx, http.MethodGet, a.path(sandboxPath(ref, "/files/list"), "path="+url.QueryEscape(path)), nil, nil, &out); err != nil {
		return nil, err
	}
	items := make([]api.FileInfo, 0, len(out.Files))
	for _, f := range out.Files {
		mt, _ := time.Parse(time.RFC3339, f.ModifiedAt)
		items = append(items, api.FileInfo{Path: f.Path, Name: f.Name, Size: f.Size, IsDir: f.IsDir, Mode: f.Mode, ModTime: mt})
	}
	return items, nil
}

func (a *Adapter) waitState(ctx context.Context, ref, want string) error {
	return httpx.Poll(ctx, time.Second, a.opts.ReadyTimeout, func() (bool, error) {
		s, err := a.get(ctx, ref)
		if err != nil {
			return false, err
		}
		if s.State == "error" {
			return false, adapter.SandboxFailed(fmt.Errorf("boat sandbox %s failed: %s", ref, s.Error))
		}
		return state(s) == want, nil
	})
}

// Stop saves the disk and releases the machine; memory is not kept.
func (a *Adapter) Stop(ctx context.Context, ref string) error {
	if err := a.call(ctx, http.MethodPost, a.path(sandboxPath(ref, "/stop"), ""), nil, map[string]any{}, nil); err != nil {
		return err
	}
	return a.waitState(ctx, ref, adapter.StateStopped)
}

// Start resumes a stopped sandbox on a new machine with the same disk.
func (a *Adapter) Start(ctx context.Context, ref string) error {
	if err := a.call(ctx, http.MethodPost, a.path(sandboxPath(ref, "/resume"), ""), nil, map[string]any{}, nil); err != nil {
		return err
	}
	return a.waitState(ctx, ref, adapter.StateRunning)
}

// SetExpiry moves Boat's auto-stop deadline: the TTL counts from now.
func (a *Adapter) SetExpiry(ctx context.Context, ref string, at time.Time) error {
	secs := int(time.Until(at).Seconds() + 0.999)
	body := map[string]any{"ttlSeconds": min(max(secs, 1), maxTTLSeconds)}
	return a.call(ctx, http.MethodPatch, a.path(sandboxPath(ref, ""), ""), nil, body, nil)
}

// Snapshot saves the sandbox's disk as a saved snapshot; Boat cannot capture
// memory.
func (a *Adapter) Snapshot(ctx context.Context, ref, kind string) (adapter.Snapshot, error) {
	if kind == "memory" {
		return adapter.Snapshot{}, adapter.Unsupported(adapter.FeatureSnapshotMem)
	}
	name := "dly-" + strings.ToLower(strings.TrimPrefix(ref, "bx_")) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if err := a.call(ctx, http.MethodPost, a.path("/saved-snapshots", ""), nil, map[string]string{"sandboxId": ref, "name": name}, nil); err != nil {
		return adapter.Snapshot{}, err
	}
	var snap struct {
		Status    string `json:"status"`
		Error     string `json:"error"`
		SizeBytes int64  `json:"sizeBytes"`
	}
	err := httpx.Poll(ctx, time.Second, a.opts.ReadyTimeout, func() (bool, error) {
		var out struct {
			Snapshot json.RawMessage `json:"snapshot"`
		}
		if err := a.call(ctx, http.MethodGet, a.path("/saved-snapshots/"+url.PathEscape(name), ""), nil, nil, &out); err != nil {
			return false, err
		}
		if err := json.Unmarshal(out.Snapshot, &snap); err != nil {
			return false, err
		}
		if snap.Status == "failed" {
			return false, adapter.SandboxFailed(fmt.Errorf("boat could not save a snapshot of %s: %s", ref, snap.Error))
		}
		return snap.Status == "ready", nil
	})
	if err != nil {
		return adapter.Snapshot{}, err
	}
	return adapter.Snapshot{Ref: name, Kind: "disk", SizeBytes: snap.SizeBytes}, nil
}

func (a *Adapter) DeleteSnapshot(ctx context.Context, ref string) error {
	err := a.call(ctx, http.MethodDelete, a.path("/saved-snapshots/"+url.PathEscape(ref), ""), nil, nil, nil)
	if api.IsCode(err, api.CodeNotFound) {
		return nil
	}
	return err
}

// ExposePort hosts a port at https://<subdomain>-<port>.on.boat.dev. By
// default the URL carries an access token: anyone with the full URL can
// connect. portAccess: public drops the token.
func (a *Adapter) ExposePort(ctx context.Context, ref string, port int) (string, error) {
	var out struct {
		URL string `json:"url"`
	}
	body := map[string]any{"port": port, "public": a.opts.PortAccess == "public"}
	if err := a.call(ctx, http.MethodPost, a.path(sandboxPath(ref, "/host"), ""), nil, body, &out); err != nil {
		return "", err
	}
	return out.URL, nil
}

var (
	_ adapter.ExecutionAdapter = (*Adapter)(nil)
	_ adapter.Streamer         = (*Adapter)(nil)
	_ adapter.FileSystem       = (*Adapter)(nil)
	_ adapter.Lifecycle        = (*Adapter)(nil)
	_ adapter.Expirer          = (*Adapter)(nil)
	_ adapter.Snapshotter      = (*Adapter)(nil)
	_ adapter.PortExposer      = (*Adapter)(nil)
)
