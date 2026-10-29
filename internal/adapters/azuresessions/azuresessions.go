// Package azuresessions adapts Azure Container Apps dynamic sessions in a
// Python code-interpreter session pool: each Differently sandbox is one
// session, named by its identifier.
//
// Microsoft runs the sessions in the operator's subscription, so pools
// default to TrustExternal; an operator whose policy treats their own Azure
// subscription as private may declare trust: private. Requests carry a
// Microsoft Entra ID token for https://dynamicsessions.io; the identity
// needs the Azure ContainerApps Session Executor role on the pool.
//
// Mapping, from the data-plane spec 2025-10-02-preview (azure-rest-api-specs,
// specification/app/data-plane/DynamicSessions, read 2026-10-28):
//
//   - the identifier is "dly-<installation>--<sandbox>" with each ID's
//     underscore as a hyphen; Azure allocates a session on the first request
//     for an identifier, so Create runs a first program and is idempotent;
//   - List pages through listSessions and keeps this installation's prefix;
//   - Delete deletes the session;
//   - Exec runs a Python program that runs the command with subprocess
//     (pyshell): code-interpreter pools execute Python, not shell commands;
//   - files use the session files API, which only reaches /mnt/data.
//
// Not available: stop/start, pause/resume, snapshots, ports, streaming.
// Shell and custom-container pools are not supported by this adapter (see
// docs/providers/azure-sessions.md); Azure Container Apps Sandboxes, the
// azure-sandboxes adapter, is the general-purpose route on Azure.
//
// Tested against a fake server built from the spec, not yet against a live
// session pool.
package azuresessions

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/different-ai/differently/internal/adapters/cloudhttp"
	"github.com/different-ai/differently/internal/adapters/pyshell"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// DefaultAPIVersion is the data-plane version the adapter was written for.
const DefaultAPIVersion = "2025-10-02-preview"

// Scope is the Entra ID scope of the dynamic sessions data plane.
const Scope = "https://dynamicsessions.io/.default"

const (
	// DataDir is the only directory the session files API reaches.
	DataDir = "/mnt/data"
	// defaultEnvFile keeps the sandbox environment outside DataDir.
	defaultEnvFile = "/tmp/.differently/env.json"
	// maxExecSeconds is the documented limit of one code execution; the
	// command gets a little less so its result still comes back.
	maxExecSeconds = 220
	maxCmdSeconds  = 210
	// defaultOutputLimit raises the service's 4096-character default.
	defaultOutputLimit = 1 << 20
)

// Options configures the adapter. Pool config keys: endpoint (the pool
// management endpoint, required), apiVersion, outputLimit. The credential
// is Entra JSON (see cloudhttp.EntraCredential).
type Options struct {
	Endpoint    string
	APIVersion  string
	OutputLimit int
	Region      string
	Credential  cloudhttp.EntraCredential
	// EnvFile is where the sandbox environment is kept in the session.
	EnvFile string
	// HTTP overrides the client used for tokens (tests).
	HTTP *http.Client
}

// FromConfig maps pool config to options.
func FromConfig(cfg map[string]string, region, credential string) (Options, error) {
	o := Options{Endpoint: strings.TrimRight(cfg["endpoint"], "/"), APIVersion: cfg["apiVersion"], Region: region, OutputLimit: defaultOutputLimit}
	if o.Endpoint == "" {
		return o, errors.New("azure-sessions needs config.endpoint, the session pool's management endpoint")
	}
	if o.APIVersion == "" {
		o.APIVersion = DefaultAPIVersion
	}
	if v := cfg["outputLimit"]; v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return o, errors.New("azure-sessions: config.outputLimit must be a positive number of characters")
		}
		o.OutputLimit = n
	}
	c, err := cloudhttp.ParseEntra(credential)
	if err != nil {
		return o, fmt.Errorf("azure-sessions: %w", err)
	}
	o.Credential = c
	return o, nil
}

// Adapter talks to one session pool.
type Adapter struct {
	opts Options
	c    *cloudhttp.Client
}

// New returns a dynamic sessions adapter.
func New(o Options) *Adapter {
	if o.EnvFile == "" {
		o.EnvFile = defaultEnvFile
	}
	hc := o.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: time.Minute}
	}
	c := cloudhttp.New("azure-sessions", o.Endpoint, cloudhttp.Bearer(cloudhttp.Entra(o.Credential, Scope, hc)))
	c.Classify = func(_ int, code string) string {
		if code == "SessionWithIdentifierNotFound" {
			return api.CodeNotFound // sent as 400
		}
		return ""
	}
	return &Adapter{opts: o, c: c}
}

// Endpoint is reported in the trust report.
func (a *Adapter) Endpoint() string { return a.opts.Endpoint }

func (a *Adapter) Info() adapter.Info {
	return adapter.Info{Kind: "azure-sessions", Version: "1", Trust: api.TrustExternal, Isolation: "Hyper-V isolated session (Azure-managed)", Protocol: adapter.ProtocolVersion,
		Description: "Azure Container Apps dynamic sessions (Python code interpreter) at " + a.opts.Endpoint}
}

func (a *Adapter) Capabilities(context.Context) (adapter.Capabilities, error) {
	var regions []string
	if a.opts.Region != "" {
		regions = []string{a.opts.Region}
	}
	return adapter.Capabilities{Features: []string{adapter.FeatureExec, adapter.FeatureFiles, adapter.FeatureMicroVM}, Regions: regions}, nil
}

func (a *Adapter) Capacity(context.Context) (adapter.Capacity, error) { return adapter.Capacity{}, nil }

func (a *Adapter) Health(ctx context.Context) error {
	return a.c.JSON(ctx, http.MethodGet, a.path("/metadata", nil), nil, nil)
}

func (a *Adapter) path(p string, q url.Values) string {
	if q == nil {
		q = url.Values{}
	}
	q.Set("api-version", a.opts.APIVersion)
	return p + "?" + q.Encode()
}

// Identifier names a sandbox's session. Differently IDs are
// "<prefix>_<lowercase ULID>"; the underscore becomes a hyphen so the
// identifier keeps to letters, digits and hyphens.
func Identifier(installation, sandboxID string) string {
	return "dly-" + strings.Replace(installation, "_", "-", 1) + "--" + strings.Replace(sandboxID, "_", "-", 1)
}

// parse reverses Identifier.
func parse(identifier string) (installation, sandboxID string, ok bool) {
	rest, ok := strings.CutPrefix(identifier, "dly-")
	inst, sbx, found := strings.Cut(rest, "--")
	if !ok || !found || inst == "" || sbx == "" {
		return "", "", false
	}
	return strings.Replace(inst, "-", "_", 1), strings.Replace(sbx, "-", "_", 1), true
}

func (a *Adapter) id(ref string) url.Values { return url.Values{"identifier": {ref}} }

func (a *Adapter) instance(identifier, sandboxID string) adapter.Instance {
	return adapter.Instance{Ref: identifier, SandboxID: sandboxID, State: adapter.StateRunning, Region: a.opts.Region}
}

type execution struct {
	Status string `json:"status"`
	Error  *struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	} `json:"error"`
	Result struct {
		Stdout string `json:"stdout"`
		Stderr string `json:"stderr"`
	} `json:"result"`
}

// run executes Python code synchronously in the session.
func (a *Adapter) run(ctx context.Context, ref, code string, timeout int) (execution, error) {
	body := map[string]any{
		"codeInputType": "Inline", "executionType": "Synchronous", "code": code,
		"timeoutInSeconds": timeout, "outputStreamsMaxLength": a.opts.OutputLimit,
	}
	var out execution
	if err := a.c.JSON(ctx, http.MethodPost, a.path("/executions", a.id(ref)), body, &out); err != nil {
		return out, err
	}
	if out.Status == "Failed" || out.Status == "Canceled" {
		msg := out.Status
		if out.Error != nil {
			msg = out.Error.Error.Code + ": " + out.Error.Error.Message
		}
		return out, adapter.SandboxFailed(fmt.Errorf("azure-sessions: execution %s", msg))
	}
	return out, nil
}

func (a *Adapter) Create(ctx context.Context, req adapter.CreateRequest) (adapter.Instance, error) {
	if req.FromSnapshot != "" {
		return adapter.Instance{}, adapter.Unsupported("snapshots")
	}
	ref := Identifier(req.Installation, req.ID)
	// The first request allocates the session; it also stores the env, and
	// writing it again on a replay is harmless.
	env := req.Env
	if env == nil {
		env = map[string]string{}
	}
	if _, err := a.run(ctx, ref, pyshell.EnvProgram(a.opts.EnvFile, env), 60); err != nil {
		return adapter.Instance{}, err
	}
	return a.instance(ref, req.ID), nil
}

func (a *Adapter) Get(ctx context.Context, ref string) (adapter.Instance, error) {
	var s struct {
		Identifier string `json:"identifier"`
	}
	if err := a.c.JSON(ctx, http.MethodGet, a.path("/session", a.id(ref)), nil, &s); err != nil {
		return adapter.Instance{}, err
	}
	_, sbx, _ := parse(ref)
	return a.instance(ref, sbx), nil
}

// List pages through every session in the pool. Pages are skip-based over a
// set that changes as sessions end, so with more than one page a session
// that ends while the list is read can hide another from this pass; the
// pool's page holds 300 sessions, and the provider page says to keep the
// pool's maxConcurrency within it.
func (a *Adapter) List(ctx context.Context, installation string) ([]adapter.Instance, error) {
	var out []adapter.Instance
	next := a.path("/listSessions", nil)
	for next != "" {
		var page struct {
			Sessions []struct {
				Identifier string `json:"identifier"`
			} `json:"sessions"`
			Value []struct {
				Identifier string `json:"identifier"`
			} `json:"value"` // the docs' name for the same list
			NextLink string `json:"nextLink"`
		}
		if err := a.c.JSON(ctx, http.MethodGet, next, nil, &page); err != nil {
			return nil, err
		}
		for _, s := range append(page.Sessions, page.Value...) {
			if inst, sbx, ok := parse(s.Identifier); ok && inst == installation {
				out = append(out, a.instance(s.Identifier, sbx))
			}
		}
		next = page.NextLink
	}
	return out, nil
}

func (a *Adapter) Delete(ctx context.Context, ref string) error {
	err := a.c.JSON(ctx, http.MethodDelete, a.path("/session", a.id(ref)), nil, nil)
	if api.IsCode(err, api.CodeNotFound) {
		return nil
	}
	return err
}

// Exec runs the command through Python's subprocess. One execution lasts
// at most 220 seconds, so commands are stopped after 210 (exit 124).
func (a *Adapter) Exec(ctx context.Context, ref string, req api.ExecRequest) (api.ExecResult, error) {
	if req.TimeoutSeconds <= 0 || req.TimeoutSeconds > maxCmdSeconds {
		req.TimeoutSeconds = maxCmdSeconds
	}
	start := time.Now()
	out, err := a.run(ctx, ref, pyshell.Program(req, a.opts.EnvFile), maxExecSeconds)
	if err != nil {
		return api.ExecResult{}, err
	}
	res, err := pyshell.Parse(out.Result.Stdout, out.Result.Stderr)
	if err != nil {
		return api.ExecResult{}, err
	}
	res.DurationMs = time.Since(start).Milliseconds()
	res.Truncated = len(out.Result.Stdout) >= a.opts.OutputLimit || len(out.Result.Stderr) >= a.opts.OutputLimit
	return res, nil
}

// split maps an absolute path under /mnt/data to the files API's directory
// and name.
func split(p string) (dir, name string, err error) {
	p = path.Clean("/" + p)
	rel, ok := strings.CutPrefix(p, DataDir+"/")
	if !ok {
		return "", "", api.Errorf(api.CodeInvalidRequest, "azure-sessions can only reach files under %s, not %s", DataDir, p)
	}
	dir, name = path.Split(rel)
	return strings.TrimSuffix(dir, "/"), name, nil
}

func (a *Adapter) ReadFile(ctx context.Context, ref, p string) ([]byte, error) {
	dir, name, err := split(p)
	if err != nil {
		return nil, err
	}
	q := a.id(ref)
	if dir != "" {
		q.Set("path", dir)
	}
	resp, err := a.c.Do(ctx, http.MethodGet, a.path("/files/"+url.PathEscape(name)+"/content", q), nil, "", http.Header{"Accept": {"application/octet-stream"}})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 128<<20))
}

func (a *Adapter) WriteFile(ctx context.Context, ref, p string, data []byte) error {
	dir, name, err := split(p)
	if err != nil {
		return err
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", name)
	if err != nil {
		return err
	}
	fw.Write(data)
	mw.Close()
	q := a.id(ref)
	if dir != "" {
		q.Set("path", dir)
	}
	resp, err := a.c.Do(ctx, http.MethodPost, a.path("/files", q), body.Bytes(), mw.FormDataContentType(), nil)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (a *Adapter) ListFiles(ctx context.Context, ref, p string) ([]api.FileInfo, error) {
	q := a.id(ref)
	base := path.Clean("/" + p)
	if base != DataDir {
		dir, name, err := split(base)
		if err != nil {
			return nil, err
		}
		q.Set("path", path.Join(dir, name))
	}
	var out []api.FileInfo
	next := a.path("/files", q)
	for next != "" {
		var page struct {
			Value []struct {
				Name           string    `json:"name"`
				Type           string    `json:"type"`
				SizeInBytes    int64     `json:"sizeInBytes"`
				LastModifiedAt time.Time `json:"lastModifiedAt"`
			} `json:"value"`
			NextLink string `json:"nextLink"`
		}
		if err := a.c.JSON(ctx, http.MethodGet, next, nil, &page); err != nil {
			return nil, err
		}
		for _, f := range page.Value {
			out = append(out, api.FileInfo{Path: base + "/" + f.Name, Name: f.Name, Size: f.SizeInBytes, IsDir: f.Type == "Directory", ModTime: f.LastModifiedAt})
		}
		next = page.NextLink
	}
	return out, nil
}

var (
	_ adapter.ExecutionAdapter = (*Adapter)(nil)
	_ adapter.FileSystem       = (*Adapter)(nil)
)
