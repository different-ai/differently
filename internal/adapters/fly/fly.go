// Package fly adapts Fly.io's Machines API.
//
// Fly.io runs each machine as a Firecracker microVM on its own hosts, so
// pools on this adapter are always TrustExternal. Every sandbox is one
// machine in the Fly app named by config.app. The machine is named after the
// sandbox ID and carries the sandbox and installation IDs in its metadata,
// which Create and List filter on.
//
// Differences from other backends, handled here:
//
//   - the machine runs an idle init process instead of the image's command,
//     so it stays up for exec (config.command replaces it);
//   - the root filesystem is kept across stop and start (rootfs persist);
//   - exec has no env field: sandbox env is written to a 0600 file when the
//     machine is created and sourced by every exec;
//   - there is no file API, so files move through exec as base64 and the
//     image needs sh, base64 and stat;
//   - suspend keeps memory but Fly.io does not guarantee the resume, so pause
//     is offered only when config.suspend is true;
//   - there are no machine snapshots, no per-machine public URL and no
//     provider-side TTL.
//
// Written against the Machines API OpenAPI document (read 2026-10-28) and
// tested with a fake server, not against a live account.
package fly

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/different-ai/differently/internal/adapters/httpx"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// DefaultAPIURL is the public Machines API.
const DefaultAPIURL = "https://api.machines.dev"

const (
	metaSandbox      = "dly-sandbox"
	metaInstallation = "dly-installation"
	envFile          = "/etc/differently/env"
	// idle keeps the machine running without the image's own command.
	idle = "trap 'exit 0' TERM INT; while :; do sleep 3600 & wait $!; done"
)

// Options configures the adapter. Pool config keys: app (required), apiURL,
// region, image, cpuKind (shared|performance), command and suspend.
type Options struct {
	APIURL       string
	Token        string
	App          string
	Region       string
	Image        string
	CPUKind      string
	Command      string
	Suspend      bool
	ReadyTimeout time.Duration
}

// FromConfig maps pool config to options.
func FromConfig(cfg map[string]string, token string) (Options, error) {
	o := Options{APIURL: cfg["apiURL"], Token: token, App: cfg["app"], Region: cfg["region"], Image: cfg["image"], CPUKind: cfg["cpuKind"], Command: cfg["command"], Suspend: cfg["suspend"] == "true"}
	if o.APIURL == "" {
		o.APIURL = DefaultAPIURL
	}
	if o.App == "" {
		return o, fmt.Errorf("the fly adapter needs config.app, the Fly app that holds the sandbox machines")
	}
	switch o.CPUKind {
	case "":
		o.CPUKind = "shared"
	case "shared", "performance":
	default:
		return o, fmt.Errorf("fly config.cpuKind must be shared or performance")
	}
	return o, nil
}

// Adapter talks to the Machines API.
type Adapter struct {
	opts Options
	c    *httpx.Client
}

// New returns a Fly Machines adapter.
func New(o Options) *Adapter {
	if o.ReadyTimeout == 0 {
		o.ReadyTimeout = 3 * time.Minute
	}
	if o.CPUKind == "" {
		o.CPUKind = "shared"
	}
	c := httpx.New("fly", o.APIURL, o.Token)
	c.HTTP.Transport = httpx.ErrorShape(nil, flatten)
	return &Adapter{opts: o, c: c}
}

// flatten reads Fly's {"error": "...", "status": "insufficient_capacity"}
// body; its status field is the provider's error code.
func flatten(raw []byte) (string, string) {
	var body struct {
		Error  string `json:"error"`
		Status string `json:"status"`
	}
	if json.Unmarshal(raw, &body) != nil || body.Status == "" || body.Status == "unknown" {
		return "", ""
	}
	return body.Status, body.Error
}

// classify turns Fly's capacity answers into insufficient_capacity. The
// documented one is status insufficient_capacity (or
// volume_placement_capacity); a host that cannot fit the machine answers 409
// with flyd's "insufficient ... available to fulfill request" text, which
// Fly's own client also reads.
func classify(err error) error {
	var e *api.Error
	if !errors.As(err, &e) {
		return err
	}
	switch code := e.Details["providerCode"]; {
	case code == "insufficient_capacity" || code == "volume_placement_capacity",
		e.Details["providerStatus"] == "409" && strings.Contains(e.Message, "insufficient") && strings.Contains(e.Message, "available to fulfill request"):
		return &api.Error{Code: adapter.CodeInsufficientCapacity, Message: e.Message, Details: e.Details}
	case code == "name_taken":
		return &api.Error{Code: api.CodeInvalidState, Message: e.Message, Details: e.Details}
	}
	return err
}

// Endpoint is reported in the trust report.
func (a *Adapter) Endpoint() string { return a.opts.APIURL }

type machine struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	State  string `json:"state"`
	Region string `json:"region"`
	Config struct {
		Metadata map[string]string `json:"metadata"`
	} `json:"config"`
}

func (m machine) gone() bool { return m.State == "destroyed" || m.State == "destroying" }

func state(m machine) string {
	switch m.State {
	case "started":
		return adapter.StateRunning
	case "stopped":
		return adapter.StateStopped
	case "suspended":
		return adapter.StatePaused
	case "failed", "launch_failed":
		return adapter.StateError
	}
	return adapter.StatePending
}

func inst(m machine) adapter.Instance {
	return adapter.Instance{Ref: m.ID, SandboxID: m.Config.Metadata[metaSandbox], State: state(m), Region: m.Region}
}

func (a *Adapter) Info() adapter.Info {
	return adapter.Info{Kind: "fly", Version: "1", Trust: api.TrustExternal, Isolation: "Firecracker microVM (Fly.io-managed)", Protocol: adapter.ProtocolVersion, Description: "Fly Machines in app " + a.opts.App}
}

func (a *Adapter) Capabilities(context.Context) (adapter.Capabilities, error) {
	f := []string{adapter.FeatureExec, adapter.FeatureFiles, adapter.FeatureStopStart, adapter.FeaturePersistDisk, adapter.FeatureMicroVM, adapter.FeatureArchAMD64}
	if a.opts.Suspend {
		f = append(f, adapter.FeaturePauseResume)
	}
	var regions []string
	if a.opts.Region != "" {
		regions = []string{a.opts.Region}
	}
	// Fly's largest sizes: shared-cpu-8x with 16 GB, performance-16x with 128 GB.
	if a.opts.CPUKind == "performance" {
		return adapter.Capabilities{Features: f, MaxCPU: 16, MaxMemoryMiB: 131072, Regions: regions}, nil
	}
	return adapter.Capabilities{Features: f, MaxCPU: 8, MaxMemoryMiB: 16384, Regions: regions}, nil
}

func (a *Adapter) Capacity(context.Context) (adapter.Capacity, error) { return adapter.Capacity{}, nil }

func (a *Adapter) Health(ctx context.Context) error {
	return a.c.JSON(ctx, http.MethodGet, "/v1/apps/"+url.PathEscape(a.opts.App), nil, nil)
}

func (a *Adapter) machines() string { return "/v1/apps/" + url.PathEscape(a.opts.App) + "/machines" }

func (a *Adapter) machinePath(ref, op string) string {
	p := a.machines() + "/" + url.PathEscape(ref)
	if op != "" {
		p += "/" + op
	}
	return p
}

// name derives the machine name from the sandbox ID. Names are unique per
// app, so a racing replay of Create is refused rather than duplicated.
func name(sandboxID string) string { return strings.ReplaceAll(strings.ToLower(sandboxID), "_", "-") }

// guest fits a request to Fly's sizes: shared CPUs come with 256 MB to 2 GB
// of memory each, performance CPUs with 2 GB to 8 GB, in steps of 256 MB.
func guest(kind string, r api.Resources) map[string]any {
	minPer, maxPer, counts := 256, 2048, []int{1, 2, 4, 6, 8}
	if kind == "performance" {
		minPer, maxPer, counts = 2048, 8192, []int{1, 2, 4, 8, 16}
	}
	cpus := max(1, r.CPU, (r.MemoryMiB+maxPer-1)/maxPer)
	for _, n := range counts {
		if n >= cpus {
			cpus = n
			break
		}
	}
	mem := max(r.MemoryMiB, minPer*cpus)
	mem = (mem + 255) / 256 * 256
	return map[string]any{"cpu_kind": kind, "cpus": cpus, "memory_mb": mem}
}

func (a *Adapter) find(ctx context.Context, key, value string) ([]machine, error) {
	var out []machine
	cursor := ""
	for {
		q := url.Values{"metadata." + key: {value}, "limit": {"1000"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		resp, err := a.c.Do(ctx, http.MethodGet, a.machines()+"?"+q.Encode(), nil, "", nil)
		if err != nil {
			return nil, err
		}
		var page []machine
		err = json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, adapter.Unavailable(fmt.Errorf("fly: unreadable machine list: %w", err))
		}
		for _, m := range page {
			if !m.gone() && m.Config.Metadata[key] == value { // re-checked, never trusted
				out = append(out, m)
			}
		}
		// A page may be short, or empty, while more remain: follow the cursor.
		if cursor = resp.Header.Get("Fly-Next-Cursor"); cursor == "" {
			return out, nil
		}
	}
}

func (a *Adapter) get(ctx context.Context, ref string) (machine, error) {
	var m machine
	if err := a.c.JSON(ctx, http.MethodGet, a.machinePath(ref, ""), nil, &m); err != nil {
		return m, err
	}
	if m.gone() {
		return m, adapter.NotFound(ref)
	}
	return m, nil
}

// wait polls the machine until it reaches want. Get Machine allows five
// requests a second, so a one-second poll stays well inside the limit.
func (a *Adapter) wait(ctx context.Context, ref, want string) (machine, error) {
	var m machine
	err := httpx.Poll(ctx, time.Second, a.opts.ReadyTimeout, func() (bool, error) {
		var err error
		if m, err = a.get(ctx, ref); err != nil {
			return false, err
		}
		if state(m) == adapter.StateError {
			return false, adapter.SandboxFailed(fmt.Errorf("fly machine %s is %s", ref, m.State))
		}
		return m.State == want, nil
	})
	return m, err
}

func (a *Adapter) Create(ctx context.Context, req adapter.CreateRequest) (adapter.Instance, error) {
	existing, err := a.find(ctx, metaSandbox, req.ID)
	if err != nil {
		return adapter.Instance{}, err
	}
	var m machine
	if len(existing) > 0 {
		m = existing[0]
	} else {
		m, err = a.create(ctx, req)
		if api.IsCode(err, api.CodeInvalidState) {
			// The name is taken: a replay won the race (use its machine), or
			// a machine Differently did not create holds it, which no retry fixes.
			existing, ferr := a.find(ctx, metaSandbox, req.ID)
			switch {
			case ferr != nil:
				err = ferr
			case len(existing) > 0:
				m, err = existing[0], nil
			default:
				err = api.Errorf(api.CodeInvalidRequest, "fly app %s has a machine named %s that Differently did not create", a.opts.App, name(req.ID))
			}
		}
		if err != nil {
			return adapter.Instance{}, err
		}
	}
	if m.State == "stopped" || m.State == "suspended" {
		if err := a.start(ctx, m.ID); err != nil { // a replay after the machine stopped
			return adapter.Instance{}, err
		}
	}
	if m, err = a.wait(ctx, m.ID, "started"); err != nil {
		return adapter.Instance{}, err
	}
	return inst(m), nil
}

func (a *Adapter) create(ctx context.Context, req adapter.CreateRequest) (machine, error) {
	image := a.opts.Image
	if req.Template != "" {
		image = req.Template
	}
	if req.FromSnapshot != "" {
		return machine{}, api.Errorf(api.CodeInvalidRequest, "fly machines have no snapshots to restore from")
	}
	if image == "" {
		return machine{}, api.Errorf(api.CodeInvalidRequest, "no image: set the pool's config.image or request a template")
	}
	command := a.opts.Command
	if command == "" {
		command = idle
	}
	rootfs := map[string]any{"persist": "always"} // stop and start keep the disk
	if req.Resources.DiskGiB > 0 {
		rootfs["size_gb"] = req.Resources.DiskGiB
	}
	config := map[string]any{
		"image":        image,
		"guest":        guest(a.opts.CPUKind, req.Resources),
		"metadata":     map[string]string{metaSandbox: req.ID, metaInstallation: req.Installation},
		"init":         map[string]any{"exec": []string{"/bin/sh", "-c", command}},
		"restart":      map[string]any{"policy": "no"},
		"auto_destroy": false,
		"rootfs":       rootfs,
	}
	if len(req.Env) > 0 {
		config["env"] = req.Env
		config["files"] = []map[string]any{{"guest_path": envFile, "raw_value": base64.StdEncoding.EncodeToString(envFileContent(req.Env)), "mode": 0o600}}
	}
	body := map[string]any{"name": name(req.ID), "config": config}
	if a.opts.Region != "" {
		body["region"] = a.opts.Region
	}
	var m machine
	err := a.c.JSON(ctx, http.MethodPost, a.machines(), body, &m)
	return m, classify(err)
}

func envFileContent(env map[string]string) []byte {
	var b strings.Builder
	for _, k := range sortedKeys(env) {
		b.WriteString(k + "=" + httpx.ShellQuote([]string{env[k]}) + "\n")
	}
	return []byte(b.String())
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (a *Adapter) Get(ctx context.Context, ref string) (adapter.Instance, error) {
	m, err := a.get(ctx, ref)
	if err != nil {
		return adapter.Instance{}, err
	}
	return inst(m), nil
}

func (a *Adapter) List(ctx context.Context, installation string) ([]adapter.Instance, error) {
	ms, err := a.find(ctx, metaInstallation, installation)
	if err != nil {
		return nil, err
	}
	out := make([]adapter.Instance, 0, len(ms))
	for _, m := range ms {
		out = append(out, inst(m))
	}
	return out, nil
}

func (a *Adapter) Delete(ctx context.Context, ref string) error {
	err := a.c.JSON(ctx, http.MethodDelete, a.machinePath(ref, "")+"?force=true", nil, nil)
	if api.IsCode(err, api.CodeNotFound) {
		return nil
	}
	return err
}

// Exec runs the command through sh, after sourcing the sandbox env file and
// exporting the request's env. The timeout is sent in seconds, as Fly's own
// client does; the API reference does not state the unit.
func (a *Adapter) Exec(ctx context.Context, ref string, req api.ExecRequest) (api.ExecResult, error) {
	var script strings.Builder
	script.WriteString("set -a; [ -r " + envFile + " ] && . " + envFile + "; set +a; ")
	for _, k := range sortedKeys(req.Env) {
		script.WriteString("export " + k + "=" + httpx.ShellQuote([]string{req.Env[k]}) + "; ")
	}
	script.WriteString(httpx.CommandLine(req))
	body := map[string]any{"command": []string{"/bin/sh", "-c", script.String()}}
	if req.TimeoutSeconds > 0 {
		body["timeout"] = req.TimeoutSeconds
	}
	if req.Stdin != "" {
		body["stdin"] = req.Stdin
	}
	var out struct {
		ExitCode   int    `json:"exit_code"`
		ExitSignal int    `json:"exit_signal"`
		Stdout     string `json:"stdout"`
		Stderr     string `json:"stderr"`
	}
	start := time.Now()
	if err := a.c.JSON(ctx, http.MethodPost, a.machinePath(ref, "exec"), body, &out); err != nil {
		return api.ExecResult{}, err
	}
	code := out.ExitCode
	if code == 0 && out.ExitSignal > 0 {
		code = 128 + out.ExitSignal // killed by a signal, as a shell reports it
	}
	return api.ExecResult{ExitCode: code, Stdout: out.Stdout, Stderr: out.Stderr, DurationMs: time.Since(start).Milliseconds()}, nil
}

// fileError maps a failed file command to not_found or invalid_request.
func fileError(p string, res api.ExecResult) error {
	if strings.Contains(res.Stderr, "No such file") || strings.Contains(res.Stderr, "can't cd") {
		return api.Errorf(api.CodeNotFound, "%s not found", p)
	}
	return api.Errorf(api.CodeInvalidRequest, "fly: %s: %s", p, strings.TrimSpace(res.Stderr))
}

func (a *Adapter) ReadFile(ctx context.Context, ref, p string) ([]byte, error) {
	res, err := a.Exec(ctx, ref, api.ExecRequest{Command: "base64 < " + httpx.ShellQuote([]string{p})})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fileError(p, res)
	}
	b, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(res.Stdout), ""))
	if err != nil {
		return nil, adapter.Unavailable(fmt.Errorf("fly: unreadable file content for %s: %w", p, err))
	}
	return b, nil
}

func (a *Adapter) WriteFile(ctx context.Context, ref, p string, data []byte) error {
	cmd := "base64 -d > " + httpx.ShellQuote([]string{p})
	if dir := path.Dir(p); dir != "." && dir != "/" {
		cmd = "mkdir -p " + httpx.ShellQuote([]string{dir}) + " && " + cmd
	}
	res, err := a.Exec(ctx, ref, api.ExecRequest{Command: cmd, Stdin: base64.StdEncoding.EncodeToString(data)})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fileError(p, res)
	}
	return nil
}

// listScript prints one "type|size|mtime|mode|name" line per entry, with
// stat options GNU coreutils and BusyBox share.
const listScript = `for f in * .[!.]* ..?*; do [ -e "$f" ] || [ -L "$f" ] || continue; stat -c '%F|%s|%Y|%a|%n' -- "$f"; done`

func (a *Adapter) ListFiles(ctx context.Context, ref, p string) ([]api.FileInfo, error) {
	res, err := a.Exec(ctx, ref, api.ExecRequest{Command: listScript, Cwd: p})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fileError(p, res)
	}
	var out []api.FileInfo
	for _, line := range strings.Split(strings.TrimRight(res.Stdout, "\n"), "\n") {
		f := strings.SplitN(line, "|", 5)
		if len(f) != 5 {
			continue
		}
		size, _ := strconv.ParseInt(f[1], 10, 64)
		secs, _ := strconv.ParseInt(f[2], 10, 64)
		out = append(out, api.FileInfo{Path: strings.TrimRight(p, "/") + "/" + f[4], Name: f[4], Size: size, Mode: f[3], IsDir: f[0] == "directory", ModTime: time.Unix(secs, 0).UTC()})
	}
	return out, nil
}

func (a *Adapter) action(ctx context.Context, ref, op string) error {
	return classify(a.c.JSON(ctx, http.MethodPost, a.machinePath(ref, op), nil, nil))
}

func (a *Adapter) start(ctx context.Context, ref string) error { return a.action(ctx, ref, "start") }

// Stop keeps the root filesystem: machines are created with rootfs persist.
func (a *Adapter) Stop(ctx context.Context, ref string) error {
	if err := a.action(ctx, ref, "stop"); err != nil {
		return err
	}
	_, err := a.wait(ctx, ref, "stopped")
	return err
}

func (a *Adapter) Start(ctx context.Context, ref string) error {
	if err := a.start(ctx, ref); err != nil {
		return err
	}
	_, err := a.wait(ctx, ref, "started")
	return err
}

// Pause suspends the machine with its memory. Fly.io tries to resume from
// that snapshot but may cold-boot instead (the disk is kept either way).
func (a *Adapter) Pause(ctx context.Context, ref string) error {
	if !a.opts.Suspend {
		return adapter.Unsupported("lifecycle.pause-resume")
	}
	if err := a.action(ctx, ref, "suspend"); err != nil {
		return err
	}
	_, err := a.wait(ctx, ref, "suspended")
	return err
}

func (a *Adapter) Resume(ctx context.Context, ref string) error { return a.Start(ctx, ref) }

var (
	_ adapter.ExecutionAdapter = (*Adapter)(nil)
	_ adapter.FileSystem       = (*Adapter)(nil)
	_ adapter.Lifecycle        = (*Adapter)(nil)
	_ adapter.Pauser           = (*Adapter)(nil)
)
