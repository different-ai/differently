package agentsandbox

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/different-ai/differently/internal/config"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

const token = "kube-token-0123456789"

// fakeKube mimics the parts of the Kubernetes API the adapter uses, with
// agent-sandbox's controllers reduced to "a Sandbox is Ready on the next
// read": Sandboxes and SandboxClaims, pods, and pods/exec over WebSocket.
type fakeKube struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	objects  map[string]map[string]any // "sandboxes/name" or "sandboxclaims/name"
	pods     map[string]map[string]any
	creates  int
	pageSize int
	protos   []string // exec subprotocols the server accepts
	failList bool

	// exec answers one command: stdout, stderr and the status to send.
	exec     func(argv []string, stdin string) (string, string, map[string]any)
	lastArgv []string
	lastQ    map[string][]string
}

func newFakeKube(t *testing.T) *fakeKube {
	f := &fakeKube{t: t, objects: map[string]map[string]any{}, pods: map[string]map[string]any{}, protos: []string{"v5.channel.k8s.io", "v4.channel.k8s.io"}}
	f.exec = func(argv []string, stdin string) (string, string, map[string]any) {
		return "out\n", "err\n", map[string]any{"status": "Success"}
	}
	mux := http.NewServeMux()
	jsonOut := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	status := func(w http.ResponseWriter, code int, reason, msg string) {
		jsonOut(w, code, map[string]any{"kind": "Status", "status": "Failure", "reason": reason, "message": msg, "code": code})
	}
	authed := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+token {
				status(w, 401, "Unauthorized", "Unauthorized")
				return
			}
			if !strings.HasSuffix(r.URL.Path, "/exec") {
				f.mu.Lock()
				defer f.mu.Unlock()
			}
			h(w, r)
		}
	}
	const sbx = "/apis/agents.x-k8s.io/v1beta1/namespaces/ns1/"
	const ext = "/apis/extensions.agents.x-k8s.io/v1beta1/namespaces/ns1/"
	for _, base := range []string{sbx + "{plural}", ext + "{plural}"} {
		mux.HandleFunc("POST "+base, authed(func(w http.ResponseWriter, r *http.Request) {
			var obj map[string]any
			_ = json.NewDecoder(r.Body).Decode(&obj)
			md := obj["metadata"].(map[string]any)
			key := r.PathValue("plural") + "/" + md["name"].(string)
			if _, ok := f.objects[key]; ok {
				status(w, 409, "AlreadyExists", key+" already exists")
				return
			}
			f.creates++
			md["generation"] = 1
			f.objects[key] = obj
			jsonOut(w, 201, obj)
		}))
		mux.HandleFunc("GET "+base, authed(func(w http.ResponseWriter, r *http.Request) {
			if f.failList && r.URL.Query().Get("continue") != "" {
				status(w, 500, "InternalError", "etcd timeout")
				return
			}
			k, v, _ := strings.Cut(r.URL.Query().Get("labelSelector"), "=")
			var names []string
			for key, obj := range f.objects {
				plural, name, _ := strings.Cut(key, "/")
				labels, _ := obj["metadata"].(map[string]any)["labels"].(map[string]any)
				if plural == r.PathValue("plural") && (k == "" || labels[k] == v) {
					names = append(names, name)
				}
			}
			slices.Sort(names)
			start := 0
			if c := r.URL.Query().Get("continue"); c != "" {
				fmt.Sscan(c, &start)
			}
			items, next := []any{}, ""
			for i := start; i < len(names); i++ {
				if f.pageSize > 0 && len(items) == f.pageSize {
					next = fmt.Sprint(i)
					break
				}
				items = append(items, f.objects[r.PathValue("plural")+"/"+names[i]])
			}
			jsonOut(w, 200, map[string]any{"items": items, "metadata": map[string]any{"continue": next}})
		}))
		mux.HandleFunc("GET "+base+"/{name}", authed(func(w http.ResponseWriter, r *http.Request) {
			key := r.PathValue("plural") + "/" + r.PathValue("name")
			obj, ok := f.objects[key]
			if !ok {
				status(w, 404, "NotFound", key+" not found")
				return
			}
			f.reconcile(key, obj)
			jsonOut(w, 200, obj)
		}))
		mux.HandleFunc("PATCH "+base+"/{name}", authed(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Content-Type") != "application/merge-patch+json" {
				status(w, 415, "UnsupportedMediaType", "want a merge patch")
				return
			}
			key := r.PathValue("plural") + "/" + r.PathValue("name")
			obj, ok := f.objects[key]
			if !ok {
				status(w, 404, "NotFound", key+" not found")
				return
			}
			var p map[string]map[string]any
			_ = json.NewDecoder(r.Body).Decode(&p)
			spec, _ := obj["spec"].(map[string]any)
			for k, v := range p["spec"] {
				spec[k] = v
			}
			md := obj["metadata"].(map[string]any)
			md["generation"] = md["generation"].(int) + 1
			jsonOut(w, 200, obj)
		}))
		mux.HandleFunc("DELETE "+base+"/{name}", authed(func(w http.ResponseWriter, r *http.Request) {
			key := r.PathValue("plural") + "/" + r.PathValue("name")
			if _, ok := f.objects[key]; !ok {
				status(w, 404, "NotFound", key+" not found")
				return
			}
			delete(f.objects, key)
			delete(f.pods, r.PathValue("name"))
			jsonOut(w, 200, map[string]any{"status": "Success"})
		}))
	}
	mux.HandleFunc("GET /api/v1/namespaces/ns1/pods/{name}", authed(func(w http.ResponseWriter, r *http.Request) {
		p, ok := f.pods[r.PathValue("name")]
		if !ok {
			status(w, 404, "NotFound", "pods "+r.PathValue("name")+" not found")
			return
		}
		jsonOut(w, 200, p)
	}))
	mux.HandleFunc("GET /api/v1/namespaces/ns1/pods/{name}/exec", authed(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		_, ok := f.pods[r.PathValue("name")]
		f.lastArgv, f.lastQ = r.URL.Query()["command"], r.URL.Query()
		f.mu.Unlock()
		if !ok {
			status(w, 404, "NotFound", "pods "+r.PathValue("name")+" not found")
			return
		}
		if r.Header.Get("Accept") == "application/json" {
			status(w, 406, "NotAcceptable", "only */* upgrades")
			return
		}
		ws, proto := upgrade(t, w, r, f.protos...)
		defer ws.conn.Close()
		stdin := ""
		if r.URL.Query().Get("stdin") == "true" {
			var err error
			if stdin, err = ws.stdin(); err != nil {
				return // the client gave up (it refuses stdin over v4)
			}
		}
		_ = proto
		f.mu.Lock()
		out, errOut, st := f.exec(r.URL.Query()["command"], stdin)
		f.mu.Unlock()
		ws.send(chStdout, nil) // channel-open frames carry nothing
		if out != "" {
			ws.send(chStdout, []byte(out))
		}
		if errOut != "" {
			ws.send(chStderr, []byte(errOut))
		}
		ws.status(st)
	}))
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// reconcile plays the controllers: a Sandbox gets a running pod and turns
// Ready for its current generation (or Suspended), and a claim adopts a
// warm Sandbox. Images named "missing" fail to pull.
func (f *fakeKube) reconcile(key string, obj map[string]any) {
	plural, name, _ := strings.Cut(key, "/")
	md := obj["metadata"].(map[string]any)
	gen := md["generation"]
	spec, _ := obj["spec"].(map[string]any)
	switch plural {
	case "sandboxclaims":
		warm := "warm-abcde"
		f.pods[warm] = runningPod()
		obj["status"] = map[string]any{
			"conditions": []any{map[string]any{"type": "Ready", "status": "True", "reason": "DependenciesReady", "observedGeneration": gen}},
			"sandbox":    map[string]any{"name": warm, "podIPs": []string{"10.0.0.9"}},
		}
	case "sandboxes":
		if spec["operatingMode"] == "Suspended" {
			delete(f.pods, name)
			obj["status"] = map[string]any{"conditions": []any{
				map[string]any{"type": "Suspended", "status": "True", "reason": "PodTerminated", "observedGeneration": gen},
				map[string]any{"type": "Ready", "status": "False", "reason": "SandboxSuspended", "observedGeneration": gen},
			}}
			return
		}
		ctr := spec["podTemplate"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
		if strings.Contains(ctr["image"].(string), "missing") {
			f.pods[name] = map[string]any{"status": map[string]any{"phase": "Pending", "containerStatuses": []any{map[string]any{
				"name": "sandbox", "ready": false, "state": map[string]any{"waiting": map[string]any{"reason": "ErrImagePull", "message": "failed to pull: docker.io/library/missing:1: not found"}}}}}}
			obj["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "False", "reason": "DependenciesNotReady", "observedGeneration": gen}}}
			return
		}
		if _, ok := f.pods[name]; !ok {
			// First read: the pod is being created. After a spec change, Ready
			// still reflects the previous generation, which must not count.
			f.pods[name] = runningPod()
			c := map[string]any{"type": "Ready", "status": "False", "reason": "DependenciesNotReady", "observedGeneration": gen}
			if g := gen.(int); g > 1 {
				c = map[string]any{"type": "Ready", "status": "True", "reason": "DependenciesReady", "observedGeneration": g - 1}
			}
			obj["status"] = map[string]any{"conditions": []any{c}}
			return
		}
		st := map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True", "reason": "DependenciesReady", "observedGeneration": gen}}, "podIPs": []string{"10.0.0.5"}}
		if spec["service"] == true {
			st["serviceFQDN"] = name + ".ns1.svc.cluster.local"
		}
		obj["status"] = st
	}
}

func runningPod() map[string]any {
	return map[string]any{"status": map[string]any{"phase": "Running", "containerStatuses": []any{map[string]any{"name": "sandbox", "ready": true}}}}
}

func (f *fakeKube) adapter(t *testing.T, cfg map[string]string) *Adapter {
	t.Helper()
	ca := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	full := map[string]string{"apiServer": f.srv.URL, "caFile": ca, "namespace": "ns1"}
	for k, v := range cfg {
		full[k] = v
	}
	o, err := FromConfig(full, api.TrustPrivate, token)
	if err != nil {
		t.Fatal(err)
	}
	o.ReadyTimeout = 5 * time.Second
	a, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// answer sets how exec replies.
func (f *fakeKube) answer(fn func(argv []string, stdin string) (string, string, map[string]any)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exec = fn
}

// last returns the last exec's command and query.
func (f *fakeKube) last() ([]string, map[string][]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastArgv, f.lastQ
}

func (f *fakeKube) object(key string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.objects[key]
}

func TestFromConfig(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	for cfg, want := range map[string]string{
		`{}`: "config.apiServer outside a Kubernetes cluster",
		`{"apiServer":"https://k","isolation":"vm"}`:                  "isolation must be microvm or container",
		`{"apiServer":"https://k","volumeSize":"ten"}`:                "Kubernetes quantity",
		`{"apiServer":"https://k","warmPool":"p","image":"x"}`:        "config.image does not apply with config.warmPool",
		`{"apiServer":"https://k","warmPool":"p","volumeSize":"1Gi"}`: "config.volumeSize does not apply",
	} {
		var m map[string]string
		_ = json.Unmarshal([]byte(cfg), &m)
		if _, err := FromConfig(m, api.TrustPrivate, ""); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", cfg, want, err)
		}
	}
	o, err := FromConfig(map[string]string{"apiServer": "https://k/", "volumeSize": "2Gi", "isolation": "microvm", "runtimeClassName": "kata"}, api.TrustPrivate, "")
	if err != nil || o.APIServer != "https://k" || o.Namespace != "default" || o.Image != "ubuntu:24.04" || o.VolumePath != "/workspace" || o.Container != "sandbox" {
		t.Fatalf("defaults: %+v %v", o, err)
	}
	for _, f := range []string{adapter.FeatureExec, adapter.FeatureExecStream, adapter.FeatureFiles, adapter.FeatureStopStart, adapter.FeaturePersistDisk, adapter.FeatureMicroVM} {
		if !slices.Contains(o.Features, f) {
			t.Errorf("features %v lack %s", o.Features, f)
		}
	}

	// In a cluster: the service account's token and CA, read as they rotate.
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "443")
	o, err = FromConfig(map[string]string{}, api.TrustPrivate, "")
	if err != nil || o.APIServer != "https://10.96.0.1:443" || o.CAFile != inClusterCA || o.TokenFile != inClusterToken {
		t.Fatalf("in-cluster: %+v %v", o, err)
	}

	// The operator must say who runs the cluster; there is no silent default.
	if _, err := config.Parse([]byte("pools:\n  - {name: k8s, adapter: agent-sandbox}\n")); err == nil || !strings.Contains(err.Error(), "set trust explicitly") {
		t.Fatalf("missing trust must be rejected: %v", err)
	}
	if cfg, err := config.Parse([]byte("pools:\n  - {name: k8s, adapter: agent-sandbox, trust: private}\n")); err != nil || cfg.Pools[0].Trust != api.TrustPrivate {
		t.Fatalf("explicit trust: %v", err)
	}
}

func TestCreate(t *testing.T) {
	f := newFakeKube(t)
	a := f.adapter(t, map[string]string{"image": "debian:12", "runtimeClassName": "gvisor", "volumeSize": "1Gi", "features": "exec,files,lifecycle.stop-start,ports.expose"})
	ctx := context.Background()
	req := adapter.CreateRequest{ID: "sbx_01ABC", Installation: "inst_1", Resources: api.Resources{CPU: 2, MemoryMiB: 1024, DiskGiB: 5},
		Env: map[string]string{"B": "2", "A": "1"}, TTLSeconds: 600, Labels: map[string]string{adapter.LabelOrg: "org_1"}}
	in, err := a.Create(ctx, req)
	if err != nil || in.Ref != "dly-sbx-01abc" || in.SandboxID != "sbx_01ABC" || in.State != adapter.StateRunning {
		t.Fatalf("create: %+v %v", in, err)
	}
	again, err := a.Create(ctx, req)
	if err != nil || again.Ref != in.Ref || f.creates != 1 {
		t.Fatalf("second create: %+v %v (%d creates)", again, err, f.creates)
	}

	obj := f.object("sandboxes/dly-sbx-01abc")
	md := obj["metadata"].(map[string]any)
	labels := md["labels"].(map[string]any)
	if labels[adapter.LabelSandboxID] != "sbx_01ABC" || labels[adapter.LabelInstallation] != "inst_1" || labels[adapter.LabelOrg] != "org_1" {
		t.Fatalf("labels %v", labels)
	}
	spec := obj["spec"].(map[string]any)
	if spec["shutdownPolicy"] != "Delete" || spec["service"] != true {
		t.Fatalf("spec %v", spec)
	}
	if at, err := time.Parse(time.RFC3339, spec["shutdownTime"].(string)); err != nil || time.Until(at) < 9*time.Minute || time.Until(at) > 11*time.Minute {
		t.Fatalf("shutdownTime %v %v", spec["shutdownTime"], err)
	}
	pod := spec["podTemplate"].(map[string]any)["spec"].(map[string]any)
	ctr := pod["containers"].([]any)[0].(map[string]any)
	b, _ := json.Marshal(map[string]any{"rc": pod["runtimeClassName"], "image": ctr["image"], "env": ctr["env"], "res": ctr["resources"], "mounts": ctr["volumeMounts"], "pvc": spec["volumeClaimTemplates"]})
	want := `{"env":[{"name":"A","value":"1"},{"name":"B","value":"2"}],"image":"debian:12",` +
		`"mounts":[{"mountPath":"/workspace","name":"workspace"}],` +
		`"pvc":[{"metadata":{"labels":{"differently.dev/installation":"inst_1","differently.dev/org":"org_1","differently.dev/sandbox-id":"sbx_01ABC"},"name":"workspace"},"spec":{"accessModes":["ReadWriteOnce"],"resources":{"requests":{"storage":"1Gi"}}}}],` +
		`"rc":"gvisor","res":{"limits":{"cpu":"2","ephemeral-storage":"5Gi","memory":"1024Mi"},"requests":{"cpu":"2","ephemeral-storage":"5Gi","memory":"1024Mi"}}}`
	if string(b) != want {
		t.Fatalf("pod spec\n got %s\nwant %s", b, want)
	}

	u, err := a.ExposePort(ctx, in.Ref, 8080)
	if err != nil || u != "http://dly-sbx-01abc.ns1.svc.cluster.local:8080" {
		t.Fatalf("port %q %v", u, err)
	}
	future := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	if err := a.SetExpiry(ctx, in.Ref, future); err != nil || f.object("sandboxes/dly-sbx-01abc")["spec"].(map[string]any)["shutdownTime"] != future.Format(time.RFC3339) {
		t.Fatalf("set expiry: %v", err)
	}
}

func TestCreateOfAnImageThatDoesNotExist(t *testing.T) {
	f := newFakeKube(t)
	a := f.adapter(t, nil)
	_, err := a.Create(context.Background(), adapter.CreateRequest{ID: "sbx_bad", Installation: "inst_1", Template: "docker.io/library/missing:1"})
	if !api.IsCode(err, api.CodeInvalidRequest) || !strings.Contains(err.Error(), "ErrImagePull") {
		t.Fatalf("want invalid_request, got %v", err)
	}
	if f.object("sandboxes/dly-sbx-bad") != nil {
		t.Fatal("the Sandbox that cannot start was left behind")
	}
}

func TestListPagesAndFailsWhole(t *testing.T) {
	f := newFakeKube(t)
	a := f.adapter(t, nil)
	ctx := context.Background()
	for _, id := range []string{"sbx_a", "sbx_b", "sbx_c"} {
		if _, err := a.Create(ctx, adapter.CreateRequest{ID: id, Installation: "inst_1"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_other", Installation: "inst_2"}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.pageSize = 2
	f.objects["sandboxes/dly-sbx-b"]["metadata"].(map[string]any)["deletionTimestamp"] = "2026-10-28T00:00:00Z"
	f.mu.Unlock()
	list, err := a.List(ctx, "inst_1")
	var ids []string
	for _, i := range list {
		ids = append(ids, i.SandboxID)
	}
	if err != nil || !slices.Equal(ids, []string{"sbx_a", "sbx_c"}) {
		t.Fatalf("list %v %v", ids, err)
	}
	f.mu.Lock()
	f.failList = true
	f.mu.Unlock()
	if _, err := a.List(ctx, "inst_1"); !api.IsCode(err, api.CodeProviderUnavailable) {
		t.Fatalf("a failed page must fail the list, got %v", err)
	}
	if _, err := a.Get(ctx, "dly-sbx-b"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("a Sandbox being deleted is gone: %v", err)
	}
	if err := a.Delete(ctx, "dly-sbx-a"); err != nil {
		t.Fatal(err)
	}
	if err := a.Delete(ctx, "dly-sbx-a"); err != nil {
		t.Fatalf("delete is idempotent: %v", err)
	}
}

func TestExec(t *testing.T) {
	f := newFakeKube(t)
	a := f.adapter(t, nil)
	ctx := context.Background()
	in, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_x", Installation: "inst_1"})
	if err != nil {
		t.Fatal(err)
	}
	var gotStdin string
	f.answer(func(argv []string, stdin string) (string, string, map[string]any) {
		gotStdin = stdin
		return "hello\n", "warn\n", exitStatus("7")
	})
	res, err := a.Exec(ctx, in.Ref, api.ExecRequest{Command: "echo hi", Env: map[string]string{"Z": "1", "A": "x y"}, Cwd: "/w d", TimeoutSeconds: 30, Stdin: "data"})
	argv, q := f.last()
	f.mu.Lock()
	stdin := gotStdin
	f.mu.Unlock()
	if err != nil || res.ExitCode != 7 || res.Stdout != "hello\n" || res.Stderr != "warn\n" || stdin != "data" {
		t.Fatalf("exec %+v %v stdin=%q", res, err, stdin)
	}
	want := []string{"timeout", "-k", "5", "30", "env", "A=x y", "Z=1", "sh", "-c", "cd '/w d' && echo hi"}
	if !slices.Equal(argv, want) || q["container"][0] != "sandbox" || q["stdin"][0] != "true" {
		t.Fatalf("argv %q query %v", argv, q)
	}

	// Arguments run without a shell unless cwd needs one.
	_, err = a.Exec(ctx, in.Ref, api.ExecRequest{Args: []string{"ls", "-l"}})
	if argv, q = f.last(); err != nil || !slices.Equal(argv, []string{"ls", "-l"}) || q["stdin"] != nil {
		t.Fatalf("args %q %v", argv, err)
	}
	_, err = a.Exec(ctx, in.Ref, api.ExecRequest{Args: []string{"ls"}, Cwd: "/tmp"})
	if argv, _ = f.last(); err != nil || !slices.Equal(argv, []string{"sh", "-c", `cd /tmp && exec "$@"`, "sh", "ls"}) {
		t.Fatalf("args with cwd %q %v", argv, err)
	}

	// The runtime failing to start the command is a result, as in a shell.
	f.answer(func([]string, string) (string, string, map[string]any) {
		return "", "", map[string]any{"status": "Failure", "reason": "InternalError", "message": `error executing command in container: exec: "nope": executable file not found in $PATH`}
	})
	if res, err := a.Exec(ctx, in.Ref, api.ExecRequest{Args: []string{"nope"}}); err != nil || res.ExitCode != 127 {
		t.Fatalf("missing binary: %+v %v", res, err)
	}

	// Streaming delivers each frame as it comes.
	f.answer(func([]string, string) (string, string, map[string]any) {
		return "a\n", "b\n", map[string]any{"status": "Success"}
	})
	var events []api.ExecEvent
	code, err := a.ExecStream(ctx, in.Ref, api.ExecRequest{Command: "x"}, func(ev api.ExecEvent) error { events = append(events, ev); return nil })
	if err != nil || code != 0 || len(events) != 2 || events[0] != (api.ExecEvent{Stream: "stdout", Data: "a\n"}) || events[1].Stream != "stderr" {
		t.Fatalf("stream %d %+v %v", code, events, err)
	}

	// Without the v5 protocol stdin cannot be closed, so it is refused.
	f.mu.Lock()
	f.protos = []string{"v4.channel.k8s.io"}
	f.mu.Unlock()
	if _, err := a.Exec(ctx, in.Ref, api.ExecRequest{Command: "cat", Stdin: "x"}); !api.IsCode(err, api.CodeUnsupportedOperation) {
		t.Fatalf("stdin over v4: want unsupported_operation, got %v", err)
	}
	if res, err := a.Exec(ctx, in.Ref, api.ExecRequest{Command: "true"}); err != nil || res.ExitCode != 0 {
		t.Fatalf("v4 without stdin: %+v %v", res, err)
	}

	if _, err := a.Exec(ctx, "dly-nope", api.ExecRequest{Command: "true"}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("exec in a missing sandbox: %v", err)
	}
}

func TestFiles(t *testing.T) {
	f := newFakeKube(t)
	a := f.adapter(t, nil)
	ctx := context.Background()
	in, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_f", Installation: "inst_1"})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	f.answer(func(argv []string, stdin string) (string, string, map[string]any) {
		script, path := argv[2], argv[4]
		switch {
		case strings.Contains(script, "cat >"):
			files[path] = stdin
			return "", "", map[string]any{"status": "Success"}
		case strings.Contains(script, "exec cat"):
			if body, ok := files[path]; ok {
				return body, "", map[string]any{"status": "Success"}
			}
			return "", "no such file: " + path + "\n", exitStatus("2")
		case strings.Contains(script, "stat -c"):
			if strings.TrimRight(path, "/") != "/w" {
				return "", "no such directory\n", exitStatus("2")
			}
			return "5|644|1761609600|regular file|a b.txt\n4096|755|1761609600|directory|sub\n", "", map[string]any{"status": "Success"}
		}
		return "", fmt.Sprintf("unexpected %q", argv), exitStatus("1")
	})
	if err := a.WriteFile(ctx, in.Ref, "/w/a b.txt", []byte("hello")); err != nil {
		t.Fatalf("write %v", err)
	}
	if b, err := a.ReadFile(ctx, in.Ref, "/w/a b.txt"); err != nil || string(b) != "hello" {
		t.Fatalf("read %q %v", b, err)
	}
	if _, err := a.ReadFile(ctx, in.Ref, "/nope"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("read missing: %v", err)
	}
	list, err := a.ListFiles(ctx, in.Ref, "/w/")
	if err != nil || len(list) != 2 || list[0].Path != "/w/a b.txt" || list[0].Size != 5 || list[0].IsDir || list[0].Mode != "644" ||
		!list[1].IsDir || list[1].ModTime.Unix() != 1761609600 {
		t.Fatalf("list %+v %v", list, err)
	}
	if _, err := a.ListFiles(ctx, in.Ref, "/nope"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("list missing: %v", err)
	}
}

func TestStopStart(t *testing.T) {
	f := newFakeKube(t)
	ctx := context.Background()
	if err := f.adapter(t, nil).Stop(ctx, "x"); !api.IsCode(err, api.CodeUnsupportedOperation) {
		t.Fatalf("stop without a volume: %v", err)
	}
	a := f.adapter(t, map[string]string{"volumeSize": "1Gi"})
	in, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_s", Installation: "inst_1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Stop(ctx, in.Ref); err != nil {
		t.Fatal(err)
	}
	if got, err := a.Get(ctx, in.Ref); err != nil || got.State != adapter.StateStopped {
		t.Fatalf("after stop %+v %v", got, err)
	}
	if _, err := a.Exec(ctx, in.Ref, api.ExecRequest{Command: "true"}); !api.IsCode(err, api.CodeInvalidState) {
		t.Fatalf("exec while stopped: %v", err)
	}
	if err := a.Start(ctx, in.Ref); err != nil {
		t.Fatal(err)
	}
	// Start waited past the Ready left over from before the change.
	obj := f.object("sandboxes/" + in.Ref)
	f.mu.Lock()
	gen := obj["metadata"].(map[string]any)["generation"]
	observed := obj["status"].(map[string]any)["conditions"].([]any)[0].(map[string]any)["observedGeneration"]
	f.mu.Unlock()
	if gen != observed {
		t.Fatalf("start returned on a Ready for generation %v, the Sandbox is at %v", observed, gen)
	}
	if got, err := a.Get(ctx, in.Ref); err != nil || got.State != adapter.StateRunning {
		t.Fatalf("after start %+v %v", got, err)
	}
}

func TestWarmPoolClaims(t *testing.T) {
	f := newFakeKube(t)
	a := f.adapter(t, map[string]string{"warmPool": "py"})
	ctx := context.Background()
	if err := a.Health(ctx); !api.IsCode(err, api.CodeProviderUnavailable) || !strings.Contains(err.Error(), "SandboxWarmPool py not found") {
		t.Fatalf("health without the warm pool: %v", err)
	}
	f.mu.Lock()
	f.objects["sandboxwarmpools/py"] = map[string]any{"metadata": map[string]any{"name": "py"}}
	f.mu.Unlock()
	if err := a.Health(ctx); err != nil {
		t.Fatal(err)
	}
	caps, _ := a.Capabilities(ctx)
	if !slices.Equal(caps.Templates, []string{"py"}) || slices.Contains(caps.Features, adapter.FeatureStopStart) {
		t.Fatalf("capabilities %+v", caps)
	}
	in, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_w", Installation: "inst_1", TTLSeconds: 60})
	if err != nil || in.Ref != "dly-sbx-w" || in.State != adapter.StateRunning {
		t.Fatalf("create %+v %v", in, err)
	}
	claim := f.object("sandboxclaims/dly-sbx-w")
	spec := claim["spec"].(map[string]any)
	if spec["warmPoolRef"].(map[string]any)["name"] != "py" || spec["env"] != nil || spec["lifecycle"].(map[string]any)["shutdownPolicy"] != "Delete" {
		t.Fatalf("claim spec %v", spec)
	}
	_, err = a.Exec(ctx, in.Ref, api.ExecRequest{Command: "true"})
	if _, q := f.last(); err != nil || q["container"] != nil {
		t.Fatalf("exec reaches the adopted pod without naming a container: %v %v", q, err)
	}
	if list, err := a.List(ctx, "inst_1"); err != nil || len(list) != 1 || list[0].SandboxID != "sbx_w" {
		t.Fatalf("list claims %+v %v", list, err)
	}
	if u, err := a.ExposePort(ctx, in.Ref, 3000); err != nil || u != "http://10.0.0.9:3000" {
		t.Fatalf("port %q %v", u, err)
	}
	if err := a.Delete(ctx, in.Ref); err != nil || f.object("sandboxclaims/dly-sbx-w") != nil {
		t.Fatalf("delete %v", err)
	}
}

func TestCredentialsAreNotEchoed(t *testing.T) {
	f := newFakeKube(t)
	a := f.adapter(t, nil)
	a.k.token = "wrong-token-abcdef"
	err := a.Health(context.Background())
	if !api.IsCode(err, api.CodeProviderUnavailable) || strings.Contains(err.Error(), "wrong-token") {
		t.Fatalf("want provider_unavailable without the token, got %v", err)
	}
}

func TestTokenFileIsReread(t *testing.T) {
	f := newFakeKube(t)
	a := f.adapter(t, nil)
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.k.token, a.k.tokenFile = "", file
	if err := a.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("rotated"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.k.cachedAt = time.Now().Add(-2 * time.Minute) // past the cache
	if err := a.Health(context.Background()); !api.IsCode(err, api.CodeProviderUnavailable) {
		t.Fatalf("the rotated token was not read: %v", err)
	}
}
