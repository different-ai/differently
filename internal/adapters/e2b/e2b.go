// Package e2b adapts E2B's sandbox API: the hosted service, or an E2B the
// operator runs (its infrastructure is open source), so pools declare trust.
//
// The control plane (api.<domain>) creates, lists, pauses and snapshots
// sandboxes. Commands and files go to envd, the daemon inside each sandbox,
// reached through E2B's shared sandbox host: files over REST, commands over
// Connect (server-streaming, JSON codec), written here with the standard
// library. Notable differences from other backends, handled here:
//
//   - CPU, memory and disk come from the template, not the request;
//   - a sandbox lives at most the plan's maximum runtime (1 h on Hobby, 24 h
//     on Pro) from start or resume; E2B then deletes it, so pools set
//     maxLifetimeSeconds to their plan's limit;
//   - there is no stop that keeps the sandbox: pause/resume keeps memory and
//     disk;
//   - ports are reachable only when the pool sets ports: public, and then
//     every listening port is public.
//
// Verified against E2B's published OpenAPI and envd specs and its SDKs
// (2026-10-28), not yet against a live account.
package e2b

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/different-ai/differently/internal/adapters/httpx"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// Defaults for E2B's hosted service (us-west-1).
const (
	DefaultAPIURL = "https://api.e2b.app"
	DefaultDomain = "e2b.app"
	envdPort      = 49983
)

// Metadata keys. E2B metadata is a flat string map.
const (
	metaSandbox      = "differently-sandbox-id"
	metaInstallation = "differently-installation"
	metaOrg          = "differently-org"
	metaProject      = "differently-project"
)

// Options configures the adapter. Pool config keys: apiURL, domain,
// template (default base), maxLifetimeSeconds (the plan's maximum runtime;
// default 3600), ports (private|public) and egress (public|none). Trust is the
// pool's declaration: external for E2B's hosted service, private only for an
// E2B the operator runs.
type Options struct {
	APIURL      string
	APIKey      string
	Trust       string
	Domain      string
	Template    string
	MaxLifetime int
	Ports       string
	Egress      string
	// SandboxURL overrides the shared sandbox host, https://sandbox.<domain>.
	SandboxURL   string
	ReadyTimeout time.Duration
}

// FromConfig maps pool config and its declared trust to options.
func FromConfig(cfg map[string]string, trust, apiKey string) Options {
	o := Options{APIURL: cfg["apiURL"], APIKey: apiKey, Trust: trust, Domain: cfg["domain"], Template: cfg["template"], Ports: cfg["ports"], Egress: cfg["egress"]}
	o.MaxLifetime, _ = strconv.Atoi(cfg["maxLifetimeSeconds"])
	if o.Domain == "" {
		o.Domain = DefaultDomain
	}
	if o.APIURL == "" {
		o.APIURL = "https://api." + o.Domain
	}
	if o.Template == "" {
		o.Template = "base"
	}
	if o.MaxLifetime <= 0 {
		o.MaxLifetime = 3600
	}
	if o.Trust == "" {
		o.Trust = api.TrustExternal
	}
	return o
}

// Adapter talks to E2B.
type Adapter struct {
	opts Options
	api  *httpx.Client // control plane, X-API-Key
	envd *httpx.Client // in-sandbox daemon, X-Access-Token per sandbox

	mu        sync.Mutex
	sandboxes map[string]envdInfo
}

type envdInfo struct{ token, domain string }

// New returns an E2B adapter.
func New(o Options) *Adapter {
	if o.ReadyTimeout == 0 {
		o.ReadyTimeout = 3 * time.Minute
	}
	if o.Trust == "" {
		o.Trust = api.TrustExternal
	}
	cp := httpx.New("e2b", o.APIURL, "")
	cp.HTTP.Transport = transport{apiKey: o.APIKey, next: http.DefaultTransport}
	ed := httpx.New("e2b", "", "")
	ed.HTTP.Transport = transport{next: http.DefaultTransport}
	return &Adapter{opts: o, api: cp, envd: ed, sandboxes: map[string]envdInfo{}}
}

// Endpoint is reported in the trust report.
func (a *Adapter) Endpoint() string { return a.opts.APIURL }

// transport adds the API key and rewrites E2B's error bodies
// ({"code": 404, "message": ..., "error_code": ...}, or Connect's
// {"code": "not_found", ...}) into the {"message", "code"} shape httpx reads.
type transport struct {
	apiKey string
	next   http.RoundTripper
}

func (t transport) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.apiKey != "" {
		r = r.Clone(r.Context())
		r.Header.Set("X-API-Key", t.apiKey)
	}
	resp, err := t.next.RoundTrip(r)
	if err != nil || resp.StatusCode < 300 {
		return resp, err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	var body struct {
		Code      any    `json:"code"`
		Message   string `json:"message"`
		ErrorCode string `json:"error_code"`
	}
	out := map[string]string{}
	if json.Unmarshal(raw, &body) == nil {
		out["message"] = body.Message
		if code, ok := body.Code.(string); ok && code != "" {
			out["code"] = code
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

// capacityCodes are E2B's error codes for "no room right now".
var capacityCodes = map[string]bool{"sandbox_capacity_unavailable": true, "sandbox_placement_timeout": true, "sandbox_no_compatible_node": true}

// classify refines httpx's mapping for control-plane calls.
func classify(err error) error {
	var e *api.Error
	if !errors.As(err, &e) {
		return err
	}
	switch {
	case capacityCodes[e.Details["providerCode"]] || e.Details["providerStatus"] == "503":
		e.Code = adapter.CodeInsufficientCapacity // E2B's 503: no capacity, or the node is busy
	case e.Details["providerCode"] == "sandbox_create_failed":
		e.Code = adapter.CodeSandboxFailed
	}
	return e
}

// classifyEnvd maps envd and edge failures, which concern one sandbox.
func classifyEnvd(ref string, err error) error {
	var e *api.Error
	if !errors.As(err, &e) {
		return err
	}
	switch e.Details["providerStatus"] {
	case "502":
		e.Code, e.Message = api.CodeInvalidState, "e2b: sandbox "+ref+" is not running or cannot be reached"
	case "401":
		e.Code, e.Message = adapter.CodeSandboxFailed, "e2b: sandbox "+ref+" refused its access token"
	case "507":
		e.Code, e.Message = api.CodeInvalidRequest, "e2b: sandbox "+ref+" is out of disk space"
	}
	return e
}

func (a *Adapter) call(ctx context.Context, method, path string, in, out any) error {
	return classify(a.api.JSON(ctx, method, path, in, out))
}

type sandbox struct {
	SandboxID          string            `json:"sandboxID"`
	State              string            `json:"state"`
	Metadata           map[string]string `json:"metadata"`
	EnvdAccessToken    string            `json:"envdAccessToken"`
	TrafficAccessToken string            `json:"trafficAccessToken"`
	Domain             *string           `json:"domain"`
}

func (a *Adapter) Info() adapter.Info {
	return adapter.Info{Kind: "e2b", Version: "1", Trust: a.opts.Trust, Isolation: "Firecracker microVM (E2B-managed)", Protocol: adapter.ProtocolVersion, Description: "E2B sandboxes at " + a.opts.APIURL + "; sizes come from the template; sandboxes live at most " + strconv.Itoa(a.opts.MaxLifetime) + " s per run"}
}

func (a *Adapter) Capabilities(context.Context) (adapter.Capabilities, error) {
	f := []string{
		adapter.FeatureExec, adapter.FeatureExecStream, adapter.FeatureFiles, adapter.FeaturePauseResume,
		adapter.FeatureSnapshotMem, adapter.FeatureSnapshotDisk, adapter.FeatureFork, adapter.FeatureMicroVM, adapter.FeatureArchAMD64,
	}
	if a.opts.Ports == "public" {
		f = append(f, adapter.FeaturePorts)
	}
	// Hobby and Pro allow 8 vCPU / 8 GiB; disk is 10 GiB (Hobby) or 20 GiB (Pro).
	return adapter.Capabilities{Features: f, MaxCPU: 8, MaxMemoryMiB: 8192, MaxDiskGiB: 20}, nil
}

func (a *Adapter) Capacity(context.Context) (adapter.Capacity, error) { return adapter.Capacity{}, nil }

func (a *Adapter) Health(ctx context.Context) error {
	return a.call(ctx, http.MethodGet, "/v2/sandboxes?limit=1", nil, nil)
}

func state(s sandbox) string {
	switch s.State {
	case "running":
		return adapter.StateRunning
	case "paused":
		return adapter.StatePaused
	}
	return adapter.StatePending
}

// remember caches what envd calls need; tokens are refreshed from every
// create, get and resume.
func (a *Adapter) remember(s sandbox) adapter.Instance {
	if s.EnvdAccessToken != "" {
		info := envdInfo{token: s.EnvdAccessToken, domain: a.opts.Domain}
		if s.Domain != nil && *s.Domain != "" {
			info.domain = *s.Domain
		}
		a.mu.Lock()
		a.sandboxes[s.SandboxID] = info
		a.mu.Unlock()
	}
	return adapter.Instance{Ref: s.SandboxID, SandboxID: s.Metadata[metaSandbox], State: state(s)}
}

func (a *Adapter) get(ctx context.Context, ref string) (sandbox, error) {
	var s sandbox
	return s, a.call(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(ref), nil, &s)
}

// metadataFilter encodes E2B's list filter: the server unescapes the value,
// splits it into key=value pairs and unescapes each again.
func metadataFilter(key, value string) string {
	inner := url.QueryEscape(url.QueryEscape(key)) + "=" + url.QueryEscape(url.QueryEscape(value))
	return "metadata=" + url.QueryEscape(inner)
}

// find pages through every running or paused sandbox with a metadata value,
// failing rather than returning part of the list.
func (a *Adapter) find(ctx context.Context, key, value string) ([]sandbox, error) {
	var out []sandbox
	next := ""
	for {
		path := "/v2/sandboxes?limit=100&state=running,paused&" + metadataFilter(key, value)
		if next != "" {
			path += "&nextToken=" + url.QueryEscape(next)
		}
		resp, err := a.api.Do(ctx, http.MethodGet, path, nil, "", nil)
		if err != nil {
			return nil, classify(err)
		}
		var page []sandbox
		err = json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, adapter.Unavailable(fmt.Errorf("e2b: unreadable sandbox list: %w", err))
		}
		out = append(out, page...)
		if next = resp.Header.Get("X-Next-Token"); next == "" || len(page) == 0 {
			return out, nil
		}
	}
}

func (a *Adapter) Create(ctx context.Context, req adapter.CreateRequest) (adapter.Instance, error) {
	existing, err := a.find(ctx, metaSandbox, req.ID)
	if err != nil {
		return adapter.Instance{}, err
	}
	if len(existing) > 0 {
		s, err := a.get(ctx, existing[0].SandboxID) // the list carries no tokens
		if err != nil {
			return adapter.Instance{}, err
		}
		if s.State == "paused" {
			if err := a.resume(ctx, s.SandboxID); err != nil {
				return adapter.Instance{}, err
			}
			s.State = "running"
		}
		return a.remember(s), nil
	}
	template := a.opts.Template
	if req.Template != "" {
		template = req.Template
	}
	if req.FromSnapshot != "" {
		template = req.FromSnapshot // a snapshot is a template
	}
	labels := adapter.OwnershipLabels(req)
	meta := map[string]string{metaSandbox: req.ID, metaInstallation: req.Installation}
	if v := labels[adapter.LabelOrg]; v != "" {
		meta[metaOrg] = v
	}
	if v := labels[adapter.LabelProject]; v != "" {
		meta[metaProject] = v
	}
	// E2B deletes a sandbox when its timeout ends: the sandbox's TTL as a
	// backstop, never more than the plan allows.
	timeout := a.opts.MaxLifetime
	if req.TTLSeconds > 0 {
		timeout = min(req.TTLSeconds, timeout)
	}
	body := map[string]any{
		"templateID":            template,
		"timeout":               timeout,
		"metadata":              meta,
		"autoPause":             false,
		"allow_internet_access": a.opts.Egress != "none",
		"network":               map[string]any{"allowPublicTraffic": a.opts.Ports == "public"},
	}
	if len(req.Env) > 0 {
		body["envVars"] = req.Env
	}
	var s sandbox
	err = a.call(ctx, http.MethodPost, "/v2/sandboxes", body, &s)
	var e *api.Error
	switch {
	case api.IsCode(err, api.CodeNotFound):
		return adapter.Instance{}, api.Errorf(api.CodeInvalidRequest, "e2b: template %q does not exist", template)
	case errors.As(err, &e) && e.Details["providerStatus"] == "403":
		// 401 is a bad key; 403 is a template this key may not use.
		return adapter.Instance{}, api.Errorf(api.CodeInvalidRequest, "e2b: this API key cannot use template %q", template)
	case err != nil:
		return adapter.Instance{}, err
	}
	if s.Metadata == nil {
		s.Metadata = meta
	}
	s.State = "running" // create answers once the sandbox runs
	return a.remember(s), nil
}

func (a *Adapter) Get(ctx context.Context, ref string) (adapter.Instance, error) {
	s, err := a.get(ctx, ref)
	if err != nil {
		return adapter.Instance{}, err
	}
	return a.remember(s), nil
}

func (a *Adapter) List(ctx context.Context, installation string) ([]adapter.Instance, error) {
	items, err := a.find(ctx, metaInstallation, installation)
	if err != nil {
		return nil, err
	}
	out := make([]adapter.Instance, 0, len(items))
	for _, s := range items {
		out = append(out, adapter.Instance{Ref: s.SandboxID, SandboxID: s.Metadata[metaSandbox], State: state(s)})
	}
	return out, nil
}

func (a *Adapter) Delete(ctx context.Context, ref string) error {
	err := a.call(ctx, http.MethodDelete, "/sandboxes/"+url.PathEscape(ref), nil, nil)
	if api.IsCode(err, api.CodeNotFound) {
		err = nil
	}
	if err == nil {
		a.mu.Lock()
		delete(a.sandboxes, ref)
		a.mu.Unlock()
	}
	return err
}

// Pause keeps memory and disk; a paused sandbox is not billed.
func (a *Adapter) Pause(ctx context.Context, ref string) error {
	err := a.call(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(ref)+"/pause", map[string]any{}, nil)
	if api.IsCode(err, api.CodeInvalidState) {
		if s, gerr := a.get(ctx, ref); gerr == nil && s.State == "paused" {
			return nil // already paused
		}
	}
	return err
}

func (a *Adapter) Resume(ctx context.Context, ref string) error { return a.resume(ctx, ref) }

// resume connects to the sandbox, resuming it if paused; the response carries
// fresh envd credentials.
func (a *Adapter) resume(ctx context.Context, ref string) error {
	var s sandbox
	if err := a.call(ctx, http.MethodPost, "/v2/sandboxes/"+url.PathEscape(ref)+"/connect", map[string]any{"timeout": a.opts.MaxLifetime}, &s); err != nil {
		return err
	}
	if s.SandboxID == "" {
		s.SandboxID = ref
	}
	a.remember(s)
	return nil
}

// SetExpiry moves E2B's deletion deadline; E2B clamps it to the plan's
// maximum runtime.
func (a *Adapter) SetExpiry(ctx context.Context, ref string, at time.Time) error {
	secs := max(int(time.Until(at).Seconds()+0.999), 1)
	return a.call(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(ref)+"/timeout", map[string]any{"timeout": secs}, nil)
}

// Snapshot captures memory and disk into a snapshot template. E2B pauses the
// source briefly and drops its open connections.
func (a *Adapter) Snapshot(ctx context.Context, ref, _ string) (adapter.Snapshot, error) {
	var out struct {
		SnapshotID string `json:"snapshotID"`
	}
	if err := a.call(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(ref)+"/snapshots", map[string]any{}, &out); err != nil {
		return adapter.Snapshot{}, err
	}
	return adapter.Snapshot{Ref: out.SnapshotID, Kind: "memory"}, nil
}

func (a *Adapter) DeleteSnapshot(ctx context.Context, ref string) error {
	err := a.call(ctx, http.MethodDelete, "/templates/"+url.PathEscape(ref), nil, nil)
	if api.IsCode(err, api.CodeNotFound) {
		return nil
	}
	return err
}

// ExposePort returns the port's public URL. E2B has no per-port switch: with
// ports: public every listening port is reachable by anyone with the URL.
func (a *Adapter) ExposePort(ctx context.Context, ref string, port int) (string, error) {
	if a.opts.Ports != "public" {
		return "", adapter.Unsupported(adapter.FeaturePorts)
	}
	info, err := a.envdFor(ctx, ref)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("https://%d-%s.%s", port, ref, info.domain), nil
}

// envdFor returns the sandbox's envd credentials, fetching them once.
func (a *Adapter) envdFor(ctx context.Context, ref string) (envdInfo, error) {
	a.mu.Lock()
	info, ok := a.sandboxes[ref]
	a.mu.Unlock()
	if ok {
		return info, nil
	}
	s, err := a.get(ctx, ref)
	if err != nil {
		return envdInfo{}, err
	}
	if s.State == "paused" {
		return envdInfo{}, api.Errorf(api.CodeInvalidState, "e2b: sandbox %s is paused", ref)
	}
	a.remember(s)
	a.mu.Lock()
	info, ok = a.sandboxes[ref]
	a.mu.Unlock()
	if !ok {
		return envdInfo{}, adapter.Unavailable(fmt.Errorf("e2b did not report envd credentials for %s", ref))
	}
	return info, nil
}

// envdDo sends one request to the sandbox's envd through the shared sandbox
// host. A refused token is dropped and fetched again once, in case the
// sandbox was resumed elsewhere.
func (a *Adapter) envdDo(ctx context.Context, ref, path string, body []byte, contentType string, extra http.Header) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		info, err := a.envdFor(ctx, ref)
		if err != nil {
			return nil, err
		}
		base := a.opts.SandboxURL
		if base == "" {
			base = "https://sandbox." + info.domain
		}
		h := extra.Clone()
		if h == nil {
			h = http.Header{}
		}
		h.Set("X-Access-Token", info.token)
		h.Set("E2b-Sandbox-Id", ref)
		h.Set("E2b-Sandbox-Port", strconv.Itoa(envdPort))
		method := http.MethodPost
		var r io.Reader
		if body == nil {
			method = http.MethodGet
		} else {
			r = bytes.NewReader(body)
		}
		resp, err := a.envd.Do(ctx, method, strings.TrimRight(base, "/")+path, r, contentType, h)
		var e *api.Error
		if attempt == 0 && errors.As(err, &e) && e.Details["providerStatus"] == "401" {
			a.mu.Lock()
			delete(a.sandboxes, ref)
			a.mu.Unlock()
			continue
		}
		if err != nil {
			return nil, classifyEnvd(ref, err)
		}
		return resp, nil
	}
}

// rpc makes a unary Connect call with the JSON codec.
func (a *Adapter) rpc(ctx context.Context, ref, method string, in, out any) error {
	b, _ := json.Marshal(in)
	resp, err := a.envdDo(ctx, ref, "/"+method, b, "application/json", http.Header{"Connect-Protocol-Version": {"1"}})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
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

// envelope frames one Connect streaming message.
func envelope(msg []byte) []byte {
	out := make([]byte, 5+len(msg))
	binary.BigEndian.PutUint32(out[1:5], uint32(len(msg)))
	copy(out[5:], msg)
	return out
}

func readEnvelope(r io.Reader) (byte, []byte, error) {
	var h [5]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n > 32<<20 {
		return 0, nil, fmt.Errorf("message of %d bytes is too large", n)
	}
	msg := make([]byte, n)
	_, err := io.ReadFull(r, msg)
	return h[0], msg, err
}

type connectError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *connectError) toAPI(ref string) error {
	code := adapter.CodeSandboxFailed // one sandbox's daemon failed
	switch e.Code {
	case "invalid_argument", "failed_precondition", "out_of_range":
		code = api.CodeInvalidRequest // a bad cwd or user, say
	case "not_found":
		code = api.CodeNotFound
	}
	return &api.Error{Code: code, Message: "e2b: " + e.Message, Details: map[string]string{"provider": "e2b", "providerCode": e.Code, "sandbox": ref}}
}

// ExecStream runs the command through process.Process/Start: bash -l -c, as
// E2B's SDKs do, with stdin sent and closed once the process starts.
func (a *Adapter) ExecStream(ctx context.Context, ref string, req api.ExecRequest, emit func(api.ExecEvent) error) (int, error) {
	proc := map[string]any{"cmd": "/bin/bash", "args": []string{"-l", "-c", httpx.CommandLine(api.ExecRequest{Command: req.Command, Args: req.Args})}}
	if len(req.Env) > 0 {
		proc["envs"] = req.Env
	}
	if req.Cwd != "" {
		proc["cwd"] = req.Cwd
	}
	// stdin defaults to an open pipe that never closes; always say.
	msg, _ := json.Marshal(map[string]any{"process": proc, "stdin": req.Stdin != ""})
	h := http.Header{"Connect-Protocol-Version": {"1"}, "Keepalive-Ping-Interval": {"50"}}
	if req.TimeoutSeconds > 0 {
		h.Set("Connect-Timeout-Ms", strconv.Itoa(req.TimeoutSeconds*1000)) // envd kills the process when it passes
	}
	resp, err := a.envdDo(ctx, ref, "/process.Process/Start", envelope(msg), "application/connect+json", h)
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()
	exit, ended := -1, false
	for {
		flags, msg, err := readEnvelope(resp.Body)
		if err != nil {
			return -1, adapter.Unavailable(fmt.Errorf("e2b: command stream from %s ended early: %w", ref, err))
		}
		if flags&0x02 != 0 { // end of stream
			var end struct {
				Error *connectError `json:"error"`
			}
			_ = json.Unmarshal(msg, &end)
			switch {
			case end.Error != nil && end.Error.Code == "deadline_exceeded" && req.TimeoutSeconds > 0:
				return 124, nil
			case end.Error != nil:
				return -1, end.Error.toAPI(ref)
			case ended:
				return exit, nil
			}
			return -1, adapter.Unavailable(fmt.Errorf("e2b: command stream from %s ended before the process did", ref))
		}
		var ev struct {
			Event struct {
				Start *struct {
					PID uint32 `json:"pid"`
				} `json:"start"`
				Data *struct {
					Stdout string `json:"stdout"`
					Stderr string `json:"stderr"`
				} `json:"data"`
				End *struct {
					ExitCode int  `json:"exitCode"` // protojson omits zero values
					Exited   bool `json:"exited"`
				} `json:"end"`
			} `json:"event"`
		}
		if err := json.Unmarshal(msg, &ev); err != nil {
			return -1, adapter.Unavailable(fmt.Errorf("e2b: unreadable command event: %w", err))
		}
		switch e := ev.Event; {
		case e.Start != nil && req.Stdin != "":
			sel := map[string]any{"pid": e.Start.PID}
			if err := a.rpc(ctx, ref, "process.Process/SendInput", map[string]any{"process": sel, "input": map[string]string{"stdin": base64.StdEncoding.EncodeToString([]byte(req.Stdin))}}, nil); err != nil {
				return -1, err
			}
			if err := a.rpc(ctx, ref, "process.Process/CloseStdin", map[string]any{"process": sel}, nil); err != nil {
				return -1, err
			}
		case e.Data != nil:
			stream, data := "stdout", e.Data.Stdout
			if data == "" {
				stream, data = "stderr", e.Data.Stderr
			}
			b, err := base64.StdEncoding.DecodeString(data)
			if err != nil {
				return -1, adapter.Unavailable(fmt.Errorf("e2b: unreadable output: %w", err))
			}
			if len(b) > 0 {
				if err := emit(api.ExecEvent{Stream: stream, Data: string(b)}); err != nil {
					return -1, err
				}
			}
		case e.End != nil:
			exit, ended = e.End.ExitCode, true
			if !e.End.Exited && exit == 0 {
				exit = -1 // killed by a signal
			}
		}
	}
}

func (a *Adapter) ReadFile(ctx context.Context, ref, path string) ([]byte, error) {
	resp, err := a.envdDo(ctx, ref, "/files?path="+url.QueryEscape(path), nil, "", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 100<<20))
}

// WriteFile uploads as multipart (field "file"), which every envd version
// accepts; envd creates missing parent directories.
func (a *Adapter) WriteFile(ctx context.Context, ref, path string, data []byte) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", path)
	if err != nil {
		return err
	}
	if _, err := part.Write(data); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	resp, err := a.envdDo(ctx, ref, "/files?path="+url.QueryEscape(path), buf.Bytes(), w.FormDataContentType(), nil)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (a *Adapter) ListFiles(ctx context.Context, ref, path string) ([]api.FileInfo, error) {
	var out struct {
		Entries []struct {
			Name         string `json:"name"`
			Type         string `json:"type"`
			Path         string `json:"path"`
			Size         string `json:"size"` // int64 is a JSON string in protojson
			Mode         uint32 `json:"mode"`
			ModifiedTime string `json:"modifiedTime"`
		} `json:"entries"`
	}
	if err := a.rpc(ctx, ref, "filesystem.Filesystem/ListDir", map[string]any{"path": path, "depth": 1}, &out); err != nil {
		return nil, err
	}
	items := make([]api.FileInfo, 0, len(out.Entries))
	for _, e := range out.Entries {
		size, _ := strconv.ParseInt(e.Size, 10, 64)
		mt, _ := time.Parse(time.RFC3339Nano, e.ModifiedTime)
		items = append(items, api.FileInfo{Path: e.Path, Name: e.Name, Size: size, IsDir: e.Type == "FILE_TYPE_DIRECTORY", Mode: fmt.Sprintf("%04o", e.Mode&0o7777), ModTime: mt})
	}
	return items, nil
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
