package freestyle

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// file is a file in the fake guest. Like the live API (2026-10-29), a write
// creates a file owned by the guest's default user but replaces an existing
// one with a file owned by root, and reads mode as a decimal number.
type file struct {
	data, owner string
	mode        int
}

func TestFreestyleAdapter(t *testing.T) {
	var mu sync.Mutex
	vms := map[string]map[string]any{}
	files := map[string]file{}
	snapshots := map[string]bool{}
	var resized map[string]int
	var execBody, tlsBody map[string]any
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
		v := map[string]any{"id": "vm-1", "slug": body["slug"], "state": "starting", "metadata": body["metadata"], "resources": map[string]int{"cpu": 2, "memory": 4096, "storage": 16384},
			"automaticRestart": body["automaticRestart"] != false}
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
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		defer mu.Unlock()
		if body["command"] == "poweroff" {
			// Only root may power off; the guest drops the connection.
			if body["linuxUser"] != "root" {
				json.NewEncoder(w).Encode(map[string]any{"statusCode": 1, "stderr": "freestyle-vms: power-off request failed\n"})
				return
			}
			// With automatic restart on, Freestyle boots it again (live: about
			// 5 s later; here before anyone looks).
			if v := lookup(r.PathValue("id")); v["automaticRestart"] == true {
				v["state"] = "running"
			} else {
				v["state"] = "stopped"
			}
			w.WriteHeader(409)
			json.NewEncoder(w).Encode(map[string]string{"code": "VM_NON_RESPONSIVE", "message": "vm stopped responding"})
			return
		}
		execBody = body
		if strings.HasSuffix(body["command"].(string), "sleep 30") {
			json.NewEncoder(w).Encode(map[string]any{"stdout": "", "stderr": ""}) // killed: no statusCode
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"statusCode": 2, "stdout": "out", "stderr": "err"})
	})
	mux.HandleFunc("POST /v5/vms/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		v := lookup(r.PathValue("id"))
		switch r.PathValue("action") {
		case "start":
			v["state"] = "running"
		case "pause":
			v["state"] = "paused"
		case "snapshot":
			snapshots["sn-1"] = true
			json.NewEncoder(w).Encode(map[string]any{"snapshotId": "sn-1", "snapshot": map[string]any{"id": "sn-1"}})
			return
		}
		json.NewEncoder(w).Encode(v)
	})
	mux.HandleFunc("DELETE /v5/snapshots/{id}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if !snapshots[r.PathValue("id")] {
			notFound(w)
			return
		}
		delete(snapshots, r.PathValue("id"))
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /v5/tls", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&tlsBody)
		json.NewEncoder(w).Encode(map[string]any{"id": "tls-1"})
	})
	mux.HandleFunc("POST /v5/vms/{id}/fs/mkdir", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	mux.HandleFunc("GET /v5/vms/{id}/fs/stat", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		f, ok := files[r.URL.Query().Get("path")]
		if !ok {
			notFound(w)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"isFile": true, "isDirectory": false, "owner": f.owner, "size": len(f.data)})
	})
	mux.HandleFunc("DELETE /v5/vms/{id}/fs/remove", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		delete(files, r.URL.Query().Get("path"))
		w.WriteHeader(204)
	})
	mux.HandleFunc("PUT /v5/vms/{id}/fs/write", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mode, err := strconv.Atoi(r.URL.Query().Get("mode"))
		if err != nil {
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		owner := "ubuntu"
		if _, ok := files[r.URL.Query().Get("path")]; ok {
			owner = "root"
		}
		files[r.URL.Query().Get("path")] = file{data: string(b), owner: owner, mode: mode}
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /v5/vms/{id}/fs/read", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		f, ok := files[r.URL.Query().Get("path")]
		if !ok {
			notFound(w)
			return
		}
		io.WriteString(w, f.data)
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

	o := FromConfig(map[string]string{"apiURL": srv.URL}, "k")
	o.ReadyTimeout = 5 * time.Second
	a := New(o)
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
	if got := files[envFile].data; got != "TOKEN='it'\\''s secret'\n" {
		t.Fatalf("env file %q", got)
	}
	if again, err := a.Create(ctx, req); err != nil || again.Ref != inst.Ref || len(vms) != 1 {
		t.Fatalf("create not idempotent: %v %v", again, err)
	}
	// The replay rewrote the env file: it must stay 0600 and owned by the
	// user exec runs as, or commands cannot read it.
	if f := files[envFile]; f.mode != 0o600 || f.owner != "ubuntu" {
		t.Fatalf("env file after a replayed create: mode %o owner %s", f.mode, f.owner)
	}
	res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "make", Cwd: "/src"})
	if err != nil || res.ExitCode != 2 || res.Stderr != "err" {
		t.Fatalf("exec %+v %v", res, err)
	}
	if cmd := execBody["command"].(string); !strings.Contains(cmd, ". "+envFile) || !strings.HasSuffix(cmd, "cd /src && make") {
		t.Fatalf("command %q", cmd)
	}
	if res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "cat", Stdin: "in"}); err != nil || execBody["stdin"] != "aW4=" || res.ExitCode != 2 {
		t.Fatalf("stdin %v %v (%v)", res, err, execBody["stdin"])
	}
	if res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "sleep 30", TimeoutSeconds: 1}); err != nil || res.ExitCode != 124 || execBody["timeoutMs"] != float64(1000) {
		t.Fatalf("timeout %+v %v", res, err)
	}
	if b, err := a.ReadFile(ctx, inst.Ref, envFile); err != nil || len(b) == 0 {
		t.Fatalf("read %q %v", b, err)
	}
	if _, err := a.ReadFile(ctx, inst.Ref, "/missing"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("missing file: %v", err)
	}
	// Stop powers off as root: the default user may not.
	if err := a.Stop(ctx, inst.Ref); err != nil || vms["vm-1"]["state"] != "stopped" {
		t.Fatalf("stop: %v, state %v", err, vms["vm-1"]["state"])
	}
	if err := a.Start(ctx, inst.Ref); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := a.Pause(ctx, inst.Ref); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if err := a.Resume(ctx, inst.Ref); err != nil {
		t.Fatalf("resume: %v", err)
	}
	snap, err := a.Snapshot(ctx, inst.Ref, "disk")
	if err != nil || snap.Ref != "sn-1" || snap.Kind != "memory" {
		t.Fatalf("snapshot %+v %v", snap, err)
	}
	for i := 0; i < 2; i++ {
		if err := a.DeleteSnapshot(ctx, snap.Ref); err != nil {
			t.Fatalf("delete snapshot #%d: %v", i+1, err)
		}
	}
	u, err := a.ExposePort(ctx, inst.Ref, 8080)
	if err != nil || u != "https://8080-sbx-abc.style.dev" || tlsBody["domain"] != "8080-sbx-abc.style.dev" {
		t.Fatalf("expose %q %v (%v)", u, err, tlsBody)
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
