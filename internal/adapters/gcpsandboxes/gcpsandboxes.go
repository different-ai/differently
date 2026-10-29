// Package gcpsandboxes adapts Google Cloud's Agent Platform sandbox
// environments (formerly Vertex AI Agent Engine code execution) in their
// shell flavour: each Differently sandbox is one sandbox environment under
// an Agent Platform engine (reasoningEngines resource) the operator owns.
//
// Google runs the sandboxes in the operator's project, so pools default to
// TrustExternal; an operator whose policy treats their own Google Cloud
// project as private may declare trust: private. Requests carry an OAuth
// 2.0 token from a service-account key (JWT bearer grant, no Google SDK).
//
// Mapping, from the aiplatform v1 discovery document (revision 20261012)
// and the sandbox guides (read 2026-10-28):
//
//   - sandboxes have no labels or client-chosen IDs, so Create looks for the
//     display name "dly:<installation>:<sandbox>" before creating, sets
//     owner to the installation, and waits on the long-running operation;
//   - List pages through the engine's sandboxes and keeps that prefix;
//   - Exec posts {command, cwd, timeout} to the shell container's /exec
//     route through :execute; env and stdin are folded into the command line
//     (shellcmd), and files move as base64 through commands;
//   - Stop and Start are :pause and :resume (the filesystem is kept);
//   - snapshots capture memory and filesystem and restore by creating from
//     them.
//
// Not available: exposed ports (the data plane needs a signed JWT and a
// routing token per request), streaming, moving the TTL after creation.
//
// Tested against a fake server built from the published API, not yet
// against a live project.
package gcpsandboxes

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/different-ai/differently/internal/adapters/cloudhttp"
	"github.com/different-ai/differently/internal/adapters/httpx"
	"github.com/different-ai/differently/internal/adapters/shellcmd"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// Scope is the OAuth scope the sandbox methods accept.
const Scope = "https://www.googleapis.com/auth/cloud-platform"

const (
	// defaultTTL applies when a sandbox has no TTL: Google's documented
	// lifetimes run from 7 days (custom containers) to 14 (code execution).
	defaultTTL = 7 * 24 * 3600
	// backstopGrace keeps Google's deadline behind Differently's own,
	// audited expiry.
	backstopGrace = 5 * 60
	// shellPort is where the shell container serves /exec.
	shellPort = "8080"
)

// Options configures the adapter. Pool config keys: project, location (or
// the pool's region), reasoningEngine, template (all required), apiURL.
// The credential is a service-account JSON key.
type Options struct {
	Project      string
	Location     string
	Engine       string
	Template     string
	APIURL       string
	ReadyTimeout time.Duration
	PollEvery    time.Duration
	Account      cloudhttp.GoogleServiceAccount
	tokens       cloudhttp.TokenSource
	// HTTP overrides the client used for tokens (tests).
	HTTP *http.Client
}

// FromConfig maps pool config to options.
func FromConfig(cfg map[string]string, region, credential string) (Options, error) {
	o := Options{Project: cfg["project"], Location: cfg["location"], Engine: cfg["reasoningEngine"], Template: cfg["template"], APIURL: cfg["apiURL"]}
	if o.Location == "" {
		o.Location = region
	}
	if o.Project == "" || o.Location == "" || o.Engine == "" || o.Template == "" {
		return o, errors.New("gcp-sandboxes needs config.project, config.location, config.reasoningEngine and config.template (a shell sandbox template in that engine)")
	}
	if o.APIURL == "" {
		o.APIURL = "https://" + o.Location + "-aiplatform.googleapis.com"
	}
	sa, key, err := cloudhttp.ParseGoogle(credential)
	if err != nil {
		return o, fmt.Errorf("gcp-sandboxes: %w", err)
	}
	o.Account = sa
	o.tokens = cloudhttp.Google(sa, key, Scope, &http.Client{Timeout: time.Minute})
	return o, nil
}

// Adapter talks to one Agent Platform engine's sandboxes.
type Adapter struct {
	opts Options
	c    *cloudhttp.Client
}

// New returns an Agent Platform sandbox adapter.
func New(o Options) *Adapter {
	if o.ReadyTimeout == 0 {
		o.ReadyTimeout = 3 * time.Minute
	}
	if o.PollEvery == 0 {
		o.PollEvery = time.Second
	}
	c := cloudhttp.New("gcp-sandboxes", strings.TrimRight(o.APIURL, "/")+"/v1", cloudhttp.Bearer(o.tokens))
	c.Classify = func(_ int, code string) string {
		if code == "FAILED_PRECONDITION" {
			return api.CodeInvalidState // sent as 400: wrong state for the call
		}
		return ""
	}
	return &Adapter{opts: o, c: c}
}

// Endpoint is reported in the trust report.
func (a *Adapter) Endpoint() string { return a.opts.APIURL }

func (a *Adapter) Info() adapter.Info {
	return adapter.Info{Kind: "gcp-sandboxes", Version: "1", Trust: api.TrustExternal, Isolation: "container sandbox (Google-managed)", Protocol: adapter.ProtocolVersion,
		Description: "Google Cloud Agent Platform shell sandboxes in " + a.parent()}
}

func (a *Adapter) Capabilities(context.Context) (adapter.Capabilities, error) {
	return adapter.Capabilities{Features: []string{
		adapter.FeatureExec, adapter.FeatureFiles, adapter.FeatureStopStart, adapter.FeaturePersistDisk,
		adapter.FeatureSnapshotMem, adapter.FeatureSnapshotDisk, adapter.FeatureFork,
	}, Regions: []string{a.opts.Location}}, nil
}

func (a *Adapter) Capacity(context.Context) (adapter.Capacity, error) { return adapter.Capacity{}, nil }

func (a *Adapter) Health(ctx context.Context) error {
	return a.c.JSON(ctx, http.MethodGet, "/"+a.parent()+"/sandboxEnvironments?pageSize=1", nil, nil)
}

func (a *Adapter) parent() string {
	if strings.HasPrefix(a.opts.Engine, "projects/") {
		return a.opts.Engine
	}
	return "projects/" + a.opts.Project + "/locations/" + a.opts.Location + "/reasoningEngines/" + a.opts.Engine
}

// name turns a ref (the sandbox ID) into its resource name.
func (a *Adapter) name(ref string) string {
	return a.parent() + "/sandboxEnvironments/" + url.PathEscape(ref)
}

func (a *Adapter) templateName(t string) string {
	if strings.HasPrefix(t, "projects/") {
		return t
	}
	return a.parent() + "/sandboxEnvironmentTemplates/" + t
}

func (a *Adapter) snapshotName(ref string) string {
	if strings.HasPrefix(ref, "projects/") {
		return ref
	}
	return a.parent() + "/sandboxEnvironmentSnapshots/" + url.PathEscape(ref)
}

type sandbox struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Owner       string `json:"owner"`
	State       string `json:"state"`
}

func lastSegment(name string) string { return name[strings.LastIndex(name, "/")+1:] }

func displayName(installation, sandboxID string) string {
	return "dly:" + installation + ":" + sandboxID
}

func state(s sandbox) string {
	switch s.State {
	case "STATE_RUNNING":
		return adapter.StateRunning
	case "STATE_PAUSED", "STATE_PAUSING", "STATE_STOPPING":
		return adapter.StateStopped
	case "STATE_TERMINATED":
		return adapter.StateError
	}
	return adapter.StatePending
}

func gone(s sandbox) bool { return s.State == "STATE_DELETED" || s.State == "STATE_DEPROVISIONING" }

func (a *Adapter) instance(s sandbox) adapter.Instance {
	_, sandboxID, _ := strings.Cut(strings.TrimPrefix(s.DisplayName, "dly:"), ":")
	return adapter.Instance{Ref: lastSegment(s.Name), SandboxID: sandboxID, State: state(s), Region: a.opts.Location}
}

// all lists the engine's sandboxes, failing if any page fails.
func (a *Adapter) all(ctx context.Context) ([]sandbox, error) {
	var out []sandbox
	token := ""
	for {
		p := a.parent() + "/sandboxEnvironments?pageSize=100"
		if token != "" {
			p += "&pageToken=" + url.QueryEscape(token)
		}
		var page struct {
			SandboxEnvironments []sandbox `json:"sandboxEnvironments"`
			NextPageToken       string    `json:"nextPageToken"`
		}
		if err := a.c.JSON(ctx, http.MethodGet, "/"+p, nil, &page); err != nil {
			return nil, err
		}
		out = append(out, page.SandboxEnvironments...)
		if page.NextPageToken == "" {
			return out, nil
		}
		token = page.NextPageToken
	}
}

type operation struct {
	Name     string          `json:"name"`
	Done     bool            `json:"done"`
	Error    *rpcStatus      `json:"error"`
	Response json.RawMessage `json:"response"`
}

type rpcStatus struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// opError classifies a failed operation by its google.rpc.Code.
func opError(what string, s *rpcStatus) error {
	err := fmt.Errorf("gcp-sandboxes: %s failed: %s (code %d)", what, s.Message, s.Code)
	switch s.Code {
	case 3, 11: // INVALID_ARGUMENT, OUT_OF_RANGE
		return api.Errorf(api.CodeInvalidRequest, "%v", err)
	case 5: // NOT_FOUND
		return api.Errorf(api.CodeNotFound, "%v", err)
	case 8: // RESOURCE_EXHAUSTED
		return adapter.InsufficientCapacity(err.Error())
	case 7, 14, 16: // PERMISSION_DENIED, UNAVAILABLE, UNAUTHENTICATED
		return adapter.Unavailable(err)
	}
	return adapter.SandboxFailed(err)
}

// wait polls a long-running operation until it is done.
func (a *Adapter) wait(ctx context.Context, what string, op operation) (operation, error) {
	err := httpx.Poll(ctx, a.opts.PollEvery, a.opts.ReadyTimeout, func() (bool, error) {
		if op.Done {
			return true, nil
		}
		return false, a.c.JSON(ctx, http.MethodGet, "/"+op.Name, nil, &op)
	})
	if err == nil && op.Error != nil {
		err = opError(what, op.Error)
	}
	return op, err
}

func (a *Adapter) get(ctx context.Context, ref string) (sandbox, error) {
	var s sandbox
	return s, a.c.JSON(ctx, http.MethodGet, "/"+a.name(ref), nil, &s)
}

// waitState polls the sandbox until it reaches want.
func (a *Adapter) waitState(ctx context.Context, ref, want string) (sandbox, error) {
	var s sandbox
	err := httpx.Poll(ctx, a.opts.PollEvery, a.opts.ReadyTimeout, func() (bool, error) {
		var err error
		if s, err = a.get(ctx, ref); err != nil {
			return false, err
		}
		if s.State == "STATE_TERMINATED" || gone(s) {
			return false, adapter.SandboxFailed(fmt.Errorf("gcp sandbox %s is %s", ref, s.State))
		}
		return s.State == want, nil
	})
	return s, err
}

func (a *Adapter) Create(ctx context.Context, req adapter.CreateRequest) (adapter.Instance, error) {
	display := displayName(req.Installation, req.ID)
	items, err := a.all(ctx)
	if err != nil {
		return adapter.Instance{}, err
	}
	var s sandbox
	for _, it := range items {
		if it.DisplayName == display && !gone(it) {
			s = it
			break
		}
	}
	if s.Name == "" {
		ttl := req.TTLSeconds
		if ttl <= 0 {
			ttl = defaultTTL
		} else {
			ttl += backstopGrace
		}
		body := map[string]any{"displayName": display, "owner": req.Installation, "ttl": strconv.Itoa(ttl) + "s"}
		if req.FromSnapshot != "" {
			body["sandboxEnvironmentSnapshot"] = a.snapshotName(req.FromSnapshot)
		} else {
			tmpl := a.opts.Template
			if req.Template != "" {
				tmpl = req.Template
			}
			body["spec"] = map[string]any{"shellEnvironment": map[string]any{}}
			body["sandboxEnvironmentTemplate"] = a.templateName(tmpl)
		}
		var op operation
		if err := a.c.JSON(ctx, http.MethodPost, "/"+a.parent()+"/sandboxEnvironments", body, &op); err != nil {
			return adapter.Instance{}, err
		}
		if op, err = a.wait(ctx, "creating the sandbox", op); err != nil {
			return adapter.Instance{}, err
		}
		// The operation's response may lack fields; read the sandbox again.
		if err := json.Unmarshal(op.Response, &s); err != nil || s.Name == "" {
			return adapter.Instance{}, adapter.Unavailable(errors.New("gcp-sandboxes: the create operation returned no sandbox"))
		}
	}
	ref := lastSegment(s.Name)
	if s.State != "STATE_RUNNING" {
		if s, err = a.waitState(ctx, ref, "STATE_RUNNING"); err != nil {
			return adapter.Instance{}, err
		}
	}
	if len(req.Env) > 0 {
		r, err := a.command(ctx, ref, shellcmd.EnvScript(req.Env), 0)
		if err != nil {
			return adapter.Instance{}, err
		}
		if r.ExitCode != 0 {
			return adapter.Instance{}, adapter.SandboxFailed(fmt.Errorf("writing the sandbox environment failed: %s", strings.TrimSpace(r.Stderr)))
		}
	}
	return a.instance(s), nil
}

func (a *Adapter) Get(ctx context.Context, ref string) (adapter.Instance, error) {
	s, err := a.get(ctx, ref)
	if err != nil {
		return adapter.Instance{}, err
	}
	if gone(s) {
		return adapter.Instance{}, adapter.NotFound(ref)
	}
	return a.instance(s), nil
}

func (a *Adapter) List(ctx context.Context, installation string) ([]adapter.Instance, error) {
	items, err := a.all(ctx)
	if err != nil {
		return nil, err
	}
	prefix := "dly:" + installation + ":"
	var out []adapter.Instance
	for _, s := range items {
		if strings.HasPrefix(s.DisplayName, prefix) && !gone(s) {
			out = append(out, a.instance(s))
		}
	}
	return out, nil
}

// Delete starts the deletion and returns; the sandbox then lists as
// deprovisioning, which List and Get treat as gone.
func (a *Adapter) Delete(ctx context.Context, ref string) error {
	err := a.c.JSON(ctx, http.MethodDelete, "/"+a.name(ref), nil, nil)
	if api.IsCode(err, api.CodeNotFound) {
		return nil
	}
	return err
}

// command posts one command line to the shell container's /exec route.
func (a *Adapter) command(ctx context.Context, ref, cmd string, timeout int) (api.ExecResult, error) {
	req := map[string]any{"command": cmd}
	if timeout > 0 {
		req["timeout"] = timeout
	}
	payload, _ := json.Marshal(req)
	chunk := func(mime string, data []byte) map[string]string {
		return map[string]string{"mimeType": mime, "data": base64.StdEncoding.EncodeToString(data)}
	}
	body := map[string]any{"inputs": []any{
		chunk("application/x.sandbox-request-uri", []byte("/exec")),
		chunk("application/x.sandbox-request-port", []byte(shellPort)),
		chunk("application/json", payload),
	}}
	var out struct {
		Outputs []struct {
			Data []byte `json:"data"`
		} `json:"outputs"`
	}
	start := time.Now()
	if err := a.c.JSON(ctx, http.MethodPost, "/"+a.name(ref)+":execute", body, &out); err != nil {
		return api.ExecResult{}, err
	}
	for _, o := range out.Outputs {
		if len(o.Data) == 0 {
			continue
		}
		var r struct {
			Stdout     string `json:"stdout"`
			Stderr     string `json:"stderr"`
			ReturnCode *int   `json:"returncode"`
			DurationMs int64  `json:"duration_ms"`
		}
		if err := json.Unmarshal(o.Data, &r); err != nil || r.ReturnCode == nil {
			return api.ExecResult{}, adapter.SandboxFailed(fmt.Errorf("gcp-sandboxes: unreadable command result: %.200s", o.Data))
		}
		d := r.DurationMs
		if d == 0 {
			d = time.Since(start).Milliseconds()
		}
		return api.ExecResult{ExitCode: *r.ReturnCode, Stdout: r.Stdout, Stderr: r.Stderr, DurationMs: d}, nil
	}
	return api.ExecResult{}, adapter.SandboxFailed(errors.New("gcp-sandboxes: the sandbox returned no output for the command"))
}

// Exec runs the command in a new shell; the sandbox enforces the timeout.
func (a *Adapter) Exec(ctx context.Context, ref string, req api.ExecRequest) (api.ExecResult, error) {
	timeout := req.TimeoutSeconds
	req.TimeoutSeconds = 0
	return a.command(ctx, ref, shellcmd.Script(req), timeout)
}

func (a *Adapter) run(ref string) shellcmd.Run {
	return func(ctx context.Context, cmd string) (api.ExecResult, error) { return a.command(ctx, ref, cmd, 0) }
}

func (a *Adapter) ReadFile(ctx context.Context, ref, path string) ([]byte, error) {
	return shellcmd.ReadFile(ctx, a.run(ref), path)
}

func (a *Adapter) WriteFile(ctx context.Context, ref, path string, data []byte) error {
	return shellcmd.WriteFile(ctx, a.run(ref), path, data)
}

func (a *Adapter) ListFiles(ctx context.Context, ref, path string) ([]api.FileInfo, error) {
	return shellcmd.ListFiles(ctx, a.run(ref), path)
}

// action runs :pause or :resume and waits for the sandbox to settle.
func (a *Adapter) action(ctx context.Context, ref, verb, want string) error {
	var op operation
	if err := a.c.JSON(ctx, http.MethodPost, "/"+a.name(ref)+":"+verb, map[string]any{}, &op); err != nil {
		return err
	}
	if _, err := a.wait(ctx, verb, op); err != nil {
		return err
	}
	_, err := a.waitState(ctx, ref, want)
	return err
}

// Stop pauses the sandbox; Google keeps its ID, endpoint and filesystem.
func (a *Adapter) Stop(ctx context.Context, ref string) error {
	return a.action(ctx, ref, "pause", "STATE_PAUSED")
}

func (a *Adapter) Start(ctx context.Context, ref string) error {
	return a.action(ctx, ref, "resume", "STATE_RUNNING")
}

// Snapshot captures memory and filesystem, whatever kind is asked for, and
// leaves the sandbox running.
func (a *Adapter) Snapshot(ctx context.Context, ref, _ string) (adapter.Snapshot, error) {
	var op operation
	body := map[string]any{"displayName": "dly:" + ref, "postSnapshotAction": "RUNNING"}
	if err := a.c.JSON(ctx, http.MethodPost, "/"+a.name(ref)+":snapshot", body, &op); err != nil {
		return adapter.Snapshot{}, err
	}
	op, err := a.wait(ctx, "the snapshot", op)
	if err != nil {
		return adapter.Snapshot{}, err
	}
	var snap struct {
		Name      string `json:"name"`
		SizeBytes string `json:"sizeBytes"`
	}
	if err := json.Unmarshal(op.Response, &snap); err != nil || snap.Name == "" {
		return adapter.Snapshot{}, adapter.Unavailable(errors.New("gcp-sandboxes: the snapshot operation returned no snapshot"))
	}
	size, _ := strconv.ParseInt(snap.SizeBytes, 10, 64)
	return adapter.Snapshot{Ref: lastSegment(snap.Name), Kind: "memory", SizeBytes: size}, nil
}

func (a *Adapter) DeleteSnapshot(ctx context.Context, ref string) error {
	err := a.c.JSON(ctx, http.MethodDelete, "/"+a.snapshotName(ref), nil, nil)
	if api.IsCode(err, api.CodeNotFound) {
		return nil
	}
	return err
}

var (
	_ adapter.ExecutionAdapter = (*Adapter)(nil)
	_ adapter.FileSystem       = (*Adapter)(nil)
	_ adapter.Lifecycle        = (*Adapter)(nil)
	_ adapter.Snapshotter      = (*Adapter)(nil)
)
