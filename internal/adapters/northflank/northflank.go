// Package northflank adapts Northflank sandboxes: each Differently sandbox
// is one deployment service in a Northflank project, kept alive with
// `sleep infinity` so commands can run in it.
//
// Northflank runs the workload (on its cloud, or in a cluster it manages in
// the operator's own cloud account with BYOC), so pools default to
// TrustExternal; an operator whose policy treats a BYOC cluster as private
// may declare trust: private.
//
// Mapping, from Northflank's API reference, sandbox guides and OpenAPI
// document (read 2026-10-28):
//
//   - the service name is "dly-<installation hash>-<sandbox ULID>"; a create
//     that conflicts (409) reads the existing service, so replays are safe;
//   - List pages through the project's services and keeps that prefix;
//   - commands run over Northflank's command-exec WebSocket, the only exec
//     API; cwd, stdin and the timeout are folded into the command line
//     (shellcmd), and files move as base64 through commands;
//   - the TTL becomes createOptions.expiryTime (5 minutes to 7 days);
//   - ports are public HTTP ports on *.code.run.
//
// Not offered: stop/start and pause (Northflank's pause wipes ephemeral
// storage), snapshots (container snapshots are new and Kata-only).
//
// Tested against a fake server built from the published API, not yet
// against a live account.
package northflank

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/different-ai/differently/internal/adapters/cloudhttp"
	"github.com/different-ai/differently/internal/adapters/httpx"
	"github.com/different-ai/differently/internal/adapters/shellcmd"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// DefaultAPIURL is Northflank's API.
const DefaultAPIURL = "https://api.northflank.com"

const (
	defaultImage = "ubuntu:24.04"
	defaultPlan  = "nf-compute-200"
	// Northflank's expiryTime accepts 300 s to 7 days.
	minExpiry = 300
	maxExpiry = 7 * 24 * 3600
	// backstopGrace keeps Northflank's deadline behind Differently's own,
	// audited expiry.
	backstopGrace = 5 * 60
	// healthEvery spaces out probes: the default API limit is 1,000
	// requests an hour and every replica probes every 5 seconds.
	healthEvery = 30 * time.Second
)

// Options configures the adapter. Pool config keys: projectId (required),
// teamId (for organization-scoped tokens), image, plan, apiURL. The
// credential is a Northflank API token.
type Options struct {
	APIURL       string
	Token        string
	Project      string
	Team         string
	Image        string
	Plan         string
	ReadyTimeout time.Duration
	PollEvery    time.Duration
}

// FromConfig maps pool config to options.
func FromConfig(cfg map[string]string, token string) (Options, error) {
	o := Options{APIURL: strings.TrimRight(cfg["apiURL"], "/"), Token: token, Project: cfg["projectId"], Team: cfg["teamId"], Image: cfg["image"], Plan: cfg["plan"]}
	if o.Project == "" {
		return o, errors.New("northflank needs config.projectId")
	}
	if o.APIURL == "" {
		o.APIURL = DefaultAPIURL
	}
	if o.Image == "" {
		o.Image = defaultImage
	}
	if o.Plan == "" {
		o.Plan = defaultPlan
	}
	return o, nil
}

// Adapter talks to one Northflank project.
type Adapter struct {
	opts Options
	c    *cloudhttp.Client
	ws   *http.Client

	mu        sync.Mutex
	checkedAt time.Time
	healthErr error
}

// New returns a Northflank adapter.
func New(o Options) *Adapter {
	if o.ReadyTimeout == 0 {
		o.ReadyTimeout = 5 * time.Minute
	}
	if o.PollEvery == 0 {
		o.PollEvery = 2 * time.Second // polling counts against the hourly limit
	}
	auth := func(r *http.Request, _ []byte) error { r.Header.Set("Authorization", "Bearer "+o.Token); return nil }
	return &Adapter{opts: o, c: cloudhttp.New("northflank", o.APIURL, auth), ws: &http.Client{}}
}

// Endpoint is reported in the trust report.
func (a *Adapter) Endpoint() string { return a.opts.APIURL }

func (a *Adapter) Info() adapter.Info {
	return adapter.Info{Kind: "northflank", Version: "1", Trust: api.TrustExternal, Isolation: "microVM for CPU workloads (Northflank-managed; gVisor for GPU)", Protocol: adapter.ProtocolVersion,
		Description: "Northflank services in project " + a.opts.Project}
}

func (a *Adapter) Capabilities(context.Context) (adapter.Capabilities, error) {
	// Resources come from the pool's plan, not from each request. The
	// default per-instance quota is 8 vCPU and 16 GiB.
	return adapter.Capabilities{Features: []string{
		adapter.FeatureExec, adapter.FeatureExecStream, adapter.FeatureFiles, adapter.FeaturePorts, adapter.FeatureMicroVM,
	}, MaxCPU: 8, MaxMemoryMiB: 16384}, nil
}

func (a *Adapter) Capacity(context.Context) (adapter.Capacity, error) { return adapter.Capacity{}, nil }

// Health probes at most every 30 seconds and repeats the last answer in
// between.
func (a *Adapter) Health(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.checkedAt.IsZero() && time.Since(a.checkedAt) < healthEvery {
		return a.healthErr
	}
	a.healthErr = a.c.JSON(ctx, http.MethodGet, a.project()+"/services?per_page=1", nil, nil)
	a.checkedAt = time.Now()
	return a.healthErr
}

func (a *Adapter) project() string {
	p := "/v1/projects/" + url.PathEscape(a.opts.Project)
	if a.opts.Team != "" {
		p = "/v1/teams/" + url.PathEscape(a.opts.Team) + p[3:]
	}
	return p
}

func (a *Adapter) service(ref string) string { return a.project() + "/services/" + url.PathEscape(ref) }

// prefix is the service-name prefix of one installation: a short hash keeps
// the name within Northflank's 54 characters.
func prefix(installation string) string {
	sum := sha256.Sum256([]byte(installation))
	return "dly-" + hex.EncodeToString(sum[:4]) + "-"
}

// serviceName is "dly-<hash>-<ULID>" for a sandbox ID "sbx_<ULID>".
func serviceName(installation, sandboxID string) string {
	_, ulid, ok := strings.Cut(sandboxID, "_")
	if !ok {
		ulid = sandboxID
	}
	return prefix(installation) + strings.ToLower(strings.ReplaceAll(ulid, "_", "-"))
}

type service struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Status      struct {
		Deployment struct {
			Status string `json:"status"`
		} `json:"deployment"`
	} `json:"status"`
	Deployment struct {
		Instances *int `json:"instances"`
	} `json:"deployment"`
	ServicePaused bool `json:"servicePaused"`
	Ports         []struct {
		Name         string `json:"name"`
		InternalPort int    `json:"internalPort"`
		Public       bool   `json:"public"`
		Protocol     string `json:"protocol"`
		DNS          string `json:"dns"`
	} `json:"ports"`
}

func state(s service) string {
	if s.ServicePaused || s.Deployment.Instances != nil && *s.Deployment.Instances == 0 {
		return adapter.StateStopped
	}
	switch s.Status.Deployment.Status {
	case "COMPLETED":
		return adapter.StateRunning
	case "FAILED":
		return adapter.StateError
	}
	return adapter.StatePending
}

func (a *Adapter) instance(s service, sandboxID string) adapter.Instance {
	return adapter.Instance{Ref: s.ID, SandboxID: sandboxID, State: state(s)}
}

func (a *Adapter) get(ctx context.Context, ref string) (service, error) {
	var out struct {
		Data service `json:"data"`
	}
	err := a.c.JSON(ctx, http.MethodGet, a.service(ref), nil, &out)
	return out.Data, err
}

func (a *Adapter) Create(ctx context.Context, req adapter.CreateRequest) (adapter.Instance, error) {
	if req.FromSnapshot != "" {
		return adapter.Instance{}, adapter.Unsupported("snapshots")
	}
	name := serviceName(req.Installation, req.ID)
	image := a.opts.Image
	if req.Template != "" {
		image = req.Template
	}
	body := map[string]any{
		"name":        name,
		"description": "Differently sandbox " + req.ID + " (installation " + req.Installation + ")",
		"billing":     map[string]any{"deploymentPlan": a.opts.Plan},
		"deployment": map[string]any{
			"instances": 1,
			"external":  map[string]any{"imagePath": image},
			"docker":    map[string]any{"configType": "customEntrypointCustomCommand", "customEntrypoint": "/bin/sh", "customCommand": "-c 'sleep infinity'"},
		},
	}
	if len(req.Env) > 0 {
		body["runtimeEnvironment"] = req.Env
	}
	if ttl := req.TTLSeconds + backstopGrace; req.TTLSeconds > 0 && ttl <= maxExpiry {
		body["createOptions"] = map[string]any{"expiryTime": max(ttl, minExpiry)}
	}
	var out struct {
		Data service `json:"data"`
	}
	err := a.c.JSON(ctx, http.MethodPost, a.project()+"/services/deployment", body, &out)
	s := out.Data
	if api.IsCode(err, api.CodeInvalidState) {
		s, err = a.get(ctx, name) // a replay: the service exists
	}
	if err != nil {
		return adapter.Instance{}, err
	}
	if s.ID == "" {
		s.ID = name
	}
	err = httpx.Poll(ctx, a.opts.PollEvery, a.opts.ReadyTimeout, func() (bool, error) {
		switch state(s) {
		case adapter.StateRunning:
			return true, nil
		case adapter.StateError:
			return false, adapter.SandboxFailed(fmt.Errorf("northflank service %s failed to deploy", s.ID))
		}
		var err error
		s, err = a.get(ctx, s.ID)
		return false, err
	})
	if err != nil {
		return adapter.Instance{}, err
	}
	return a.instance(s, req.ID), nil
}

func (a *Adapter) Get(ctx context.Context, ref string) (adapter.Instance, error) {
	s, err := a.get(ctx, ref)
	if err != nil {
		return adapter.Instance{}, err
	}
	return a.instance(s, sandboxFromDescription(s.Description)), nil
}

// sandboxFromDescription reads the sandbox ID back from the description
// Create writes.
func sandboxFromDescription(d string) string {
	rest, ok := strings.CutPrefix(d, "Differently sandbox ")
	if !ok {
		return ""
	}
	id, _, _ := strings.Cut(rest, " ")
	return id
}

func (a *Adapter) List(ctx context.Context, installation string) ([]adapter.Instance, error) {
	p := prefix(installation)
	var out []adapter.Instance
	cursor := ""
	for {
		path := a.project() + "/services?per_page=100"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		var page struct {
			Data struct {
				Services []service `json:"services"`
			} `json:"data"`
			Pagination struct {
				HasNextPage bool   `json:"hasNextPage"`
				Cursor      string `json:"cursor"`
			} `json:"pagination"`
		}
		if err := a.c.JSON(ctx, http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		for _, s := range page.Data.Services {
			if !strings.HasPrefix(s.Name, p) {
				continue
			}
			if s.ID == "" {
				s.ID = s.Name
			}
			// The ULID in the name gives the sandbox ID without a read per service.
			out = append(out, a.instance(s, "sbx_"+strings.TrimPrefix(s.Name, p)))
		}
		if !page.Pagination.HasNextPage || page.Pagination.Cursor == "" {
			return out, nil
		}
		cursor = page.Pagination.Cursor
	}
}

func (a *Adapter) Delete(ctx context.Context, ref string) error {
	err := a.c.JSON(ctx, http.MethodDelete, a.service(ref)+"?delete_child_objects=true", nil, nil)
	if api.IsCode(err, api.CodeNotFound) || isGone(err) {
		return nil
	}
	return err
}

func isGone(err error) bool {
	var e *api.Error
	return errors.As(err, &e) && e.Details["providerStatus"] == "410"
}

// execURL is the command-exec WebSocket for a service.
func (a *Adapter) execURL(ref string) string {
	base := strings.Replace(strings.Replace(a.opts.APIURL, "https://", "wss://", 1), "http://", "ws://", 1)
	p := "/v1/command-exec/projects/" + url.PathEscape(a.opts.Project) + "/services/" + url.PathEscape(ref)
	if a.opts.Team != "" {
		p = "/v1/command-exec/teams/" + url.PathEscape(a.opts.Team) + "/projects/" + url.PathEscape(a.opts.Project) + "/services/" + url.PathEscape(ref)
	}
	return base + p
}

// stream runs one command line over the command-exec WebSocket, passing
// output to emit, and returns the exit code.
func (a *Adapter) stream(ctx context.Context, ref, cmd string, emit func(api.ExecEvent) error) (int, error) {
	conn, err := dialWS(ctx, a.ws, a.execURL(ref))
	if err != nil {
		var ue *upgradeError
		if errors.As(err, &ue) {
			return 0, a.c.Error(ue.status, ue.header, ue.body)
		}
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, adapter.Unavailable(fmt.Errorf("northflank: %w", err))
	}
	defer conn.close()
	init, _ := json.Marshal(map[string]any{"type": "init", "data": map[string]any{
		"auth":    map[string]string{"type": "apiToken", "apiToken": a.opts.Token},
		"context": map[string]any{"command": []string{"/bin/sh", "-c", cmd}, "shell": "none", "encoding": "utf-8", "tty": false},
	}})
	if err := conn.writeText(init); err != nil {
		return 0, adapter.Unavailable(fmt.Errorf("northflank: %w", err))
	}
	end, _ := json.Marshal(map[string]string{"type": "stdInEnd"})
	exit := -1
	for {
		raw, err := conn.read()
		if err != nil {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			if exit >= 0 {
				return exit, nil // closed after the completion message
			}
			return 0, adapter.Unavailable(fmt.Errorf("northflank: the command stream ended without an exit code: %w", err))
		}
		var msg struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(raw, &msg) != nil {
			continue
		}
		switch msg.Type {
		case "init":
			var d struct {
				Auth    string `json:"auth"`
				Message string `json:"message"`
			}
			json.Unmarshal(msg.Data, &d)
			if d.Auth != "successful" {
				return 0, adapter.Unavailable(errors.New("northflank: credentials were rejected; check the pool's credential reference"))
			}
			if err := conn.writeText(end); err != nil {
				return 0, adapter.Unavailable(fmt.Errorf("northflank: %w", err))
			}
		case "stdOut", "stdErr":
			var data string
			json.Unmarshal(msg.Data, &data)
			stream := "stdout"
			if msg.Type == "stdErr" {
				stream = "stderr"
			}
			if err := emit(api.ExecEvent{Stream: stream, Data: data}); err != nil {
				return 0, err
			}
		case "completion":
			var d struct {
				ExitCode json.RawMessage `json:"exitCode"`
				Status   string          `json:"status"`
				Message  string          `json:"message"`
			}
			json.Unmarshal(msg.Data, &d)
			// exitCode arrives as a number or as a string such as "42".
			code, err := strconv.Atoi(strings.Trim(string(d.ExitCode), `"`))
			if err != nil {
				return 0, adapter.SandboxFailed(fmt.Errorf("northflank: the command ended without an exit code (%s %s)", d.Status, d.Message))
			}
			exit = code
			// Output can still follow; stop waiting for the server's close
			// after a short grace.
			t := time.AfterFunc(2*time.Second, func() { conn.rw.Close() })
			defer t.Stop()
		case "error":
			var d struct {
				Message string `json:"message"`
			}
			json.Unmarshal(msg.Data, &d)
			return 0, adapter.SandboxFailed(fmt.Errorf("northflank: %s", d.Message))
		}
	}
}

func (a *Adapter) ExecStream(ctx context.Context, ref string, req api.ExecRequest, emit func(api.ExecEvent) error) (int, error) {
	return a.stream(ctx, ref, shellcmd.Script(req), emit)
}

func (a *Adapter) Exec(ctx context.Context, ref string, req api.ExecRequest) (api.ExecResult, error) {
	return a.command(ctx, ref, shellcmd.Script(req))
}

func (a *Adapter) command(ctx context.Context, ref, cmd string) (api.ExecResult, error) {
	var stdout, stderr strings.Builder
	start := time.Now()
	code, err := a.stream(ctx, ref, cmd, func(e api.ExecEvent) error {
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

func (a *Adapter) run(ref string) shellcmd.Run {
	return func(ctx context.Context, cmd string) (api.ExecResult, error) { return a.command(ctx, ref, cmd) }
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

// ExposePort adds a public HTTP port and returns its *.code.run URL. The
// URL is public: anyone with it can reach the port.
func (a *Adapter) ExposePort(ctx context.Context, ref string, port int) (string, error) {
	s, err := a.get(ctx, ref)
	if err != nil {
		return "", err
	}
	ports := []map[string]any{}
	for _, p := range s.Ports {
		if p.InternalPort == port && p.Public && p.DNS != "" {
			return "https://" + p.DNS, nil
		}
		if p.InternalPort != port {
			ports = append(ports, map[string]any{"name": p.Name, "internalPort": p.InternalPort, "public": p.Public, "protocol": p.Protocol})
		}
	}
	// PATCH replaces the whole port list, so the existing ports are kept.
	ports = append(ports, map[string]any{"name": "p" + strconv.Itoa(port), "internalPort": port, "public": true, "protocol": "HTTP"})
	if err := a.c.JSON(ctx, http.MethodPatch, a.project()+"/services/deployment/"+url.PathEscape(ref), map[string]any{"ports": ports}, nil); err != nil {
		return "", err
	}
	var dns string
	err = httpx.Poll(ctx, a.opts.PollEvery, a.opts.ReadyTimeout, func() (bool, error) {
		s, err := a.get(ctx, ref)
		for _, p := range s.Ports {
			if p.InternalPort == port && p.DNS != "" {
				dns = p.DNS
			}
		}
		return dns != "", err
	})
	if err != nil {
		return "", err
	}
	return "https://" + dns, nil
}

var (
	_ adapter.ExecutionAdapter = (*Adapter)(nil)
	_ adapter.Streamer         = (*Adapter)(nil)
	_ adapter.FileSystem       = (*Adapter)(nil)
	_ adapter.PortExposer      = (*Adapter)(nil)
)
