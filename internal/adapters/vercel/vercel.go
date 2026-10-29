// Package vercel adapts Vercel Sandbox's REST API.
//
// Vercel runs every sandbox as a Firecracker microVM it operates, so pools on
// this adapter are always TrustExternal. A Vercel sandbox is a named,
// persistent resource; each time it runs, it runs as a session, and commands,
// files, stop and snapshots address the current session. The adapter names
// each sandbox after its Differently sandbox ID (names are unique per
// project, which makes Create idempotent) and tags it with the sandbox and
// installation IDs, which List filters on.
//
// Differences from other backends, handled here:
//
//   - a session has a hard maximum duration (45 minutes on Hobby, 24 hours on
//     Pro and Enterprise; config.maxTimeoutSeconds). When it ends, Vercel
//     snapshots the sandbox and stops it, and Start resumes it from there;
//   - taking a snapshot ends the session, so Snapshot resumes the sandbox
//     afterwards;
//   - memory is 2 GiB per vCPU, and vCPUs are 1 or an even number;
//   - ports must be declared when the sandbox is created (config.ports);
//   - there is no directory listing endpoint, so ListFiles runs find.
//
// Written against Vercel's REST API reference and OpenAPI document (read
// 2026-10-28) and tested with a fake server, not against a live account.
package vercel

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/different-ai/differently/internal/adapters/httpx"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// DefaultAPIURL is Vercel's REST API.
const DefaultAPIURL = "https://api.vercel.com"

// Tag keys stamped on every sandbox. Vercel allows five tags per sandbox.
const (
	tagSandbox      = "dly-sandbox"
	tagInstallation = "dly-installation"
)

// defaultMaxTimeout is the longest session the Hobby plan allows, so the
// default works on every plan.
const defaultMaxTimeout = 45 * time.Minute

// Options configures the adapter. Pool config keys: project (required),
// team, apiURL, region, image, architecture (amd64|arm64), ports (comma
// separated, at most 15, 1024-65535) and maxTimeoutSeconds.
type Options struct {
	APIURL       string
	Token        string
	Project      string
	Team         string
	Region       string
	Image        string
	Architecture string
	Ports        []int
	MaxTimeout   time.Duration
	ReadyTimeout time.Duration
}

// FromConfig maps pool config to options.
func FromConfig(cfg map[string]string, token string) (Options, error) {
	o := Options{APIURL: cfg["apiURL"], Token: token, Project: cfg["project"], Team: cfg["team"], Region: cfg["region"],
		Image: cfg["image"], Architecture: cfg["architecture"], MaxTimeout: defaultMaxTimeout}
	if o.APIURL == "" {
		o.APIURL = DefaultAPIURL
	}
	if o.Project == "" {
		return o, fmt.Errorf("the vercel adapter needs config.project, the Vercel project sandboxes are created in")
	}
	switch o.Architecture {
	case "":
		o.Architecture = "amd64"
	case "amd64", "arm64":
	default:
		return o, fmt.Errorf("vercel config.architecture must be amd64 or arm64")
	}
	if s := cfg["ports"]; s != "" {
		for _, p := range strings.Split(s, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(p))
			if err != nil || n < 1024 || n > 65535 {
				return o, fmt.Errorf("vercel config.ports: %q is not a port from 1024 to 65535", p)
			}
			o.Ports = append(o.Ports, n)
		}
		if len(o.Ports) > 15 {
			return o, fmt.Errorf("vercel config.ports: at most 15 ports")
		}
	}
	if s := cfg["maxTimeoutSeconds"]; s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return o, fmt.Errorf("vercel config.maxTimeoutSeconds must be a positive number of seconds")
		}
		o.MaxTimeout = time.Duration(n) * time.Second
	}
	return o, nil
}

// Adapter talks to Vercel.
type Adapter struct {
	opts Options
	c    *httpx.Client
}

// New returns a Vercel adapter.
func New(o Options) *Adapter {
	if o.ReadyTimeout == 0 {
		o.ReadyTimeout = 3 * time.Minute
	}
	if o.MaxTimeout == 0 {
		o.MaxTimeout = defaultMaxTimeout
	}
	if o.Architecture == "" {
		o.Architecture = "amd64"
	}
	c := httpx.New("vercel", o.APIURL, o.Token)
	c.HTTP.Transport = httpx.ErrorShape(nil, flatten)
	return &Adapter{opts: o, c: c}
}

// flatten reads Vercel's {"error": {"code", "message"}} error body.
func flatten(raw []byte) (string, string) {
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &body)
	return body.Error.Code, body.Error.Message
}

// Endpoint is reported in the trust report.
func (a *Adapter) Endpoint() string { return a.opts.APIURL }

type session struct {
	ID        string  `json:"id"`
	Status    string  `json:"status"`
	Region    string  `json:"region"`
	StartedAt float64 `json:"startedAt"` // epoch milliseconds
	Timeout   float64 `json:"timeout"`   // milliseconds
}

type sandbox struct {
	Name             string            `json:"name"`
	Status           string            `json:"status"`
	Region           string            `json:"region"`
	CurrentSessionID string            `json:"currentSessionId"`
	Tags             map[string]string `json:"tags"`
}

type route struct {
	Port      int    `json:"port"`
	Subdomain string `json:"subdomain"`
	URL       string `json:"url"`
}

// view is what create, get and update return.
type view struct {
	Sandbox sandbox  `json:"sandbox"`
	Session *session `json:"session"`
	Routes  []route  `json:"routes"`
}

func (v view) state() string {
	if s := v.Session; s != nil && (v.Sandbox.CurrentSessionID == "" || s.ID == v.Sandbox.CurrentSessionID) {
		switch s.Status {
		case "running":
			return adapter.StateRunning
		case "stopped":
			return adapter.StateStopped
		case "failed", "aborted":
			return adapter.StateError
		}
		return adapter.StatePending // pending, stopping, snapshotting
	}
	switch v.Sandbox.Status {
	case "running":
		return adapter.StateRunning
	case "stopped":
		return adapter.StateStopped
	}
	return adapter.StatePending
}

func (v view) instance() adapter.Instance {
	region := v.Sandbox.Region
	if v.Session != nil && v.Session.Region != "" {
		region = v.Session.Region
	}
	return adapter.Instance{Ref: v.Sandbox.Name, SandboxID: v.Sandbox.Tags[tagSandbox], State: v.state(), Region: region}
}

func (a *Adapter) Info() adapter.Info {
	return adapter.Info{Kind: "vercel", Version: "1", Trust: api.TrustExternal, Isolation: "Firecracker microVM (Vercel-managed)", Protocol: adapter.ProtocolVersion, Description: "Vercel Sandbox in project " + a.opts.Project}
}

func (a *Adapter) Capabilities(context.Context) (adapter.Capabilities, error) {
	f := []string{
		adapter.FeatureExec, adapter.FeatureExecStream, adapter.FeatureFiles, adapter.FeatureStopStart, adapter.FeaturePersistDisk,
		adapter.FeatureSnapshotDisk, adapter.FeatureFork, adapter.FeatureMicroVM, "arch." + a.opts.Architecture,
	}
	if len(a.opts.Ports) > 0 {
		f = append(f, adapter.FeaturePorts)
	}
	var regions []string
	if a.opts.Region != "" {
		regions = []string{a.opts.Region}
	}
	// The Pro plan's per-sandbox maximum; Hobby allows 4 vCPU, Enterprise 32.
	return adapter.Capabilities{Features: f, MaxCPU: 8, MaxMemoryMiB: 16384, Regions: regions}, nil
}

func (a *Adapter) Capacity(context.Context) (adapter.Capacity, error) { return adapter.Capacity{}, nil }

func (a *Adapter) Health(ctx context.Context) error {
	return a.c.JSON(ctx, http.MethodGet, a.path("/v2/sandboxes", url.Values{"project": {a.opts.Project}, "limit": {"1"}}), nil, nil)
}

// path adds the team and any query parameters to an API path.
func (a *Adapter) path(p string, q url.Values) string {
	if a.opts.Team != "" {
		if q == nil {
			q = url.Values{}
		}
		q.Set("teamId", a.opts.Team)
	}
	if len(q) == 0 {
		return p
	}
	return p + "?" + q.Encode()
}

func sessionPath(id, op string) string {
	return "/v2/sandboxes/sessions/" + url.PathEscape(id) + "/" + op
}

func (a *Adapter) get(ctx context.Context, name string, resume bool) (view, error) {
	q := url.Values{"projectId": {a.opts.Project}}
	if resume {
		q.Set("resume", "true") // a stopped sandbox starts a new session from its snapshot
	}
	var v view
	return v, a.c.JSON(ctx, http.MethodGet, a.path("/v2/sandboxes/"+url.PathEscape(name), q), nil, &v)
}

// waitRunning polls until the sandbox's session runs. A session that fails
// or ends before it runs is this sandbox's failure, not an outage.
func (a *Adapter) waitRunning(ctx context.Context, v view) (view, error) {
	err := httpx.Poll(ctx, time.Second, a.opts.ReadyTimeout, func() (bool, error) {
		switch v.state() {
		case adapter.StateRunning:
			return true, nil
		case adapter.StateError, adapter.StateStopped:
			status := v.Sandbox.Status
			if v.Session != nil {
				status = v.Session.Status
			}
			return false, adapter.SandboxFailed(fmt.Errorf("vercel sandbox %s did not start: session %s", v.Sandbox.Name, status))
		}
		var err error
		v, err = a.get(ctx, v.Sandbox.Name, false)
		return false, err
	})
	return v, err
}

func (a *Adapter) waitStopped(ctx context.Context, ref string) error {
	return httpx.Poll(ctx, time.Second, a.opts.ReadyTimeout, func() (bool, error) {
		v, err := a.get(ctx, ref, false)
		return err == nil && v.state() == adapter.StateStopped, err
	})
}

// vcpus fits a request to Vercel's sizes: 1 or an even number of vCPUs, with
// 2 GiB of memory each.
func vcpus(r api.Resources) int {
	n := max(1, r.CPU, (r.MemoryMiB+2047)/2048)
	if n > 1 && n%2 == 1 {
		n++
	}
	return n
}

func (a *Adapter) Create(ctx context.Context, req adapter.CreateRequest) (adapter.Instance, error) {
	v, err := a.get(ctx, req.ID, false)
	switch {
	case api.IsCode(err, api.CodeNotFound):
		v, err = a.create(ctx, req)
		if api.IsCode(err, api.CodeInvalidState) {
			v, err = a.get(ctx, req.ID, false) // a replay won the race for the name
		}
	case err == nil && v.Sandbox.Tags[tagSandbox] != req.ID:
		// Permanent, and this sandbox's problem alone: never an outage.
		return adapter.Instance{}, api.Errorf(api.CodeInvalidRequest, "vercel project %s has a sandbox named %s that Differently did not create", a.opts.Project, req.ID)
	case err == nil && v.state() == adapter.StateStopped:
		v, err = a.get(ctx, req.ID, true) // a replay after its session ended
	}
	if err != nil {
		return adapter.Instance{}, err
	}
	if v, err = a.waitRunning(ctx, v); err != nil {
		return adapter.Instance{}, err
	}
	return v.instance(), nil
}

func (a *Adapter) create(ctx context.Context, req adapter.CreateRequest) (view, error) {
	n := vcpus(req.Resources)
	timeout := time.Duration(req.TTLSeconds) * time.Second // provider-side backstop: the session stops
	if timeout <= 0 || timeout > a.opts.MaxTimeout {
		timeout = a.opts.MaxTimeout
	}
	body := map[string]any{
		"name":         req.ID,
		"projectId":    a.opts.Project,
		"resources":    map[string]int{"vcpus": n, "memory": n * 2048},
		"timeout":      timeout.Milliseconds(),
		"tags":         map[string]string{tagSandbox: req.ID, tagInstallation: req.Installation},
		"persistent":   true,
		"architecture": a.opts.Architecture,
	}
	if len(req.Env) > 0 {
		body["env"] = req.Env
	}
	image := a.opts.Image
	if req.Template != "" {
		image = req.Template
	}
	switch {
	case req.FromSnapshot != "":
		body["source"] = map[string]string{"type": "snapshot", "snapshotId": req.FromSnapshot}
	case image != "":
		body["image"] = image
	}
	if a.opts.Region != "" {
		body["region"] = a.opts.Region
	}
	if len(a.opts.Ports) > 0 {
		body["ports"] = a.opts.Ports
	}
	var v view
	return v, a.c.JSON(ctx, http.MethodPost, a.path("/v4/sandboxes", nil), body, &v)
}

func (a *Adapter) Get(ctx context.Context, ref string) (adapter.Instance, error) {
	v, err := a.get(ctx, ref, false)
	if err != nil {
		return adapter.Instance{}, err
	}
	return v.instance(), nil
}

func (a *Adapter) List(ctx context.Context, installation string) ([]adapter.Instance, error) {
	var out []adapter.Instance
	cursor := ""
	for {
		q := url.Values{"project": {a.opts.Project}, "limit": {"50"}, "tags": {tagInstallation + ":" + installation}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page struct {
			Sandboxes  []sandbox `json:"sandboxes"`
			Pagination struct {
				Next *string `json:"next"`
			} `json:"pagination"`
		}
		if err := a.c.JSON(ctx, http.MethodGet, a.path("/v2/sandboxes", q), nil, &page); err != nil {
			return nil, err
		}
		for _, s := range page.Sandboxes {
			if s.Tags[tagInstallation] == installation { // re-checked, never trusted
				out = append(out, view{Sandbox: s}.instance())
			}
		}
		if page.Pagination.Next == nil || *page.Pagination.Next == "" {
			return out, nil
		}
		cursor = *page.Pagination.Next
	}
}

// Delete removes the sandbox and stops its session. Its snapshots stay until
// they expire or DeleteSnapshot removes them, so a snapshot can outlive its
// source sandbox.
func (a *Adapter) Delete(ctx context.Context, ref string) error {
	err := a.c.JSON(ctx, http.MethodDelete, a.path("/v2/sandboxes/"+url.PathEscape(ref), url.Values{"projectId": {a.opts.Project}}), nil, nil)
	if api.IsCode(err, api.CodeNotFound) {
		return nil
	}
	return err
}

// running returns the ID of the sandbox's running session.
func (a *Adapter) running(ctx context.Context, ref string) (string, error) {
	v, err := a.get(ctx, ref, false)
	if err != nil {
		return "", err
	}
	if v.state() != adapter.StateRunning || v.Session == nil {
		return "", api.Errorf(api.CodeInvalidState, "vercel sandbox %s is %s, not running", ref, v.state())
	}
	return v.Session.ID, nil
}

type command struct {
	ID       string `json:"id"`
	ExitCode *int   `json:"exitCode"`
}

func exitCode(c command) int {
	if c.ExitCode == nil {
		return 124 // no exit code: the command was killed, by its timeout or a signal
	}
	return *c.ExitCode
}

func (a *Adapter) Exec(ctx context.Context, ref string, req api.ExecRequest) (api.ExecResult, error) {
	var stdout, stderr strings.Builder
	start := time.Now()
	code, err := a.run(ctx, ref, req, func(e api.ExecEvent) error {
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

func (a *Adapter) ExecStream(ctx context.Context, ref string, req api.ExecRequest, emit func(api.ExecEvent) error) (int, error) {
	return a.run(ctx, ref, req, emit)
}

// logLine is one line of a command's NDJSON stream: the command when it
// starts and when it finishes, and its output in between.
type logLine struct {
	Command *command        `json:"command"`
	Stream  string          `json:"stream"`
	Data    json.RawMessage `json:"data"`
}

// run starts a command with wait and logs set, so one NDJSON response
// carries the start, the output and the exit code.
func (a *Adapter) run(ctx context.Context, ref string, req api.ExecRequest, emit func(api.ExecEvent) error) (int, error) {
	sid, err := a.running(ctx, ref)
	if err != nil {
		return -1, err
	}
	name, args := "sh", []string{"-c", req.Command}
	if req.Command == "" {
		if len(req.Args) == 0 {
			return -1, api.Errorf(api.CodeInvalidRequest, "exec needs a command or args")
		}
		name, args = req.Args[0], req.Args[1:] // run directly, without a shell
	}
	body := map[string]any{"command": name, "args": args, "wait": true, "logs": true}
	if req.Cwd != "" {
		body["cwd"] = req.Cwd
	}
	if len(req.Env) > 0 {
		body["env"] = req.Env
	}
	if req.TimeoutSeconds > 0 {
		body["timeout"] = req.TimeoutSeconds * 1000
	}
	if req.Stdin != "" {
		body["attachStdin"] = true
	}
	b, err := json.Marshal(body)
	if err != nil {
		return -1, err
	}
	cmdPath := sessionPath(sid, "cmd")
	resp, err := a.c.Do(ctx, http.MethodPost, a.path(cmdPath, nil), bytes.NewReader(b), "application/json", http.Header{"Accept": {"application/x-ndjson"}})
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()
	var started *command
	delivered := map[string]int{}
	code, done, err := readLines(resp.Body, func(l logLine) (bool, error) {
		if started == nil && l.Command != nil {
			started = l.Command
			if req.Stdin != "" {
				return false, a.writeStdin(ctx, cmdPath+"/"+url.PathEscape(started.ID)+"/stdin", req.Stdin)
			}
			return false, nil
		}
		return a.handle(l, delivered, nil, emit)
	})
	if err != nil || done {
		return code, err
	}
	if started == nil {
		return -1, adapter.Unavailable(fmt.Errorf("vercel: the command stream ended before the command started"))
	}
	// The stream ended early: fetch the rest of the output, then the result.
	logs, err := a.c.Do(ctx, http.MethodGet, a.path(cmdPath+"/"+url.PathEscape(started.ID)+"/logs", nil), nil, "", http.Header{"Accept": {"application/x-ndjson"}})
	if err != nil {
		return -1, err
	}
	defer logs.Body.Close()
	skip := map[string]int{"stdout": delivered["stdout"], "stderr": delivered["stderr"]}
	if _, _, err := readLines(logs.Body, func(l logLine) (bool, error) { return a.handle(l, delivered, skip, emit) }); err != nil {
		return -1, err
	}
	var out struct {
		Command command `json:"command"`
	}
	if err := a.c.JSON(ctx, http.MethodGet, a.path(cmdPath+"/"+url.PathEscape(started.ID), url.Values{"wait": {"true"}}), nil, &out); err != nil {
		return -1, err
	}
	return exitCode(out.Command), nil
}

// handle emits one output line, skipping output already delivered before a
// reconnect. It reports done once the finished command arrives.
func (a *Adapter) handle(l logLine, delivered, skip map[string]int, emit func(api.ExecEvent) error) (bool, error) {
	switch {
	case l.Command != nil:
		return true, nil
	case l.Stream == "error":
		var e struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(l.Data, &e)
		if e.Code == "sandbox_stream_closed" {
			return false, api.Errorf(api.CodeInvalidState, "vercel: the sandbox stopped while the command ran")
		}
		return false, adapter.Unavailable(fmt.Errorf("vercel: %s (%s)", e.Message, e.Code))
	case l.Stream == "stdout" || l.Stream == "stderr":
		var data string
		_ = json.Unmarshal(l.Data, &data)
		if n := min(skip[l.Stream], len(data)); n > 0 {
			skip[l.Stream] -= n
			data = data[n:]
		}
		if data == "" {
			return false, nil
		}
		delivered[l.Stream] += len(data)
		return false, emit(api.ExecEvent{Stream: l.Stream, Data: data})
	}
	return false, nil
}

// readLines feeds each NDJSON line to fn until fn reports done. It returns
// the exit code of the finished command when one arrives.
func readLines(r io.Reader, fn func(logLine) (bool, error)) (int, bool, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var l logLine
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			return -1, false, adapter.Unavailable(fmt.Errorf("vercel: unreadable command stream: %w", err))
		}
		done, err := fn(l)
		if err != nil {
			return -1, false, err
		}
		if done {
			return exitCode(*l.Command), true, nil
		}
	}
	// The stream ended (or the connection dropped) before the command
	// finished; the caller picks up the rest from the logs endpoint.
	return -1, false, nil
}

// writeStdin sends stdin in chunks under Vercel's 1 MB request limit, each
// with its offset so a resend is safe, and closes it after the last.
func (a *Adapter) writeStdin(ctx context.Context, p, data string) error {
	const chunk = 512 << 10
	for off := 0; off < len(data); {
		end := min(off+chunk, len(data))
		body := map[string]any{"data": base64.StdEncoding.EncodeToString([]byte(data[off:end])), "offset": off, "close": end == len(data)}
		if err := a.c.JSON(ctx, http.MethodPost, a.path(p, nil), body, nil); err != nil {
			return err
		}
		off = end
	}
	return nil
}

func (a *Adapter) ReadFile(ctx context.Context, ref, p string) ([]byte, error) {
	sid, err := a.running(ctx, ref)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(map[string]string{"path": p})
	resp, err := a.c.Do(ctx, http.MethodPost, a.path(sessionPath(sid, "fs/read"), nil), bytes.NewReader(b), "application/json", http.Header{"Accept": {"application/octet-stream"}})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 100<<20))
}

// WriteFile uploads one file as a gzipped tarball, which Vercel extracts
// into the file's directory (created first).
func (a *Adapter) WriteFile(ctx context.Context, ref, p string, data []byte) error {
	sid, err := a.running(ctx, ref)
	if err != nil {
		return err
	}
	name, header := p, http.Header{}
	if strings.HasPrefix(p, "/") {
		dir, base := path.Split(p)
		if err := a.c.JSON(ctx, http.MethodPost, a.path(sessionPath(sid, "fs/mkdir"), nil), map[string]any{"path": dir, "recursive": true}, nil); err != nil {
			return err
		}
		name = base
		header.Set("X-Cwd", dir)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg, ModTime: time.Now()}); err != nil {
		return err
	}
	if _, err := tw.Write(data); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	resp, err := a.c.Do(ctx, http.MethodPost, a.path(sessionPath(sid, "fs/write"), nil), &buf, "application/gzip", header)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// ListFiles runs find in the sandbox: Vercel has no listing endpoint.
func (a *Adapter) ListFiles(ctx context.Context, ref, p string) ([]api.FileInfo, error) {
	res, err := a.Exec(ctx, ref, api.ExecRequest{Args: []string{"find", p, "-mindepth", "1", "-maxdepth", "1", "-printf", `%y\t%s\t%T@\t%m\t%f\n`}})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		if strings.Contains(res.Stderr, "No such file") {
			return nil, api.Errorf(api.CodeNotFound, "%s not found", p)
		}
		return nil, api.Errorf(api.CodeInvalidRequest, "vercel: listing %s failed: %s", p, strings.TrimSpace(res.Stderr))
	}
	var out []api.FileInfo
	for _, line := range strings.Split(strings.TrimRight(res.Stdout, "\n"), "\n") {
		f := strings.SplitN(line, "\t", 5)
		if len(f) != 5 {
			continue
		}
		size, _ := strconv.ParseInt(f[1], 10, 64)
		secs, _ := strconv.ParseFloat(f[2], 64)
		out = append(out, api.FileInfo{Path: strings.TrimRight(p, "/") + "/" + f[4], Name: f[4], Size: size, Mode: f[3], IsDir: f[0] == "d",
			ModTime: time.UnixMilli(int64(secs * 1000)).UTC()})
	}
	return out, nil
}

// Stop ends the session. The sandbox is persistent, so Vercel snapshots its
// filesystem first and Start resumes from it.
func (a *Adapter) Stop(ctx context.Context, ref string) error {
	v, err := a.get(ctx, ref, false)
	if err != nil {
		return err
	}
	if v.state() == adapter.StateStopped {
		return nil
	}
	if v.Session == nil {
		return api.Errorf(api.CodeInvalidState, "vercel sandbox %s has no session to stop", ref)
	}
	if err := a.c.JSON(ctx, http.MethodPost, a.path(sessionPath(v.Session.ID, "stop"), nil), nil, nil); err != nil {
		return err
	}
	return a.waitStopped(ctx, ref)
}

func (a *Adapter) Start(ctx context.Context, ref string) error {
	v, err := a.get(ctx, ref, true)
	if err != nil {
		return err
	}
	_, err = a.waitRunning(ctx, v)
	return err
}

// Snapshot captures the filesystem. Vercel ends the session to take it, so
// the sandbox is resumed afterwards; if that fails it is left stopped, which
// the reconciler reports.
func (a *Adapter) Snapshot(ctx context.Context, ref, kind string) (adapter.Snapshot, error) {
	if kind == "memory" {
		return adapter.Snapshot{}, adapter.Unsupported("snapshots.memory")
	}
	sid, err := a.running(ctx, ref)
	if err != nil {
		return adapter.Snapshot{}, err
	}
	var out struct {
		Snapshot struct {
			ID        string  `json:"id"`
			SizeBytes float64 `json:"sizeBytes"`
		} `json:"snapshot"`
	}
	// expiration 0 keeps it until DeleteSnapshot: Differently owns its lifetime.
	if err := a.c.JSON(ctx, http.MethodPost, a.path("/v3/sandboxes/sessions/"+url.PathEscape(sid)+"/snapshot", nil), map[string]any{"expiration": 0}, &out); err != nil {
		return adapter.Snapshot{}, err
	}
	if a.waitStopped(ctx, ref) == nil {
		if v, err := a.get(ctx, ref, true); err == nil {
			_, _ = a.waitRunning(ctx, v)
		}
	}
	return adapter.Snapshot{Ref: out.Snapshot.ID, Kind: "disk", SizeBytes: int64(out.Snapshot.SizeBytes)}, nil
}

func (a *Adapter) DeleteSnapshot(ctx context.Context, ref string) error {
	err := a.c.JSON(ctx, http.MethodDelete, a.path("/v2/sandboxes/snapshots/"+url.PathEscape(ref), nil), nil, nil)
	if api.IsCode(err, api.CodeNotFound) {
		return nil
	}
	return err
}

// ExposePort returns the public URL Vercel routes to a port declared in the
// pool's config.ports. Anyone with the URL can reach the port.
func (a *Adapter) ExposePort(ctx context.Context, ref string, port int) (string, error) {
	if !slices.Contains(a.opts.Ports, port) {
		return "", api.Errorf(api.CodeInvalidRequest, "port %d is not in this pool's config.ports; Vercel routes only ports declared when the sandbox is created", port)
	}
	v, err := a.get(ctx, ref, false)
	if err != nil {
		return "", err
	}
	for _, r := range v.Routes {
		switch {
		case r.Port != port:
		case r.URL != "":
			return r.URL, nil
		case r.Subdomain != "":
			return "https://" + r.Subdomain + ".vercel.run", nil
		}
	}
	return "", adapter.Unavailable(fmt.Errorf("vercel reported no route for port %d", port))
}

// SetExpiry extends the running session's timeout towards the new deadline,
// up to the plan's maximum. It never shortens it.
func (a *Adapter) SetExpiry(ctx context.Context, ref string, at time.Time) error {
	v, err := a.get(ctx, ref, false)
	if err != nil || v.state() != adapter.StateRunning || v.Session == nil || v.Session.StartedAt == 0 {
		return err // only a running session has a deadline to move
	}
	started := time.UnixMilli(int64(v.Session.StartedAt))
	end := started.Add(time.Duration(v.Session.Timeout) * time.Millisecond)
	if limit := started.Add(a.opts.MaxTimeout); at.After(limit) {
		at = limit
	}
	if at.Sub(end) < time.Second {
		return nil
	}
	return a.c.JSON(ctx, http.MethodPost, a.path(sessionPath(v.Session.ID, "extend-timeout"), nil), map[string]int64{"duration": at.Sub(end).Milliseconds()}, nil)
}

var (
	_ adapter.ExecutionAdapter = (*Adapter)(nil)
	_ adapter.Streamer         = (*Adapter)(nil)
	_ adapter.FileSystem       = (*Adapter)(nil)
	_ adapter.Lifecycle        = (*Adapter)(nil)
	_ adapter.Snapshotter      = (*Adapter)(nil)
	_ adapter.PortExposer      = (*Adapter)(nil)
	_ adapter.Expirer          = (*Adapter)(nil)
)
