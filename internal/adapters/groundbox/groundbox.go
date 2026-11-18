//go:build experimental_adapters

// Package groundbox adapts Groundbox's hosted sandbox API (/v1).
//
// Groundbox is hosted-only, so pools on this adapter are always
// TrustExternal. Each sandbox is a Firecracker microVM. Notable differences
// from other backends, handled here:
//
//   - create is made safe to repeat with an Idempotency-Key derived from the
//     sandbox ID; a replay is marked with the Idempotent-Replayed header;
//   - a deleted sandbox stays readable as "stopped", which Get reports as
//     not found. A sandbox stopped with its disk kept is "shut_down";
//   - a snapshot is kept on its own (memory, disk and processes) and
//     outlives its sandbox; a fork is a new sandbox started from one;
//   - every sandbox has an auto-stop time, at most 30 days away, which is
//     the TTL backstop and is moved with SetExpiry;
//   - buffered exec is a REST call; streamed exec, long commands and large
//     stdin use the WebSocket endpoint (websocket.go);
//   - Groundbox pauses an idle sandbox itself (memory kept) and the next
//     exec, file or preview request wakes it, so the adapter is a
//     NativeIdler: the idle timeout goes with the create.
//
// Written from Groundbox's published API reference and run against the live
// API on 2026-11-18 (live_test.go).
package groundbox

import (
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
	"strings"
	"time"

	"github.com/different-ai/differently/internal/adapters/httpx"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// DefaultAPIURL is Groundbox's API.
const DefaultAPIURL = "https://api.groundbox.dev/v1"

const (
	// backstopGrace keeps Groundbox's own auto-stop behind Differently's
	// audited expiry: Groundbox counts from the create request, Differently
	// from the moment the sandbox is ready.
	backstopGrace = 5 * 60
	// maxTimeoutMinutes is the longest auto-stop time Groundbox accepts.
	maxTimeoutMinutes = 30 * 24 * 60
	// maxExecSeconds is the longest command the buffered exec call waits
	// for. It is sent when a request sets no timeout: Groundbox's own default
	// is 60 s. Longer commands go over the stream.
	maxExecSeconds = 3600
	// maxStreamSeconds is the longest a resumable stream command may run.
	maxStreamSeconds = 30 * 24 * 3600
	// maxExecStdin is the most stdin the buffered exec call takes.
	maxExecStdin = 512 << 10
	// execOutputCap is what the buffered exec call keeps of stdout and of
	// stderr: the last 4 MiB of each.
	execOutputCap = 4 << 20
	// maxFileBytes is the largest file Groundbox moves in one call.
	maxFileBytes = 100 << 20
	// minIdleSeconds and maxIdleSeconds bound the idle timeout Groundbox
	// takes: 30 s to 30 days.
	minIdleSeconds = 30
	maxIdleSeconds = maxTimeoutMinutes * 60
	// createAttempts bounds how many ended sandboxes Create steps over for
	// one sandbox ID before it gives up.
	createAttempts = 10
)

// Metadata keys. Groundbox metadata is a flat string map (keys up to 64
// characters, values up to 512).
const (
	metaSandbox      = adapter.LabelSandboxID
	metaInstallation = adapter.LabelInstallation
	metaOrg          = adapter.LabelOrg
	metaProject      = adapter.LabelProject
)

// Options configures the adapter. Pool config keys: apiURL, template or
// image (what new sandboxes start from), egress (public|none|allowlist, with
// egressAllow a comma list of hosts), egressDeny (a comma list of hosts) and
// ports (private|public).
type Options struct {
	APIURL string
	APIKey string
	// Template is the default template for new sandboxes; Image a Docker
	// image to start them from instead. At most one is set.
	Template string
	Image    string
	// AllowHosts is the egress allowlist sent with every create. nil leaves
	// Groundbox's default (the open internet); an empty list denies all.
	AllowHosts []string
	// DenyHosts is the egress deny list. A denied host wins over an allowed one.
	DenyHosts []string
	// PublicPorts makes exposed ports public. Otherwise ExposePort returns a
	// share link that carries a token and expires.
	PublicPorts  bool
	ReadyTimeout time.Duration
	// PollInterval is how often a sandbox is read while it changes state.
	PollInterval time.Duration
}

func hosts(list string) []string {
	var out []string
	for _, h := range strings.Split(list, ",") {
		if h = strings.TrimSpace(h); h != "" {
			out = append(out, h)
		}
	}
	return out
}

// FromConfig maps pool config to options.
func FromConfig(cfg map[string]string, apiKey string) (Options, error) {
	o := Options{APIURL: strings.TrimRight(cfg["apiURL"], "/"), APIKey: apiKey, Template: cfg["template"], Image: cfg["image"], DenyHosts: hosts(cfg["egressDeny"])}
	if o.APIURL == "" {
		o.APIURL = DefaultAPIURL
	}
	if o.Template != "" && o.Image != "" {
		return o, errors.New("groundbox config takes template or image, not both")
	}
	switch cfg["egress"] {
	case "", "public":
	case "none":
		o.AllowHosts = []string{}
	case "allowlist":
		if o.AllowHosts = hosts(cfg["egressAllow"]); len(o.AllowHosts) == 0 {
			return o, errors.New("groundbox config.egress=allowlist needs config.egressAllow, a comma list of hosts")
		}
	default:
		return o, fmt.Errorf("groundbox config.egress must be public, none or allowlist, not %q", cfg["egress"])
	}
	switch cfg["ports"] {
	case "", "private":
	case "public":
		o.PublicPorts = true
	default:
		return o, fmt.Errorf("groundbox config.ports must be private or public, not %q", cfg["ports"])
	}
	return o, nil
}

// Adapter talks to Groundbox.
type Adapter struct {
	opts Options
	c    *httpx.Client
	// ws performs the WebSocket upgrade of a streamed exec.
	ws *http.Client
}

// New returns a Groundbox adapter.
func New(o Options) *Adapter {
	if o.APIURL == "" {
		o.APIURL = DefaultAPIURL
	}
	if o.ReadyTimeout == 0 {
		// Under the scheduler's default provision timeout (120 s), so a
		// sandbox that is still queued is reported as such.
		o.ReadyTimeout = 90 * time.Second
	}
	if o.PollInterval == 0 {
		o.PollInterval = 500 * time.Millisecond
	}
	c := httpx.New("groundbox", o.APIURL, o.APIKey)
	c.HTTP.Transport = httpx.ErrorShape(nil, errorBody)
	return &Adapter{opts: o, c: c, ws: &http.Client{}}
}

// errorBody flattens Groundbox's error envelope, {"error": {"code",
// "message"}}, into the shape httpx reads.
func errorBody(raw []byte) (code, message string) {
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

func (a *Adapter) Info() adapter.Info {
	return adapter.Info{Kind: "groundbox", Version: "1", Trust: api.TrustExternal, Isolation: "Firecracker microVM (Groundbox-managed)", Protocol: adapter.ProtocolVersion, Description: "Groundbox sandboxes at " + a.opts.APIURL}
}

func (a *Adapter) Capabilities(context.Context) (adapter.Capabilities, error) {
	return adapter.Capabilities{Features: []string{
		adapter.FeatureExec, adapter.FeatureExecStream, adapter.FeatureFiles, adapter.FeatureStopStart, adapter.FeaturePersistDisk,
		adapter.FeaturePauseResume, adapter.FeaturePauseCheckpoint, adapter.FeatureSnapshotMem, adapter.FeatureSnapshotDisk, adapter.FeatureFork,
		adapter.FeaturePorts, adapter.FeatureEgressControl, adapter.FeatureMicroVM, adapter.FeatureArchAMD64,
	}, MaxCPU: 16, MaxMemoryMiB: 65536, MaxDiskGiB: 100}, nil
}

func (a *Adapter) Capacity(context.Context) (adapter.Capacity, error) { return adapter.Capacity{}, nil }

func (a *Adapter) Health(ctx context.Context) error {
	return a.call(ctx, http.MethodGet, "/sandboxes?limit=1", nil, "", nil)
}

// call sends one JSON request. key, when set, is the Idempotency-Key.
func (a *Adapter) call(ctx context.Context, method, path string, in any, key string, out any) error {
	_, err := a.send(ctx, method, path, in, key, out)
	return err
}

// send is call, and also reports whether Groundbox replayed the answer of
// an earlier request with the same Idempotency-Key.
func (a *Adapter) send(ctx context.Context, method, path string, in any, key string, out any) (replayed bool, err error) {
	var body io.Reader
	contentType := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return false, err
		}
		body, contentType = bytes.NewReader(b), "application/json"
	}
	var header http.Header
	if key != "" {
		header = http.Header{"Idempotency-Key": {key}}
	}
	resp, err := a.c.Do(ctx, method, path, body, contentType, header)
	if err != nil {
		return false, classify(err)
	}
	defer resp.Body.Close()
	replayed = resp.Header.Get("Idempotent-Replayed") == "true"
	if out == nil {
		return replayed, nil
	}
	return replayed, json.NewDecoder(resp.Body).Decode(out)
}

// classify corrects the codes httpx derives from the status alone, using
// Groundbox's own error code.
func classify(err error) error {
	var e *api.Error
	if !errors.As(err, &e) {
		return err
	}
	switch {
	case e.Details["providerCode"] == "no_capacity": // 503: no box has room right now
		e.Code = adapter.CodeInsufficientCapacity
	case e.Details["providerCode"] == "not_available": // 503: a feature that is switched off
		e.Code = api.CodeUnsupportedOperation
	case e.Details["providerStatus"] == "413": // a file over the size limit
		e.Code = api.CodeInvalidRequest
	}
	return e
}

func providerCode(err error) string {
	var e *api.Error
	if errors.As(err, &e) {
		return e.Details["providerCode"]
	}
	return ""
}

type sandbox struct {
	ID         string            `json:"id"`
	Status     string            `json:"status"`
	Metadata   map[string]string `json:"metadata"`
	StopReason string            `json:"stop_reason"`
}

// gone reports whether a sandbox is over. Groundbox keeps ended sandboxes
// readable, so this is what "deleted" looks like.
func (s sandbox) gone() bool {
	return s.Status == "stopping" || s.Status == "stopped" || s.Status == "failed"
}

func (s sandbox) state() string {
	switch s.Status {
	case "running":
		return adapter.StateRunning
	case "pausing", "paused":
		return adapter.StatePaused
	case "shutting_down", "shut_down": // stopped with the disk kept
		return adapter.StateStopped
	case "stopping", "stopped", "failed":
		return adapter.StateError // over for good; see gone
	}
	return adapter.StatePending // queued, starting, resuming
}

func inst(s sandbox) adapter.Instance {
	return adapter.Instance{Ref: s.ID, SandboxID: s.Metadata[metaSandbox], State: s.state()}
}

func (a *Adapter) path(ref string) string { return "/sandboxes/" + url.PathEscape(ref) }

func (a *Adapter) get(ctx context.Context, ref string) (sandbox, error) {
	var s sandbox
	return s, a.call(ctx, http.MethodGet, a.path(ref), nil, "", &s)
}

// wait reads the sandbox until done says so. A sandbox that ends meanwhile
// is one sandbox's failure; one still queued when the time is up is a lack
// of capacity.
func (a *Adapter) wait(ctx context.Context, ref string, done func(sandbox) bool) (sandbox, error) {
	deadline := time.Now().Add(a.opts.ReadyTimeout)
	for {
		s, err := a.get(ctx, ref)
		switch {
		case err != nil:
			return s, err
		case done(s):
			return s, nil
		case s.gone():
			return s, adapter.SandboxFailed(fmt.Errorf("groundbox: sandbox %s %s", ref, ended(s)))
		case time.Now().After(deadline) && s.Status == "queued":
			return s, adapter.InsufficientCapacity(fmt.Sprintf("groundbox: sandbox %s is still queued after %s", ref, a.opts.ReadyTimeout))
		case time.Now().After(deadline):
			return s, adapter.Unavailable(fmt.Errorf("groundbox: sandbox %s is still %s after %s", ref, s.Status, a.opts.ReadyTimeout))
		}
		select {
		case <-ctx.Done():
			return s, ctx.Err()
		case <-time.After(a.opts.PollInterval):
		}
	}
}

func ended(s sandbox) string {
	if s.StopReason != "" {
		return s.Status + " (" + s.StopReason + ")"
	}
	return s.Status
}

// timeoutMinutes is the auto-stop time for a TTL: the TTL plus the grace,
// in whole minutes, within Groundbox's limits. Without a TTL it is the
// longest Groundbox allows (30 days).
func timeoutMinutes(ttlSeconds int) int {
	if ttlSeconds <= 0 {
		return maxTimeoutMinutes
	}
	return min((ttlSeconds+backstopGrace+59)/60, maxTimeoutMinutes)
}

func metadata(req adapter.CreateRequest) map[string]string {
	m := map[string]string{metaSandbox: req.ID, metaInstallation: req.Installation}
	for _, k := range []string{metaOrg, metaProject} {
		if v := req.Labels[k]; v != "" {
			m[k] = v
		}
	}
	return m
}

// Create is idempotent on req.ID through Groundbox's Idempotency-Key, which
// replays the sandbox the key first made. When that sandbox has ended (a
// failed attempt the scheduler is retrying), the next key in the series is
// used, so a retry gets a new sandbox and a replay still gets the same one.
func (a *Adapter) Create(ctx context.Context, req adapter.CreateRequest) (adapter.Instance, error) {
	for attempt := 1; attempt <= createAttempts; attempt++ {
		key := fmt.Sprintf("differently:%s:%s:%d", req.Installation, req.ID, attempt)
		s, replayed, err := a.start(ctx, req, key)
		if providerCode(err) == "idempotency_key_reused" {
			// The key was used with another body: the pool's config changed
			// between the first call and this replay. Look the sandbox up.
			found, ferr := a.find(ctx, req.Installation, req.ID)
			if ferr != nil {
				return adapter.Instance{}, ferr
			}
			if found == nil {
				continue
			}
			s, err = *found, nil
		}
		if err != nil {
			return adapter.Instance{}, err
		}
		if s.gone() {
			if !replayed { // this call made it, and it ended before it ran
				return adapter.Instance{}, adapter.SandboxFailed(fmt.Errorf("groundbox: sandbox %s %s", s.ID, ended(s)))
			}
			continue // an earlier attempt's sandbox
		}
		s, err = a.wait(ctx, s.ID, func(s sandbox) bool { return s.Status == "running" || s.Status == "paused" || s.Status == "shut_down" })
		if err != nil {
			return adapter.Instance{}, err
		}
		return inst(s), nil
	}
	return adapter.Instance{}, adapter.SandboxFailed(fmt.Errorf("groundbox: %d sandboxes for %s ended before they ran", createAttempts, req.ID))
}

// templateRef is what Groundbox takes as a template: an id, or a name with
// an optional version number.
var templateRef = regexp.MustCompile(`^(tpl_[A-Za-z0-9]{8,40}|[a-z0-9][a-z0-9-]{0,62})(:[1-9][0-9]{0,5})?$`)

// snapshotID is a kept snapshot's id.
var snapshotID = regexp.MustCompile(`^snap_[A-Za-z0-9]{8,40}$`)

// start asks for the sandbox: a new one, or one started from a kept snapshot.
func (a *Adapter) start(ctx context.Context, req adapter.CreateRequest, key string) (sandbox, bool, error) {
	body := map[string]any{"metadata": metadata(req), "timeout_minutes": timeoutMinutes(req.TTLSeconds)}
	if len(req.Env) > 0 {
		body["env"] = req.Env
	}
	if a.opts.AllowHosts != nil {
		body["allow_hosts"] = a.opts.AllowHosts
	}
	if len(a.opts.DenyHosts) > 0 {
		body["deny_hosts"] = a.opts.DenyHosts
	}
	if req.IdleTimeoutSeconds > 0 {
		// Groundbox pauses the sandbox (memory kept) after this long with no
		// exec, file, preview or other request reaching it, and the next such
		// request wakes it. A sandbox paused through Pause stays paused.
		body["idle_timeout_seconds"] = min(max(req.IdleTimeoutSeconds, minIdleSeconds), maxIdleSeconds)
		body["wake_on_request"] = true
	}
	template, image := a.opts.Template, a.opts.Image
	if req.FromSnapshot != "" {
		// The whole machine the snapshot holds, at the snapshot's size: no
		// template, image or size goes with it.
		if !snapshotID.MatchString(req.FromSnapshot) {
			return sandbox{}, false, api.Errorf(api.CodeInvalidRequest, "groundbox: %q is not a snapshot of this adapter", req.FromSnapshot)
		}
		body["snapshot_id"] = req.FromSnapshot
		template, image = "", ""
	} else {
		// Groundbox takes whole GB and refuses a size outside its limits (1 to
		// 16 vCPU, at most 4 GB per vCPU, 1 to 100 GB of disk); it never rounds.
		if req.Resources.CPU > 0 {
			body["cpus"] = req.Resources.CPU
		}
		if req.Resources.MemoryMiB > 0 {
			body["memory_gb"] = (req.Resources.MemoryMiB + 1023) / 1024
		}
		if req.Resources.DiskGiB > 0 {
			body["disk_gb"] = req.Resources.DiskGiB
		}
		// A request's template is a Groundbox template when it reads as one
		// (python-ml, python-ml:3, tpl_...), and a Docker image otherwise
		// (python:3.12-slim, ghcr.io/org/app:tag, docker.io/library/ubuntu).
		if req.Template != "" {
			template, image = req.Template, ""
			if !templateRef.MatchString(req.Template) {
				template, image = "", req.Template
			}
		}
	}
	if template != "" {
		body["template"] = template
	}
	if image != "" {
		body["image"] = image
	}
	var s sandbox
	replayed, err := a.send(ctx, http.MethodPost, "/sandboxes", body, key, &s)
	// "conflict" here is a second create racing the first with the same key,
	// or a snapshot that is not ready: still being copied, which passes, or
	// deleted, which does not.
	for tries := 0; providerCode(err) == "conflict" && tries < 10; tries++ {
		if req.FromSnapshot != "" {
			var snap keptSnapshot
			if gerr := a.call(ctx, http.MethodGet, "/snapshots/"+url.PathEscape(req.FromSnapshot), nil, "", &snap); gerr == nil && snap.Status != "creating" && snap.Status != "ready" {
				return s, false, api.Errorf(api.CodeInvalidRequest, "groundbox: snapshot %s is %s", req.FromSnapshot, snap.Status)
			}
		}
		select {
		case <-ctx.Done():
			return s, false, ctx.Err()
		case <-time.After(a.opts.PollInterval):
		}
		replayed, err = a.send(ctx, http.MethodPost, "/sandboxes", body, key, &s)
	}
	switch {
	case api.IsCode(err, api.CodeNotFound) && req.FromSnapshot != "":
		return s, false, api.Errorf(api.CodeInvalidRequest, "groundbox: snapshot %s does not exist", req.FromSnapshot)
	case api.IsCode(err, api.CodeNotFound):
		return s, false, api.Errorf(api.CodeInvalidRequest, "groundbox: template %q does not exist (a Docker image is given with its registry, as in docker.io/library/%s)", template, template)
	}
	return s, replayed, err
}

func (a *Adapter) Get(ctx context.Context, ref string) (adapter.Instance, error) {
	s, err := a.get(ctx, ref)
	if err != nil {
		return adapter.Instance{}, err
	}
	if s.gone() {
		return adapter.Instance{}, adapter.NotFound(ref)
	}
	return inst(s), nil
}

// list returns the installation's sandboxes that are not over, and with
// sandboxID only the one made for it. It fails if any page does.
func (a *Adapter) list(ctx context.Context, installation, sandboxID string) ([]sandbox, error) {
	q := url.Values{"live": {"true"}, "limit": {"200"}, "metadata[" + metaInstallation + "]": {installation}}
	if sandboxID != "" {
		q.Set("metadata["+metaSandbox+"]", sandboxID)
	}
	var out []sandbox
	for {
		var page struct {
			Data       []sandbox `json:"data"`
			NextCursor string    `json:"next_cursor"`
		}
		if err := a.call(ctx, http.MethodGet, "/sandboxes?"+q.Encode(), nil, "", &page); err != nil {
			return nil, err
		}
		for _, s := range page.Data {
			// Never trust a filter we did not write: the reconciler deletes
			// what List returns and the control plane does not know. A
			// sandbox being deleted ("stopping") still counts as live there.
			if s.Metadata[metaInstallation] == installation && s.Metadata[metaSandbox] != "" && !s.gone() {
				out = append(out, s)
			}
		}
		if page.NextCursor == "" {
			return out, nil
		}
		q.Set("cursor", page.NextCursor)
	}
}

func (a *Adapter) List(ctx context.Context, installation string) ([]adapter.Instance, error) {
	live, err := a.list(ctx, installation, "")
	if err != nil {
		return nil, err
	}
	out := make([]adapter.Instance, 0, len(live))
	for _, s := range live {
		out = append(out, inst(s))
	}
	return out, nil
}

// find returns the live sandbox made for a sandbox ID, if there is one.
func (a *Adapter) find(ctx context.Context, installation, sandboxID string) (*sandbox, error) {
	live, err := a.list(ctx, installation, sandboxID)
	if err != nil {
		return nil, err
	}
	for _, s := range live {
		if s.Metadata[metaSandbox] == sandboxID {
			return &s, nil
		}
	}
	return nil, nil
}

// Delete is safe to repeat: Groundbox answers a second delete with the
// stopped sandbox, and an unknown one with 404.
func (a *Adapter) Delete(ctx context.Context, ref string) error {
	err := a.call(ctx, http.MethodDelete, a.path(ref), nil, "", nil)
	if api.IsCode(err, api.CodeNotFound) {
		return nil
	}
	return err
}

// execBody is the command as both exec endpoints take it: cmd runs with
// sh -c, args run with no shell, as the image's default user. limit is the
// longest timeout the endpoint takes; it is sent when the request sets none,
// because Groundbox's own default is 60 s.
func execBody(req api.ExecRequest, limit int) map[string]any {
	timeout := req.TimeoutSeconds
	if timeout <= 0 || timeout > limit {
		timeout = limit
	}
	body := map[string]any{"timeout_s": timeout}
	if req.Command != "" {
		body["cmd"] = req.Command
	} else {
		body["args"] = req.Args
	}
	if req.Cwd != "" {
		body["cwd"] = req.Cwd
	}
	if len(req.Env) > 0 {
		body["env"] = req.Env
	}
	return body
}

// exitCode reports a command killed at its timeout as 124, as timeout(1)
// does; Groundbox reports the kill signal (137) and a flag.
func exitCode(code int, timedOut bool) int {
	if timedOut {
		return 124
	}
	return code
}

func (a *Adapter) Exec(ctx context.Context, ref string, req api.ExecRequest) (api.ExecResult, error) {
	if req.TimeoutSeconds > maxExecSeconds || len(req.Stdin) > maxExecStdin {
		return a.execBuffered(ctx, ref, req) // more than the buffered call takes
	}
	body := execBody(req, maxExecSeconds)
	if req.Stdin != "" {
		body["stdin_base64"] = base64.StdEncoding.EncodeToString([]byte(req.Stdin))
	}
	var out struct {
		ExitCode   int    `json:"exit_code"`
		Stdout     string `json:"stdout"`
		Stderr     string `json:"stderr"`
		DurationMs int64  `json:"duration_ms"`
		TimedOut   bool   `json:"timed_out"`
	}
	if err := a.call(ctx, http.MethodPost, a.path(ref)+"/exec", body, "", &out); err != nil {
		return api.ExecResult{}, err
	}
	return api.ExecResult{
		ExitCode: exitCode(out.ExitCode, out.TimedOut), Stdout: out.Stdout, Stderr: out.Stderr, DurationMs: out.DurationMs,
		// Groundbox keeps the end of each stream and does not say when it
		// cut one: a stream at the cap has very likely lost its beginning.
		Truncated: len(out.Stdout) >= execOutputCap-4 || len(out.Stderr) >= execOutputCap-4,
	}, nil
}

// execBuffered runs a command over the stream endpoint and collects its
// output.
func (a *Adapter) execBuffered(ctx context.Context, ref string, req api.ExecRequest) (api.ExecResult, error) {
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

func absPath(p string) string {
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

func (a *Adapter) ReadFile(ctx context.Context, ref, path string) ([]byte, error) {
	resp, err := a.c.Do(ctx, http.MethodGet, a.path(ref)+"/files?path="+url.QueryEscape(absPath(path)), nil, "", http.Header{"Accept": {"application/octet-stream"}})
	if err != nil {
		return nil, classify(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFileBytes+1))
	if err != nil {
		return nil, adapter.Unavailable(fmt.Errorf("groundbox: read %s: %w", path, err))
	}
	if len(data) > maxFileBytes {
		// Never hand back the start of a file as if it were all of it.
		return nil, api.Errorf(api.CodeInvalidRequest, "groundbox: %s is larger than the %d MiB one call moves", path, maxFileBytes>>20)
	}
	return data, nil
}

// WriteFile uploads the bytes; Groundbox creates missing parent folders.
func (a *Adapter) WriteFile(ctx context.Context, ref, path string, data []byte) error {
	resp, err := a.c.Do(ctx, http.MethodPut, a.path(ref)+"/files?path="+url.QueryEscape(absPath(path)), bytes.NewReader(data), "application/octet-stream", nil)
	if err != nil {
		return classify(err)
	}
	return resp.Body.Close()
}

func (a *Adapter) ListFiles(ctx context.Context, ref, path string) ([]api.FileInfo, error) {
	var out struct {
		Entries []struct {
			Name       string    `json:"name"`
			Path       string    `json:"path"`
			Type       string    `json:"type"`
			SizeBytes  int64     `json:"size_bytes"`
			ModifiedAt time.Time `json:"modified_at"`
		} `json:"entries"`
	}
	if err := a.call(ctx, http.MethodGet, a.path(ref)+"/files?dir="+url.QueryEscape(absPath(path)), nil, "", &out); err != nil {
		return nil, err
	}
	items := make([]api.FileInfo, 0, len(out.Entries))
	for _, e := range out.Entries {
		items = append(items, api.FileInfo{Path: e.Path, Name: e.Name, Size: e.SizeBytes, IsDir: e.Type == "dir", ModTime: e.ModifiedAt})
	}
	return items, nil
}

// Stop shuts the sandbox down and keeps only its disk: no memory, no
// processes. A stopped sandbox is billed for storage only.
func (a *Adapter) Stop(ctx context.Context, ref string) error {
	if err := a.call(ctx, http.MethodPost, a.path(ref)+"/stop", nil, "", nil); err != nil {
		return err
	}
	_, err := a.wait(ctx, ref, func(s sandbox) bool { return s.Status == "shut_down" })
	return err
}

// Start boots a stopped sandbox from its disk.
func (a *Adapter) Start(ctx context.Context, ref string) error {
	if err := a.call(ctx, http.MethodPost, a.path(ref)+"/start", nil, "", nil); err != nil {
		return err
	}
	_, err := a.wait(ctx, ref, func(s sandbox) bool { return s.Status == "running" })
	return err
}

// Pause snapshots the sandbox (memory, disk and processes) and frees its
// VM; a paused sandbox is billed for storage only.
func (a *Adapter) Pause(ctx context.Context, ref string) error {
	if err := a.call(ctx, http.MethodPost, a.path(ref)+"/pause", nil, "", nil); err != nil {
		return err
	}
	_, err := a.wait(ctx, ref, func(s sandbox) bool { return s.Status == "paused" })
	return err
}

// Resume restores a paused sandbox. A full account answers 429, which is
// reported as insufficient capacity.
func (a *Adapter) Resume(ctx context.Context, ref string) error {
	if err := a.call(ctx, http.MethodPost, a.path(ref)+"/resume", nil, "", nil); err != nil {
		return err
	}
	_, err := a.wait(ctx, ref, func(s sandbox) bool { return s.Status == "running" })
	return err
}

// IdlesNatively reports that Groundbox pauses idle sandboxes itself, from
// the idle timeout sent at create, and wakes them on the next request that
// needs the VM. Get and List report such a sandbox as paused, so the
// reconciler follows the pause and the wake.
func (a *Adapter) IdlesNatively() bool { return true }

// SetExpiry moves Groundbox's auto-stop time, which counts from now and is
// at most 30 days away.
func (a *Adapter) SetExpiry(ctx context.Context, ref string, at time.Time) error {
	minutes := min(max(int((time.Until(at)+time.Minute-1)/time.Minute), 1), maxTimeoutMinutes)
	return a.call(ctx, http.MethodPatch, a.path(ref), map[string]any{"timeout_minutes": minutes}, "", nil)
}

// Snapshot keeps the sandbox as it is now (memory, disk and processes) as a
// snapshot of the team's, whichever kind is asked for. It outlives the
// sandbox; Groundbox deletes one no sandbox was started from for 30 days.
// The sandbox must be running.
func (a *Adapter) Snapshot(ctx context.Context, ref, _ string) (adapter.Snapshot, error) {
	var snap keptSnapshot
	if err := a.call(ctx, http.MethodPost, "/snapshots", map[string]any{"sandbox_id": ref, "label": "differently"}, "", &snap); err != nil {
		return adapter.Snapshot{}, err
	}
	// The box copies it in well under a second, and the call waits for that;
	// a first save point of a big sandbox can take longer.
	deadline := time.Now().Add(a.opts.ReadyTimeout)
	for snap.Status == "creating" {
		if time.Now().After(deadline) {
			return adapter.Snapshot{}, adapter.Unavailable(fmt.Errorf("groundbox: snapshot %s of %s is still being made after %s", snap.ID, ref, a.opts.ReadyTimeout))
		}
		select {
		case <-ctx.Done():
			return adapter.Snapshot{}, ctx.Err()
		case <-time.After(a.opts.PollInterval):
		}
		if err := a.call(ctx, http.MethodGet, "/snapshots/"+url.PathEscape(snap.ID), nil, "", &snap); err != nil {
			return adapter.Snapshot{}, err
		}
	}
	if snap.Status != "ready" {
		return adapter.Snapshot{}, adapter.SandboxFailed(fmt.Errorf("groundbox: snapshot %s of %s is %s: %s", snap.ID, ref, snap.Status, snap.Error))
	}
	kind := "disk"
	if snap.Memory {
		kind = "memory"
	}
	return adapter.Snapshot{Ref: snap.ID, Kind: kind, SizeBytes: snap.SizeBytes}, nil
}

type keptSnapshot struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Memory    bool   `json:"memory"`
	SizeBytes int64  `json:"size_bytes"`
	Error     string `json:"error"`
}

// DeleteSnapshot deletes a kept snapshot. It is safe to repeat: a deleted
// snapshot is answered as deleted, an unknown one with 404. Sandboxes started
// from it keep running.
func (a *Adapter) DeleteSnapshot(ctx context.Context, snapshotRef string) error {
	if !snapshotID.MatchString(snapshotRef) {
		return nil // not one of Groundbox's: there is nothing to delete
	}
	path := "/snapshots/" + url.PathEscape(snapshotRef)
	err := a.call(ctx, http.MethodDelete, path, nil, "", nil)
	// "conflict": a sandbox is still starting from it, for a few seconds.
	for tries := 0; providerCode(err) == "conflict" && tries < 20; tries++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(a.opts.PollInterval):
		}
		err = a.call(ctx, http.MethodDelete, path, nil, "", nil)
	}
	if api.IsCode(err, api.CodeNotFound) {
		return nil
	}
	return err
}

// ExposePort gives a port an HTTPS URL. A private preview (the default)
// answers only with the share link returned here, which carries a token and
// stops working after 24 hours; asking again renews it. With ports: public
// anyone with the URL can connect.
func (a *Adapter) ExposePort(ctx context.Context, ref string, port int) (string, error) {
	var out struct {
		URL      string `json:"url"`
		ShareURL string `json:"share_url"`
	}
	if err := a.call(ctx, http.MethodPost, a.path(ref)+"/previews", map[string]any{"port": port, "public": a.opts.PublicPorts}, "", &out); err != nil {
		return "", err
	}
	if !a.opts.PublicPorts && out.ShareURL != "" {
		return out.ShareURL, nil
	}
	return out.URL, nil
}

var (
	_ adapter.ExecutionAdapter = (*Adapter)(nil)
	_ adapter.Streamer         = (*Adapter)(nil)
	_ adapter.FileSystem       = (*Adapter)(nil)
	_ adapter.Lifecycle        = (*Adapter)(nil)
	_ adapter.Pauser           = (*Adapter)(nil)
	_ adapter.Snapshotter      = (*Adapter)(nil)
	_ adapter.PortExposer      = (*Adapter)(nil)
	_ adapter.Expirer          = (*Adapter)(nil)
	_ adapter.NativeIdler      = (*Adapter)(nil)
)
