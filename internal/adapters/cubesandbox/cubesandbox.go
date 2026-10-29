// Package cubesandbox adapts CubeSandbox (github.com/TencentCloud/CubeSandbox,
// Apache-2.0): KVM microVMs (cube-hypervisor, a Cloud Hypervisor fork) behind
// an E2B-compatible API.
//
// CubeSandbox is self-hosted, so the pool's trust boundary is whatever the
// operator declares. Two APIs are involved, both E2B's:
//
//   - CubeAPI, the lifecycle API (create, get, list by metadata, delete,
//     pause, connect, timeout, snapshots), authenticated with X-API-Key;
//   - envd, E2B's agent inside every sandbox, reached through CubeProxy at
//     <port>-<sandboxID>.<domain>, or with E2b-Sandbox-Id/-Port headers at
//     config.sandboxURL (envd.go).
//
// Written against CubeSandbox v0.7.3 (CubeAPI routes, openapi.yml and the Go
// SDK at commit e298a426, read 2026-10-28) and tested with a fake server only:
// no live run, since the one-click install needs a dedicated KVM host. Known
// differences from E2B: the timeout counts idle time, there is no stop/start,
// envd needs no access token, and snapshots, forks and restores are
// CubeSandbox's own routes.
package cubesandbox

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/different-ai/differently/internal/adapters/httpx"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

const (
	// APIKeyHeader authenticates CubeAPI requests; the server's rate limiter
	// keys on it, so it is preferred over a bearer token.
	APIKeyHeader = "X-API-Key"

	// Metadata keys. CubeAPI matches its metadata filter literally, so keys and
	// values keep to [A-Za-z0-9_.-].
	metaSandbox      = "differently.sandbox-id"
	metaInstallation = "differently.installation"
)

// DefaultFeatures are advertised unless the pool overrides config.features.
// Snapshots always capture memory and disk. ports.expose is opt-in: CubeProxy
// serves every port of a sandbox without authentication unless the sandbox is
// private, so a pool lists it only when that is acceptable.
var DefaultFeatures = []string{
	adapter.FeatureExec, adapter.FeatureExecStream, adapter.FeatureFiles, adapter.FeaturePauseResume,
	adapter.FeatureSnapshotDisk, adapter.FeatureSnapshotMem, adapter.FeatureFork, adapter.FeatureMicroVM,
}

// Options configures the adapter. Pool config keys: apiURL (required, CubeAPI,
// e.g. http://cube-api:3000), templateId (default template; a request's
// template overrides it), domain (default: what CubeAPI reports, else
// cube.app), sandboxURL (CubeProxy's address for header routing instead of
// <port>-<id>.<domain> host names), proxyScheme (https|http, default https),
// caFile (CubeProxy's CA, e.g. the one-click mkcert CA), user (envd user,
// default root), features (comma list).
type Options struct {
	APIURL       string
	APIKey       string
	Trust        string
	TemplateID   string
	Domain       string
	SandboxURL   string
	ProxyScheme  string
	CAFile       string
	User         string
	Features     []string
	ReadyTimeout time.Duration
}

// FromConfig maps pool config to options.
func FromConfig(cfg map[string]string, trust, apiKey string) (Options, error) {
	o := Options{
		APIURL: strings.TrimRight(cfg["apiURL"], "/"), APIKey: apiKey, Trust: trust, TemplateID: cfg["templateId"],
		Domain: cfg["domain"], SandboxURL: strings.TrimRight(cfg["sandboxURL"], "/"), ProxyScheme: cfg["proxyScheme"],
		CAFile: cfg["caFile"], User: cfg["user"], Features: slices.Clone(DefaultFeatures),
	}
	if o.APIURL == "" {
		return o, errors.New("cubesandbox pools need config.apiURL (CubeAPI, e.g. http://cube-api:3000)")
	}
	switch o.ProxyScheme {
	case "":
		o.ProxyScheme = "https"
	case "https", "http":
	default:
		return o, fmt.Errorf("cubesandbox config.proxyScheme must be https or http, not %q", o.ProxyScheme)
	}
	if o.SandboxURL != "" && !strings.HasPrefix(o.SandboxURL, "http://") && !strings.HasPrefix(o.SandboxURL, "https://") {
		return o, fmt.Errorf("cubesandbox config.sandboxURL must start with http:// or https://, not %q", o.SandboxURL)
	}
	if o.User == "" {
		o.User = "root"
	}
	if f := strings.TrimSpace(cfg["features"]); f != "" {
		o.Features = nil
		for _, v := range strings.Split(f, ",") {
			if v = strings.TrimSpace(v); v != "" {
				o.Features = append(o.Features, v)
			}
		}
	}
	return o, nil
}

// Adapter talks to one CubeSandbox cluster.
type Adapter struct {
	opts Options
	c    *httpx.Client // CubeAPI
	data *httpx.Client // envd and ports, through CubeProxy

	mu      sync.Mutex
	domains map[string]string // sandbox id -> its domain, as CubeAPI reported it
}

// New returns an adapter.
func New(o Options) (*Adapter, error) {
	if o.ReadyTimeout == 0 {
		o.ReadyTimeout = 2 * time.Minute
	}
	data := httpx.New("cubesandbox envd", "", "")
	if o.CAFile != "" {
		pem, err := os.ReadFile(o.CAFile)
		if err != nil {
			return nil, fmt.Errorf("cubesandbox: read config.caFile: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("cubesandbox: config.caFile %s holds no PEM certificate", o.CAFile)
		}
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		data.HTTP.Transport = tr
	}
	return &Adapter{opts: o, c: httpx.New("cubesandbox", o.APIURL, ""), data: data, domains: map[string]string{}}, nil
}

// Endpoint is reported in the trust report.
func (a *Adapter) Endpoint() string { return a.opts.APIURL }

func (a *Adapter) header() http.Header {
	h := http.Header{}
	if a.opts.APIKey != "" {
		h.Set(APIKeyHeader, a.opts.APIKey)
	}
	return h
}

// call sends a CubeAPI request and decodes the JSON response into out.
func (a *Adapter) call(ctx context.Context, method, path string, in, out any) (http.Header, error) {
	var body io.Reader
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body, ct = strings.NewReader(string(b)), "application/json"
	}
	resp, err := a.c.Do(ctx, method, path, body, ct, a.header())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return resp.Header, nil
	}
	return resp.Header, json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

// sandbox is CubeAPI's Sandbox, ListedSandbox and SandboxDetail in one.
type sandbox struct {
	SandboxID  string            `json:"sandboxID"`
	TemplateID string            `json:"templateID"`
	State      string            `json:"state"`
	Domain     string            `json:"domain"`
	StartedAt  string            `json:"startedAt"`
	Metadata   map[string]string `json:"metadata"`
}

func (a *Adapter) Info() adapter.Info {
	return adapter.Info{Kind: "cubesandbox", Version: "1", Trust: a.opts.Trust, Isolation: "KVM microVM (cube-hypervisor)", Protocol: adapter.ProtocolVersion, Description: "CubeSandbox at " + a.opts.APIURL}
}

func (a *Adapter) Capabilities(context.Context) (adapter.Capabilities, error) {
	return adapter.Capabilities{Features: a.opts.Features}, nil
}

func (a *Adapter) Capacity(context.Context) (adapter.Capacity, error) { return adapter.Capacity{}, nil }

// Health checks CubeAPI, and with a default template that the key works and
// the template exists. It avoids listing sandboxes: every CubeAPI list walks
// the whole cluster, and the pool is probed every few seconds.
func (a *Adapter) Health(ctx context.Context) error {
	if _, err := a.call(ctx, http.MethodGet, "/health", nil, nil); err != nil {
		return err
	}
	if a.opts.TemplateID == "" {
		return nil
	}
	_, err := a.call(ctx, http.MethodGet, "/templates/"+url.PathEscape(a.opts.TemplateID), nil, nil)
	if api.IsCode(err, api.CodeNotFound) {
		return adapter.Unavailable(fmt.Errorf("cubesandbox: template %s not found", a.opts.TemplateID))
	}
	return err
}

func state(s sandbox) string {
	switch s.State {
	case "running":
		return adapter.StateRunning
	case "paused":
		return adapter.StatePaused
	}
	return adapter.StatePending // pausing, unknown (creating, exited, unreadable)
}

func (a *Adapter) inst(s sandbox) adapter.Instance {
	if s.Domain != "" {
		a.mu.Lock()
		a.domains[s.SandboxID] = s.Domain
		a.mu.Unlock()
	}
	return adapter.Instance{Ref: s.SandboxID, SandboxID: s.Metadata[metaSandbox], State: state(s)}
}

// find lists every sandbox whose metadata has key=value, page by page. Any
// failed page fails the whole call.
func (a *Adapter) find(ctx context.Context, key, value string) ([]sandbox, error) {
	var out []sandbox
	next := ""
	for range 1000 {
		q := url.Values{"metadata": {key + "=" + value}, "limit": {"100"}}
		if next != "" {
			q.Set("nextToken", next)
		}
		var page []sandbox
		h, err := a.call(ctx, http.MethodGet, "/v2/sandboxes?"+q.Encode(), nil, &page)
		if err != nil {
			return nil, err
		}
		for _, s := range page {
			if s.Metadata[key] == value { // never trust a filter we did not write
				out = append(out, s)
			}
		}
		if next = h.Get("X-Next-Token"); next == "" {
			return out, nil
		}
	}
	return nil, adapter.Unavailable(errors.New("cubesandbox: too many pages of sandboxes"))
}

func (a *Adapter) get(ctx context.Context, ref string) (sandbox, error) {
	var s sandbox
	_, err := a.call(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(ref), nil, &s)
	return s, err
}

func (a *Adapter) Get(ctx context.Context, ref string) (adapter.Instance, error) {
	s, err := a.get(ctx, ref)
	return a.inst(s), err
}

// Create implements adapter.ExecutionAdapter. CubeAPI takes no client ID, so
// Create is idempotent through metadata: a sandbox already carrying the
// Differently ID is reused. A snapshot is restored by creating from its ID,
// which CubeSandbox treats as a template.
func (a *Adapter) Create(ctx context.Context, req adapter.CreateRequest) (adapter.Instance, error) {
	existing, err := a.find(ctx, metaSandbox, req.ID)
	if err != nil {
		return adapter.Instance{}, err
	}
	var s sandbox
	if len(existing) > 0 {
		s = existing[0]
	} else {
		template := a.opts.TemplateID
		switch {
		case req.FromSnapshot != "":
			template = req.FromSnapshot
		case req.Template != "":
			template = req.Template
		}
		if template == "" {
			return adapter.Instance{}, api.Errorf(api.CodeInvalidRequest, "cubesandbox: no template: set the pool's config.templateId or the request's template")
		}
		body := map[string]any{
			"templateID": template,
			"metadata":   map[string]string{metaSandbox: req.ID, metaInstallation: req.Installation},
			"autoPause":  false, // Differently owns pause and expiry
		}
		if len(req.Env) > 0 {
			body["envVars"] = req.Env
		}
		if req.TTLSeconds > 0 {
			// The backstop. CubeSandbox counts idle seconds, so it never fires
			// before the TTL; it may fire later, or not at all while in use.
			body["timeout"] = req.TTLSeconds
		}
		if _, err := a.call(ctx, http.MethodPost, "/sandboxes", body, &s); err != nil {
			return adapter.Instance{}, createError(err)
		}
	}
	if s, err = a.waitRunning(ctx, s.SandboxID); err != nil {
		return adapter.Instance{}, err
	}
	if err := a.waitEnvd(ctx, s.SandboxID); err != nil {
		return adapter.Instance{}, err
	}
	return a.inst(s), nil
}

// createError reclassifies failures that concern this one sandbox: a
// template that does not exist is the request's fault, and a create that ran
// past CubeAPI's 30 s budget (408) is worth retrying without blaming the pool.
func createError(err error) error {
	var e *api.Error
	if !errors.As(err, &e) {
		return err
	}
	switch e.Details["providerStatus"] {
	case "404":
		return &api.Error{Code: api.CodeInvalidRequest, Message: e.Message + "; the template does not exist", Details: e.Details}
	case "408":
		return &api.Error{Code: adapter.CodeSandboxFailed, Message: e.Message + "; the sandbox did not start within CubeAPI's create budget", Details: e.Details}
	}
	return err
}

func (a *Adapter) waitRunning(ctx context.Context, ref string) (sandbox, error) {
	var s sandbox
	err := httpx.Poll(ctx, 300*time.Millisecond, a.opts.ReadyTimeout, func() (bool, error) {
		var err error
		s, err = a.get(ctx, ref)
		return err == nil && s.State == "running", err
	})
	return s, err
}

func (a *Adapter) List(ctx context.Context, installation string) ([]adapter.Instance, error) {
	items, err := a.find(ctx, metaInstallation, installation)
	if err != nil {
		return nil, err
	}
	out := make([]adapter.Instance, 0, len(items))
	for _, s := range items {
		out = append(out, a.inst(s))
	}
	return out, nil
}

func (a *Adapter) Delete(ctx context.Context, ref string) error {
	_, err := a.call(ctx, http.MethodDelete, "/sandboxes/"+url.PathEscape(ref), nil, nil)
	a.mu.Lock()
	delete(a.domains, ref)
	a.mu.Unlock()
	if api.IsCode(err, api.CodeNotFound) {
		return nil
	}
	return err
}

// SetExpiry moves CubeSandbox's idle timeout so it cannot fire before at. The
// timeout is measured from the last activity, which may be as early as the
// start, so it is set to the whole span from the start to at.
func (a *Adapter) SetExpiry(ctx context.Context, ref string, at time.Time) error {
	s, err := a.get(ctx, ref)
	if err != nil {
		return err
	}
	started, err := time.Parse(time.RFC3339, s.StartedAt)
	if err != nil {
		started = time.Now()
	}
	secs := int(at.Sub(started).Seconds()) + 1
	_, err = a.call(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(ref)+"/timeout", map[string]int{"timeout": max(secs, 1)}, nil)
	return err
}

// Pause snapshots memory and disk in place and waits for paused.
func (a *Adapter) Pause(ctx context.Context, ref string) error {
	if _, err := a.call(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(ref)+"/pause", nil, nil); err != nil {
		return err
	}
	return httpx.Poll(ctx, 300*time.Millisecond, a.opts.ReadyTimeout, func() (bool, error) {
		s, err := a.get(ctx, ref)
		return err == nil && s.State == "paused", err
	})
}

// Resume uses connect, which resumes a paused sandbox (and answers 200 for a
// running one), then waits for envd.
func (a *Adapter) Resume(ctx context.Context, ref string) error {
	if _, err := a.call(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(ref)+"/connect", map[string]any{}, nil); err != nil {
		return err
	}
	if _, err := a.waitRunning(ctx, ref); err != nil {
		return err
	}
	return a.waitEnvd(ctx, ref)
}

// Snapshot captures memory and the filesystem; it is reported as memory
// whatever kind was asked for, since that is what a restore brings back.
func (a *Adapter) Snapshot(ctx context.Context, ref, kind string) (adapter.Snapshot, error) {
	var out struct {
		SnapshotID string `json:"snapshotID"`
	}
	if _, err := a.call(ctx, http.MethodPost, "/sandboxes/"+url.PathEscape(ref)+"/snapshots", map[string]string{"name": "differently-" + ref + "-" + fmt.Sprint(time.Now().Unix())}, &out); err != nil {
		return adapter.Snapshot{}, err
	}
	if out.SnapshotID == "" {
		return adapter.Snapshot{}, adapter.Unavailable(errors.New("cubesandbox: snapshot response carried no snapshotID"))
	}
	return adapter.Snapshot{Ref: out.SnapshotID, Kind: "memory"}, nil
}

// DeleteSnapshot removes a snapshot, which CubeSandbox stores as a template.
func (a *Adapter) DeleteSnapshot(ctx context.Context, ref string) error {
	_, err := a.call(ctx, http.MethodDelete, "/templates/"+url.PathEscape(ref), nil, nil)
	if api.IsCode(err, api.CodeNotFound) {
		return nil
	}
	return err
}

var (
	_ adapter.ExecutionAdapter = (*Adapter)(nil)
	_ adapter.Streamer         = (*Adapter)(nil)
	_ adapter.FileSystem       = (*Adapter)(nil)
	_ adapter.Pauser           = (*Adapter)(nil)
	_ adapter.Snapshotter      = (*Adapter)(nil)
	_ adapter.Expirer          = (*Adapter)(nil)
	_ adapter.PortExposer      = (*Adapter)(nil)
)
