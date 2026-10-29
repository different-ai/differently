package opensandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/different-ai/differently/internal/config"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// fakeOpenSandbox mimics the lifecycle API and execd from the published
// specs: async create (202 then Pending -> Running), execd reached through
// GET /sandboxes/{id}/endpoints/44772 with a per-sandbox header, SSE
// command output, multipart uploads, pause/resume and snapshots.
type fakeOpenSandbox struct {
	t       *testing.T
	mu      sync.Mutex
	srv     *httptest.Server
	creates int
	boxes   map[string]map[string]any
	files   map[string][]byte
	lastCmd map[string]any
	snaps   map[string]string
}

const apiKey = "osb-key-1234567890"

func newFake(t *testing.T) *fakeOpenSandbox {
	f := &fakeOpenSandbox{t: t, boxes: map[string]map[string]any{}, files: map[string][]byte{}, snaps: map[string]string{}}
	mux := http.NewServeMux()
	jsonOut := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	lifecycle := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get(APIKeyHeader) != apiKey {
				jsonOut(w, 401, map[string]string{"code": "UNAUTHORIZED", "message": "missing api key"})
				return
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			h(w, r)
		}
	}
	box := func(w http.ResponseWriter, id string) map[string]any {
		b, ok := f.boxes[id]
		if !ok {
			jsonOut(w, 404, map[string]string{"code": "SANDBOX_NOT_FOUND", "message": "no sandbox " + id})
		}
		return b
	}
	setState := func(b map[string]any, s string) { b["status"] = map[string]any{"state": s} }
	stateOf := func(b map[string]any) string { return b["status"].(map[string]any)["state"].(string) }

	mux.HandleFunc("POST /v1/sandboxes", lifecycle(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if img, ok := body["image"].(map[string]any); ok && img["uri"] != "" && body["entrypoint"] == nil {
			jsonOut(w, 400, map[string]string{"code": "INVALID", "message": "entrypoint is required with image"})
			return
		}
		f.creates++
		id := fmt.Sprintf("osb-%d", f.creates)
		b := map[string]any{"id": id, "metadata": body["metadata"], "request": body}
		setState(b, "Pending")
		f.boxes[id] = b
		jsonOut(w, 202, b)
	}))
	mux.HandleFunc("GET /v1/sandboxes", lifecycle(func(w http.ResponseWriter, r *http.Request) {
		// metadata=<url-encoded k=v>
		k, v, _ := strings.Cut(r.URL.Query().Get("metadata"), "=")
		items := []any{}
		for _, b := range f.boxes {
			md, _ := b["metadata"].(map[string]any)
			if k == "" || md[k] == v {
				items = append(items, b)
			}
		}
		jsonOut(w, 200, map[string]any{"items": items, "pagination": map[string]any{"page": 1, "pageSize": 100, "totalItems": len(items), "totalPages": 1, "hasNextPage": false}})
	}))
	mux.HandleFunc("GET /v1/sandboxes/{id}", lifecycle(func(w http.ResponseWriter, r *http.Request) {
		b := box(w, r.PathValue("id"))
		if b == nil {
			return
		}
		switch stateOf(b) { // transitions complete on the next read
		case "Pending", "Resuming":
			setState(b, "Running")
		case "Pausing":
			setState(b, "Paused")
		}
		jsonOut(w, 200, b)
	}))
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", lifecycle(func(w http.ResponseWriter, r *http.Request) {
		if box(w, r.PathValue("id")) == nil {
			return
		}
		delete(f.boxes, r.PathValue("id"))
		w.WriteHeader(204)
	}))
	mux.HandleFunc("POST /v1/sandboxes/{id}/{action}", lifecycle(func(w http.ResponseWriter, r *http.Request) {
		b := box(w, r.PathValue("id"))
		if b == nil {
			return
		}
		switch r.PathValue("action") {
		case "pause":
			setState(b, "Pausing")
			w.WriteHeader(202)
		case "resume":
			setState(b, "Resuming")
			w.WriteHeader(202)
		case "renew-expiration":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			b["expiresAt"] = body["expiresAt"]
			jsonOut(w, 200, map[string]any{"expiresAt": body["expiresAt"]})
		case "snapshots":
			sid := "snap-" + r.PathValue("id")
			f.snaps[sid] = "Creating"
			jsonOut(w, 202, map[string]any{"id": sid, "sandboxId": r.PathValue("id"), "status": map[string]string{"state": "Creating"}})
		}
	}))
	mux.HandleFunc("GET /v1/snapshots/{id}", lifecycle(func(w http.ResponseWriter, r *http.Request) {
		f.snaps[r.PathValue("id")] = "Ready"
		jsonOut(w, 200, map[string]any{"id": r.PathValue("id"), "status": map[string]string{"state": "Ready"}})
	}))
	mux.HandleFunc("GET /v1/sandboxes/{id}/endpoints/{port}", lifecycle(func(w http.ResponseWriter, r *http.Request) {
		id, port := r.PathValue("id"), r.PathValue("port")
		host := strings.TrimPrefix(f.srv.URL, "http://")
		if port == "44772" {
			// Scheme-less, as the spec documents, plus a per-sandbox token.
			jsonOut(w, 200, map[string]any{"endpoint": host + "/execd/" + id, "headers": map[string]string{"X-EXECD-ACCESS-TOKEN": "tok-" + id}})
			return
		}
		if port == "9000" {
			jsonOut(w, 200, map[string]any{"endpoint": host + "/p/" + id + "/9000", "headers": map[string]string{"X-Needs": "secret"}})
			return
		}
		jsonOut(w, 200, map[string]any{"endpoint": host + "/p/" + id + "/" + port})
	}))
	execd := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("id")
			if r.Header.Get("X-EXECD-ACCESS-TOKEN") != "tok-"+id || r.Header.Get(APIKeyHeader) != apiKey {
				jsonOut(w, 401, map[string]string{"code": "UNAUTHORIZED", "message": "bad execd token"})
				return
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if b, ok := f.boxes[id]; !ok || stateOf(b) != "Running" {
				jsonOut(w, 409, map[string]string{"code": "NOT_RUNNING", "message": "sandbox is not running"})
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /execd/{id}/ping", execd(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	mux.HandleFunc("POST /execd/{id}/command", execd(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.lastCmd = body
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(v any) { b, _ := json.Marshal(v); fmt.Fprintf(w, "data: %s\n\n", b) }
		send(map[string]any{"type": "init", "text": "cmd-1"})
		if cmd, _ := body["command"].(string); strings.Contains(cmd, "exact") {
			// execd release-1.2.0: terminators kept, a final fragment has none.
			for _, l := range []string{"a\n", "\n", "c"} {
				send(map[string]any{"type": "stdout", "text": l})
			}
			send(map[string]any{"type": "stderr", "text": "no newline"})
			send(map[string]any{"type": "execution_complete", "execution_time": 1})
			return
		}
		if cmd, _ := body["command"].(string); strings.Contains(cmd, "two lines") {
			// execd 1.1: one event per line, terminators stripped, bare JSON lines.
			for _, l := range []string{"a", "b"} {
				b, _ := json.Marshal(map[string]any{"type": "stdout", "text": l})
				fmt.Fprintf(w, "%s\n\n", b)
			}
			send(map[string]any{"type": "execution_complete", "execution_time": 1})
			return
		}
		send(map[string]any{"type": "stdout", "text": "hello\n"})
		send(map[string]any{"type": "stderr", "text": "warn\n"})
		if cmd, _ := body["command"].(string); strings.Contains(cmd, "exit 3") {
			send(map[string]any{"type": "error", "error": map[string]any{"ename": "CommandExecError", "evalue": "3", "exit_code": 3}})
			return
		}
		send(map[string]any{"type": "execution_complete", "exit_code": 0, "execution_time": 12})
	}))
	mux.HandleFunc("POST /execd/{id}/files/upload", execd(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			jsonOut(w, 400, map[string]string{"code": "BAD", "message": err.Error()})
			return
		}
		var meta struct {
			Path string `json:"path"`
		}
		// Like execd: metadata must be a JSON file part, not a form field.
		mf, _, err := r.FormFile("metadata")
		if err != nil {
			jsonOut(w, 400, map[string]string{"code": "INVALID_FILE_METADATA", "message": "metadata file is missing"})
			return
		}
		_ = json.NewDecoder(mf).Decode(&meta)
		file, _, err := r.FormFile("file")
		if err != nil {
			jsonOut(w, 400, map[string]string{"code": "BAD", "message": "no file part"})
			return
		}
		data, _ := io.ReadAll(file)
		f.files[meta.Path] = data
		w.WriteHeader(200)
	}))
	mux.HandleFunc("GET /execd/{id}/files/download", execd(func(w http.ResponseWriter, r *http.Request) {
		data, ok := f.files[r.URL.Query().Get("path")]
		if !ok {
			jsonOut(w, 404, map[string]string{"code": "FILE_NOT_FOUND", "message": "missing"})
			return
		}
		w.Write(data)
	}))
	mux.HandleFunc("GET /execd/{id}/directories/list", execd(func(w http.ResponseWriter, r *http.Request) {
		dir := r.URL.Query().Get("path")
		items := []any{map[string]any{"path": dir, "type": "directory", "size": 0, "mode": 755}}
		for p, d := range f.files {
			if strings.HasPrefix(p, dir+"/") {
				items = append(items, map[string]any{"path": p, "type": "file", "size": len(d), "mode": 644, "modified_at": "2026-10-28T12:00:00Z"})
			}
		}
		jsonOut(w, 200, items)
	}))
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func newAdapter(t *testing.T, f *fakeOpenSandbox, cfg map[string]string) *Adapter {
	t.Helper()
	full := map[string]string{"apiURL": f.srv.URL + "/v1", "image": "python:3.12-slim", "isolation": "container"}
	for k, v := range cfg {
		full[k] = v
	}
	o, err := FromConfig(full, api.TrustPrivate, apiKey)
	if err != nil {
		t.Fatal(err)
	}
	return New(o)
}

func TestOpenSandboxLifecycleExecFiles(t *testing.T) {
	f := newFake(t)
	a := newAdapter(t, f, nil)
	ctx := context.Background()
	if err := a.Health(ctx); err != nil {
		t.Fatal(err)
	}
	req := adapter.CreateRequest{ID: "sbx_1", Installation: "inst_1", Resources: api.Resources{CPU: 2, MemoryMiB: 1024}, Env: map[string]string{"A": "1"}, TTLSeconds: 30}
	inst, err := a.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != adapter.StateRunning || inst.SandboxID != "sbx_1" {
		t.Fatalf("instance %+v", inst)
	}
	sent := f.boxes[inst.Ref]["request"].(map[string]any)
	limits := sent["resourceLimits"].(map[string]any)
	if limits["cpu"] != "2" || limits["memory"] != "1024Mi" || sent["timeout"].(float64) != 60 || sent["image"].(map[string]any)["uri"] != "python:3.12-slim" {
		t.Fatalf("create body %v", sent)
	}
	if again, err := a.Create(ctx, req); err != nil || again.Ref != inst.Ref || f.creates != 1 {
		t.Fatalf("create not idempotent: %+v %v (%d creates)", again, err, f.creates)
	}
	deadline := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := a.SetExpiry(ctx, inst.Ref, deadline); err != nil || f.boxes[inst.Ref]["expiresAt"] != "2030-01-02T03:04:05Z" {
		t.Fatalf("renew-expiration: %v %v", f.boxes[inst.Ref]["expiresAt"], err)
	}

	res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "echo hello", Env: map[string]string{"B": "2"}, Cwd: "/work", TimeoutSeconds: 5})
	if err != nil || res.ExitCode != 0 || res.Stdout != "hello\n" || res.Stderr != "warn\n" {
		t.Fatalf("exec %+v %v", res, err)
	}
	if f.lastCmd["timeout"].(float64) != 5000 || f.lastCmd["cwd"] != "/work" || f.lastCmd["envs"].(map[string]any)["B"] != "2" {
		t.Fatalf("command body %v", f.lastCmd)
	}
	if res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "printf 'two lines'"}); err != nil || res.Stdout != "a\nb\n" {
		t.Fatalf("line endings from execd 1.1 not restored: %q %v", res.Stdout, err)
	}
	exact := New(func() Options { o := a.opts; o.LineEndingsKept = true; return o }())
	if res, err := exact.Exec(ctx, inst.Ref, api.ExecRequest{Command: "printf exact"}); err != nil || res.Stdout != "a\n\nc" || res.Stderr != "no newline" {
		t.Fatalf("lineEndings: kept must pass output through exactly: %+v %v", res, err)
	}
	if res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "false; exit 3"}); err != nil || res.ExitCode != 3 {
		t.Fatalf("non-zero exit: %+v %v", res, err)
	}
	if _, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Args: []string{"ls", "-l", "my file"}}); err != nil || f.lastCmd["command"] != "ls -l 'my file'" {
		t.Fatalf("argv mode: %v %v", f.lastCmd, err)
	}
	// stdin is staged in a file the command reads, then removed.
	if _, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Args: []string{"cat"}, Stdin: "x"}); err != nil || !strings.HasPrefix(fmt.Sprint(f.lastCmd["command"]), "(cat) < /tmp/.differently-stdin-") || !strings.Contains(fmt.Sprint(f.lastCmd["command"]), "; rm -f ") {
		t.Fatalf("stdin: %v %v", f.lastCmd, err)
	}
	var streamed []string
	code, err := a.ExecStream(ctx, inst.Ref, api.ExecRequest{Command: "echo hello"}, func(ev api.ExecEvent) error {
		streamed = append(streamed, ev.Stream)
		return nil
	})
	if err != nil || code != 0 || strings.Join(streamed, ",") != "stdout,stderr" {
		t.Fatalf("stream %v %d %v", streamed, code, err)
	}

	if err := a.WriteFile(ctx, inst.Ref, "/work/a.txt", []byte("data")); err != nil {
		t.Fatal(err)
	}
	if b, err := a.ReadFile(ctx, inst.Ref, "/work/a.txt"); err != nil || string(b) != "data" {
		t.Fatalf("read %q %v", b, err)
	}
	if _, err := a.ReadFile(ctx, inst.Ref, "/missing"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("missing file: %v", err)
	}
	items, err := a.ListFiles(ctx, inst.Ref, "/work")
	if err != nil || len(items) != 1 || items[0].Name != "a.txt" || items[0].Size != 4 {
		t.Fatalf("list %+v %v", items, err)
	}

	if err := a.Pause(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.Get(ctx, inst.Ref); got.State != adapter.StatePaused {
		t.Fatalf("after pause: %+v", got)
	}
	if err := a.Resume(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	snap, err := a.Snapshot(ctx, inst.Ref, "disk")
	if err != nil || snap.Ref == "" {
		t.Fatalf("snapshot %+v %v", snap, err)
	}
	if _, err := a.Snapshot(ctx, inst.Ref, "memory"); !api.IsCode(err, api.CodeUnsupportedOperation) {
		t.Fatalf("memory snapshot should need the feature: %v", err)
	}
	child, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_2", Installation: "inst_1", FromSnapshot: snap.Ref})
	if err != nil || f.boxes[child.Ref]["request"].(map[string]any)["snapshotId"] != snap.Ref {
		t.Fatalf("restore %+v %v", child, err)
	}

	if _, err := a.ExposePort(ctx, inst.Ref, 9000); !api.IsCode(err, api.CodeUnsupportedOperation) {
		t.Fatalf("header-guarded endpoints must not be handed out: %v", err)
	}
	if u, err := a.ExposePort(ctx, inst.Ref, 8080); err != nil || !strings.HasSuffix(u, "/p/"+inst.Ref+"/8080") {
		t.Fatalf("expose %q %v", u, err)
	}

	list, err := a.List(ctx, "inst_1")
	if err != nil || len(list) != 2 {
		t.Fatalf("list by installation: %v %v", list, err)
	}
	if err := a.Delete(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	if err := a.Delete(ctx, inst.Ref); err != nil {
		t.Fatalf("second delete must succeed: %v", err)
	}
	if _, err := a.Get(ctx, inst.Ref); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("deleted sandbox: %v", err)
	}
}

func TestOpenSandboxCredentialsAndConfig(t *testing.T) {
	f := newFake(t)
	o, _ := FromConfig(map[string]string{"apiURL": f.srv.URL + "/v1"}, api.TrustPrivate, "wrong-key")
	err := New(o).Health(context.Background())
	if !api.IsCode(err, api.CodeProviderUnavailable) || strings.Contains(err.Error(), "wrong-key") {
		t.Fatalf("rejected key should be provider_unavailable without echoing it: %v", err)
	}
	if _, err := FromConfig(map[string]string{}, api.TrustPrivate, ""); err == nil {
		t.Fatal("apiURL must be required")
	}
	if _, err := FromConfig(map[string]string{"apiURL": "http://x/v1", "lineEndings": "lf"}, api.TrustPrivate, ""); err == nil {
		t.Fatal("unknown lineEndings accepted")
	}
	if _, err := FromConfig(map[string]string{"apiURL": "http://x/v1", "isolation": "vm"}, api.TrustPrivate, ""); err == nil {
		t.Fatal("unknown isolation accepted")
	}
	o, _ = FromConfig(map[string]string{"apiURL": "http://x/v1", "features": "exec,files,snapshots.memory", "isolation": "microvm"}, api.TrustPrivate, "")
	caps, _ := New(o).Capabilities(context.Background())
	got := adapter.EffectiveFeatures(New(o), caps.Features)
	if strings.Join(got, ",") != "exec,files,isolation.microvm,snapshots.memory" {
		t.Fatalf("features %v", got)
	}
	// The operator must say who runs OpenSandbox; there is no silent default.
	_, err = config.Parse([]byte("pools:\n  - {name: osb, adapter: opensandbox, config: {apiURL: 'http://osb:8090/v1'}}\n"))
	if err == nil || !strings.Contains(err.Error(), "set trust explicitly") {
		t.Fatalf("missing trust must be rejected: %v", err)
	}
	cfg, err := config.Parse([]byte("pools:\n  - {name: osb, adapter: opensandbox, trust: private, config: {apiURL: 'http://osb:8090/v1'}}\n"))
	if err != nil || cfg.Pools[0].Trust != api.TrustPrivate {
		t.Fatalf("explicit trust: %v", err)
	}
}
