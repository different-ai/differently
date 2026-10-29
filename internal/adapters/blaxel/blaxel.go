// Package blaxel adapts Blaxel's hosted sandbox API.
//
// Blaxel is hosted-only, so pools on this adapter are always TrustExternal.
// The control plane (api.blaxel.ai/v0) creates and deletes sandboxes; each
// sandbox serves its own API at its metadata.url, which runs commands and
// moves files. Notable differences from other backends, handled here:
//
//   - sandboxes go to standby on their own after a few seconds without a
//     connection, keeping memory, and wake on the next request; there is no
//     stop, start or pause call, so none is advertised;
//   - CPU follows memory (one core per 2048 MB), and the root filesystem lives
//     in memory, so there is no disk size to request;
//   - the API has no stdin for a streamed command: stdin is staged in a file;
//   - snapshots and forks are a private preview and are not advertised.
//
// Verified against Blaxel's published control-plane and sandbox OpenAPI specs
// (2026-10-28), not yet against a live account.
package blaxel

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
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/different-ai/differently/internal/adapters/httpx"
	"github.com/different-ai/differently/internal/id"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// DefaultAPIURL is Blaxel's control plane.
const DefaultAPIURL = "https://api.blaxel.ai/v0"

// DefaultImage is Blaxel's base sandbox image.
const DefaultImage = "blaxel/base-image:latest"

// listVersion is the first API version with paginated listings.
const listVersion = "2026-04-28"

// Label keys. Blaxel labels are plain strings, so the keys stay short.
const (
	labelSandbox      = "dly-sandbox"
	labelInstallation = "dly-installation"
)

// Options configures the adapter. Pool config keys: apiURL, workspace
// (required), region, image.
type Options struct {
	APIURL       string
	APIKey       string
	Workspace    string
	Region       string
	Image        string
	ReadyTimeout time.Duration
}

// FromConfig maps pool config to options.
func FromConfig(cfg map[string]string, apiKey string) (Options, error) {
	o := Options{APIURL: cfg["apiURL"], APIKey: apiKey, Workspace: cfg["workspace"], Region: cfg["region"], Image: cfg["image"]}
	if o.Workspace == "" {
		return o, errors.New("blaxel pools need config.workspace (the Blaxel workspace name)")
	}
	if o.APIURL == "" {
		o.APIURL = DefaultAPIURL
	}
	if o.Image == "" {
		o.Image = DefaultImage
	}
	return o, nil
}

// Adapter talks to Blaxel.
type Adapter struct {
	opts Options
	c    *httpx.Client

	mu   sync.Mutex
	urls map[string]string // sandbox name -> sandbox API base URL
}

// New returns a Blaxel adapter.
func New(o Options) *Adapter {
	if o.ReadyTimeout == 0 {
		o.ReadyTimeout = 5 * time.Minute
	}
	c := httpx.New("blaxel", o.APIURL, o.APIKey)
	// The sandbox API reads the key from X-Blaxel-Authorization; the control
	// plane accepts either header.
	c.Header = http.Header{"X-Blaxel-Workspace": {o.Workspace}, "X-Blaxel-Authorization": {"Bearer " + o.APIKey}}
	return &Adapter{opts: o, c: c, urls: map[string]string{}}
}

// Endpoint is reported in the trust report.
func (a *Adapter) Endpoint() string { return a.opts.APIURL }

type sandbox struct {
	Metadata struct {
		Name   string            `json:"name"`
		URL    string            `json:"url"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Region string `json:"region"`
	} `json:"spec"`
	Status string `json:"status"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func (a *Adapter) Info() adapter.Info {
	return adapter.Info{Kind: "blaxel", Version: "1", Trust: api.TrustExternal, Isolation: "microVM (Blaxel-managed)", Protocol: adapter.ProtocolVersion, Description: "Blaxel sandboxes in workspace " + a.opts.Workspace}
}

func (a *Adapter) Capabilities(context.Context) (adapter.Capabilities, error) {
	var regions []string
	if a.opts.Region != "" {
		regions = []string{a.opts.Region}
	}
	// Per-sandbox limits depend on the workspace's tier; Blaxel shows them only
	// in its console, so none is declared here.
	return adapter.Capabilities{Features: []string{
		adapter.FeatureExec, adapter.FeatureExecStream, adapter.FeatureFiles, adapter.FeaturePorts, adapter.FeatureMicroVM,
	}, Regions: regions}, nil
}

func (a *Adapter) Capacity(context.Context) (adapter.Capacity, error) { return adapter.Capacity{}, nil }

func (a *Adapter) Health(ctx context.Context) error {
	resp, err := a.c.Do(ctx, http.MethodGet, "/sandboxes?limit=1", nil, "", http.Header{"Blaxel-Version": {listVersion}})
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// name derives Blaxel's per-workspace unique name from the sandbox ID, which
// makes create idempotent.
func name(sandboxID string) string { return strings.ReplaceAll(strings.ToLower(sandboxID), "_", "-") }

func state(s sandbox) string {
	switch s.Status {
	case "DEPLOYED":
		return adapter.StateRunning // active or in standby; a request wakes it
	case "FAILED":
		return adapter.StateError
	case "DEACTIVATING", "DEACTIVATED", "ARCHIVING", "ARCHIVED":
		return adapter.StateStopped
	}
	return adapter.StatePending
}

func gone(s sandbox) bool { return s.Status == "TERMINATED" || s.Status == "DELETING" }

func (s sandbox) message() string {
	var msgs []string
	for _, e := range s.Errors {
		if e.Message != "" {
			msgs = append(msgs, e.Message)
		}
	}
	return strings.Join(msgs, "; ")
}

func (a *Adapter) instance(s sandbox) adapter.Instance {
	a.mu.Lock()
	if s.Metadata.URL != "" {
		a.urls[s.Metadata.Name] = strings.TrimRight(s.Metadata.URL, "/")
	}
	a.mu.Unlock()
	return adapter.Instance{Ref: s.Metadata.Name, SandboxID: s.Metadata.Labels[labelSandbox], State: state(s), Region: s.Spec.Region, Message: s.message()}
}

func (a *Adapter) get(ctx context.Context, ref string) (sandbox, error) {
	var s sandbox
	return s, a.c.JSON(ctx, http.MethodGet, "/sandboxes/"+url.PathEscape(ref), nil, &s)
}

func (a *Adapter) Create(ctx context.Context, req adapter.CreateRequest) (adapter.Instance, error) {
	if req.FromSnapshot != "" {
		return adapter.Instance{}, adapter.Unsupported("snapshots.restore")
	}
	image := a.opts.Image
	if req.Template != "" {
		image = req.Template
	}
	runtime := map[string]any{"image": image}
	// Blaxel sizes CPU from memory: one core per 2048 MB.
	if mem := max(req.Resources.MemoryMiB, req.Resources.CPU*2048); mem > 0 {
		runtime["memory"] = mem
	}
	if len(req.Env) > 0 {
		envs := make([]map[string]string, 0, len(req.Env))
		for k, v := range req.Env {
			envs = append(envs, map[string]string{"name": k, "value": v})
		}
		runtime["envs"] = envs
	}
	if req.TTLSeconds > 0 {
		// Backstop in case this control plane is lost; Blaxel's minimum is 5m.
		runtime["ttl"] = strconv.Itoa(max(req.TTLSeconds, 300)) + "s"
	}
	spec := map[string]any{"runtime": runtime}
	if a.opts.Region != "" {
		spec["region"] = a.opts.Region
	}
	body := map[string]any{
		"metadata": map[string]any{"name": name(req.ID), "labels": map[string]string{labelSandbox: req.ID, labelInstallation: req.Installation}},
		"spec":     spec,
	}
	// createIfNotExist returns the live sandbox of this name instead of a 409,
	// so a replayed create resolves to the same sandbox.
	var s sandbox
	err := a.c.JSON(ctx, http.MethodPost, "/sandboxes?createIfNotExist=true", body, &s)
	if status(err) == http.StatusGatewayTimeout {
		// The edge gave up while Blaxel keeps deploying; follow it by name.
		s.Metadata.Name, err = name(req.ID), nil
	}
	if err != nil {
		return adapter.Instance{}, createError(err)
	}
	ref := s.Metadata.Name
	err = httpx.Poll(ctx, time.Second, a.opts.ReadyTimeout, func() (bool, error) {
		switch {
		case s.Status == "DEPLOYED":
			return true, nil
		case s.Status == "FAILED":
			return false, adapter.SandboxFailed(fmt.Errorf("blaxel sandbox %s failed: %s", ref, s.message()))
		case gone(s):
			return false, adapter.SandboxFailed(fmt.Errorf("blaxel sandbox %s was deleted while starting", ref))
		}
		var err error
		s, err = a.get(ctx, ref)
		if api.IsCode(err, api.CodeNotFound) {
			err = adapter.Unavailable(fmt.Errorf("blaxel: sandbox %s disappeared before it was ready", ref))
		}
		return false, err
	})
	if err != nil {
		return adapter.Instance{}, err
	}
	return a.instance(s), nil
}

func status(err error) int {
	var e *api.Error
	if errors.As(err, &e) && e.Details != nil {
		n, _ := strconv.Atoi(e.Details["providerStatus"])
		return n
	}
	return 0
}

// createError reclassifies create failures that concern this one request
// rather than Blaxel. A bad image stays invalid_request (Blaxel answers 400
// INVALID_IMAGE).
func createError(err error) error {
	var e *api.Error
	if !errors.As(err, &e) {
		return err
	}
	switch {
	case e.Details["providerCode"] == "QUOTA_EXCEEDED":
		// The workspace is at its sandbox cap: wait for room.
		return &api.Error{Code: adapter.CodeInsufficientCapacity, Message: e.Message, Details: e.Details}
	case status(err) == http.StatusRequestTimeout:
		// CREATION_TIMEOUT: this sandbox missed its ready deadline and Blaxel removed it.
		return &api.Error{Code: adapter.CodeSandboxFailed, Message: e.Message, Details: e.Details}
	}
	return err
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

// List pages through every sandbox in the workspace and keeps this
// installation's. Blaxel's only server-side search is fuzzy, so filtering
// happens here; a failed page fails the whole call.
func (a *Adapter) List(ctx context.Context, installation string) ([]adapter.Instance, error) {
	var out []adapter.Instance
	for cursor := ""; ; {
		path := "/sandboxes?limit=200"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		resp, err := a.c.Do(ctx, http.MethodGet, path, nil, "", http.Header{"Blaxel-Version": {listVersion}})
		if err != nil {
			return nil, err
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if err != nil {
			return nil, adapter.Unavailable(fmt.Errorf("blaxel: read sandbox list: %w", err))
		}
		var page struct {
			Data []sandbox `json:"data"`
			Meta struct {
				HasMore    bool   `json:"hasMore"`
				NextCursor string `json:"nextCursor"`
			} `json:"meta"`
		}
		if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
			err = json.Unmarshal(raw, &page.Data) // versions before listVersion: everything, unpaged
		} else {
			err = json.Unmarshal(raw, &page)
		}
		if err != nil {
			return nil, adapter.Unavailable(fmt.Errorf("blaxel: decode sandbox list: %w", err))
		}
		for _, s := range page.Data {
			if s.Metadata.Labels[labelInstallation] == installation && !gone(s) {
				out = append(out, a.instance(s))
			}
		}
		if !page.Meta.HasMore || page.Meta.NextCursor == "" {
			return out, nil
		}
		cursor = page.Meta.NextCursor
	}
}

func (a *Adapter) Delete(ctx context.Context, ref string) error {
	err := a.c.JSON(ctx, http.MethodDelete, "/sandboxes/"+url.PathEscape(ref), nil, nil)
	a.mu.Lock()
	delete(a.urls, ref)
	a.mu.Unlock()
	if api.IsCode(err, api.CodeNotFound) {
		return nil
	}
	return err
}

// sandboxURL is the base of the sandbox's own API.
func (a *Adapter) sandboxURL(ctx context.Context, ref string) (string, error) {
	a.mu.Lock()
	base, ok := a.urls[ref]
	a.mu.Unlock()
	if !ok {
		if _, err := a.Get(ctx, ref); err != nil {
			return "", err
		}
		a.mu.Lock()
		base = a.urls[ref]
		a.mu.Unlock()
	}
	if base == "" {
		return "", adapter.Unavailable(fmt.Errorf("blaxel did not report a URL for sandbox %s", ref))
	}
	return base, nil
}

type event struct {
	Type string `json:"type"`
	Data string `json:"data"`
}

type process struct {
	Status   string `json:"status"`
	ExitCode int    `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// run starts a command and reads its NDJSON event stream to the end. The
// final result carries the complete stdout and stderr.
func (a *Adapter) run(ctx context.Context, ref string, req api.ExecRequest, emit func(api.ExecEvent) error) (process, error) {
	base, err := a.sandboxURL(ctx, ref)
	if err != nil {
		return process{}, err
	}
	command := req.Command // the sandbox runs it with sh -c
	if command == "" {
		command = httpx.ShellQuote(req.Args)
	}
	if req.Stdin != "" {
		// A streamed command takes no stdin: stage it in a file the command
		// reads, and remove the file whatever the command's exit code.
		file := "/tmp/.differently-stdin-" + id.New("in")
		if err := a.WriteFile(ctx, ref, file, []byte(req.Stdin)); err != nil {
			return process{}, err
		}
		q := httpx.ShellQuote([]string{file})
		command = "(" + command + ") < " + q + "; rc=$?; rm -f " + q + "; exit $rc"
	}
	body := map[string]any{"command": command}
	if req.Cwd != "" {
		body["workingDir"] = req.Cwd
	}
	if len(req.Env) > 0 {
		body["env"] = req.Env
	}
	if req.TimeoutSeconds > 0 {
		body["timeout"] = req.TimeoutSeconds // Blaxel kills the process at the deadline
	}
	b, _ := json.Marshal(body)
	resp, err := a.c.Do(ctx, http.MethodPost, base+"/process", bytes.NewReader(b), "application/json", http.Header{"Accept": {"application/x-ndjson"}})
	if err != nil {
		return process{}, err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev event
		if err := json.Unmarshal(line, &ev); err != nil {
			return process{}, fmt.Errorf("blaxel: decode exec event: %w", err)
		}
		switch ev.Type {
		case "stdout", "stderr":
			if emit != nil {
				if err := emit(api.ExecEvent{Stream: ev.Type, Data: ev.Data}); err != nil {
					return process{}, err
				}
			}
		case "result":
			var p process
			if err := json.Unmarshal([]byte(ev.Data), &p); err != nil {
				return process{}, fmt.Errorf("blaxel: decode exec result: %w", err)
			}
			if p.Status == "killed" && p.ExitCode == 0 {
				p.ExitCode = 137 // killed by signal, at the deadline or on request
			}
			return p, nil
		case "error":
			// The command could not start (a missing working directory, say).
			return process{}, api.Errorf(api.CodeInvalidRequest, "blaxel: %s", ev.Data)
		}
	}
	if err := sc.Err(); err != nil {
		return process{}, adapter.Unavailable(fmt.Errorf("blaxel: exec stream: %w", err))
	}
	return process{}, adapter.Unavailable(errors.New("blaxel: exec stream ended without a result"))
}

func (a *Adapter) Exec(ctx context.Context, ref string, req api.ExecRequest) (api.ExecResult, error) {
	start := time.Now()
	p, err := a.run(ctx, ref, req, nil)
	if err != nil {
		return api.ExecResult{}, err
	}
	return api.ExecResult{ExitCode: p.ExitCode, Stdout: p.Stdout, Stderr: p.Stderr, DurationMs: time.Since(start).Milliseconds()}, nil
}

func (a *Adapter) ExecStream(ctx context.Context, ref string, req api.ExecRequest, emit func(api.ExecEvent) error) (int, error) {
	p, err := a.run(ctx, ref, req, emit)
	if err != nil {
		return -1, err
	}
	return p.ExitCode, nil
}

// fsURL addresses a path in the sandbox filesystem API. An absolute path's
// leading slash is sent encoded, as Blaxel documents.
func fsURL(base, path string) string {
	parts := strings.Split(strings.TrimLeft(path, "/"), "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return base + "/filesystem/%2F" + strings.Join(parts, "/")
}

func (a *Adapter) ReadFile(ctx context.Context, ref, path string) ([]byte, error) {
	base, err := a.sandboxURL(ctx, ref)
	if err != nil {
		return nil, err
	}
	resp, err := a.c.Do(ctx, http.MethodGet, fsURL(base, path)+"?download=true", nil, "", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 100<<20))
}

func (a *Adapter) WriteFile(ctx context.Context, ref, path string, data []byte) error {
	base, err := a.sandboxURL(ctx, ref)
	if err != nil {
		return err
	}
	if dir := path[:max(strings.LastIndex(path, "/"), 0)]; strings.Trim(dir, "/") != "" {
		if err := a.c.JSON(ctx, http.MethodPut, fsURL(base, dir), map[string]any{"isDirectory": true}, nil); err != nil {
			return err
		}
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
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
	resp, err := a.c.Do(ctx, http.MethodPut, fsURL(base, path), &buf, w.FormDataContentType(), nil)
	if status(err) == http.StatusRequestEntityTooLarge {
		return api.Errorf(api.CodeInvalidRequest, "blaxel: the sandbox's proxy rejected a %d-byte file; sandboxes on Blaxel's mk3.0 generation accept up to 8 MiB per request", len(data))
	}
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (a *Adapter) ListFiles(ctx context.Context, ref, path string) ([]api.FileInfo, error) {
	base, err := a.sandboxURL(ctx, ref)
	if err != nil {
		return nil, err
	}
	var dir struct {
		Files []struct {
			Path         string `json:"path"`
			Name         string `json:"name"`
			Size         int64  `json:"size"`
			Permissions  string `json:"permissions"`
			LastModified string `json:"lastModified"`
		} `json:"files"`
		Subdirectories []struct {
			Path string `json:"path"`
			Name string `json:"name"`
		} `json:"subdirectories"`
	}
	if err := a.c.JSON(ctx, http.MethodGet, fsURL(base, path), nil, &dir); err != nil {
		return nil, err
	}
	out := make([]api.FileInfo, 0, len(dir.Files)+len(dir.Subdirectories))
	for _, d := range dir.Subdirectories {
		out = append(out, api.FileInfo{Path: d.Path, Name: d.Name, IsDir: true})
	}
	for _, f := range dir.Files {
		mt, err := time.Parse(time.RFC3339, f.LastModified)
		if err != nil {
			mt, _ = http.ParseTime(f.LastModified)
		}
		out = append(out, api.FileInfo{Path: f.Path, Name: f.Name, Size: f.Size, Mode: f.Permissions, ModTime: mt})
	}
	return out, nil
}

// ExposePort returns a private preview URL for the port with a token valid
// for one hour in its query string. Anyone holding the URL can reach the port
// until the token expires.
func (a *Adapter) ExposePort(ctx context.Context, ref string, port int) (string, error) {
	previews := "/sandboxes/" + url.PathEscape(ref) + "/previews"
	preview := fmt.Sprintf("port-%d", port)
	var p struct {
		Spec struct {
			URL string `json:"url"`
		} `json:"spec"`
	}
	body := map[string]any{"metadata": map[string]any{"name": preview}, "spec": map[string]any{"port": port, "public": false}}
	err := a.c.JSON(ctx, http.MethodPost, previews, body, &p)
	if api.IsCode(err, api.CodeInvalidState) {
		err = a.c.JSON(ctx, http.MethodGet, previews+"/"+preview, nil, &p) // exposed before
	}
	if err != nil {
		return "", err
	}
	if p.Spec.URL == "" {
		return "", adapter.Unavailable(fmt.Errorf("blaxel did not report a URL for the preview of port %d", port))
	}
	var tok struct {
		Spec struct {
			Token string `json:"token"`
		} `json:"spec"`
	}
	body = map[string]any{
		"metadata": map[string]any{"name": "dly-" + strconv.FormatInt(time.Now().UnixNano(), 36)},
		"spec":     map[string]any{"expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
	}
	if err := a.c.JSON(ctx, http.MethodPost, previews+"/"+preview+"/tokens", body, &tok); err != nil {
		return "", err
	}
	return p.Spec.URL + "?bl_preview_token=" + url.QueryEscape(tok.Spec.Token), nil
}

var (
	_ adapter.ExecutionAdapter = (*Adapter)(nil)
	_ adapter.Streamer         = (*Adapter)(nil)
	_ adapter.FileSystem       = (*Adapter)(nil)
	_ adapter.PortExposer      = (*Adapter)(nil)
)
