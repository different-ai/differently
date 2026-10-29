package boat

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// fakeBoat mimics the documented /api/v1 shapes.
type fakeBoat struct {
	mu        sync.Mutex
	next      int
	creates   []map[string]any
	boxes     map[string]map[string]any
	files     map[string][]byte
	folders   []string
	saved     map[string]string
	commands  []map[string]any
	patches   []map[string]any
	deletes   []string
	failNext  string // state the next created sandbox settles in
	srv       *httptest.Server
	listPages int
}

func boatError(w http.ResponseWriter, status int, code string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"ok": false, "type": "sandbox.error", "status": status, "code": code, "message": code,
		"error": map[string]any{"code": code, "message": code, "status": status}})
}

func newFake(t *testing.T) *fakeBoat {
	f := &fakeBoat{boxes: map[string]map[string]any{}, files: map[string][]byte{}, saved: map[string]string{}}
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer boat_test" {
				boatError(w, 401, "unauthorized")
				return
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			h(w, r)
		}
	}
	box := func(w http.ResponseWriter, r *http.Request) map[string]any {
		b, ok := f.boxes[r.PathValue("id")]
		if !ok {
			boatError(w, 404, "not_found")
		}
		return b
	}
	mux.HandleFunc("POST /api/v1/sandboxes", auth(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["from"] == "missing" {
			boatError(w, 404, "snapshot_not_found")
			return
		}
		f.creates = append(f.creates, body)
		f.next++
		id := fmt.Sprintf("bx_%08d", f.next)
		settle := "ready"
		if f.failNext != "" {
			settle, f.failNext = f.failNext, ""
		}
		b := map[string]any{"id": id, "name": "Sandbox", "state": "provisioning", "labels": body["labels"], "settle": settle}
		f.boxes[id] = b
		w.WriteHeader(202)
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "type": "sandbox.created", "status": "provisioning", "sandbox": b})
	}))
	mux.HandleFunc("GET /api/v1/sandboxes", auth(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != liveStates {
			t.Errorf("list must ask for every state, got %q", q.Get("state"))
		}
		var items []any
		for _, b := range f.boxes {
			labels, _ := b["labels"].(map[string]any)
			match := true
			for _, l := range q["label"] {
				k, v, _ := strings.Cut(l, "=")
				if labels[k] != v {
					match = false
				}
			}
			if match {
				items = append(items, b)
			}
		}
		// Serve one sandbox per page to exercise the cursor.
		page, start := map[string]any{"hasMore": false, "nextCursor": nil, "limit": 200}, 0
		if c := q.Get("cursor"); c != "" {
			fmt.Sscan(c, &start)
		}
		if q.Get("limit") == "1" {
			start = 0
		}
		var out []any
		if start < len(items) {
			out = items[start : start+1]
			if start+1 < len(items) {
				page["hasMore"], page["nextCursor"] = true, fmt.Sprint(start+1)
			}
		}
		f.listPages++
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "type": "sandbox.list", "sandboxes": out, "pageInfo": page})
	}))
	mux.HandleFunc("GET /api/v1/sandboxes/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		b := box(w, r)
		if b == nil {
			return
		}
		switch b["state"] {
		case "provisioning", "provisioned":
			b["state"] = b["settle"]
		case "archiving":
			b["state"] = "archived"
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "type": "sandbox.info", "sandbox": b})
	}))
	mux.HandleFunc("PATCH /api/v1/sandboxes/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		if b := box(w, r); b != nil {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.patches = append(f.patches, body)
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "sandbox": b})
		}
	}))
	mux.HandleFunc("DELETE /api/v1/sandboxes/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Ascii-Confirm-Delete") != r.PathValue("id") {
			boatError(w, 409, "confirmation_required")
			return
		}
		if box(w, r) == nil {
			return
		}
		delete(f.boxes, r.PathValue("id"))
		f.deletes = append(f.deletes, r.PathValue("id"))
		w.WriteHeader(202)
		w.Write([]byte(`{"ok":true,"type":"sandbox.deleting","operation":{"id":"bdop_1","status":"pending"}}`))
	}))
	mux.HandleFunc("POST /api/v1/sandboxes/{id}/stop", auth(func(w http.ResponseWriter, r *http.Request) {
		if b := box(w, r); b != nil {
			b["state"] = "archiving"
			w.WriteHeader(202)
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "type": "sandbox.stopping", "status": "archiving", "sandbox": b})
		}
	}))
	mux.HandleFunc("POST /api/v1/sandboxes/{id}/resume", auth(func(w http.ResponseWriter, r *http.Request) {
		if b := box(w, r); b != nil {
			b["state"], b["settle"] = "provisioning", "ready"
			w.WriteHeader(202)
			w.Write([]byte(`{"ok":true,"type":"sandbox.resuming","status":"resuming"}`))
		}
	}))
	mux.HandleFunc("POST /api/v1/sandboxes/{id}/commands", auth(func(w http.ResponseWriter, r *http.Request) {
		if box(w, r) == nil {
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.commands = append(f.commands, body)
		cmd := body["command"].(string)
		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := json.NewEncoder(w)
		enc.Encode(map[string]any{"type": "started", "commandId": "cmd_1"})
		switch {
		case strings.Contains(cmd, "sleep"):
			enc.Encode(map[string]any{"type": "exit", "exitCode": nil, "timedOut": true})
		case strings.Contains(cmd, "restoring"):
			enc.Encode(map[string]any{"type": "error", "error": "sandbox_restoring", "retryable": true})
		default:
			enc.Encode(map[string]any{"type": "stdout", "data": "out "})
			enc.Encode(map[string]any{"type": "stderr", "data": "err"})
			enc.Encode(map[string]any{"type": "stdout", "data": "more"})
			enc.Encode(map[string]any{"type": "exit", "exitCode": 3, "success": false})
		}
	}))
	mux.HandleFunc("POST /api/v1/sandboxes/{id}/files/folder", auth(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.folders = append(f.folders, body["path"])
		w.Write([]byte(`{"ok":true,"type":"folder.created"}`))
	}))
	mux.HandleFunc("PUT /api/v1/sandboxes/{id}/files", auth(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !strings.HasPrefix(body["path"], "/home/user") && !strings.HasPrefix(body["path"], "/tmp") {
			boatError(w, 400, "invalid_path")
			return
		}
		b, err := base64.StdEncoding.DecodeString(body["content"])
		if err != nil || body["encoding"] != "base64" {
			boatError(w, 400, "invalid_files")
			return
		}
		f.files[body["path"]] = b
		w.Write([]byte(`{"ok":true,"type":"file.written","success":true}`))
	}))
	mux.HandleFunc("GET /api/v1/sandboxes/{id}/files", auth(func(w http.ResponseWriter, r *http.Request) {
		b, ok := f.files[r.URL.Query().Get("path")]
		if !ok {
			boatError(w, 404, "not_found")
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "type": "file.read", "encoding": r.URL.Query().Get("encoding"), "content": base64.StdEncoding.EncodeToString(b)})
	}))
	mux.HandleFunc("GET /api/v1/sandboxes/{id}/files/list", auth(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "type": "file.list", "path": "/home/user", "truncated": false, "files": []any{
			map[string]any{"name": "src", "path": "/home/user/src", "isDir": true, "isSymlink": false, "size": 0, "mode": "0755", "modifiedAt": "2026-10-28T10:00:00Z"},
			map[string]any{"name": "a.txt", "path": "/home/user/a.txt", "isDir": false, "isSymlink": false, "size": 5, "mode": "0644", "modifiedAt": "2026-10-28T10:00:00Z"},
		}})
	}))
	mux.HandleFunc("POST /api/v1/saved-snapshots", auth(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.saved[body["name"]] = "saving"
		w.WriteHeader(202)
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "status": "saving", "snapshot": map[string]any{"name": body["name"], "status": "saving", "sourceSandboxId": body["sandboxId"]}})
	}))
	mux.HandleFunc("GET /api/v1/saved-snapshots/{name}", auth(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := f.saved[r.PathValue("name")]; !ok {
			boatError(w, 404, "not_found")
			return
		}
		f.saved[r.PathValue("name")] = "ready"
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "snapshot": map[string]any{"name": r.PathValue("name"), "status": "ready", "sizeBytes": 42}})
	}))
	mux.HandleFunc("DELETE /api/v1/saved-snapshots/{name}", auth(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := f.saved[r.PathValue("name")]; !ok {
			boatError(w, 404, "not_found")
			return
		}
		delete(f.saved, r.PathValue("name"))
		w.Write([]byte(`{"ok":true}`))
	}))
	mux.HandleFunc("POST /api/v1/sandboxes/{id}/host", auth(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		u := fmt.Sprintf("https://%s-%v.on.boat.dev", r.PathValue("id"), body["port"])
		if body["public"] != true {
			u += "?_token=tok"
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "type": "port.hosted", "url": u})
	}))
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func newAdapter(f *fakeBoat, cfg map[string]string) *Adapter {
	if cfg == nil {
		cfg = map[string]string{}
	}
	cfg["apiURL"] = f.srv.URL + "/api/v1"
	a := New(FromConfig(cfg, "boat_test"))
	a.opts.ReadyTimeout = 5 * time.Second
	return a
}

func TestBoatLifecycle(t *testing.T) {
	f := newFake(t)
	a := newAdapter(f, map[string]string{"snapshot": "golden"})
	ctx := context.Background()
	req := adapter.CreateRequest{ID: "sbx_1", Installation: "inst_1", Resources: api.Resources{CPU: 3, MemoryMiB: 4096, DiskGiB: 20},
		Env: map[string]string{"TOKEN": "s"}, Labels: map[string]string{adapter.LabelOrg: "org_1", "bad key!": "x"}}
	inst, err := a.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != adapter.StateRunning || inst.SandboxID != "sbx_1" {
		t.Fatalf("instance %+v", inst)
	}
	body := f.creates[0]
	labels := body["labels"].(map[string]any)
	if body["type"] != "default" || body["from"] != "golden" || body["ttlSeconds"] != nil || labels[adapter.LabelInstallation] != "inst_1" || labels[adapter.LabelOrg] != "org_1" || labels["bad key!"] != nil {
		t.Fatalf("create body %v", body)
	}
	if again, err := a.Create(ctx, req); err != nil || again.Ref != inst.Ref || len(f.creates) != 1 {
		t.Fatalf("replayed create made %d sandboxes (%v)", len(f.creates), err)
	}

	res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Args: []string{"echo", "hi there"}, Cwd: "/home/user/src", Env: map[string]string{"B": "2", "A": "it's"}})
	if err != nil || res.ExitCode != 3 || res.Stdout != "out more" || res.Stderr != "err" {
		t.Fatalf("exec %+v %v", res, err)
	}
	if cmd := f.commands[0]["command"]; cmd != `export A='it'\''s' B=2; cd /home/user/src && echo 'hi there'` {
		t.Fatalf("command %q", cmd)
	}
	if f.commands[0]["timeoutSeconds"] != float64(600) || f.commands[0]["stream"] != true {
		t.Fatalf("exec body %v", f.commands[0])
	}
	if res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "sleep 99", TimeoutSeconds: 5}); err != nil || res.ExitCode != 124 || f.commands[1]["timeoutSeconds"] != float64(5) {
		t.Fatalf("timed-out exec %+v %v", res, err)
	}
	if _, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "cat", Stdin: "x"}); !api.IsCode(err, api.CodeUnsupportedOperation) {
		t.Fatalf("stdin should be unsupported: %v", err)
	}
	if _, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "true", Env: map[string]string{"A;rm": "x"}}); !api.IsCode(err, api.CodeInvalidRequest) {
		t.Fatalf("bad env name must be refused: %v", err)
	}
	if _, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "restoring"}); !api.IsCode(err, api.CodeInvalidState) {
		t.Fatalf("retryable stream error: %v", err)
	}
	var events []api.ExecEvent
	code, err := a.ExecStream(ctx, inst.Ref, api.ExecRequest{Command: "make"}, func(e api.ExecEvent) error { events = append(events, e); return nil })
	if err != nil || code != 3 || len(events) != 3 || events[1].Stream != "stderr" {
		t.Fatalf("stream %v %d %v", events, code, err)
	}

	if err := a.WriteFile(ctx, inst.Ref, "/home/user/src/a.txt", []byte("hello\x00")); err != nil {
		t.Fatal(err)
	}
	if len(f.folders) != 1 || f.folders[0] != "/home/user/src" {
		t.Fatalf("parent folder not created: %v", f.folders)
	}
	if b, err := a.ReadFile(ctx, inst.Ref, "/home/user/src/a.txt"); err != nil || string(b) != "hello\x00" {
		t.Fatalf("read %q %v", b, err)
	}
	if err := a.WriteFile(ctx, inst.Ref, "/etc/x", []byte("v")); !api.IsCode(err, api.CodeInvalidRequest) {
		t.Fatalf("paths outside /home/user and /tmp: %v", err)
	}
	if err := a.WriteFile(ctx, inst.Ref, "/tmp/big", make([]byte, maxFileBytes+1)); !api.IsCode(err, api.CodeInvalidRequest) {
		t.Fatalf("oversized write: %v", err)
	}
	if _, err := a.ReadFile(ctx, inst.Ref, "/home/user/nope"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("missing file: %v", err)
	}
	ls, err := a.ListFiles(ctx, inst.Ref, "/home/user")
	if err != nil || len(ls) != 2 || !ls[0].IsDir || ls[1].Size != 5 || ls[1].Mode != "0644" || ls[1].ModTime.IsZero() {
		t.Fatalf("list files %+v %v", ls, err)
	}

	if err := a.Stop(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.Get(ctx, inst.Ref); got.State != adapter.StateStopped {
		t.Fatalf("after stop %+v", got)
	}
	if list, err := a.List(ctx, "inst_1"); err != nil || len(list) != 1 || list[0].State != adapter.StateStopped {
		t.Fatalf("a stopped sandbox must stay listed: %v %v", list, err)
	}
	if err := a.Start(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	if err := a.SetExpiry(ctx, inst.Ref, time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ttl := f.patches[0]["ttlSeconds"].(float64); ttl < 7190 || ttl > 7200 {
		t.Fatalf("ttl %v", ttl)
	}

	snap, err := a.Snapshot(ctx, inst.Ref, "disk")
	if err != nil || snap.Kind != "disk" || snap.SizeBytes != 42 || !strings.HasPrefix(snap.Ref, "dly-00000001-") {
		t.Fatalf("snapshot %+v %v", snap, err)
	}
	if _, err := a.Snapshot(ctx, inst.Ref, "memory"); !api.IsCode(err, api.CodeUnsupportedOperation) {
		t.Fatalf("memory snapshots: %v", err)
	}
	restored, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_2", Installation: "inst_1", FromSnapshot: snap.Ref, TTLSeconds: 600})
	if err != nil || f.creates[1]["from"] != snap.Ref || f.creates[1]["ttlSeconds"] != float64(600) {
		t.Fatalf("restore %v %v", f.creates[1], err)
	}
	if err := a.DeleteSnapshot(ctx, snap.Ref); err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteSnapshot(ctx, snap.Ref); err != nil {
		t.Fatalf("idempotent snapshot delete: %v", err)
	}

	if u, err := a.ExposePort(ctx, inst.Ref, 3000); err != nil || !strings.Contains(u, "-3000.on.boat.dev?_token=") {
		t.Fatalf("port %q %v", u, err)
	}

	list, err := a.List(ctx, "inst_1")
	if err != nil || len(list) != 2 {
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
	if caps.Has(adapter.FeaturePauseResume) || caps.Has(adapter.FeatureSnapshotMem) || !caps.Has(adapter.FeatureExecStream) {
		t.Fatalf("features %v", caps.Features)
	}
	if got := adapter.EffectiveFeatures(a, caps.Features); len(got) != len(caps.Features) {
		t.Fatalf("declared features not implemented: %v vs %v", got, caps.Features)
	}
}

func TestBoatCreateFailures(t *testing.T) {
	f := newFake(t)
	a := newAdapter(f, nil)
	ctx := context.Background()

	if _, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_big", Installation: "i", Resources: api.Resources{CPU: 32}}); !api.IsCode(err, api.CodeInvalidRequest) {
		t.Fatalf("oversized: %v", err)
	}
	if _, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_tpl", Installation: "i", Template: "missing"}); !api.IsCode(err, api.CodeInvalidRequest) {
		t.Fatalf("unknown snapshot must be invalid_request: %v", err)
	}

	f.failNext = "error"
	if _, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_err", Installation: "i"}); !api.IsCode(err, adapter.CodeSandboxFailed) {
		t.Fatalf("a failed sandbox must be sandbox_failed: %v", err)
	}
	// The retry clears the failed sandbox and starts a fresh one.
	inst, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_err", Installation: "i"})
	if err != nil || inst.State != adapter.StateRunning || len(f.deletes) != 1 || len(f.creates) != 2 {
		t.Fatalf("retry after failure %+v %v (deletes %v)", inst, err, f.deletes)
	}

	f.failNext = "cancelled"
	if _, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_cx", Installation: "i"}); !api.IsCode(err, adapter.CodeInsufficientCapacity) {
		t.Fatalf("cancelled must requeue as insufficient_capacity: %v", err)
	}

	small := newAdapter(f, map[string]string{"type": "large"})
	if _, err := small.Create(ctx, adapter.CreateRequest{ID: "sbx_floor", Installation: "i", Resources: api.Resources{CPU: 1}}); err != nil || f.creates[len(f.creates)-1]["type"] != "large" {
		t.Fatalf("type floor: %v %v", f.creates[len(f.creates)-1], err)
	}
	if _, err := newAdapter(f, map[string]string{"type": "huge"}).Create(ctx, adapter.CreateRequest{ID: "sbx_t", Installation: "i"}); !api.IsCode(err, api.CodeInvalidRequest) {
		t.Fatalf("unknown type: %v", err)
	}
	if _, err := newAdapter(f, map[string]string{"autoStopSeconds": "7200"}).Create(ctx, adapter.CreateRequest{ID: "sbx_auto", Installation: "i"}); err != nil || f.creates[len(f.creates)-1]["ttlSeconds"] != float64(7200) {
		t.Fatalf("autoStopSeconds: %v %v", f.creates[len(f.creates)-1], err)
	}
}

func TestBoatErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		code   string
		want   string
	}{
		{429, "rate_limited", adapter.CodeInsufficientCapacity},
		{429, "limit_reached", adapter.CodeInsufficientCapacity},
		{503, "out_of_capacity", adapter.CodeInsufficientCapacity},
		{503, "no_ready_machine", adapter.CodeInsufficientCapacity},
		{502, "sandbox_direct_failed", adapter.CodeSandboxFailed},
		{400, "unknown_environment", api.CodeInvalidRequest},
		{403, "machine_class_plan_required", api.CodeInvalidRequest},
		{401, "unauthorized", api.CodeProviderUnavailable},
		{402, "billing_required", api.CodeProviderUnavailable},
		{500, "http_500", api.CodeProviderUnavailable},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				json.NewEncoder(w).Encode(map[string]any{"ok": true, "sandboxes": []any{}, "pageInfo": map[string]any{"hasMore": false}})
				return
			}
			boatError(w, c.status, c.code)
		}))
		a := New(Options{APIURL: srv.URL, APIKey: "boat_secret"})
		_, err := a.Create(context.Background(), adapter.CreateRequest{ID: "sbx_x", Installation: "i"})
		if !api.IsCode(err, c.want) {
			t.Errorf("%d %s: got %v, want %s", c.status, c.code, err, c.want)
		}
		if err != nil && strings.Contains(err.Error(), "boat_secret") {
			t.Errorf("key leaked in %v", err)
		}
		srv.Close()
	}
	// An unreachable API is an outage.
	a := New(Options{APIURL: "http://127.0.0.1:1", APIKey: "k"})
	if err := a.Health(context.Background()); !api.IsCode(err, api.CodeProviderUnavailable) {
		t.Fatalf("unreachable: %v", err)
	}
}
