package fly

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// fakeFly mimics the documented Machines endpoints: metadata filters with
// cursor paging, machine states, exec, and Fly's error body.
type fakeFly struct {
	mu       sync.Mutex
	srv      *httptest.Server
	creates  int
	lastBody map[string]any
	execBody map[string]any
	machines map[string]map[string]any
	files    map[string][]byte
	// race makes the next create lose to a concurrent one with the same name.
	race bool
}

func newFake(t *testing.T) *fakeFly {
	f := &fakeFly{machines: map[string]map[string]any{}, files: map[string][]byte{}}
	mux := http.NewServeMux()
	fail := func(w http.ResponseWriter, status int, status2, msg string) {
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]string{"error": msg, "status": status2})
	}
	machine := func(id string) map[string]any {
		if m := f.machines[id]; m != nil && m["state"] != "destroyed" {
			return m
		}
		return nil
	}
	mux.HandleFunc("GET /v1/apps/{app}", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"name": r.PathValue("app")})
	})
	mux.HandleFunc("GET /v1/apps/{app}/machines", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") == "" {
			t.Error("list must page")
		}
		var key, value string
		for k, v := range r.URL.Query() {
			if strings.HasPrefix(k, "metadata.") {
				key, value = strings.TrimPrefix(k, "metadata."), v[0]
			}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		var ids []string
		for id, m := range f.machines {
			if m["state"] != "destroyed" && m["config"].(map[string]any)["metadata"].(map[string]any)[key] == value {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		// Page 0 is empty but has a cursor, as the API allows; then one
		// machine per page.
		page := 0
		fmt.Sscan(r.URL.Query().Get("cursor"), &page)
		out := []any{}
		if page > 0 && page <= len(ids) {
			out = append(out, f.machines[ids[page-1]])
		}
		if page < len(ids) {
			w.Header().Set("Fly-Next-Cursor", fmt.Sprint(page+1))
		}
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("POST /v1/apps/{app}/machines", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		config := body["config"].(map[string]any)
		f.mu.Lock()
		defer f.mu.Unlock()
		switch config["image"] {
		case "missing:image":
			fail(w, 422, "unknown", "failed to resolve image missing:image")
			return
		case "full-region":
			fail(w, 422, "insufficient_capacity", "no host in iad can fit this machine")
			return
		case "full-host":
			fail(w, 409, "", "could not reserve resource for machine: insufficient memory available to fulfill request")
			return
		case "outage":
			w.WriteHeader(503)
			return
		}
		for _, m := range f.machines {
			if m["name"] == body["name"] && m["state"] != "destroyed" {
				fail(w, 422, "name_taken", "a machine with this name already exists")
				return
			}
		}
		f.creates++
		f.lastBody = body
		id := fmt.Sprintf("m%d", f.creates)
		m := map[string]any{"id": id, "name": body["name"], "state": "created", "region": "iad", "config": config}
		if config["image"] == "crash" {
			m["state"] = "crashing"
		}
		f.machines[id] = m
		if f.race {
			f.race = false
			fail(w, 422, "name_taken", "a machine with this name already exists")
			return
		}
		json.NewEncoder(w).Encode(m)
	})
	mux.HandleFunc("GET /v1/apps/{app}/machines/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		m := f.machines[r.PathValue("id")]
		if m == nil {
			fail(w, 404, "", "machine not found")
			return
		}
		// Each read moves a transition forward.
		switch m["state"] {
		case "created", "starting":
			m["state"] = "started"
		case "stopping":
			m["state"] = "stopped"
		case "suspending":
			m["state"] = "suspended"
		case "crashing":
			m["state"] = "failed"
		}
		json.NewEncoder(w).Encode(m)
	})
	action := func(from map[string]string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			m := machine(r.PathValue("id"))
			if m == nil {
				fail(w, 404, "", "machine not found")
				return
			}
			next, ok := from[m["state"].(string)]
			if !ok {
				fail(w, 412, "", fmt.Sprintf("machine is %s", m["state"]))
				return
			}
			m["state"] = next
			json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		}
	}
	mux.HandleFunc("POST /v1/apps/{app}/machines/{id}/stop", action(map[string]string{"started": "stopping"}))
	mux.HandleFunc("POST /v1/apps/{app}/machines/{id}/suspend", action(map[string]string{"started": "suspending"}))
	mux.HandleFunc("POST /v1/apps/{app}/machines/{id}/start", action(map[string]string{"stopped": "starting", "suspended": "starting"}))
	mux.HandleFunc("DELETE /v1/apps/{app}/machines/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("force") != "true" {
			fail(w, 412, "", "machine is started; use force")
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		m := f.machines[r.PathValue("id")]
		if m == nil {
			fail(w, 404, "", "machine not found")
			return
		}
		m["state"] = "destroyed"
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /v1/apps/{app}/machines/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		cmd := body["command"].([]any)
		script := cmd[2].(string)
		stdin, _ := body["stdin"].(string)
		f.mu.Lock()
		defer f.mu.Unlock()
		reply := func(v map[string]any) { json.NewEncoder(w).Encode(v) }
		switch {
		case strings.Contains(script, "base64 -d > "):
			b, _ := base64.StdEncoding.DecodeString(stdin)
			f.files[script[strings.LastIndex(script, "> ")+2:]] = b
			reply(map[string]any{})
		case strings.Contains(script, "base64 < "):
			b, ok := f.files[script[strings.LastIndex(script, "< ")+2:]]
			if !ok {
				reply(map[string]any{"exit_code": 1, "stderr": "sh: can't open: No such file or directory"})
				return
			}
			enc := base64.StdEncoding.EncodeToString(b)
			reply(map[string]any{"stdout": enc[:4] + "\n" + enc[4:] + "\n"}) // wrapped, as base64 does
		case strings.Contains(script, "stat -c"):
			if strings.Contains(script, "cd /nowhere") {
				reply(map[string]any{"exit_code": 2, "stderr": "sh: cd: can't cd to /nowhere"})
				return
			}
			reply(map[string]any{"stdout": "regular file|5|1700000000|644|a.txt\ndirectory|4096|1700000000|755|sub dir\n"})
		case strings.HasSuffix(script, "sleep 999"):
			reply(map[string]any{"exit_signal": 9})
		default:
			f.execBody = body
			reply(map[string]any{"exit_code": 2, "stdout": "out", "stderr": "err"})
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func TestFlyAdapter(t *testing.T) {
	f := newFake(t)
	a := New(Options{APIURL: f.srv.URL, Token: "fly-secret", App: "dly-sandboxes", Region: "iad", Image: "ubuntu:24.04", ReadyTimeout: 5 * time.Second})
	ctx := context.Background()
	req := adapter.CreateRequest{ID: "sbx_ABC", Installation: "inst_1", Resources: api.Resources{CPU: 3, MemoryMiB: 1000, DiskGiB: 20}, Env: map[string]string{"TOKEN": "it's secret"}}
	inst, err := a.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Ref != "m1" || inst.SandboxID != "sbx_ABC" || inst.State != adapter.StateRunning || inst.Region != "iad" {
		t.Fatalf("instance %+v", inst)
	}
	config := f.lastBody["config"].(map[string]any)
	if f.lastBody["name"] != "sbx-abc" || f.lastBody["region"] != "iad" || config["image"] != "ubuntu:24.04" {
		t.Fatalf("create body %v", f.lastBody)
	}
	if g, _ := json.Marshal(config["guest"]); string(g) != `{"cpu_kind":"shared","cpus":4,"memory_mb":1024}` {
		t.Fatalf("guest %s", g)
	}
	if r, _ := json.Marshal(config["rootfs"]); string(r) != `{"persist":"always","size_gb":20}` {
		t.Fatalf("rootfs %s", r)
	}
	if init := config["init"].(map[string]any)["exec"].([]any); init[2] != idle {
		t.Fatalf("init %v", init)
	}
	file := config["files"].([]any)[0].(map[string]any)
	env, _ := base64.StdEncoding.DecodeString(file["raw_value"].(string))
	if file["guest_path"] != envFile || file["mode"] != float64(0o600) || string(env) != "TOKEN='it'\\''s secret'\n" {
		t.Fatalf("env file %v %q", file, env)
	}
	if again, err := a.Create(ctx, req); err != nil || again.Ref != inst.Ref || f.creates != 1 {
		t.Fatalf("replayed create made %d machines (%v)", f.creates, err)
	}

	res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "make", Cwd: "/src", Env: map[string]string{"X": "a b"}, Stdin: "in", TimeoutSeconds: 30})
	if err != nil || res.ExitCode != 2 || res.Stdout != "out" || res.Stderr != "err" {
		t.Fatalf("exec %+v %v", res, err)
	}
	script := f.execBody["command"].([]any)[2].(string)
	if !strings.Contains(script, ". "+envFile) || !strings.Contains(script, "export X='a b'; ") || !strings.HasSuffix(script, "cd /src && make") {
		t.Fatalf("script %q", script)
	}
	if f.execBody["timeout"] != float64(30) || f.execBody["stdin"] != "in" {
		t.Fatalf("exec body %v", f.execBody)
	}
	if res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "sleep 999"}); err != nil || res.ExitCode != 137 {
		t.Fatalf("a signal is reported as 128+signal: %+v %v", res, err)
	}

	data := []byte{0, 1, 2, 'x', 0xff}
	if err := a.WriteFile(ctx, inst.Ref, "/home/a.bin", data); err != nil {
		t.Fatal(err)
	}
	if b, err := a.ReadFile(ctx, inst.Ref, "/home/a.bin"); err != nil || string(b) != string(data) {
		t.Fatalf("read %v %v", b, err)
	}
	if _, err := a.ReadFile(ctx, inst.Ref, "/missing"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("missing file: %v", err)
	}
	files, err := a.ListFiles(ctx, inst.Ref, "/home/")
	if err != nil || len(files) != 2 || files[0].Path != "/home/a.txt" || files[0].Size != 5 || !files[1].IsDir || files[1].Name != "sub dir" {
		t.Fatalf("list files %+v %v", files, err)
	}
	if _, err := a.ListFiles(ctx, inst.Ref, "/nowhere"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("missing directory: %v", err)
	}

	if err := a.Stop(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.Get(ctx, inst.Ref); got.State != adapter.StateStopped {
		t.Fatalf("after stop %+v", got)
	}
	if again, err := a.Create(ctx, req); err != nil || again.State != adapter.StateRunning || f.creates != 1 {
		t.Fatalf("a replay after the machine stopped starts it: %+v %v", again, err)
	}
	if err := a.Pause(ctx, inst.Ref); !api.IsCode(err, api.CodeUnsupportedOperation) {
		t.Fatalf("pause needs config.suspend: %v", err)
	}

	// Another installation's machine, and a lost create race.
	if _, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_OTHER", Installation: "inst_2"}); err != nil {
		t.Fatal(err)
	}
	f.race = true
	raced, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_RACE", Installation: "inst_1"})
	if err != nil || raced.SandboxID != "sbx_RACE" || f.creates != 3 {
		t.Fatalf("a lost name race resolves to the winner's machine: %+v %v", raced, err)
	}
	list, err := a.List(ctx, "inst_1")
	if err != nil || len(list) != 2 || list[0].SandboxID != "sbx_ABC" || list[1].SandboxID != "sbx_RACE" {
		t.Fatalf("list %+v %v", list, err)
	}

	for range 2 {
		if err := a.Delete(ctx, inst.Ref); err != nil {
			t.Fatalf("delete must be idempotent: %v", err)
		}
	}
	if _, err := a.Get(ctx, inst.Ref); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("a destroyed machine is not found: %v", err)
	}
	if list, _ := a.List(ctx, "inst_1"); len(list) != 1 {
		t.Fatalf("destroyed machines are not listed: %+v", list)
	}
	if err := a.Delete(ctx, "m-never"); err != nil {
		t.Fatalf("deleting a missing machine: %v", err)
	}
	if err := a.Health(ctx); err != nil {
		t.Fatal(err)
	}
	caps, _ := a.Capabilities(ctx)
	if !caps.Has(adapter.FeatureFiles) || !caps.Has(adapter.FeatureStopStart) || caps.Has(adapter.FeaturePauseResume) || caps.Has(adapter.FeaturePorts) || caps.Has(adapter.FeatureSnapshotDisk) {
		t.Fatalf("features %v", caps.Features)
	}
}

func TestFlySuspend(t *testing.T) {
	f := newFake(t)
	a := New(Options{APIURL: f.srv.URL, App: "app", Image: "img", Suspend: true, ReadyTimeout: 5 * time.Second})
	ctx := context.Background()
	inst, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_1", Installation: "inst_1"})
	if err != nil {
		t.Fatal(err)
	}
	if caps, _ := a.Capabilities(ctx); !caps.Has(adapter.FeaturePauseResume) {
		t.Fatal("suspend pools advertise pause")
	}
	if err := a.Pause(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.Get(ctx, inst.Ref); got.State != adapter.StatePaused {
		t.Fatalf("after pause %+v", got)
	}
	if err := a.Resume(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.Get(ctx, inst.Ref); got.State != adapter.StateRunning {
		t.Fatalf("after resume %+v", got)
	}
}

func TestFlyErrorClassification(t *testing.T) {
	f := newFake(t)
	a := New(Options{APIURL: f.srv.URL, Token: "fly-secret", App: "app", ReadyTimeout: 5 * time.Second})
	ctx := context.Background()
	create := func(id, image string) error {
		_, err := a.Create(ctx, adapter.CreateRequest{ID: id, Installation: "inst_1", Template: image})
		return err
	}
	if err := create("sbx_0", ""); !api.IsCode(err, api.CodeInvalidRequest) {
		t.Fatalf("no image: %v", err)
	}
	if err := create("sbx_1", "missing:image"); !api.IsCode(err, api.CodeInvalidRequest) {
		t.Fatalf("a bad image is the request's fault: %v", err)
	}
	err := create("sbx_2", "full-region")
	if !api.IsCode(err, adapter.CodeInsufficientCapacity) || api.AsError(err).Details["providerCode"] != "insufficient_capacity" {
		t.Fatalf("a full region is a capacity limit: %v", err)
	}
	if err := create("sbx_3", "full-host"); !api.IsCode(err, adapter.CodeInsufficientCapacity) {
		t.Fatalf("a full host is a capacity limit: %v", err)
	}
	if err := create("sbx_4", "crash"); !api.IsCode(err, adapter.CodeSandboxFailed) {
		t.Fatalf("a machine that fails to start is this sandbox's failure: %v", err)
	}
	if err := create("sbx_5", "outage"); !api.IsCode(err, api.CodeProviderUnavailable) {
		t.Fatalf("5xx is an outage: %v", err)
	}
	if _, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_6", FromSnapshot: "snap", Template: "img"}); !api.IsCode(err, api.CodeInvalidRequest) {
		t.Fatalf("restoring a snapshot: %v", err)
	}
	f.mu.Lock()
	f.machines["foreign"] = map[string]any{"id": "foreign", "name": "sbx-7", "state": "started", "config": map[string]any{"metadata": map[string]any{}}}
	f.mu.Unlock()
	if err := create("sbx_7", "img"); !api.IsCode(err, api.CodeInvalidRequest) || !strings.Contains(err.Error(), "did not create") {
		t.Fatalf("a name held by a machine Differently did not create: %v", err)
	}

	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer denied.Close()
	b := New(Options{APIURL: denied.URL, Token: "fly-secret", App: "app"})
	if err := b.Health(ctx); !api.IsCode(err, api.CodeProviderUnavailable) || strings.Contains(err.Error(), "fly-secret") {
		t.Fatalf("rejected credentials: %v", err)
	}
}

func TestFlyConfigAndSizing(t *testing.T) {
	if _, err := FromConfig(map[string]string{}, "k"); err == nil {
		t.Fatal("app is required")
	}
	if _, err := FromConfig(map[string]string{"app": "a", "cpuKind": "gpu"}, "k"); err == nil {
		t.Fatal("unknown cpu kinds are refused")
	}
	o, err := FromConfig(map[string]string{"app": "a", "suspend": "true", "cpuKind": "performance"}, "k")
	if err != nil || !o.Suspend || o.APIURL != DefaultAPIURL {
		t.Fatalf("%+v %v", o, err)
	}
	for _, c := range []struct {
		kind string
		r    api.Resources
		want string
	}{
		{"shared", api.Resources{}, `{"cpu_kind":"shared","cpus":1,"memory_mb":256}`},
		{"shared", api.Resources{CPU: 1, MemoryMiB: 4096}, `{"cpu_kind":"shared","cpus":2,"memory_mb":4096}`},
		{"shared", api.Resources{CPU: 5, MemoryMiB: 300}, `{"cpu_kind":"shared","cpus":6,"memory_mb":1536}`},
		{"performance", api.Resources{CPU: 3, MemoryMiB: 1024}, `{"cpu_kind":"performance","cpus":4,"memory_mb":8192}`},
	} {
		if got, _ := json.Marshal(guest(c.kind, c.r)); string(got) != c.want {
			t.Errorf("guest(%s, %+v) = %s, want %s", c.kind, c.r, got, c.want)
		}
	}
}
