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

// fakeDaytona mimics the documented control-plane and toolbox shapes.
type fakeDaytona struct {
	mu      sync.Mutex
	creates int
	boxes   map[string]map[string]any
	files   map[string][]byte
	srv     *httptest.Server
}

func newFake(t *testing.T) *fakeDaytona {
	f := &fakeDaytona{boxes: map[string]map[string]any{}, files: map[string][]byte{}}
	mux := http.NewServeMux()
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
		b["state"] = "started" // becomes ready on first poll
		json.NewEncoder(w).Encode(b)
	})
	mux.HandleFunc("DELETE /api/sandbox/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.boxes[r.PathValue("id")]; !ok {
			w.WriteHeader(404)
			return
		}
		delete(f.boxes, r.PathValue("id"))
		w.Write([]byte(`{}`))
	})
	mux.HandleFunc("POST /toolbox/{id}/process/execute", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["timeout"] == nil {
			t.Error("timeout must always be sent")
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
	list, err := a.List(ctx, "inst_1")
	if err != nil || len(list) != 1 {
		t.Fatalf("list %v %v", list, err)
	}
	if err := a.Delete(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	if err := a.Delete(ctx, inst.Ref); err != nil {
		t.Fatalf("second delete must succeed: %v", err)
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
