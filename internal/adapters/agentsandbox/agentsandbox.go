// Package agentsandbox adapts kubernetes-sigs/agent-sandbox
// (github.com/kubernetes-sigs/agent-sandbox, Apache-2.0): a Sandbox custom
// resource that a controller turns into one stateful pod, with optional warm
// pools behind SandboxClaims.
//
// agent-sandbox is self-hosted, so the pool's trust boundary is whatever the
// operator declares, and its isolation is the pod's RuntimeClass (runc
// containers by default, gVisor or Kata when the cluster offers them).
//
// The adapter talks to the Kubernetes API server directly, with the standard
// library (kube.go), instead of through client-go:
//
//   - Sandboxes (agents.x-k8s.io/v1beta1) for create, get, list, delete,
//     stop/start (spec.operatingMode) and the TTL backstop (spec.shutdownTime);
//     or, with config.warmPool, SandboxClaims (extensions.agents.x-k8s.io)
//     that adopt an already running Sandbox from a SandboxWarmPool;
//   - pods/exec over WebSocket for commands and files, so any image with a
//     POSIX shell works and no agent runs inside the sandbox.
//
// Written against agent-sandbox v1.0.8 (2026-10-22) and checked live on a
// single-node kind cluster (Kubernetes 1.37, runc); see
// docs/providers/agent-sandbox.md.
package agentsandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/different-ai/differently/internal/adapters/httpx"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

const (
	group      = "agents.x-k8s.io"
	extensions = "extensions.agents.x-k8s.io"
	version    = "v1beta1"
	// container is the name of the container in Sandboxes the adapter creates.
	container = "sandbox"
	// maxFile bounds ReadFile, like the other adapters.
	maxFile = 100 << 20
)

// DefaultFeatures are advertised unless the pool overrides config.features.
// Stop/start is added when the pool gives sandboxes a volume (config.volumeSize),
// since a suspended sandbox's pod, and with it every file outside the volume, is
// deleted. ports.expose is opt-in: the addresses are cluster-internal.
var DefaultFeatures = []string{adapter.FeatureExec, adapter.FeatureExecStream, adapter.FeatureFiles}

// Options configures the adapter. Pool config keys: apiServer (default: the
// in-cluster API server and service account), caFile, namespace (default
// "default"), image (default ubuntu:24.04; a request's template overrides it),
// runtimeClassName (gvisor, kata…), isolation (container|microvm), features
// (comma list), volumeSize, volumePath (default /workspace) and
// storageClassName, serviceAccountName; or warmPool (a SandboxWarmPool to
// claim from) with container (its pod's container, if it has several).
type Options struct {
	APIServer          string
	CAFile             string
	Token              string
	TokenFile          string
	Trust              string
	Namespace          string
	Image              string
	RuntimeClassName   string
	Isolation          string
	Features           []string
	VolumeSize         string
	VolumePath         string
	StorageClassName   string
	ServiceAccountName string
	WarmPool           string
	Container          string
	ReadyTimeout       time.Duration
}

var quantity = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(Ki|Mi|Gi|Ti|k|M|G|T)?$`)

// FromConfig maps pool config to options. token is the pool's credential;
// without one and without config.apiServer the adapter uses the in-cluster
// service account.
func FromConfig(cfg map[string]string, trust, token string) (Options, error) {
	o := Options{
		APIServer: strings.TrimRight(cfg["apiServer"], "/"), CAFile: cfg["caFile"], Token: token, Trust: trust,
		Namespace: cfg["namespace"], Image: cfg["image"], RuntimeClassName: cfg["runtimeClassName"], Isolation: cfg["isolation"],
		VolumeSize: cfg["volumeSize"], VolumePath: cfg["volumePath"], StorageClassName: cfg["storageClassName"],
		ServiceAccountName: cfg["serviceAccountName"], WarmPool: cfg["warmPool"], Container: cfg["container"],
		Features: slices.Clone(DefaultFeatures),
	}
	if o.APIServer == "" {
		host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
		if host == "" {
			return o, errors.New("agent-sandbox pools need config.apiServer outside a Kubernetes cluster")
		}
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		o.APIServer = "https://" + host + ":" + port
		if o.CAFile == "" {
			o.CAFile = inClusterCA
		}
		if o.Token == "" {
			o.TokenFile = inClusterToken
		}
	}
	if o.Namespace == "" {
		o.Namespace = "default"
	}
	if o.WarmPool != "" {
		// The pool's SandboxTemplate decides the image, resources, RuntimeClass
		// and volumes.
		for _, k := range []string{"image", "runtimeClassName", "volumeSize", "storageClassName", "serviceAccountName"} {
			if cfg[k] != "" {
				return o, fmt.Errorf("agent-sandbox config.%s does not apply with config.warmPool: the warm pool's SandboxTemplate sets it", k)
			}
		}
	} else {
		if o.Image == "" {
			o.Image = "ubuntu:24.04"
		}
		if o.Container == "" {
			o.Container = container
		}
	}
	if o.VolumeSize != "" {
		if !quantity.MatchString(o.VolumeSize) {
			return o, fmt.Errorf("agent-sandbox config.volumeSize must be a Kubernetes quantity such as 10Gi, not %q", o.VolumeSize)
		}
		if o.VolumePath == "" {
			o.VolumePath = "/workspace"
		}
		o.Features = append(o.Features, adapter.FeatureStopStart, adapter.FeaturePersistDisk)
	}
	if f := strings.TrimSpace(cfg["features"]); f != "" {
		o.Features = nil
		for _, v := range strings.Split(f, ",") {
			if v = strings.TrimSpace(v); v != "" {
				o.Features = append(o.Features, v)
			}
		}
	}
	if o.WarmPool != "" && slices.Contains(o.Features, adapter.FeatureStopStart) {
		return o, errors.New("agent-sandbox: stop/start is not offered with config.warmPool")
	}
	switch o.Isolation {
	case "":
	case "microvm":
		o.Features = append(o.Features, adapter.FeatureMicroVM)
	case "container":
		o.Features = append(o.Features, adapter.FeatureContainer)
	default:
		return o, fmt.Errorf("agent-sandbox config.isolation must be microvm or container, not %q", o.Isolation)
	}
	return o, nil
}

// Adapter manages Sandboxes (or SandboxClaims) in one namespace of one cluster.
type Adapter struct {
	opts Options
	k    *kube

	mu    sync.Mutex
	bound map[string]string // claim -> the Sandbox it adopted
}

// New returns an adapter.
func New(o Options) (*Adapter, error) {
	if o.ReadyTimeout == 0 {
		o.ReadyTimeout = 3 * time.Minute
	}
	k, err := newKube(o.APIServer, o.CAFile, o.Token, o.TokenFile)
	if err != nil {
		return nil, err
	}
	return &Adapter{opts: o, k: k, bound: map[string]string{}}, nil
}

// Endpoint is reported in the trust report.
func (a *Adapter) Endpoint() string {
	return a.opts.APIServer + " (namespace " + a.opts.Namespace + ")"
}

func (a *Adapter) path(grp, plural string) string {
	return "/apis/" + grp + "/" + version + "/namespaces/" + url.PathEscape(a.opts.Namespace) + "/" + plural
}

func (a *Adapter) sandboxes() string { return a.path(group, "sandboxes") }

// owned is where the adapter's own objects live: Sandboxes, or claims.
func (a *Adapter) owned() string {
	if a.opts.WarmPool != "" {
		return a.path(extensions, "sandboxclaims")
	}
	return a.sandboxes()
}

type condition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason"`
	Message            string `json:"message"`
	ObservedGeneration int64  `json:"observedGeneration"`
}

// object is a Sandbox or a SandboxClaim: the fields the adapter reads.
type object struct {
	Metadata struct {
		Name              string            `json:"name"`
		Labels            map[string]string `json:"labels"`
		Generation        int64             `json:"generation"`
		DeletionTimestamp string            `json:"deletionTimestamp"`
	} `json:"metadata"`
	Spec struct {
		OperatingMode string `json:"operatingMode"`
	} `json:"spec"`
	Status struct {
		Conditions  []condition `json:"conditions"`
		PodIPs      []string    `json:"podIPs"`
		ServiceFQDN string      `json:"serviceFQDN"`
		Sandbox     struct {    // claims only
			Name        string   `json:"name"`
			PodIPs      []string `json:"podIPs"`
			ServiceFQDN string   `json:"serviceFQDN"`
		} `json:"sandbox"`
	} `json:"status"`
}

// cond returns a current condition: one observed for the object's latest
// generation, so a Ready left over from before a spec change does not count.
func (s object) cond(t string) (condition, bool) {
	for _, c := range s.Status.Conditions {
		if c.Type == t && (c.ObservedGeneration == 0 || c.ObservedGeneration == s.Metadata.Generation) {
			return c, true
		}
	}
	return condition{}, false
}

func (s object) is(t string) bool { c, ok := s.cond(t); return ok && c.Status == "True" }

func (s object) state() string {
	switch {
	case s.Spec.OperatingMode == "Suspended":
		return adapter.StateStopped
	case s.is("Ready"):
		return adapter.StateRunning
	}
	if c, ok := s.cond("Ready"); ok && (failedReasons[c.Reason] || invalidReasons[c.Reason]) {
		return adapter.StateError
	}
	return adapter.StatePending
}

func (s object) inst() adapter.Instance {
	msg := ""
	if c, ok := s.cond("Ready"); ok && c.Status != "True" {
		msg = strings.TrimSpace(c.Reason + " " + c.Message)
	}
	return adapter.Instance{Ref: s.Metadata.Name, SandboxID: s.Metadata.Labels[adapter.LabelSandboxID], State: s.state(), Message: msg}
}

// Ready=False reasons the controllers do not recover from: the request is
// wrong, or this one sandbox has ended.
var (
	invalidReasons = map[string]bool{"InvalidConfiguration": true, "InvalidMetadata": true, "EnvVarsInjectionRejected": true, "VolumeClaimTemplatesError": true}
	failedReasons  = map[string]bool{"PodFailed": true, "PodSucceeded": true, "SandboxExpired": true, "ClaimExpired": true}
)

func (a *Adapter) Info() adapter.Info {
	iso := "container (runc, the cluster's default runtime)"
	switch {
	case a.opts.Isolation == "microvm" && a.opts.RuntimeClassName != "":
		iso = "microVM (RuntimeClass " + a.opts.RuntimeClassName + ")"
	case a.opts.Isolation == "microvm":
		iso = "microVM (declared by the pool)"
	case a.opts.WarmPool != "":
		iso = "set by the SandboxTemplate behind warm pool " + a.opts.WarmPool
	case a.opts.RuntimeClassName != "":
		iso = "pod with RuntimeClass " + a.opts.RuntimeClassName
	}
	return adapter.Info{Kind: "agent-sandbox", Version: "1", Trust: a.opts.Trust, Isolation: iso, Protocol: adapter.ProtocolVersion, Description: "agent-sandbox at " + a.Endpoint()}
}

// Capabilities advertises the warm pool as the only template: its
// SandboxTemplate fixes the image, so requests for another one go elsewhere.
func (a *Adapter) Capabilities(context.Context) (adapter.Capabilities, error) {
	var templates []string
	if a.opts.WarmPool != "" {
		templates = []string{a.opts.WarmPool}
	}
	return adapter.Capabilities{Features: a.opts.Features, Templates: templates}, nil
}

func (a *Adapter) Capacity(context.Context) (adapter.Capacity, error) { return adapter.Capacity{}, nil }

// Health fails when the API server is unreachable, the credential is refused,
// the agent-sandbox CRDs are not installed or the warm pool does not exist.
func (a *Adapter) Health(ctx context.Context) error {
	if a.opts.WarmPool != "" {
		err := a.k.do(ctx, http.MethodGet, a.path(extensions, "sandboxwarmpools")+"/"+url.PathEscape(a.opts.WarmPool), "", nil, nil)
		if api.IsCode(err, api.CodeNotFound) {
			return adapter.Unavailable(fmt.Errorf("agent-sandbox: SandboxWarmPool %s not found in namespace %s", a.opts.WarmPool, a.opts.Namespace))
		}
		return err
	}
	return a.k.do(ctx, http.MethodGet, a.sandboxes()+"?limit=1", "", nil, nil)
}

// name derives the object name from the Differently ID, which makes Create
// idempotent: Kubernetes names are unique in a namespace.
func name(id string) string {
	return "dly-" + strings.ToLower(strings.ReplaceAll(id, "_", "-"))
}

func (a *Adapter) get(ctx context.Context, base, ref string) (object, error) {
	var s object
	if err := a.k.do(ctx, http.MethodGet, base+"/"+url.PathEscape(ref), "", nil, &s); err != nil {
		return s, err
	}
	if s.Metadata.DeletionTimestamp != "" {
		return s, adapter.NotFound(ref)
	}
	return s, nil
}

func (a *Adapter) Get(ctx context.Context, ref string) (adapter.Instance, error) {
	s, err := a.get(ctx, a.owned(), ref)
	return s.inst(), err
}

// sandbox returns the Sandbox behind ref: ref itself, or the one its claim
// adopted. The pod has the Sandbox's name.
func (a *Adapter) sandbox(ctx context.Context, ref string) (string, error) {
	if a.opts.WarmPool == "" {
		return ref, nil
	}
	a.mu.Lock()
	sb, ok := a.bound[ref]
	a.mu.Unlock()
	if ok {
		return sb, nil
	}
	c, err := a.get(ctx, a.owned(), ref)
	if err != nil {
		return "", err
	}
	if c.Status.Sandbox.Name == "" {
		return "", api.Errorf(api.CodeInvalidState, "agent-sandbox: claim %s has no Sandbox yet", ref)
	}
	a.mu.Lock()
	a.bound[ref] = c.Status.Sandbox.Name
	a.mu.Unlock()
	return c.Status.Sandbox.Name, nil
}

func envList(env map[string]string) []map[string]string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]map[string]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, map[string]string{"name": k, "value": env[k]})
	}
	return out
}

// manifest is the Sandbox, or with a warm pool the SandboxClaim, for req.
func (a *Adapter) manifest(req adapter.CreateRequest) map[string]any {
	labels := adapter.OwnershipLabels(req)
	var shutdown map[string]any
	if req.TTLSeconds > 0 {
		// The provider-side backstop: the controller deletes the object then.
		shutdown = map[string]any{
			"shutdownTime":   time.Now().Add(time.Duration(req.TTLSeconds) * time.Second).UTC().Format(time.RFC3339),
			"shutdownPolicy": "Delete",
		}
	}
	meta := map[string]any{"name": name(req.ID), "labels": labels}
	if a.opts.WarmPool != "" {
		spec := map[string]any{"warmPoolRef": map[string]string{"name": a.opts.WarmPool}}
		if len(req.Env) > 0 {
			// Env makes the controller start a fresh Sandbox instead of adopting a
			// warm one, and needs the template's envVarsInjectionPolicy: Allowed.
			spec["env"] = envList(req.Env)
		}
		if shutdown != nil {
			spec["lifecycle"] = shutdown
		}
		return map[string]any{"apiVersion": extensions + "/" + version, "kind": "SandboxClaim", "metadata": meta, "spec": spec}
	}

	image := a.opts.Image
	if req.Template != "" {
		image = req.Template
	}
	res := map[string]string{
		"cpu": strconv.Itoa(max(req.Resources.CPU, 1)), "memory": strconv.Itoa(max(req.Resources.MemoryMiB, 128)) + "Mi",
	}
	if req.Resources.DiskGiB > 0 {
		res["ephemeral-storage"] = strconv.Itoa(req.Resources.DiskGiB) + "Gi"
	}
	ctr := map[string]any{
		"name": container, "image": image, "env": envList(req.Env),
		// Keep the pod alive whatever the image's own entrypoint does, and stop
		// at once when the pod is deleted or suspended.
		"command":   []string{"sh", "-c", "trap 'exit 0' TERM; while :; do sleep 3600 & wait $!; done"},
		"resources": map[string]any{"requests": res, "limits": res},
	}
	pod := map[string]any{
		"containers": []any{ctr}, "restartPolicy": "Always",
		"terminationGracePeriodSeconds": 5, "automountServiceAccountToken": false,
	}
	if a.opts.RuntimeClassName != "" {
		pod["runtimeClassName"] = a.opts.RuntimeClassName
	}
	if a.opts.ServiceAccountName != "" {
		pod["serviceAccountName"] = a.opts.ServiceAccountName
	}
	spec := map[string]any{"podTemplate": map[string]any{"metadata": map[string]any{"labels": labels}, "spec": pod}}
	if a.opts.VolumeSize != "" {
		ctr["volumeMounts"] = []any{map[string]string{"name": "workspace", "mountPath": a.opts.VolumePath}}
		pvc := map[string]any{"accessModes": []string{"ReadWriteOnce"}, "resources": map[string]any{"requests": map[string]string{"storage": a.opts.VolumeSize}}}
		if a.opts.StorageClassName != "" {
			pvc["storageClassName"] = a.opts.StorageClassName
		}
		spec["volumeClaimTemplates"] = []any{map[string]any{"metadata": map[string]any{"name": "workspace", "labels": labels}, "spec": pvc}}
	}
	if slices.Contains(a.opts.Features, adapter.FeaturePorts) {
		spec["service"] = true // a headless Service, so the sandbox has a stable DNS name
	}
	for k, v := range shutdown {
		spec[k] = v
	}
	return map[string]any{"apiVersion": group + "/" + version, "kind": "Sandbox", "metadata": meta, "spec": spec}
}

// Create implements adapter.ExecutionAdapter. It is idempotent on req.ID: the
// object's name is derived from it, so a second create finds the first.
func (a *Adapter) Create(ctx context.Context, req adapter.CreateRequest) (adapter.Instance, error) {
	if req.FromSnapshot != "" {
		return adapter.Instance{}, adapter.Unsupported(adapter.FeatureSnapshotDisk)
	}
	ref := name(req.ID)
	err := a.k.do(ctx, http.MethodPost, a.owned(), "", a.manifest(req), nil)
	if err != nil && !api.IsCode(err, api.CodeInvalidState) { // 409: it already exists
		return adapter.Instance{}, err
	}
	s, err := a.get(ctx, a.owned(), ref)
	if err != nil {
		return adapter.Instance{}, err
	}
	if got := s.Metadata.Labels[adapter.LabelSandboxID]; got != req.ID {
		return adapter.Instance{}, api.Errorf(api.CodeInvalidState, "agent-sandbox: %s belongs to %q, not %s", ref, got, req.ID)
	}
	if s, err = a.waitReady(ctx, ref); err != nil {
		var e *api.Error
		if errors.As(err, &e) && (e.Code == api.CodeInvalidRequest || e.Code == adapter.CodeSandboxFailed) {
			// It will not start: remove it so a retry starts afresh.
			_ = a.Delete(context.WithoutCancel(ctx), ref)
		}
		return adapter.Instance{}, err
	}
	return s.inst(), nil
}

// waitReady waits for the object and its pod to be ready. A pod that can
// never start (an image that does not exist, a bad spec) fails at once rather
// than at the timeout.
func (a *Adapter) waitReady(ctx context.Context, ref string) (object, error) {
	var s object
	err := httpx.Poll(ctx, 500*time.Millisecond, a.opts.ReadyTimeout, func() (bool, error) {
		var err error
		if s, err = a.get(ctx, a.owned(), ref); err != nil {
			return false, err
		}
		if c, ok := s.cond("Ready"); ok && c.Status != "True" {
			switch {
			case invalidReasons[c.Reason]:
				return false, api.Errorf(api.CodeInvalidRequest, "agent-sandbox: %s: %s %s", ref, c.Reason, c.Message)
			case failedReasons[c.Reason]:
				return false, adapter.SandboxFailed(fmt.Errorf("agent-sandbox: %s: %s %s", ref, c.Reason, c.Message))
			}
		}
		sb := ref
		if a.opts.WarmPool != "" {
			if sb = s.Status.Sandbox.Name; sb == "" {
				return false, nil // not adopted or created yet
			}
		}
		p, err := a.pod(ctx, sb)
		if err != nil {
			return false, nil // the controller has not created it yet
		}
		if s.is("Ready") {
			return p.ready(), nil
		}
		return false, p.problem(ref)
	})
	if err != nil && api.IsCode(err, api.CodeProviderUnavailable) && strings.Contains(err.Error(), "timed out") {
		// The server is fine; this one pod did not become ready in time.
		return s, adapter.SandboxFailed(fmt.Errorf("agent-sandbox: %s was not ready within %s: %s", ref, a.opts.ReadyTimeout, s.inst().Message))
	}
	return s, err
}

type pod struct {
	Metadata struct {
		DeletionTimestamp string `json:"deletionTimestamp"`
	} `json:"metadata"`
	Status struct {
		Phase             string `json:"phase"`
		ContainerStatuses []struct {
			Name  string `json:"name"`
			Ready bool   `json:"ready"`
			State struct {
				Waiting *struct {
					Reason  string `json:"reason"`
					Message string `json:"message"`
				} `json:"waiting"`
			} `json:"state"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

func (a *Adapter) pod(ctx context.Context, name string) (pod, error) {
	var p pod
	err := a.k.do(ctx, http.MethodGet, "/api/v1/namespaces/"+url.PathEscape(a.opts.Namespace)+"/pods/"+url.PathEscape(name), "", nil, &p)
	return p, err
}

func (p pod) ready() bool {
	if p.Metadata.DeletionTimestamp != "" || p.Status.Phase != "Running" || len(p.Status.ContainerStatuses) == 0 {
		return false
	}
	for _, c := range p.Status.ContainerStatuses {
		if !c.Ready {
			return false
		}
	}
	return true
}

// problem classifies a pod that cannot start. Pull errors for an image that
// does not exist are the request's fault; a crash loop is this sandbox's.
// Other waiting reasons (a slow pull, scheduling) keep the wait going.
func (p pod) problem(ref string) error {
	if p.Status.Phase == "Failed" {
		return adapter.SandboxFailed(fmt.Errorf("agent-sandbox: the pod of %s failed", ref))
	}
	for _, c := range p.Status.ContainerStatuses {
		w := c.State.Waiting
		if w == nil {
			continue
		}
		msg := fmt.Sprintf("agent-sandbox: %s: %s: %s", ref, w.Reason, w.Message)
		switch {
		case w.Reason == "InvalidImageName" || w.Reason == "CreateContainerConfigError" || w.Reason == "CreateContainerError":
			return api.Errorf(api.CodeInvalidRequest, "%s", msg)
		case (w.Reason == "ErrImagePull" || w.Reason == "ImagePullBackOff") && notFound(w.Message):
			return api.Errorf(api.CodeInvalidRequest, "%s", msg)
		case w.Reason == "CrashLoopBackOff":
			return adapter.SandboxFailed(errors.New(msg))
		}
	}
	return nil
}

func notFound(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "not found") || strings.Contains(m, "manifest unknown") || strings.Contains(m, "does not exist")
}

// List returns every object labelled with the installation, page by page;
// any failed page fails the whole call.
func (a *Adapter) List(ctx context.Context, installation string) ([]adapter.Instance, error) {
	out := []adapter.Instance{}
	cont := ""
	for range 1000 {
		var page struct {
			Items    []object `json:"items"`
			Metadata struct {
				Continue string `json:"continue"`
			} `json:"metadata"`
		}
		q := url.Values{"labelSelector": {adapter.LabelInstallation + "=" + installation}, "limit": {"200"}}
		if cont != "" {
			q.Set("continue", cont)
		}
		if err := a.k.do(ctx, http.MethodGet, a.owned()+"?"+q.Encode(), "", nil, &page); err != nil {
			return nil, err
		}
		for _, s := range page.Items {
			if s.Metadata.Labels[adapter.LabelInstallation] == installation && s.Metadata.DeletionTimestamp == "" {
				out = append(out, s.inst())
			}
		}
		if cont = page.Metadata.Continue; cont == "" {
			return out, nil
		}
	}
	return nil, adapter.Unavailable(errors.New("agent-sandbox: too many pages"))
}

// Delete removes the Sandbox or claim; the pod, Service and volumes are
// owned by it and go with it.
func (a *Adapter) Delete(ctx context.Context, ref string) error {
	err := a.k.do(ctx, http.MethodDelete, a.owned()+"/"+url.PathEscape(ref), "", map[string]string{"propagationPolicy": "Background"}, nil)
	a.mu.Lock()
	delete(a.bound, ref)
	a.mu.Unlock()
	if api.IsCode(err, api.CodeNotFound) {
		return nil
	}
	return err
}

// argv turns an exec request into the command run in the container: env and
// cwd through env(1) and sh, and the timeout through timeout(1), which ends
// the command with exit code 124 (and kills it 5 s later if it lingers).
func argv(req api.ExecRequest) []string {
	var out []string
	if req.TimeoutSeconds > 0 {
		out = append(out, "timeout", "-k", "5", strconv.Itoa(req.TimeoutSeconds))
	}
	if len(req.Env) > 0 {
		out = append(out, "env")
		for _, e := range envList(req.Env) {
			out = append(out, e["name"]+"="+e["value"])
		}
	}
	cd := ""
	if req.Cwd != "" {
		cd = "cd " + httpx.ShellQuote([]string{req.Cwd}) + " && "
	}
	switch {
	case req.Command != "":
		out = append(out, "sh", "-c", cd+req.Command)
	case cd != "":
		out = append(append(out, "sh", "-c", cd+`exec "$@"`, "sh"), req.Args...)
	default:
		out = append(out, req.Args...)
	}
	return out
}

func (a *Adapter) run(ctx context.Context, ref string, req api.ExecRequest, emit func(api.ExecEvent) error) (int, error) {
	if req.Command == "" && len(req.Args) == 0 {
		return -1, api.Errorf(api.CodeInvalidRequest, "agent-sandbox: exec needs a command or args")
	}
	if req.TimeoutSeconds > 0 {
		// A backstop in case timeout(1) is missing from the image.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutSeconds+10)*time.Second)
		defer cancel()
	}
	var stdin io.Reader
	if req.Stdin != "" {
		stdin = strings.NewReader(req.Stdin)
	}
	return a.exec(ctx, ref, argv(req), stdin, func(stream string, data []byte) error {
		return emit(api.ExecEvent{Stream: stream, Data: string(data)})
	})
}

// exec runs argv in the sandbox's pod. A pod that does not exist maps to the
// sandbox not being found, or, while it is stopped, to invalid_state.
func (a *Adapter) exec(ctx context.Context, ref string, cmd []string, stdin io.Reader, emit func(string, []byte) error) (int, error) {
	sb, err := a.sandbox(ctx, ref)
	if err != nil {
		return -1, err
	}
	code, err := a.k.exec(ctx, a.opts.Namespace, sb, a.opts.Container, cmd, stdin, emit)
	if api.IsCode(err, api.CodeNotFound) {
		if s, gerr := a.get(ctx, a.sandboxes(), sb); gerr == nil && s.Spec.OperatingMode == "Suspended" {
			return -1, api.Errorf(api.CodeInvalidState, "agent-sandbox: %s is stopped", ref)
		}
		return -1, adapter.NotFound(ref)
	}
	if api.IsCode(err, api.CodeInvalidRequest) && notRunning(err.Error()) {
		return -1, api.Errorf(api.CodeInvalidState, "agent-sandbox: %s is not running: %v", ref, err)
	}
	return code, err
}

// notRunning recognises the API server refusing an exec because the pod's
// container is not up (pending, restarting, terminating).
func notRunning(msg string) bool {
	for _, s := range []string{"container not found", "is not running", "does not have a host assigned", "is terminated"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// Exec implements adapter.ExecutionAdapter.
func (a *Adapter) Exec(ctx context.Context, ref string, req api.ExecRequest) (api.ExecResult, error) {
	var stdout, stderr strings.Builder
	start := time.Now()
	code, err := a.run(ctx, ref, req, func(ev api.ExecEvent) error {
		if ev.Stream == "stderr" {
			stderr.WriteString(ev.Data)
		} else {
			stdout.WriteString(ev.Data)
		}
		return nil
	})
	if err != nil {
		return api.ExecResult{}, err
	}
	return api.ExecResult{ExitCode: code, Stdout: stdout.String(), Stderr: stderr.String(), DurationMs: time.Since(start).Milliseconds()}, nil
}

// ExecStream implements adapter.Streamer.
func (a *Adapter) ExecStream(ctx context.Context, ref string, req api.ExecRequest, emit func(api.ExecEvent) error) (int, error) {
	return a.run(ctx, ref, req, emit)
}

// shell runs a small script with arguments and returns stdout, or an error
// built from stderr: not_found when the script exits 2.
func (a *Adapter) shell(ctx context.Context, ref, script string, stdin io.Reader, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	code, err := a.exec(ctx, ref, append([]string{"sh", "-c", script, "sh"}, args...), stdin, func(stream string, data []byte) error {
		if stream == "stderr" {
			stderr.Write(data)
			return nil
		}
		if stdout.Len()+len(data) > maxFile {
			return api.Errorf(api.CodeInvalidRequest, "agent-sandbox: output is larger than %d bytes", maxFile)
		}
		stdout.Write(data)
		return nil
	})
	switch {
	case err != nil:
		return nil, err
	case code == 2:
		return nil, api.Errorf(api.CodeNotFound, "agent-sandbox: %s", strings.TrimSpace(stderr.String()))
	case code != 0:
		return nil, api.Errorf(api.CodeInvalidRequest, "agent-sandbox: exit %d: %s", code, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func (a *Adapter) ReadFile(ctx context.Context, ref, path string) ([]byte, error) {
	return a.shell(ctx, ref, `[ -f "$1" ] || { echo "no such file: $1" >&2; exit 2; }; exec cat -- "$1"`, nil, path)
}

// WriteFile streams data to cat(1) over the exec's stdin, creating parents.
func (a *Adapter) WriteFile(ctx context.Context, ref, path string, data []byte) error {
	_, err := a.shell(ctx, ref, `mkdir -p -- "$(dirname -- "$1")" && cat > "$1"`, bytes.NewReader(data), path)
	return err
}

// ListFiles lists one directory with stat(1), which GNU coreutils and
// BusyBox both provide: size|octal mode|mtime|type|name per line.
func (a *Adapter) ListFiles(ctx context.Context, ref, path string) ([]api.FileInfo, error) {
	out, err := a.shell(ctx, ref, `cd -- "$1" 2>/dev/null || { echo "no such directory: $1" >&2; exit 2; }
for f in * .[!.]* ..?*; do if [ -e "$f" ] || [ -L "$f" ]; then stat -c '%s|%a|%Y|%F|%n' -- "$f"; fi; done`, nil, path)
	if err != nil {
		return nil, err
	}
	dir := strings.TrimRight(path, "/")
	files := []api.FileInfo{}
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		f := strings.SplitN(line, "|", 5)
		if len(f) != 5 {
			continue
		}
		size, _ := strconv.ParseInt(f[0], 10, 64)
		secs, _ := strconv.ParseInt(f[2], 10, 64)
		files = append(files, api.FileInfo{Path: dir + "/" + f[4], Name: f[4], Size: size, Mode: f[1], IsDir: f[3] == "directory", ModTime: time.Unix(secs, 0).UTC()})
	}
	return files, nil
}

func (a *Adapter) patch(ctx context.Context, base, ref string, spec map[string]any) error {
	return a.k.do(ctx, http.MethodPatch, base+"/"+url.PathEscape(ref), "application/merge-patch+json", map[string]any{"spec": spec}, nil)
}

// Stop suspends the Sandbox: the controller deletes its pod and keeps its
// volumes, so only files under config.volumePath survive.
func (a *Adapter) Stop(ctx context.Context, ref string) error {
	if !slices.Contains(a.opts.Features, adapter.FeatureStopStart) || a.opts.WarmPool != "" {
		return adapter.Unsupported(adapter.FeatureStopStart)
	}
	if err := a.patch(ctx, a.sandboxes(), ref, map[string]any{"operatingMode": "Suspended"}); err != nil {
		return err
	}
	return httpx.Poll(ctx, 500*time.Millisecond, a.opts.ReadyTimeout, func() (bool, error) {
		s, err := a.get(ctx, a.sandboxes(), ref)
		return err == nil && s.is("Suspended"), err
	})
}

func (a *Adapter) Start(ctx context.Context, ref string) error {
	if !slices.Contains(a.opts.Features, adapter.FeatureStopStart) || a.opts.WarmPool != "" {
		return adapter.Unsupported(adapter.FeatureStopStart)
	}
	if err := a.patch(ctx, a.sandboxes(), ref, map[string]any{"operatingMode": "Running"}); err != nil {
		return err
	}
	_, err := a.waitReady(ctx, ref)
	return err
}

// SetExpiry moves the controller's deletion time (the TTL backstop).
func (a *Adapter) SetExpiry(ctx context.Context, ref string, at time.Time) error {
	shutdown := map[string]any{"shutdownTime": at.UTC().Format(time.RFC3339), "shutdownPolicy": "Delete"}
	if a.opts.WarmPool != "" {
		return a.patch(ctx, a.owned(), ref, map[string]any{"lifecycle": shutdown})
	}
	return a.patch(ctx, a.sandboxes(), ref, shutdown)
}

// ExposePort returns the sandbox's in-cluster address for a port: the
// headless Service's DNS name when there is one, else the pod IP. It is
// reachable only from inside the cluster (and, for Sandboxes from a template
// with a managed NetworkPolicy, only from the sandbox router), which is why
// the feature is opt-in.
func (a *Adapter) ExposePort(ctx context.Context, ref string, port int) (string, error) {
	s, err := a.get(ctx, a.owned(), ref)
	if err != nil {
		return "", err
	}
	fqdn, ips := s.Status.ServiceFQDN, s.Status.PodIPs
	if a.opts.WarmPool != "" {
		fqdn, ips = s.Status.Sandbox.ServiceFQDN, s.Status.Sandbox.PodIPs
	}
	host := fqdn
	if host == "" && len(ips) > 0 {
		host = ips[0]
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
	}
	if host == "" {
		return "", api.Errorf(api.CodeInvalidState, "agent-sandbox: %s has no address yet", ref)
	}
	return fmt.Sprintf("http://%s:%d", host, port), nil
}

var (
	_ adapter.ExecutionAdapter = (*Adapter)(nil)
	_ adapter.Streamer         = (*Adapter)(nil)
	_ adapter.FileSystem       = (*Adapter)(nil)
	_ adapter.Lifecycle        = (*Adapter)(nil)
	_ adapter.Expirer          = (*Adapter)(nil)
	_ adapter.PortExposer      = (*Adapter)(nil)
)
