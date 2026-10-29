package morph

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// fakeMorph mimics the documented /api shapes. Exec runs the command through
// the local shell, as Morph runs it through the VM's, so the adapter's
// scripts for env, cwd and files are exercised for real.
type fakeMorph struct {
	mu        sync.Mutex
	next      int
	instances map[string]map[string]any
	snapshots map[string]map[string]any
	creates   []map[string]any
	baseSnaps int
	ttl       map[string]any
	sse       string // "spec" or "sdk"
	settle    string // status the next instance settles in
	srv       *httptest.Server
}

func detail(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"detail": msg})
}

func runShell(cmd string) (int, string, string) {
	c := exec.Command("/bin/sh", "-c", cmd)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	err := c.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	}
	return code, out.String(), errb.String()
}

func newFake(t *testing.T) *fakeMorph {
	f := &fakeMorph{instances: map[string]map[string]any{}, snapshots: map[string]map[string]any{}, sse: "sdk"}
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer morph_test" {
				w.Header().Set("Content-Type", "text/plain")
				w.WriteHeader(401)
				w.Write([]byte("Unauthorized"))
				return
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			h(w, r)
		}
	}
	instance := func(w http.ResponseWriter, r *http.Request) map[string]any {
		i, ok := f.instances[r.PathValue("id")]
		if !ok {
			detail(w, 404, "Instance not found")
		}
		return i
	}
	command := func(r *http.Request) string {
		var body struct {
			Command []string `json:"command"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Command) != 1 {
			t.Errorf("command must be one shell string, got %q", body.Command)
		}
		return strings.Join(body.Command, " ")
	}
	mux.HandleFunc("GET /api/snapshot", auth(func(w http.ResponseWriter, r *http.Request) {
		data := []any{}
		for _, s := range f.snapshots {
			if s["digest"] == r.URL.Query().Get("digest") {
				data = append(data, s)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	}))
	mux.HandleFunc("POST /api/snapshot", auth(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["image_id"] == "nope" {
			detail(w, 404, "Image not found")
			return
		}
		f.baseSnaps++
		id := fmt.Sprintf("snapshot_base%d", f.baseSnaps)
		f.snapshots[id] = map[string]any{"id": id, "object": "snapshot", "status": "pending", "digest": body["digest"],
			"spec": map[string]any{"vcpus": body["vcpus"], "memory": body["memory"], "disk_size": body["disk_size"]}}
		json.NewEncoder(w).Encode(f.snapshots[id])
	}))
	mux.HandleFunc("GET /api/snapshot/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		s, ok := f.snapshots[r.PathValue("id")]
		if !ok {
			detail(w, 404, "Snapshot not found")
			return
		}
		s["status"] = "ready"
		json.NewEncoder(w).Encode(s)
	}))
	mux.HandleFunc("DELETE /api/snapshot/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := f.snapshots[r.PathValue("id")]; !ok {
			detail(w, 404, "Snapshot not found")
			return
		}
		delete(f.snapshots, r.PathValue("id"))
		w.WriteHeader(204)
	}))
	mux.HandleFunc("POST /api/instance", auth(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, ok := f.snapshots[body["snapshot_id"].(string)]; !ok {
			detail(w, 404, "Snapshot not found")
			return
		}
		f.creates = append(f.creates, body)
		f.next++
		id := fmt.Sprintf("morphvm_%d", f.next)
		settle := "ready"
		if f.settle != "" {
			settle, f.settle = f.settle, ""
		}
		f.instances[id] = map[string]any{"id": id, "object": "instance", "status": "pending", "metadata": body["metadata"], "settle": settle,
			"refs": map[string]any{"snapshot_id": body["snapshot_id"]}, "networking": map[string]any{"http_services": []any{}}}
		json.NewEncoder(w).Encode(f.instances[id])
	}))
	mux.HandleFunc("GET /api/instance/list", auth(func(w http.ResponseWriter, r *http.Request) {
		var match []any
		for _, i := range f.instances {
			meta, _ := i["metadata"].(map[string]any)
			ok := true
			for k, v := range r.URL.Query() {
				if key, found := strings.CutPrefix(k, "metadata["); found && meta[strings.TrimSuffix(key, "]")] != v[0] {
					ok = false
				}
			}
			if ok {
				match = append(match, i)
			}
		}
		// One instance per page to exercise paging.
		var page int
		fmt.Sscan(r.URL.Query().Get("page"), &page)
		out := []any{}
		if page >= 1 && page <= len(match) {
			out = match[page-1 : page]
		}
		json.NewEncoder(w).Encode(map[string]any{"instances": out, "total": len(match), "page": page, "has_next": page < len(match)})
	}))
	mux.HandleFunc("GET /api/instance/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		if i := instance(w, r); i != nil {
			if i["status"] == "pending" {
				i["status"] = i["settle"]
			}
			json.NewEncoder(w).Encode(i)
		}
	}))
	mux.HandleFunc("DELETE /api/instance/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		if instance(w, r) != nil {
			delete(f.instances, r.PathValue("id"))
			w.WriteHeader(204)
		}
	}))
	mux.HandleFunc("POST /api/instance/{id}/exec", auth(func(w http.ResponseWriter, r *http.Request) {
		if i := instance(w, r); i != nil {
			if i["status"] == "paused" {
				detail(w, 412, "Instance is paused")
				return
			}
			code, out, errs := runShell(command(r))
			json.NewEncoder(w).Encode(map[string]any{"exit_code": code, "stdout": out, "stderr": errs})
		}
	}))
	mux.HandleFunc("POST /api/instance/{id}/exec/sse", auth(func(w http.ResponseWriter, r *http.Request) {
		if instance(w, r) == nil {
			return
		}
		code, out, errs := runShell(command(r))
		w.Header().Set("Content-Type", "text/event-stream")
		if f.sse == "spec" {
			for _, line := range strings.SplitAfter(out, "\n") {
				if line != "" {
					fmt.Fprintf(w, "event: stdout\ndata: %s\n\n", strings.ReplaceAll(line, "\n", "\ndata: "))
				}
			}
			fmt.Fprintf(w, "event: stderr\ndata: %s\n\nevent: exit\ndata: %d\n\n", errs, code)
			return
		}
		enc := func(typ, content string) {
			b, _ := json.Marshal(map[string]string{"type": typ, "content": content})
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		enc("stdout", out)
		enc("stderr", errs)
		enc("exit_code", fmt.Sprint(code))
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	mux.HandleFunc("POST /api/instance/{id}/pause", auth(func(w http.ResponseWriter, r *http.Request) {
		if i := instance(w, r); i != nil {
			i["status"] = "paused"
			json.NewEncoder(w).Encode(i)
		}
	}))
	mux.HandleFunc("POST /api/instance/{id}/resume", auth(func(w http.ResponseWriter, r *http.Request) {
		if i := instance(w, r); i != nil {
			i["status"], i["settle"] = "pending", "ready"
			json.NewEncoder(w).Encode(i)
		}
	}))
	mux.HandleFunc("POST /api/instance/{id}/ttl", auth(func(w http.ResponseWriter, r *http.Request) {
		if i := instance(w, r); i != nil {
			_ = json.NewDecoder(r.Body).Decode(&f.ttl)
			json.NewEncoder(w).Encode(i)
		}
	}))
	mux.HandleFunc("POST /api/instance/{id}/snapshot", auth(func(w http.ResponseWriter, r *http.Request) {
		if instance(w, r) == nil {
			return
		}
		id := fmt.Sprintf("snapshot_%s_%d", r.PathValue("id"), len(f.snapshots))
		f.snapshots[id] = map[string]any{"id": id, "status": "pending"}
		json.NewEncoder(w).Encode(f.snapshots[id])
	}))
	mux.HandleFunc("POST /api/instance/{id}/http", auth(func(w http.ResponseWriter, r *http.Request) {
		if i := instance(w, r); i != nil {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["auth_mode"] != "none" {
				t.Errorf("auth_mode %v", body["auth_mode"])
			}
			i["networking"] = map[string]any{"http_services": []any{map[string]any{"name": body["name"], "port": body["port"], "url": "https://" + body["name"].(string) + "-" + r.PathValue("id") + ".http.cloud.morph.so", "auth_mode": "none"}}}
			json.NewEncoder(w).Encode(i)
		}
	}))
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func newAdapter(f *fakeMorph, cfg map[string]string) *Adapter {
	if cfg == nil {
		cfg = map[string]string{}
	}
	cfg["apiURL"] = f.srv.URL + "/api"
	a := New(FromConfig(cfg, "morph_test"))
	a.opts.ReadyTimeout = 5 * time.Second
	return a
}

func has(tool ...string) bool { return exec.Command(tool[0], tool[1:]...).Run() == nil }

func TestMorphLifecycle(t *testing.T) {
	f := newFake(t)
	a := newAdapter(f, nil)
	ctx := context.Background()
	req := adapter.CreateRequest{ID: "sbx_1", Installation: "inst_1", Resources: api.Resources{CPU: 2, MemoryMiB: 4096, DiskGiB: 20}, TTLSeconds: 3600,
		Labels: map[string]string{adapter.LabelOrg: "org_1"}}
	inst, err := a.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != adapter.StateRunning || inst.SandboxID != "sbx_1" {
		t.Fatalf("instance %+v", inst)
	}
	c := f.creates[0]
	meta := c["metadata"].(map[string]any)
	if c["ttl_seconds"] != float64(3600) || c["ttl_action"] != "stop" || meta[metaInstallation] != "inst_1" || meta[metaOrg] != "org_1" {
		t.Fatalf("create body %v", c)
	}
	base := f.snapshots[c["snapshot_id"].(string)]
	if base["digest"] != "differently-morphvm-minimal-2c-4096m-20480d" {
		t.Fatalf("base snapshot %v", base)
	}
	if again, err := a.Create(ctx, req); err != nil || again.Ref != inst.Ref || len(f.creates) != 1 {
		t.Fatalf("replayed create made %d instances (%v)", len(f.creates), err)
	}
	if _, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_2", Installation: "inst_1", Resources: req.Resources}); err != nil || f.baseSnaps != 1 {
		t.Fatalf("same-size sandboxes must share the base snapshot: %d (%v)", f.baseSnaps, err)
	}

	dir := t.TempDir()
	res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: `echo "$A|$B"; pwd; echo oops >&2; exit 3`, Cwd: dir, Env: map[string]string{"A": "it's", "B": "x y"}})
	if err != nil || res.ExitCode != 3 || res.Stderr != "oops\n" || !strings.HasPrefix(res.Stdout, "it's|x y\n") || !strings.Contains(res.Stdout, filepath.Base(dir)) {
		t.Fatalf("exec %+v %v", res, err)
	}
	if res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Args: []string{"printf", "%s", "a b"}}); err != nil || res.Stdout != "a b" {
		t.Fatalf("args %+v %v", res, err)
	}
	if has("timeout", "1", "true") {
		if res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "sleep 5", TimeoutSeconds: 1}); err != nil || res.ExitCode != 124 {
			t.Fatalf("timeout %+v %v", res, err)
		}
	}
	if _, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "cat", Stdin: "x"}); !api.IsCode(err, api.CodeUnsupportedOperation) {
		t.Fatalf("stdin should be unsupported: %v", err)
	}
	if _, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "true", Env: map[string]string{"A;B": "x"}}); !api.IsCode(err, api.CodeInvalidRequest) {
		t.Fatalf("bad env name must be refused: %v", err)
	}
	for _, format := range []string{"sdk", "spec"} {
		f.sse = format
		var out, errs strings.Builder
		code, err := a.ExecStream(ctx, inst.Ref, api.ExecRequest{Command: "printf 'one\\ntwo\\n'; printf e >&2; exit 2"}, func(e api.ExecEvent) error {
			if e.Stream == "stderr" {
				errs.WriteString(e.Data)
			} else {
				out.WriteString(e.Data)
			}
			return nil
		})
		if err != nil || code != 2 || out.String() != "one\ntwo\n" || errs.String() != "e" {
			t.Fatalf("%s stream: %d %q %q %v", format, code, out.String(), errs.String(), err)
		}
	}

	data := make([]byte, 3*chunkBytes+17)
	rand.Read(data)
	path := filepath.Join(dir, "nested dir", "it's.bin")
	if err := a.WriteFile(ctx, inst.Ref, path, data); err != nil {
		t.Fatal(err)
	}
	if got, err := a.ReadFile(ctx, inst.Ref, path); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read back %d bytes (%v)", len(got), err)
	}
	if err := a.WriteFile(ctx, inst.Ref, path, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := a.ReadFile(ctx, inst.Ref, path); err != nil || len(got) != 0 {
		t.Fatalf("empty overwrite left %d bytes (%v)", len(got), err)
	}
	if _, err := a.ReadFile(ctx, inst.Ref, filepath.Join(dir, "missing")); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("missing file: %v", err)
	}
	if _, err := a.ReadFile(ctx, inst.Ref, dir); !api.IsCode(err, api.CodeInvalidRequest) {
		t.Fatalf("reading a directory: %v", err)
	}
	if _, err := a.ListFiles(ctx, inst.Ref, filepath.Join(dir, "missing")); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("listing a missing directory: %v", err)
	}
	if has("find", ".", "-maxdepth", "0", "-printf", "") { // GNU find, as in Morph's images
		ls, err := a.ListFiles(ctx, inst.Ref, filepath.Join(dir, "nested dir"))
		if err != nil || len(ls) != 1 || ls[0].Name != "it's.bin" || ls[0].IsDir || ls[0].ModTime.IsZero() || len(ls[0].Mode) != 4 {
			t.Fatalf("list %+v %v", ls, err)
		}
	}

	if err := a.Pause(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.Get(ctx, inst.Ref); got.State != adapter.StatePaused {
		t.Fatalf("after pause %+v", got)
	}
	if _, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "true"}); !api.IsCode(err, api.CodeInvalidState) {
		t.Fatalf("exec while paused (412): %v", err)
	}
	if err := a.Resume(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	if err := a.SetExpiry(ctx, inst.Ref, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if s := f.ttl["ttl_seconds"].(float64); s < 3590 || s > 3600 || f.ttl["ttl_action"] != "stop" {
		t.Fatalf("ttl %v", f.ttl)
	}

	snap, err := a.Snapshot(ctx, inst.Ref, "disk")
	if err != nil || snap.Kind != "memory" || f.snapshots[snap.Ref]["status"] != "ready" {
		t.Fatalf("snapshot %+v %v", snap, err)
	}
	restored, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_3", Installation: "inst_1", FromSnapshot: snap.Ref})
	if err != nil || f.creates[len(f.creates)-1]["snapshot_id"] != snap.Ref {
		t.Fatalf("restore %+v %v", restored, err)
	}
	if err := a.DeleteSnapshot(ctx, snap.Ref); err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteSnapshot(ctx, snap.Ref); err != nil {
		t.Fatalf("idempotent snapshot delete: %v", err)
	}
	if u, err := a.ExposePort(ctx, inst.Ref, 8080); err != nil || !strings.HasPrefix(u, "https://port-8080-") {
		t.Fatalf("port %q %v", u, err)
	}

	if list, err := a.List(ctx, "inst_1"); err != nil || len(list) != 3 {
		t.Fatalf("paged list %v %v", list, err)
	}
	if err := a.Delete(ctx, restored.Ref); err != nil {
		t.Fatal(err)
	}
	if err := a.Delete(ctx, restored.Ref); err != nil {
		t.Fatalf("second delete must succeed: %v", err)
	}
	if _, err := a.Get(ctx, restored.Ref); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("get deleted: %v", err)
	}
	caps, _ := a.Capabilities(ctx)
	if caps.Has(adapter.FeatureStopStart) || len(adapter.EffectiveFeatures(a, caps.Features)) != len(caps.Features) {
		t.Fatalf("features %v", caps.Features)
	}
}

func TestMorphCreateFailures(t *testing.T) {
	f := newFake(t)
	a := newAdapter(f, nil)
	ctx := context.Background()
	if _, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_t", Installation: "i", Template: "snapshot_missing"}); !api.IsCode(err, api.CodeInvalidRequest) {
		t.Fatalf("unknown snapshot must be invalid_request: %v", err)
	}
	if _, err := newAdapter(f, map[string]string{"image": "nope"}).Create(ctx, adapter.CreateRequest{ID: "sbx_i", Installation: "i"}); !api.IsCode(err, api.CodeInvalidRequest) {
		t.Fatalf("unknown image must be invalid_request: %v", err)
	}
	f.settle = "error"
	if _, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_e", Installation: "i"}); !api.IsCode(err, adapter.CodeSandboxFailed) {
		t.Fatalf("a failed instance must be sandbox_failed: %v", err)
	}
	inst, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_e", Installation: "i"})
	if err != nil || inst.State != adapter.StateRunning || len(f.instances) != 1 {
		t.Fatalf("retry must replace the failed instance: %+v %v (%d instances)", inst, err, len(f.instances))
	}
}

func TestMorphErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
		msg    string
	}{
		{422, `{"detail":[{"loc":["body","ttl_seconds"],"msg":"Input should be a valid integer","type":"int_parsing"}]}`, api.CodeInvalidRequest, "Input should be a valid integer"},
		{429, `{"detail":"Too many requests"}`, adapter.CodeInsufficientCapacity, "Too many requests"},
		{412, `{"detail":"Instance not ready","error_code":"not_ready"}`, api.CodeInvalidState, "not_ready"},
		{409, `{"detail":"pause in progress"}`, api.CodeInvalidState, "pause in progress"},
		{401, `Unauthorized`, api.CodeProviderUnavailable, "credentials were rejected"},
		{402, `{"detail":"Payment required"}`, api.CodeProviderUnavailable, "Payment required"},
		{502, `<html>bad gateway</html>`, api.CodeProviderUnavailable, "Bad Gateway"},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			w.Write([]byte(c.body))
		}))
		a := New(Options{APIURL: srv.URL, APIKey: "morph_secret"})
		err := a.Health(context.Background())
		if !api.IsCode(err, c.want) || !strings.Contains(err.Error(), c.msg) || strings.Contains(err.Error(), "morph_secret") {
			t.Errorf("%d: got %v, want %s with %q", c.status, err, c.want, c.msg)
		}
		srv.Close()
	}
}
