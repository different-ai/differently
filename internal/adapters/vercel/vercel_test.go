package vercel

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
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

// fakeVercel mimics the documented Sandbox endpoints: named sandboxes with
// sessions, NDJSON command streams, tarball uploads and snapshots.
type fakeVercel struct {
	t         *testing.T
	mu        sync.Mutex
	srv       *httptest.Server
	creates   int
	createReq map[string]any
	boxes     map[string]map[string]any // name -> sandbox
	sessions  map[string]map[string]any // id -> session
	files     map[string][]byte
	snapshots map[string]bool
	cmd       map[string]any
	stdin     chan string
	extended  float64
	nextID    int
	// failSession makes new sessions fail instead of running.
	failSession bool
}

func newFake(t *testing.T) *fakeVercel {
	f := &fakeVercel{t: t, boxes: map[string]map[string]any{}, sessions: map[string]map[string]any{}, files: map[string][]byte{}, snapshots: map[string]bool{}, stdin: make(chan string, 1)}
	mux := http.NewServeMux()
	errorBody := func(w http.ResponseWriter, status int, code, msg string) {
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": msg}})
	}
	project := func(w http.ResponseWriter, r *http.Request, key string) bool {
		if r.URL.Query().Get(key) != "prj_1" || r.URL.Query().Get("teamId") != "team_1" {
			errorBody(w, 400, "bad_request", "project and team are required")
			return false
		}
		return true
	}
	view := func(name string) map[string]any {
		b := f.boxes[name]
		s := f.sessions[b["currentSessionId"].(string)]
		return map[string]any{"sandbox": b, "session": s, "routes": []any{map[string]any{"port": 3000, "subdomain": "sb-3000", "url": "https://sb-3000.vercel.run"}}}
	}
	mux.HandleFunc("POST /v4/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case body["image"] == "missing:image":
			errorBody(w, 400, "bad_request", "image missing:image could not be pulled")
			return
		case body["image"] == "busy":
			errorBody(w, 429, "rate_limited", "concurrency limit has been exceeded")
			return
		case body["image"] == "broken":
			errorBody(w, 500, "internal_server_error", "internal error")
			return
		case f.boxes[body["name"].(string)] != nil:
			errorBody(w, 409, "conflict", "name taken")
			return
		}
		f.creates++
		f.createReq = body
		name := body["name"].(string)
		f.boxes[name] = map[string]any{"name": name, "status": "running", "region": "iad1", "tags": body["tags"]}
		f.newSession(name)
		json.NewEncoder(w).Encode(view(name))
	})
	mux.HandleFunc("GET /v2/sandboxes/{name}", func(w http.ResponseWriter, r *http.Request) {
		if !project(w, r, "projectId") {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		name := r.PathValue("name")
		b := f.boxes[name]
		if b == nil {
			errorBody(w, 404, "not_found", "sandbox not found")
			return
		}
		s := f.sessions[b["currentSessionId"].(string)]
		switch {
		case s["status"] == "pending" && f.failSession:
			s["status"], b["status"] = "failed", "stopped"
		case s["status"] == "pending":
			s["status"] = "running" // ready on the next poll
		case s["status"] == "snapshotting" || s["status"] == "stopping":
			s["status"], b["status"] = "stopped", "stopped"
		case b["status"] == "stopped" && r.URL.Query().Get("resume") == "true":
			b["status"] = "running"
			f.newSession(name)
		}
		json.NewEncoder(w).Encode(view(name))
	})
	mux.HandleFunc("GET /v2/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		if !project(w, r, "project") {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		k, v, _ := strings.Cut(r.URL.Query().Get("tags"), ":")
		var names []string
		for name, b := range f.boxes {
			if tags, _ := b["tags"].(map[string]any); k == "" || tags[k] == v {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		// One sandbox per page exercises the cursor.
		start := 0
		if c := r.URL.Query().Get("cursor"); c != "" {
			fmt.Sscan(c, &start)
		}
		page := map[string]any{"sandboxes": []any{}, "pagination": map[string]any{"count": len(names), "next": nil}}
		if start < len(names) {
			page["sandboxes"] = []any{f.boxes[names[start]]}
			if start+1 < len(names) {
				page["pagination"] = map[string]any{"count": len(names), "next": fmt.Sprint(start + 1)}
			}
		}
		json.NewEncoder(w).Encode(page)
	})
	mux.HandleFunc("DELETE /v2/sandboxes/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.boxes[r.PathValue("name")] == nil {
			errorBody(w, 404, "not_found", "sandbox not found")
			return
		}
		delete(f.boxes, r.PathValue("name"))
		w.Write([]byte(`{}`))
	})
	mux.HandleFunc("POST /v2/sandboxes/sessions/{sid}/cmd", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["wait"] != true || body["logs"] != true {
			t.Errorf("commands must wait and stream logs: %v", body)
		}
		f.mu.Lock()
		f.cmd = body
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := json.NewEncoder(w)
		line := func(v any) {
			enc.Encode(v)
			w.(http.Flusher).Flush()
		}
		line(map[string]any{"command": map[string]any{"id": "cmd_1", "exitCode": nil}})
		args, _ := body["args"].([]any)
		switch {
		case body["command"] == "find":
			line(map[string]any{"stream": "stdout", "data": "f\t5\t1700000000.5\t644\ta.txt\nd\t4096\t1700000000\t755\tsub\n"})
			line(map[string]any{"command": map[string]any{"id": "cmd_1", "exitCode": 0}})
		case len(args) == 2 && args[1] == "drop":
			line(map[string]any{"stream": "stdout", "data": "part1-"})
			return // the connection drops before the command finishes
		case body["attachStdin"] == true:
			select {
			case in := <-f.stdin:
				line(map[string]any{"stream": "stdout", "data": in})
			case <-time.After(5 * time.Second):
				t.Error("stdin never arrived")
			}
			line(map[string]any{"command": map[string]any{"id": "cmd_1", "exitCode": 0}})
		default:
			line(map[string]any{"stream": "stdout", "data": "out\n"})
			line(map[string]any{"stream": "stderr", "data": "err\n"})
			line(map[string]any{"command": map[string]any{"id": "cmd_1", "exitCode": 3}})
		}
	})
	mux.HandleFunc("POST /v2/sandboxes/sessions/{sid}/cmd/{cmd}/stdin", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Data   string `json:"data"`
			Offset int    `json:"offset"`
			Close  bool   `json:"close"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		b, _ := base64.StdEncoding.DecodeString(body.Data)
		if body.Offset != 0 || !body.Close {
			t.Errorf("one small write expected: %+v", body)
		}
		f.stdin <- string(b)
		json.NewEncoder(w).Encode(map[string]any{"bytesWritten": len(b)})
	})
	mux.HandleFunc("GET /v2/sandboxes/sessions/{sid}/cmd/{cmd}/logs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := json.NewEncoder(w)
		enc.Encode(map[string]any{"stream": "stdout", "data": "part1-part2"}) // replays what was already sent
		enc.Encode(map[string]any{"stream": "stderr", "data": "warn"})
	})
	mux.HandleFunc("GET /v2/sandboxes/sessions/{sid}/cmd/{cmd}", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("wait") != "true" {
			t.Error("the result must be waited for")
		}
		json.NewEncoder(w).Encode(map[string]any{"command": map[string]any{"id": "cmd_1", "exitCode": 7}})
	})
	mux.HandleFunc("POST /v2/sandboxes/sessions/{sid}/fs/mkdir", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) })
	mux.HandleFunc("POST /v2/sandboxes/sessions/{sid}/fs/write", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/gzip" {
			t.Errorf("content type %q", r.Header.Get("Content-Type"))
		}
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		tr := tar.NewReader(gz)
		for {
			h, err := tr.Next()
			if err != nil {
				break
			}
			b, _ := io.ReadAll(tr)
			f.mu.Lock()
			f.files[r.Header.Get("X-Cwd")+h.Name] = b
			f.mu.Unlock()
		}
		w.Write([]byte(`{}`))
	})
	mux.HandleFunc("POST /v2/sandboxes/sessions/{sid}/fs/read", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		b, ok := f.files[body["path"]]
		if !ok {
			errorBody(w, 404, "file_not_found", "no such file")
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(b)
	})
	mux.HandleFunc("POST /v2/sandboxes/sessions/{sid}/stop", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.sessions[r.PathValue("sid")]["status"] = "stopping"
		json.NewEncoder(w).Encode(map[string]any{"session": f.sessions[r.PathValue("sid")]})
	})
	mux.HandleFunc("POST /v3/sandboxes/sessions/{sid}/snapshot", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["expiration"] != float64(0) {
			t.Errorf("snapshots must not expire on their own: %v", body)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.sessions[r.PathValue("sid")]["status"] = "snapshotting"
		f.snapshots["snap_1"] = true
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{"snapshot": map[string]any{"id": "snap_1", "sizeBytes": 1024, "status": "created"}})
	})
	mux.HandleFunc("DELETE /v2/sandboxes/snapshots/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.snapshots[r.PathValue("id")] {
			errorBody(w, 404, "not_found", "snapshot not found")
			return
		}
		delete(f.snapshots, r.PathValue("id"))
		w.Write([]byte(`{}`))
	})
	mux.HandleFunc("POST /v2/sandboxes/sessions/{sid}/extend-timeout", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]float64
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.extended = body["duration"]
		f.mu.Unlock()
		w.Write([]byte(`{}`))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// newSession starts a pending session for a sandbox; f.mu is held.
func (f *fakeVercel) newSession(name string) {
	f.nextID++
	id := fmt.Sprintf("sbx_session_%d", f.nextID)
	f.sessions[id] = map[string]any{"id": id, "status": "pending", "region": "iad1", "startedAt": 1_700_000_000_000, "timeout": 600_000}
	f.boxes[name]["currentSessionId"] = id
}

func newAdapter(f *fakeVercel) *Adapter {
	return New(Options{APIURL: f.srv.URL, Token: "vercel-secret-token", Project: "prj_1", Team: "team_1", Ports: []int{3000}, MaxTimeout: time.Hour, ReadyTimeout: 5 * time.Second})
}

func TestVercelAdapter(t *testing.T) {
	f := newFake(t)
	a := newAdapter(f)
	ctx := context.Background()
	req := adapter.CreateRequest{ID: "sbx_A", Installation: "inst_1", Resources: api.Resources{CPU: 3, MemoryMiB: 1024}, Env: map[string]string{"K": "v"}, TTLSeconds: 7200}
	inst, err := a.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Ref != "sbx_A" || inst.SandboxID != "sbx_A" || inst.State != adapter.StateRunning || inst.Region != "iad1" {
		t.Fatalf("instance %+v", inst)
	}
	res, _ := json.Marshal(f.createReq["resources"])
	if string(res) != `{"memory":8192,"vcpus":4}` || f.createReq["persistent"] != true || f.createReq["timeout"] != float64(time.Hour.Milliseconds()) {
		t.Fatalf("create body %v", f.createReq)
	}
	if tags := f.createReq["tags"].(map[string]any); tags[tagInstallation] != "inst_1" || tags[tagSandbox] != "sbx_A" {
		t.Fatalf("tags %v", tags)
	}
	if again, err := a.Create(ctx, req); err != nil || again.Ref != inst.Ref || f.creates != 1 {
		t.Fatalf("replayed create made %d sandboxes (%v)", f.creates, err)
	}

	// Commands: a shell string, an argument list run directly, stdin.
	out, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "make test", Cwd: "/src", Env: map[string]string{"X": "1"}, TimeoutSeconds: 30})
	if err != nil || out.ExitCode != 3 || out.Stdout != "out\n" || out.Stderr != "err\n" {
		t.Fatalf("exec %+v %v", out, err)
	}
	if f.cmd["command"] != "sh" || f.cmd["cwd"] != "/src" || f.cmd["timeout"] != float64(30000) || f.cmd["env"].(map[string]any)["X"] != "1" {
		t.Fatalf("command body %v", f.cmd)
	}
	if _, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Args: []string{"echo", "hi there"}}); err != nil || f.cmd["command"] != "echo" || f.cmd["args"].([]any)[0] != "hi there" {
		t.Fatalf("args must run directly: %v %v", f.cmd, err)
	}
	if out, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "cat", Stdin: "from stdin"}); err != nil || out.Stdout != "from stdin" {
		t.Fatalf("stdin %+v %v", out, err)
	}
	var events []api.ExecEvent
	code, err := a.ExecStream(ctx, inst.Ref, api.ExecRequest{Command: "drop"}, func(e api.ExecEvent) error { events = append(events, e); return nil })
	if err != nil || code != 7 || len(events) != 3 || events[0].Data+events[1].Data != "part1-part2" || events[2].Stream != "stderr" {
		t.Fatalf("a dropped stream must resume from the logs without repeating output: %d %+v %v", code, events, err)
	}

	// Files.
	if err := a.WriteFile(ctx, inst.Ref, "/home/user/a.txt", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if b, err := a.ReadFile(ctx, inst.Ref, "/home/user/a.txt"); err != nil || string(b) != "hello" {
		t.Fatalf("read %q %v", b, err)
	}
	if _, err := a.ReadFile(ctx, inst.Ref, "/missing"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("missing file: %v", err)
	}
	files, err := a.ListFiles(ctx, inst.Ref, "/home/user/")
	if err != nil || len(files) != 2 || files[0].Path != "/home/user/a.txt" || files[0].Size != 5 || !files[1].IsDir || files[0].ModTime.UnixMilli() != 1_700_000_000_500 {
		t.Fatalf("list files %+v %v", files, err)
	}

	// Ports: only declared ones.
	if u, err := a.ExposePort(ctx, inst.Ref, 3000); err != nil || u != "https://sb-3000.vercel.run" {
		t.Fatalf("port url %q %v", u, err)
	}
	if _, err := a.ExposePort(ctx, inst.Ref, 8080); !api.IsCode(err, api.CodeInvalidRequest) {
		t.Fatalf("undeclared port: %v", err)
	}

	// TTL: extends towards the new deadline, capped at the plan maximum.
	started := time.UnixMilli(1_700_000_000_000)
	if err := a.SetExpiry(ctx, inst.Ref, started.Add(30*time.Minute)); err != nil || f.extended != float64((20*time.Minute).Milliseconds()) {
		t.Fatalf("extend %v %v", f.extended, err)
	}
	if err := a.SetExpiry(ctx, inst.Ref, started.Add(5*time.Hour)); err != nil || f.extended != float64((50*time.Minute).Milliseconds()) {
		t.Fatalf("extend is capped at maxTimeout: %v %v", f.extended, err)
	}

	// Stop keeps the files (persistent sandbox); start resumes.
	if err := a.Stop(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.Get(ctx, inst.Ref); got.State != adapter.StateStopped {
		t.Fatalf("after stop %+v", got)
	}
	if _, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "true"}); !api.IsCode(err, api.CodeInvalidState) {
		t.Fatalf("exec on a stopped sandbox: %v", err)
	}
	if err := a.Start(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.Get(ctx, inst.Ref); got.State != adapter.StateRunning {
		t.Fatalf("after start %+v", got)
	}

	// Snapshot ends the session; the adapter resumes the sandbox.
	if _, err := a.Snapshot(ctx, inst.Ref, "memory"); !api.IsCode(err, api.CodeUnsupportedOperation) {
		t.Fatalf("memory snapshots: %v", err)
	}
	snap, err := a.Snapshot(ctx, inst.Ref, "disk")
	if err != nil || snap.Ref != "snap_1" || snap.Kind != "disk" || snap.SizeBytes != 1024 {
		t.Fatalf("snapshot %+v %v", snap, err)
	}
	if got, _ := a.Get(ctx, inst.Ref); got.State != adapter.StateRunning {
		t.Fatalf("snapshot must leave the sandbox running: %+v", got)
	}
	fork, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_B", Installation: "inst_1", FromSnapshot: snap.Ref, Template: "ignored-for-snapshots"})
	if err != nil || fork.Ref != "sbx_B" {
		t.Fatalf("fork %+v %v", fork, err)
	}
	if src, _ := json.Marshal(f.createReq["source"]); string(src) != `{"snapshotId":"snap_1","type":"snapshot"}` || f.createReq["image"] != nil {
		t.Fatalf("fork body %v", f.createReq)
	}

	// List pages through every sandbox of this installation only.
	if _, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_C", Installation: "inst_other"}); err != nil {
		t.Fatal(err)
	}
	list, err := a.List(ctx, "inst_1")
	if err != nil || len(list) != 2 || list[0].SandboxID != "sbx_A" || list[1].SandboxID != "sbx_B" {
		t.Fatalf("list %+v %v", list, err)
	}

	for _, ref := range []string{"sbx_A", "sbx_A"} {
		if err := a.Delete(ctx, ref); err != nil {
			t.Fatalf("delete must be idempotent: %v", err)
		}
	}
	if _, err := a.Get(ctx, "sbx_A"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	for range 2 {
		if err := a.DeleteSnapshot(ctx, snap.Ref); err != nil {
			t.Fatalf("snapshot delete must be idempotent: %v", err)
		}
	}
	caps, _ := a.Capabilities(ctx)
	for _, want := range []string{adapter.FeatureExecStream, adapter.FeatureFiles, adapter.FeatureStopStart, adapter.FeatureSnapshotDisk, adapter.FeatureFork, adapter.FeaturePorts, adapter.FeatureArchAMD64} {
		if !caps.Has(want) {
			t.Errorf("missing feature %s", want)
		}
	}
	if caps.Has(adapter.FeatureSnapshotMem) || caps.Has(adapter.FeaturePauseResume) {
		t.Errorf("memory is never kept: %v", caps.Features)
	}
}

func TestVercelErrorClassification(t *testing.T) {
	f := newFake(t)
	a := newAdapter(f)
	ctx := context.Background()
	create := func(id, image string) error {
		_, err := a.Create(ctx, adapter.CreateRequest{ID: id, Installation: "inst_1", Template: image})
		return err
	}
	err := create("sbx_1", "missing:image")
	if !api.IsCode(err, api.CodeInvalidRequest) || api.AsError(err).Details["providerCode"] != "bad_request" || !strings.Contains(err.Error(), "could not be pulled") {
		t.Fatalf("a bad image is the request's fault: %v", err)
	}
	if err := create("sbx_2", "busy"); !api.IsCode(err, adapter.CodeInsufficientCapacity) {
		t.Fatalf("429 is a capacity limit: %v", err)
	}
	if err := create("sbx_3", "broken"); !api.IsCode(err, api.CodeProviderUnavailable) {
		t.Fatalf("5xx is an outage: %v", err)
	}
	f.mu.Lock()
	f.boxes["sbx_5"] = map[string]any{"name": "sbx_5", "status": "running", "tags": map[string]any{}, "currentSessionId": "none"}
	f.mu.Unlock()
	if err := create("sbx_5", ""); !api.IsCode(err, api.CodeInvalidRequest) || !strings.Contains(err.Error(), "did not create") {
		t.Fatalf("a name held by a sandbox Differently did not create: %v", err)
	}
	f.failSession = true
	if err := create("sbx_4", ""); !api.IsCode(err, adapter.CodeSandboxFailed) {
		t.Fatalf("a failed session is this sandbox's failure: %v", err)
	}

	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte(`{"error":{"code":"forbidden","message":"Not authorized"}}`))
	}))
	defer denied.Close()
	b := New(Options{APIURL: denied.URL, Token: "vercel-secret-token", Project: "prj_1"})
	if err := b.Health(ctx); !api.IsCode(err, api.CodeProviderUnavailable) || strings.Contains(err.Error(), "vercel-secret-token") {
		t.Fatalf("rejected credentials: %v", err)
	}
}

func TestVercelConfig(t *testing.T) {
	if _, err := FromConfig(map[string]string{}, "k"); err == nil {
		t.Fatal("project is required")
	}
	if _, err := FromConfig(map[string]string{"project": "p", "ports": "80"}, "k"); err == nil {
		t.Fatal("ports below 1024 are refused")
	}
	o, err := FromConfig(map[string]string{"project": "p", "ports": "3000, 8080", "maxTimeoutSeconds": "86400", "architecture": "arm64"}, "k")
	if err != nil || len(o.Ports) != 2 || o.MaxTimeout != 24*time.Hour || o.APIURL != DefaultAPIURL {
		t.Fatalf("%+v %v", o, err)
	}
	if caps, _ := New(o).Capabilities(context.Background()); !caps.Has(adapter.FeatureArchARM64) {
		t.Fatalf("arm64 pools advertise arm64: %v", caps.Features)
	}
	if caps, _ := New(Options{Project: "p"}).Capabilities(context.Background()); caps.Has(adapter.FeaturePorts) {
		t.Fatal("ports.expose needs declared ports")
	}
	for _, c := range []struct {
		r    api.Resources
		want int
	}{{api.Resources{}, 1}, {api.Resources{CPU: 1, MemoryMiB: 2048}, 1}, {api.Resources{CPU: 1, MemoryMiB: 3000}, 2}, {api.Resources{CPU: 5}, 6}} {
		if got := vcpus(c.r); got != c.want {
			t.Errorf("vcpus(%+v) = %d, want %d", c.r, got, c.want)
		}
	}
}
