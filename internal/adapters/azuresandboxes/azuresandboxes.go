// Package azuresandboxes adapts Azure Container Apps Sandboxes: each
// Differently sandbox is one sandbox in an Azure sandbox group.
//
// Microsoft runs the sandboxes in the operator's subscription, so pools
// default to TrustExternal; an operator whose policy treats their own Azure
// subscription as private may declare trust: private. Requests carry a
// Microsoft Entra ID token for https://management.azuredevcompute.io from a
// service principal (client secret or workload identity federation).
//
// Mapping, from the data-plane OpenAPI spec 2026-09-01-preview
// (azure-rest-api-specs, specification/app/data-plane/ContainerApps, read
// 2026-10-28):
//
//   - Create looks up the labels dly-sandbox and dly-installation, then
//     creates with them, so a replay finds the first sandbox;
//   - the TTL becomes an auto-delete policy (AfterCreation), moved by
//     SetExpiry; auto-suspend is off so Differently owns the lifecycle;
//   - Exec uses executeShellCommand, which reports the exit code; stdin and
//     the timeout are folded into the command line (shellcmd);
//   - Stop saves the sandbox as a snapshot and Start resumes from it;
//   - ports are added with anonymous access, so their URLs are public.
//
// Tested against a fake server built from the spec, not yet against a live
// sandbox group. The service is in preview.
package azuresandboxes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// DefaultAPIVersion is the data-plane version the adapter was written for.
const DefaultAPIVersion = "2026-09-01-preview"

// Scope is the Entra ID scope of the sandbox data plane.
const Scope = "https://management.azuredevcompute.io/.default"

// backstopGrace keeps the auto-delete deadline behind Differently's own,
// audited expiry, as the server does when a TTL changes.
const backstopGrace = 5 * 60

const (
	labelSandbox      = "dly-sandbox"
	labelInstallation = "dly-installation"
)

// Options configures the adapter. Pool config keys: endpoint,
// subscriptionId, resourceGroup, sandboxGroup (required), diskImage,
// egress (none|public), apiVersion. The credential is Entra JSON (see
// cloudhttp.EntraCredential).
type Options struct {
	Endpoint      string
	Subscription  string
	ResourceGroup string
	SandboxGroup  string
	DiskImage     string
	Egress        string
	APIVersion    string
	Region        string
	ReadyTimeout  time.Duration
	Credential    cloudhttp.EntraCredential
	// HTTP overrides the client used for tokens (tests).
	HTTP *http.Client
}

// FromConfig maps pool config to options.
func FromConfig(cfg map[string]string, region, credential string) (Options, error) {
	o := Options{Endpoint: strings.TrimRight(cfg["endpoint"], "/"), Subscription: cfg["subscriptionId"], ResourceGroup: cfg["resourceGroup"],
		SandboxGroup: cfg["sandboxGroup"], DiskImage: cfg["diskImage"], Egress: cfg["egress"], APIVersion: cfg["apiVersion"], Region: region}
	if o.Endpoint == "" || o.Subscription == "" || o.ResourceGroup == "" || o.SandboxGroup == "" {
		return o, errors.New("azure-sandboxes needs config.endpoint, config.subscriptionId, config.resourceGroup and config.sandboxGroup")
	}
	if o.APIVersion == "" {
		o.APIVersion = DefaultAPIVersion
	}
	c, err := cloudhttp.ParseEntra(credential)
	if err != nil {
		return o, fmt.Errorf("azure-sandboxes: %w", err)
	}
	o.Credential = c
	return o, nil
}

// Adapter talks to one Azure sandbox group.
type Adapter struct {
	opts Options
	c    *cloudhttp.Client
}

// New returns an Azure Container Apps Sandboxes adapter.
func New(o Options) *Adapter {
	if o.ReadyTimeout == 0 {
		o.ReadyTimeout = 3 * time.Minute
	}
	hc := o.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: time.Minute}
	}
	base := o.Endpoint + "/subscriptions/" + url.PathEscape(o.Subscription) + "/resourceGroups/" + url.PathEscape(o.ResourceGroup) + "/sandboxGroups/" + url.PathEscape(o.SandboxGroup)
	return &Adapter{opts: o, c: cloudhttp.New("azure-sandboxes", base, cloudhttp.Bearer(cloudhttp.Entra(o.Credential, Scope, hc)))}
}

// Endpoint is reported in the trust report.
func (a *Adapter) Endpoint() string { return a.opts.Endpoint }

func (a *Adapter) Info() adapter.Info {
	return adapter.Info{Kind: "azure-sandboxes", Version: "1", Trust: api.TrustExternal, Isolation: "per-sandbox isolation boundary (Azure-managed)", Protocol: adapter.ProtocolVersion,
		Description: "Azure Container Apps Sandboxes in sandbox group " + a.opts.SandboxGroup}
}

func (a *Adapter) Capabilities(context.Context) (adapter.Capabilities, error) {
	var regions []string
	if a.opts.Region != "" {
		regions = []string{a.opts.Region}
	}
	// The largest documented tier (3XL) is 16 cores, 32 GiB, 320 GiB.
	return adapter.Capabilities{Features: []string{
		adapter.FeatureExec, adapter.FeatureFiles, adapter.FeatureStopStart, adapter.FeaturePersistDisk,
		adapter.FeatureSnapshotDisk, adapter.FeatureFork, adapter.FeaturePorts, adapter.FeatureEgressControl,
	}, MaxCPU: 16, MaxMemoryMiB: 32768, MaxDiskGiB: 320, Regions: regions}, nil
}

func (a *Adapter) Capacity(context.Context) (adapter.Capacity, error) { return adapter.Capacity{}, nil }

func (a *Adapter) Health(ctx context.Context) error {
	return a.c.JSON(ctx, http.MethodGet, a.path("/sandboxes/count", nil), nil, nil)
}

// path adds api-version and the given query to a path under the group.
func (a *Adapter) path(p string, q url.Values) string {
	if q == nil {
		q = url.Values{}
	}
	q.Set("api-version", a.opts.APIVersion)
	return p + "?" + q.Encode()
}

func sandboxPath(ref, action string) string {
	p := "/sandboxes/" + url.PathEscape(ref)
	if action != "" {
		p += "/" + action
	}
	return p
}

type sandbox struct {
	ID        string            `json:"id"`
	Labels    map[string]string `json:"labels"`
	State     string            `json:"state"`
	Region    string            `json:"region"`
	CreatedAt time.Time         `json:"createdAt"`
}

func state(s sandbox) string {
	switch s.State {
	case "Running", "Idle":
		return adapter.StateRunning
	case "Stopped", "Stopping":
		return adapter.StateStopped
	case "StopFailed":
		return adapter.StateError
	}
	return adapter.StatePending
}

func (a *Adapter) instance(s sandbox) adapter.Instance {
	region := s.Region
	if region == "" {
		region = a.opts.Region
	}
	return adapter.Instance{Ref: s.ID, SandboxID: s.Labels[labelSandbox], State: state(s), Region: region}
}

// find lists sandboxes with a label, following nextLink, and re-checks the
// label locally. Any failed page fails the whole call.
func (a *Adapter) find(ctx context.Context, key, value string) ([]sandbox, error) {
	var out []sandbox
	next := a.path("/sandboxes", url.Values{"labels": {key + "=" + value}})
	for next != "" {
		var page struct {
			Value    []sandbox `json:"value"`
			NextLink string    `json:"nextLink"`
		}
		if err := a.c.JSON(ctx, http.MethodGet, next, nil, &page); err != nil {
			return nil, err
		}
		for _, s := range page.Value {
			if s.Labels[key] == value {
				out = append(out, s)
			}
		}
		next = page.NextLink
	}
	return out, nil
}

func (a *Adapter) get(ctx context.Context, ref string) (sandbox, error) {
	var s sandbox
	return s, a.c.JSON(ctx, http.MethodGet, a.path(sandboxPath(ref, ""), nil), nil, &s)
}

func (a *Adapter) waitRunning(ctx context.Context, s sandbox) (sandbox, error) {
	err := httpx.Poll(ctx, time.Second, a.opts.ReadyTimeout, func() (bool, error) {
		switch state(s) {
		case adapter.StateRunning:
			return true, nil
		case adapter.StateError:
			return false, adapter.SandboxFailed(fmt.Errorf("azure sandbox %s is %s", s.ID, s.State))
		}
		var err error
		s, err = a.get(ctx, s.ID)
		return false, err
	})
	return s, err
}

func ttl(seconds int) int64 {
	if seconds <= 0 {
		return 0
	}
	return int64(seconds + backstopGrace)
}

// lifecycle is the auto-suspend and auto-delete policy: suspend is off, and
// deletion follows the TTL as a backstop.
func lifecycle(ttlSeconds int64) map[string]any {
	del := map[string]any{"enabled": false}
	if ttlSeconds > 0 {
		del = map[string]any{"enabled": true, "deleteIntervalInSeconds": ttlSeconds, "trigger": "AfterCreation"}
	}
	return map[string]any{"autoSuspendPolicy": map[string]any{"enabled": false}, "autoDeletePolicy": del}
}

func (a *Adapter) Create(ctx context.Context, req adapter.CreateRequest) (adapter.Instance, error) {
	existing, err := a.find(ctx, labelSandbox, req.ID)
	if err != nil {
		return adapter.Instance{}, err
	}
	var s sandbox
	if len(existing) > 0 {
		s = existing[0]
	} else {
		body := map[string]any{
			"labels":    map[string]string{labelSandbox: req.ID, labelInstallation: req.Installation},
			"lifecycle": lifecycle(ttl(req.TTLSeconds)),
		}
		if len(req.Env) > 0 {
			body["environment"] = req.Env
		}
		image := a.opts.DiskImage
		if req.Template != "" {
			image = req.Template
		}
		switch {
		case req.FromSnapshot != "":
			body["sourcesRef"] = map[string]any{"snapshot": map[string]string{"id": req.FromSnapshot}}
		case image != "":
			body["sourcesRef"] = map[string]any{"diskImage": map[string]any{"name": image, "isPublic": true}}
		}
		if r := req.Resources; r.CPU > 0 {
			mem := r.MemoryMiB
			if mem == 0 {
				mem = 2048 * r.CPU
			}
			res := map[string]string{"cpu": strconv.Itoa(r.CPU), "memory": strconv.Itoa(mem) + "Mi"}
			if r.DiskGiB > 0 {
				res["disk"] = strconv.Itoa(r.DiskGiB) + "Gi"
			}
			body["resources"] = res
		}
		action := "Deny"
		if a.opts.Egress == "public" {
			action = "Allow"
		}
		body["egressPolicy"] = map[string]any{"defaultAction": action}
		if err := a.c.JSON(ctx, http.MethodPost, a.path("/sandboxes", nil), body, &s); err != nil {
			return adapter.Instance{}, err
		}
	}
	if s, err = a.waitRunning(ctx, s); err != nil {
		return adapter.Instance{}, err
	}
	return a.instance(s), nil
}

func (a *Adapter) Get(ctx context.Context, ref string) (adapter.Instance, error) {
	s, err := a.get(ctx, ref)
	if err != nil {
		return adapter.Instance{}, err
	}
	return a.instance(s), nil
}

func (a *Adapter) List(ctx context.Context, installation string) ([]adapter.Instance, error) {
	items, err := a.find(ctx, labelInstallation, installation)
	if err != nil {
		return nil, err
	}
	out := make([]adapter.Instance, 0, len(items))
	for _, s := range items {
		out = append(out, a.instance(s))
	}
	return out, nil
}

func (a *Adapter) Delete(ctx context.Context, ref string) error {
	err := a.c.JSON(ctx, http.MethodDelete, a.path(sandboxPath(ref, ""), nil), nil, nil)
	if api.IsCode(err, api.CodeNotFound) {
		return nil
	}
	return err
}

func (a *Adapter) Exec(ctx context.Context, ref string, req api.ExecRequest) (api.ExecResult, error) {
	var out struct {
		ExitCode        int    `json:"exitCode"`
		Stdout          string `json:"stdout"`
		Stderr          string `json:"stderr"`
		ExecutionTimeMs int64  `json:"executionTimeMs"`
	}
	body := map[string]any{"command": shellcmd.Script(req)}
	if err := a.c.JSON(ctx, http.MethodPost, a.path(sandboxPath(ref, "executeShellCommand"), nil), body, &out); err != nil {
		return api.ExecResult{}, err
	}
	return api.ExecResult{ExitCode: out.ExitCode, Stdout: out.Stdout, Stderr: out.Stderr, DurationMs: out.ExecutionTimeMs}, nil
}

func (a *Adapter) ReadFile(ctx context.Context, ref, path string) ([]byte, error) {
	resp, err := a.c.Do(ctx, http.MethodGet, a.path(sandboxPath(ref, "files"), url.Values{"path": {path}}), nil, "", http.Header{"Accept": {"application/octet-stream"}})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 100<<20))
}

func (a *Adapter) WriteFile(ctx context.Context, ref, path string, data []byte) error {
	q := url.Values{"path": {path}, "createDirs": {"true"}}
	resp, err := a.c.Do(ctx, http.MethodPost, a.path(sandboxPath(ref, "files"), q), bytes.Clone(data), "application/octet-stream", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var out struct {
		Success *bool  `json:"success"`
		Error   string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)
	if out.Success != nil && !*out.Success {
		return api.Errorf(api.CodeInvalidRequest, "azure-sandboxes: writing %s failed: %s", path, out.Error)
	}
	return nil
}

func (a *Adapter) ListFiles(ctx context.Context, ref, path string) ([]api.FileInfo, error) {
	var out struct {
		Entries []struct {
			Name         string `json:"name"`
			Path         string `json:"path"`
			Size         int64  `json:"size"`
			Mode         int    `json:"mode"`
			IsDir        bool   `json:"isDir"`
			ModifiedTime int64  `json:"modifiedTime"`
		} `json:"entries"`
	}
	if err := a.c.JSON(ctx, http.MethodGet, a.path(sandboxPath(ref, "files/list"), url.Values{"path": {path}}), nil, &out); err != nil {
		return nil, err
	}
	items := make([]api.FileInfo, 0, len(out.Entries))
	for _, e := range out.Entries {
		fi := api.FileInfo{Path: e.Path, Name: e.Name, Size: e.Size, IsDir: e.IsDir, Mode: fmt.Sprintf("%o", e.Mode)}
		if fi.Path == "" {
			fi.Path = strings.TrimRight(path, "/") + "/" + e.Name
		}
		if e.ModifiedTime > 0 {
			// The spec does not give the unit; values past 1e12 are milliseconds.
			if e.ModifiedTime > 1e12 {
				fi.ModTime = time.UnixMilli(e.ModifiedTime).UTC()
			} else {
				fi.ModTime = time.Unix(e.ModifiedTime, 0).UTC()
			}
		}
		items = append(items, fi)
	}
	return items, nil
}

// Stop saves the sandbox's state as a snapshot and stops it.
func (a *Adapter) Stop(ctx context.Context, ref string) error {
	if err := a.c.JSON(ctx, http.MethodPost, a.path(sandboxPath(ref, "stop"), nil), nil, nil); err != nil {
		return err
	}
	return httpx.Poll(ctx, time.Second, a.opts.ReadyTimeout, func() (bool, error) {
		s, err := a.get(ctx, ref)
		if err != nil {
			return false, err
		}
		if s.State == "StopFailed" {
			return false, adapter.SandboxFailed(fmt.Errorf("azure sandbox %s failed to stop", ref))
		}
		return s.State == "Stopped", nil
	})
}

// Start resumes a stopped sandbox from the snapshot Stop saved.
func (a *Adapter) Start(ctx context.Context, ref string) error {
	var s sandbox
	if err := a.c.JSON(ctx, http.MethodPost, a.path(sandboxPath(ref, "resume"), nil), nil, &s); err != nil {
		return err
	}
	if s.ID == "" {
		s.ID = ref
	}
	_, err := a.waitRunning(ctx, s)
	return err
}

// SetExpiry moves the auto-delete deadline; a zero time turns it off.
func (a *Adapter) SetExpiry(ctx context.Context, ref string, at time.Time) error {
	var secs int64
	if !at.IsZero() {
		s, err := a.get(ctx, ref)
		if err != nil {
			return err
		}
		secs = max(int64(at.Sub(s.CreatedAt).Seconds()), 1)
	}
	return a.c.JSON(ctx, http.MethodPost, a.path(sandboxPath(ref, "lifecycle"), nil), lifecycle(secs), nil)
}

// Snapshot captures the sandbox; restore is CreateRequest.FromSnapshot.
func (a *Adapter) Snapshot(ctx context.Context, ref, kind string) (adapter.Snapshot, error) {
	if kind == "memory" {
		return adapter.Snapshot{}, adapter.Unsupported("snapshots.memory")
	}
	var out struct {
		ID       string `json:"id"`
		SizeInMB int64  `json:"sizeInMB"`
	}
	body := map[string]any{"labels": map[string]string{labelSandbox: ref}}
	if err := a.c.JSON(ctx, http.MethodPost, a.path(sandboxPath(ref, "snapshot"), nil), body, &out); err != nil {
		return adapter.Snapshot{}, err
	}
	return adapter.Snapshot{Ref: out.ID, Kind: "disk", SizeBytes: out.SizeInMB << 20}, nil
}

func (a *Adapter) DeleteSnapshot(ctx context.Context, ref string) error {
	err := a.c.JSON(ctx, http.MethodDelete, a.path("/snapshots/"+url.PathEscape(ref), nil), nil, nil)
	if api.IsCode(err, api.CodeNotFound) {
		return nil
	}
	return err
}

type ports struct {
	Ports []struct {
		Port int    `json:"port"`
		URL  string `json:"url"`
	} `json:"ports"`
}

func (p ports) url(port int) string {
	for _, it := range p.Ports {
		if it.Port == port {
			return it.URL
		}
	}
	return ""
}

// ExposePort adds the port with anonymous access: anyone with the URL can
// reach it. A port added before is returned as it is.
func (a *Adapter) ExposePort(ctx context.Context, ref string, port int) (string, error) {
	var out ports
	body := map[string]any{"name": "p" + strconv.Itoa(port), "port": port, "auth": map[string]any{"anonymous": true}, "protocol": "Http"}
	err := a.c.JSON(ctx, http.MethodPost, a.path(sandboxPath(ref, "ports/add"), nil), body, &out)
	if api.IsCode(err, api.CodeInvalidState) {
		err = a.c.JSON(ctx, http.MethodGet, a.path(sandboxPath(ref, "ports"), nil), nil, &out)
	}
	if err != nil {
		return "", err
	}
	if u := out.url(port); u != "" {
		return u, nil
	}
	return "", adapter.Unavailable(fmt.Errorf("azure-sandboxes did not return a URL for port %d", port))
}

var (
	_ adapter.ExecutionAdapter = (*Adapter)(nil)
	_ adapter.FileSystem       = (*Adapter)(nil)
	_ adapter.Lifecycle        = (*Adapter)(nil)
	_ adapter.Expirer          = (*Adapter)(nil)
	_ adapter.Snapshotter      = (*Adapter)(nil)
	_ adapter.PortExposer      = (*Adapter)(nil)
)
