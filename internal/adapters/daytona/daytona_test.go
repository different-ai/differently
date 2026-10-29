package daytona

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// fakeDaytona mimics the control-plane and toolbox shapes, and what the live
// API was seen to do (2026-10-29): stop, start and delete answer while the
// change is still in progress, a repeated delete during it is a 409, a
// command timeout is a 408, and snapshots are deleted by id only.
type fakeDaytona struct {
	mu        sync.Mutex
	creates   int
	boxes     map[string]map[string]any
	files     map[string][]byte
	snapshots map[string]map[string]any // by name
	srv       *httptest.Server
}

// settle is what one GET later finds for a state in progress.
var settle = map[string]string{"creating": "started", "starting": "started", "stopping": "stopped", "snapshotting": "started"}

func newFake(t *testing.T) *fakeDaytona {
	f := &fakeDaytona{boxes: map[string]map[string]any{}, files: map[string][]byte{}, snapshots: map[string]map[string]any{}}
	mux := http.NewServeMux()
	fail := func(w http.ResponseWriter, status int, msg string) {
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]any{"statusCode": status, "message": msg})
	}
	mux.HandleFunc("POST /api/sandbox/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		b, ok := f.boxes[r.PathValue("id")]
		if !ok {
			fail(w, 404, "Sandbox not found")
			return
		}
		switch r.PathValue("action") {
		case "stop":
			b["state"] = "stopping"
		case "start":
			b["state"] = "starting"
		case "pause":
			fail(w, 422, "Pausing is not supported for this sandbox")
			return
		case "snapshot":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			name := body["name"].(string)
			f.snapshots[name] = map[string]any{"id": "0d6c7c6e-1f0a-4c55-9e43-" + strings.Repeat("a", 12), "name": name, "state": "pending"}
			b["state"] = "snapshotting"
		}
		json.NewEncoder(w).Encode(b)
	})
	mux.HandleFunc("GET /api/snapshots/{ref}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		s, ok := f.snapshots[r.PathValue("ref")]
		if !ok {
			fail(w, 404, "Snapshot not found")
			return
		}
		json.NewEncoder(w).Encode(s)
		s["state"] = "active"
	})
	mux.HandleFunc("DELETE /api/snapshots/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		for name, s := range f.snapshots {
			if s["id"] == r.PathValue("id") {
				delete(f.snapshots, name)
				return
			}
		}
		if !strings.Contains(r.PathValue("id"), "-4c55-") {
			fail(w, 400, "Validation failed (uuid is expected)")
			return
		}
		fail(w, 404, "Snapshot not found")
	})
	mux.HandleFunc("POST /api/sandbox", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.creates++
		id := "dt-" + body["name"].(string)
		box := map[string]any{"id": id, "state": "creating", "labels": body["labels"], "target": "us", "toolboxProxyUrl": f.srv.URL + "/toolbox"}
		f.boxes[id] = box
		f.mu.Unlock()
		json.NewEncoder(w).Encode(box)
	})
	mux.HandleFunc("GET /api/sandbox", func(w http.ResponseWriter, r *http.Request) {
		var want map[string]string
		_ = json.Unmarshal([]byte(r.URL.Query().Get("labels")), &want)
		f.mu.Lock()
		defer f.mu.Unlock()
		items := []any{}
		for _, b := range f.boxes {
			labels, _ := b["labels"].(map[string]any)
			match := true
			for k, v := range want {
				if labels[k] != v {
					match = false
				}
			}
			if match {
				items = append(items, b)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"items": items, "nextCursor": nil})
	})
	mux.HandleFunc("GET /api/sandbox/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		b, ok := f.boxes[r.PathValue("id")]
		if !ok {
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(map[string]any{"statusCode": 404, "error": "Not Found", "message": "Sandbox not found"})
			return
		}
		// A change in progress is over by the first poll; a destroyed
		// sandbox is still readable once, as "destroying".
		if next, ok := settle[b["state"].(string)]; ok {
			b["state"] = next
		}
		json.NewEncoder(w).Encode(b)
		if b["state"] == "destroying" {
			delete(f.boxes, r.PathValue("id"))
		}
	})
	mux.HandleFunc("DELETE /api/sandbox/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		b, ok := f.boxes[r.PathValue("id")]
		if !ok {
			fail(w, 404, "Sandbox not found")
			return
		}
		if b["state"] == "destroying" || b["state"] == "stopping" {
			fail(w, 409, "Sandbox state change in progress")
			return
		}
		b["state"] = "destroying"
		json.NewEncoder(w).Encode(b)
	})
	mux.HandleFunc("POST /toolbox/{id}/process/execute", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["timeout"] == nil {
			t.Error("timeout must always be sent")
		}
		if strings.HasPrefix(body["command"].(string), "sleep") {
			w.WriteHeader(408)
			json.NewEncoder(w).Encode(map[string]any{"statusCode": 408, "message": "command execution timeout", "code": "PROCESS_EXECUTION_TIMEOUT"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"exitCode": 0, "result": "ran: " + body["command"].(string)})
	})
	mux.HandleFunc("POST /toolbox/{id}/files/upload-v2", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.files[r.URL.Query().Get("path")] = b
		f.mu.Unlock()
		w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /toolbox/{id}/files/download", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Write(f.files[r.URL.Query().Get("path")])
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDaytona) state(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, _ := f.boxes[id]["state"].(string)
	return s
}

func TestDaytonaAdapter(t *testing.T) {
	f := newFake(t)
	a := New(Options{APIURL: f.srv.URL + "/api", APIKey: "k", Snapshot: "daytona-small"})
	ctx := context.Background()
	req := adapter.CreateRequest{ID: "sbx_1", Installation: "inst_1", Resources: api.Resources{CPU: 1, MemoryMiB: 1024}}
	inst, err := a.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != adapter.StateRunning || inst.SandboxID != "sbx_1" {
		t.Fatalf("instance %+v", inst)
	}
	if _, err := a.Create(ctx, req); err != nil || f.creates != 1 {
		t.Fatalf("replayed create made %d sandboxes (%v)", f.creates, err)
	}
	res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Args: []string{"echo", "hi there"}, Cwd: "/tmp"})
	if err != nil || !strings.Contains(res.Stdout, `echo 'hi there'`) {
		t.Fatalf("exec %+v %v", res, err)
	}
	if _, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "cat", Stdin: "x"}); !api.IsCode(err, api.CodeUnsupportedOperation) {
		t.Fatalf("stdin should be unsupported: %v", err)
	}
	if err := a.WriteFile(ctx, inst.Ref, "/home/a.txt", []byte("v")); err != nil {
		t.Fatal(err)
	}
	if b, err := a.ReadFile(ctx, inst.Ref, "/home/a.txt"); err != nil || string(b) != "v" {
		t.Fatalf("read %q %v", b, err)
	}
	if res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "sleep 30", TimeoutSeconds: 1}); err != nil || res.ExitCode != 124 {
		t.Fatalf("a command timeout is exit 124, not an error: %+v %v", res, err)
	}
	// Stop and start return once the sandbox has settled.
	if err := a.Stop(ctx, inst.Ref); err != nil || f.state(inst.Ref) != "stopped" {
		t.Fatalf("stop: %v, state %s", err, f.state(inst.Ref))
	}
	if err := a.Start(ctx, inst.Ref); err != nil || f.state(inst.Ref) != "started" {
		t.Fatalf("start: %v, state %s", err, f.state(inst.Ref))
	}
	if err := a.Pause(ctx, inst.Ref); !api.IsCode(err, api.CodeInvalidRequest) {
		t.Fatalf("a container cannot pause: %v", err)
	}
	snap, err := a.Snapshot(ctx, inst.Ref, "disk")
	if err != nil || snap.Kind != "disk" || f.snapshots[snap.Ref]["state"] != "active" {
		t.Fatalf("snapshot %+v %v (%v)", snap, err, f.snapshots)
	}
	for i := 0; i < 2; i++ {
		if err := a.DeleteSnapshot(ctx, snap.Ref); err != nil || len(f.snapshots) != 0 {
			t.Fatalf("delete snapshot #%d: %v", i+1, err)
		}
	}
	list, err := a.List(ctx, "inst_1")
	if err != nil || len(list) != 1 {
		t.Fatalf("list %v %v", list, err)
	}
	// A delete during a state change waits for it; a repeated delete while
	// the first is in progress succeeds.
	f.mu.Lock()
	f.boxes[inst.Ref]["state"] = "stopping"
	f.mu.Unlock()
	if err := a.Delete(ctx, inst.Ref); err != nil || f.state(inst.Ref) != "destroying" {
		t.Fatalf("delete while stopping: %v, state %s", err, f.state(inst.Ref))
	}
	if err := a.Delete(ctx, inst.Ref); err != nil {
		t.Fatalf("second delete must succeed: %v", err)
	}
	if err := a.Delete(ctx, inst.Ref); err != nil || f.state(inst.Ref) != "" {
		t.Fatalf("delete after it is gone: %v, state %s", err, f.state(inst.Ref))
	}
	caps, _ := a.Capabilities(ctx)
	if caps.Has(adapter.FeaturePauseResume) {
		t.Fatal("container class must not advertise pause")
	}
}

func TestDaytonaErrorMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "limit=1") {
			w.WriteHeader(401)
			return
		}
		w.WriteHeader(429)
		json.NewEncoder(w).Encode(map[string]any{"statusCode": 429, "message": "Rate limit exceeded"})
	}))
	defer srv.Close()
	a := New(Options{APIURL: srv.URL + "/api", APIKey: "k"})
	if err := a.Health(context.Background()); !api.IsCode(err, api.CodeProviderUnavailable) || strings.Contains(err.Error(), "k") && strings.Contains(err.Error(), "Bearer") {
		t.Fatalf("401 mapping: %v", err)
	}
	if _, err := a.Create(context.Background(), adapter.CreateRequest{ID: "sbx_2"}); !api.IsCode(err, adapter.CodeInsufficientCapacity) {
		t.Fatalf("429 should requeue as insufficient capacity: %v", err)
	}
}
