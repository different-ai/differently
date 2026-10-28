package freestyle

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

func TestFreestyleAdapter(t *testing.T) {
	var mu sync.Mutex
	vms := map[string]map[string]any{}
	files := map[string]string{}
	var resized map[string]int
	var execBody map[string]any
	mux := http.NewServeMux()
	notFound := func(w http.ResponseWriter) {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"code": "NOT_FOUND", "message": "no vm"})
	}
	lookup := func(key string) map[string]any {
		for _, v := range vms {
			if v["id"] == key || v["slug"] == key {
				return v
			}
		}
		return nil
	}
	mux.HandleFunc("POST /v5/vms", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, ok := body["firewall"]; !ok {
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		v := map[string]any{"id": "vm-1", "slug": body["slug"], "state": "starting", "metadata": body["metadata"], "resources": map[string]int{"cpu": 2, "memory": 4096, "storage": 16384}}
		vms["vm-1"] = v
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(v)
	})
	mux.HandleFunc("GET /v5/vms/{id}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		v := lookup(r.PathValue("id"))
		if v == nil {
			notFound(w)
			return
		}
		if v["state"] == "starting" {
			v["state"] = "running"
		}
		json.NewEncoder(w).Encode(v)
	})
	mux.HandleFunc("GET /v5/vms", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		list := []any{}
		for _, v := range vms {
			if strings.HasSuffix(r.URL.Query().Get("metadata"), v["metadata"].(map[string]any)[metaInstallation].(string)) {
				list = append(list, v)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"vms": list, "totalCount": len(list)})
	})
	mux.HandleFunc("POST /v5/vms/{id}/resize", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&resized)
		mu.Lock()
		defer mu.Unlock()
		json.NewEncoder(w).Encode(lookup(r.PathValue("id")))
	})
	mux.HandleFunc("POST /v5/vms/{id}/exec-await", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&execBody)
		json.NewEncoder(w).Encode(map[string]any{"statusCode": 2, "stdout": "out", "stderr": "err"})
	})
	mux.HandleFunc("POST /v5/vms/{id}/fs/mkdir", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	mux.HandleFunc("PUT /v5/vms/{id}/fs/write", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		files[r.URL.Query().Get("path")] = string(b)
		mu.Unlock()
		w.WriteHeader(204)
	})
	mux.HandleFunc("DELETE /v5/vms/{id}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if lookup(r.PathValue("id")) == nil {
			notFound(w)
			return
		}
		delete(vms, r.PathValue("id"))
		w.WriteHeader(204)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := New(Options{APIURL: srv.URL, APIKey: "k"})
	ctx := context.Background()
	req := adapter.CreateRequest{ID: "sbx_ABC", Installation: "inst_1", Resources: api.Resources{CPU: 4, MemoryMiB: 2048, DiskGiB: 10}, Env: map[string]string{"TOKEN": "it's secret"}}
	inst, err := a.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != adapter.StateRunning || inst.SandboxID != "sbx_ABC" {
		t.Fatalf("instance %+v", inst)
	}
	if resized["cpu"] != 4 || resized["memory"] != 0 || resized["storage"] != 0 {
		t.Fatalf("resize should only grow cpu: %v", resized)
	}
	if got := files[envFile]; got != "TOKEN='it'\\''s secret'\n" {
		t.Fatalf("env file %q", got)
	}
	if again, err := a.Create(ctx, req); err != nil || again.Ref != inst.Ref || len(vms) != 1 {
		t.Fatalf("create not idempotent: %v %v", again, err)
	}
	res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "make", Cwd: "/src"})
	if err != nil || res.ExitCode != 2 || res.Stderr != "err" {
		t.Fatalf("exec %+v %v", res, err)
	}
	if cmd := execBody["command"].(string); !strings.Contains(cmd, ". "+envFile) || !strings.HasSuffix(cmd, "cd /src && make") {
		t.Fatalf("command %q", cmd)
	}
	if list, err := a.List(ctx, "inst_1"); err != nil || len(list) != 1 {
		t.Fatalf("list %v %v", list, err)
	}
	if err := a.Delete(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	if err := a.Delete(ctx, inst.Ref); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
}
