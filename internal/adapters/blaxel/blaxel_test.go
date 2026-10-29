package blaxel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// fakeBlaxel mimics the documented control-plane and sandbox API shapes.
type fakeBlaxel struct {
	t        *testing.T
	mu       sync.Mutex
	creates  int
	boxes    map[string]map[string]any
	files    map[string][]byte
	dirs     map[string]bool
	previews map[string]bool
	lastExec map[string]any
	srv      *httptest.Server
}

func newFake(t *testing.T) *fakeBlaxel {
	f := &fakeBlaxel{t: t, boxes: map[string]map[string]any{}, files: map[string][]byte{}, dirs: map[string]bool{}, previews: map[string]bool{}}
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Blaxel-Workspace") != "ws" || r.Header.Get("X-Blaxel-Authorization") != "Bearer k" {
				w.WriteHeader(401)
				json.NewEncoder(w).Encode(map[string]any{"error": "AUTHENTICATION_REQUIRED", "code": 401})
				return
			}
			h(w, r)
		}
	}
	notFound := func(w http.ResponseWriter) {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]any{"error": "Not Found", "code": 404, "message": "Sandbox not found"})
	}
	mux.HandleFunc("POST /v0/sandboxes", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("createIfNotExist") != "true" {
			t.Error("create must ask for createIfNotExist")
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		meta := body["metadata"].(map[string]any)
		runtime := body["spec"].(map[string]any)["runtime"].(map[string]any)
		if runtime["image"] == "bad/image" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]any{"code": "INVALID_IMAGE", "message": "Sandbox image bad/image is not supported", "status_code": 400})
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		name := meta["name"].(string)
		if box, ok := f.boxes[name]; ok {
			json.NewEncoder(w).Encode(box) // createIfNotExist: the live sandbox
			return
		}
		f.creates++
		box := map[string]any{
			"metadata": map[string]any{"name": name, "labels": meta["labels"], "url": f.srv.URL + "/sbx/" + name},
			"spec":     body["spec"],
			"status":   "DEPLOYING",
		}
		f.boxes[name] = box
		json.NewEncoder(w).Encode(box)
	}))
	mux.HandleFunc("GET /v0/sandboxes/{name}", auth(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		box, ok := f.boxes[r.PathValue("name")]
		if !ok {
			notFound(w)
			return
		}
		if box["status"] == "DEPLOYING" {
			box["status"] = "DEPLOYED" // ready on the first poll
		}
		json.NewEncoder(w).Encode(box)
	}))
	mux.HandleFunc("GET /v0/sandboxes", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Blaxel-Version") != listVersion {
			t.Errorf("list without the paginated version: %q", r.Header.Get("Blaxel-Version"))
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		names := make([]string, 0, len(f.boxes))
		for n := range f.boxes {
			names = append(names, n)
		}
		sort.Strings(names)
		// One sandbox per page, so paging is exercised.
		i, _ := strconv.Atoi(r.URL.Query().Get("cursor"))
		page := map[string]any{"data": []any{}, "meta": map[string]any{"hasMore": false}}
		if i < len(names) {
			page["data"] = []any{f.boxes[names[i]]}
			if i+1 < len(names) {
				page["meta"] = map[string]any{"hasMore": true, "nextCursor": strconv.Itoa(i + 1)}
			}
		}
		json.NewEncoder(w).Encode(page)
	}))
	mux.HandleFunc("DELETE /v0/sandboxes/{name}", auth(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		box, ok := f.boxes[r.PathValue("name")]
		if !ok {
			notFound(w)
			return
		}
		delete(f.boxes, r.PathValue("name"))
		json.NewEncoder(w).Encode(box)
	}))
	mux.HandleFunc("POST /v0/sandboxes/{name}/previews", auth(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		spec := body["spec"].(map[string]any)
		if spec["public"] != false {
			t.Error("previews must be token-protected")
		}
		key := r.PathValue("name") + "/" + body["metadata"].(map[string]any)["name"].(string)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.previews[key] {
			w.WriteHeader(409)
			json.NewEncoder(w).Encode(map[string]any{"error": "preview already exists", "code": 409})
			return
		}
		f.previews[key] = true
		json.NewEncoder(w).Encode(map[string]any{"metadata": body["metadata"], "spec": map[string]any{"port": spec["port"], "url": "https://p1.us-pdx-1.preview.bl.run"}})
	}))
	mux.HandleFunc("GET /v0/sandboxes/{name}/previews/{preview}", auth(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"spec": map[string]any{"url": "https://p1.us-pdx-1.preview.bl.run"}})
	}))
	mux.HandleFunc("POST /v0/sandboxes/{name}/previews/{preview}/tokens", auth(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["spec"].(map[string]any)["expiresAt"] == "" {
			t.Error("tokens must expire")
		}
		json.NewEncoder(w).Encode(map[string]any{"metadata": body["metadata"], "spec": map[string]any{"token": "tok/1"}})
	}))
	mux.HandleFunc("POST /sbx/{name}/process", auth(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept"), "application/x-ndjson") {
			t.Errorf("exec must ask for the NDJSON stream, sent Accept %q", r.Header.Get("Accept"))
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.lastExec = body
		f.mu.Unlock()
		cmd := body["command"].(string)
		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := json.NewEncoder(w)
		if body["workingDir"] == "/no-such-dir" {
			enc.Encode(map[string]string{"type": "error", "data": "working directory does not exist"})
			return
		}
		stdout := "ran " + cmd + "\n"
		if i := strings.Index(cmd, ") < "); i >= 0 { // staged stdin: echo the file
			file := strings.Trim(strings.Fields(cmd[i+4:])[0], "';")
			f.mu.Lock()
			stdout = string(f.files[file])
			f.mu.Unlock()
		}
		code, status := 0, "completed"
		if strings.Contains(cmd, "exit 3") {
			code, status = 3, "failed"
		}
		enc.Encode(map[string]string{"type": "stdout", "data": stdout})
		enc.Encode(map[string]string{"type": "keepalive"})
		enc.Encode(map[string]string{"type": "stderr", "data": "warn\n"})
		result, _ := json.Marshal(map[string]any{"pid": "42", "status": status, "exitCode": code, "stdout": stdout, "stderr": "warn\n"})
		enc.Encode(map[string]string{"type": "result", "data": string(result)})
	}))
	fsPath := func(r *http.Request) string {
		if !strings.Contains(r.URL.EscapedPath(), "/filesystem/%2F") {
			t.Errorf("absolute paths must be sent with an encoded leading slash: %s", r.URL.EscapedPath())
		}
		return "/" + strings.TrimLeft(r.PathValue("path"), "/")
	}
	mux.HandleFunc("GET /sbx/{name}/filesystem/{path...}", auth(func(w http.ResponseWriter, r *http.Request) {
		p := fsPath(r)
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Query().Get("download") == "true" {
			b, ok := f.files[p]
			if !ok {
				w.WriteHeader(404)
				json.NewEncoder(w).Encode(map[string]string{"error": "file not found"})
				return
			}
			w.Write(b)
			return
		}
		dir := map[string]any{"name": p, "path": p, "files": []any{}, "subdirectories": []any{}}
		for fp, b := range f.files {
			if strings.HasPrefix(fp, p+"/") && !strings.Contains(fp[len(p)+1:], "/") {
				dir["files"] = append(dir["files"].([]any), map[string]any{"path": fp, "name": fp[len(p)+1:], "size": len(b), "permissions": "-rw-r--r--", "lastModified": "2026-10-28T10:00:00Z", "owner": "root", "group": "root"})
			}
		}
		for d := range f.dirs {
			if strings.HasPrefix(d, p+"/") && !strings.Contains(d[len(p)+1:], "/") {
				dir["subdirectories"] = append(dir["subdirectories"].([]any), map[string]any{"path": d, "name": d[len(p)+1:]})
			}
		}
		json.NewEncoder(w).Encode(dir)
	}))
	mux.HandleFunc("PUT /sbx/{name}/filesystem/{path...}", auth(func(w http.ResponseWriter, r *http.Request) {
		p := fsPath(r)
		f.mu.Lock()
		defer f.mu.Unlock()
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["isDirectory"] == true {
				f.dirs[p] = true
			}
		} else {
			file, _, err := r.FormFile("file")
			if err != nil {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
				return
			}
			if parent := p[:strings.LastIndex(p, "/")]; parent != "" && !f.dirs[parent] {
				t.Errorf("parent %s was not created before %s", parent, p)
			}
			f.files[p], _ = io.ReadAll(file)
		}
		json.NewEncoder(w).Encode(map[string]string{"message": "ok", "path": p})
	}))
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func newAdapter(t *testing.T, apiURL string) *Adapter {
	o, err := FromConfig(map[string]string{"apiURL": apiURL, "workspace": "ws", "region": "us-pdx-1"}, "k")
	if err != nil {
		t.Fatal(err)
	}
	return New(o)
}

func TestBlaxelAdapter(t *testing.T) {
	f := newFake(t)
	a := newAdapter(t, f.srv.URL+"/v0")
	ctx := context.Background()
	if err := a.Health(ctx); err != nil {
		t.Fatal(err)
	}
	req := adapter.CreateRequest{ID: "sbx_01ABC", Installation: "inst_1", Resources: api.Resources{CPU: 2, MemoryMiB: 1024}, Env: map[string]string{"A": "1"}, TTLSeconds: 60}
	inst, err := a.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Ref != "sbx-01abc" || inst.SandboxID != "sbx_01ABC" || inst.State != adapter.StateRunning || inst.Region != "us-pdx-1" {
		t.Fatalf("instance %+v", inst)
	}
	f.mu.Lock()
	runtime := f.boxes["sbx-01abc"]["spec"].(map[string]any)["runtime"].(map[string]any)
	f.mu.Unlock()
	if runtime["memory"] != float64(4096) || runtime["ttl"] != "300s" || runtime["image"] != DefaultImage {
		t.Fatalf("runtime: CPU should size memory, TTL floors at 5m: %v", runtime)
	}
	again, err := a.Create(ctx, req)
	f.mu.Lock()
	creates := f.creates
	f.mu.Unlock()
	if err != nil || again.Ref != inst.Ref || creates != 1 {
		t.Fatalf("replayed create made %d sandboxes (%v)", creates, err)
	}

	res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Args: []string{"echo", "hi there"}, Cwd: "/src", Env: map[string]string{"X": "y"}, TimeoutSeconds: 30})
	if err != nil || res.ExitCode != 0 || res.Stdout != "ran echo 'hi there'\n" || res.Stderr != "warn\n" {
		t.Fatalf("exec %+v %v", res, err)
	}
	f.mu.Lock()
	body := f.lastExec
	f.mu.Unlock()
	if body["workingDir"] != "/src" || body["timeout"] != float64(30) || body["env"].(map[string]any)["X"] != "y" {
		t.Fatalf("exec body %v", body)
	}
	if res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "make; exit 3"}); err != nil || res.ExitCode != 3 {
		t.Fatalf("non-zero exit is a result: %+v %v", res, err)
	}
	if res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "cat", Stdin: "from stdin"}); err != nil || res.Stdout != "from stdin" {
		t.Fatalf("stdin %+v %v", res, err)
	}
	if _, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "ls", Cwd: "/no-such-dir"}); !api.IsCode(err, api.CodeInvalidRequest) {
		t.Fatalf("a command that cannot start is invalid_request: %v", err)
	}
	var events []api.ExecEvent
	code, err := a.ExecStream(ctx, inst.Ref, api.ExecRequest{Command: "make"}, func(ev api.ExecEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil || code != 0 || len(events) != 2 || events[0].Stream != "stdout" || events[1] != (api.ExecEvent{Stream: "stderr", Data: "warn\n"}) {
		t.Fatalf("stream %d %v %+v", code, err, events)
	}

	if err := a.WriteFile(ctx, inst.Ref, "/home/user/a b.txt", []byte("v\x001")); err != nil {
		t.Fatal(err)
	}
	if b, err := a.ReadFile(ctx, inst.Ref, "/home/user/a b.txt"); err != nil || string(b) != "v\x001" {
		t.Fatalf("read %q %v", b, err)
	}
	if _, err := a.ReadFile(ctx, inst.Ref, "/missing"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("missing file: %v", err)
	}
	f.mu.Lock()
	f.dirs["/home/user/sub"] = true
	f.mu.Unlock()
	files, err := a.ListFiles(ctx, inst.Ref, "/home/user")
	if err != nil || len(files) != 2 || !files[0].IsDir || files[1].Name != "a b.txt" || files[1].Size != 3 || files[1].ModTime.IsZero() {
		t.Fatalf("list files %+v %v", files, err)
	}

	u, err := a.ExposePort(ctx, inst.Ref, 3000)
	if err != nil || u != "https://p1.us-pdx-1.preview.bl.run?bl_preview_token=tok%2F1" {
		t.Fatalf("expose %q %v", u, err)
	}
	if again, err := a.ExposePort(ctx, inst.Ref, 3000); err != nil || again != u {
		t.Fatalf("exposing a port twice: %q %v", again, err)
	}

	if err := a.Delete(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	if err := a.Delete(ctx, inst.Ref); err != nil {
		t.Fatalf("second delete must succeed: %v", err)
	}
	if _, err := a.Get(ctx, inst.Ref); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
}

func TestBlaxelListPagesAndFilters(t *testing.T) {
	f := newFake(t)
	label := func(sbx, inst string) map[string]any {
		return map[string]any{labelSandbox: sbx, labelInstallation: inst}
	}
	f.boxes["sbx-a"] = map[string]any{"metadata": map[string]any{"name": "sbx-a", "labels": label("sbx_a", "inst_1")}, "status": "DEPLOYED"}
	f.boxes["sbx-b"] = map[string]any{"metadata": map[string]any{"name": "sbx-b", "labels": label("sbx_b", "inst_2")}, "status": "DEPLOYED"}
	f.boxes["sbx-c"] = map[string]any{"metadata": map[string]any{"name": "sbx-c", "labels": label("sbx_c", "inst_1")}, "status": "FAILED"}
	f.boxes["sbx-d"] = map[string]any{"metadata": map[string]any{"name": "sbx-d", "labels": label("sbx_d", "inst_1")}, "status": "TERMINATED"}
	f.boxes["user-box"] = map[string]any{"metadata": map[string]any{"name": "user-box"}, "status": "DEPLOYED"}
	a := newAdapter(t, f.srv.URL+"/v0")
	list, err := a.List(context.Background(), "inst_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].SandboxID != "sbx_a" || list[1].State != adapter.StateError {
		t.Fatalf("list %+v", list)
	}
}

func TestBlaxelListFailsRatherThanPartial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("cursor") != "" {
			w.WriteHeader(500)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"metadata": map[string]any{"name": "x", "labels": map[string]string{labelInstallation: "inst_1"}}, "status": "DEPLOYED"}}, "meta": map[string]any{"hasMore": true, "nextCursor": "2"}})
	}))
	defer srv.Close()
	if list, err := newAdapter(t, srv.URL).List(context.Background(), "inst_1"); err == nil || !api.IsCode(err, api.CodeProviderUnavailable) {
		t.Fatalf("a failed page must fail List: %v %v", list, err)
	}
}

func TestBlaxelErrorClassification(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"rate limited", 429, `{"code":"RATE_LIMIT_EXCEEDED","message":"slow down"}`, adapter.CodeInsufficientCapacity},
		{"workspace quota", 400, `{"code":"QUOTA_EXCEEDED","message":"sandbox quota reached"}`, adapter.CodeInsufficientCapacity},
		{"bad image", 400, `{"code":"INVALID_IMAGE","message":"Sandbox image nope is not supported"}`, api.CodeInvalidRequest},
		{"ready deadline", 408, `{"code":"CREATION_TIMEOUT","message":"sandbox was not ready in time"}`, adapter.CodeSandboxFailed},
		{"outage", 503, `{"error":"ROUTING_UNAVAILABLE"}`, api.CodeProviderUnavailable},
		{"bad key", 401, `{"error":"AUTHENTICATION_FAILED","code":401}`, api.CodeProviderUnavailable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				fmt.Fprint(w, c.body)
			}))
			defer srv.Close()
			_, err := newAdapter(t, srv.URL).Create(context.Background(), adapter.CreateRequest{ID: "sbx_1", Installation: "inst_1"})
			if !api.IsCode(err, c.want) {
				t.Fatalf("got %v, want %s", err, c.want)
			}
			if strings.Contains(err.Error(), "Bearer") {
				t.Fatalf("error echoes the credential: %v", err)
			}
		})
	}
}

func TestBlaxelCreateFollowsAnEdgeTimeout(t *testing.T) {
	var gets int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			w.WriteHeader(504) // the edge gave up; Blaxel keeps deploying
			fmt.Fprint(w, "<html>Gateway Timeout</html>")
		case r.URL.Path == "/sandboxes/sbx-slow":
			gets++
			json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": "sbx-slow", "labels": map[string]string{labelSandbox: "sbx_SLOW"}}, "status": "DEPLOYED"})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	a := newAdapter(t, srv.URL)
	if inst, err := a.Create(context.Background(), adapter.CreateRequest{ID: "sbx_SLOW", Installation: "inst_1"}); err != nil || inst.State != adapter.StateRunning || gets == 0 {
		t.Fatalf("create after a 504: %+v %v", inst, err)
	}
	if _, err := a.Create(context.Background(), adapter.CreateRequest{ID: "sbx_LOST", Installation: "inst_1"}); !api.IsCode(err, api.CodeProviderUnavailable) {
		t.Fatalf("a 504 for a sandbox that never appears: %v", err)
	}
}

func TestBlaxelFailedSandboxIsSandboxFailed(t *testing.T) {
	f := newFake(t)
	a := newAdapter(t, f.srv.URL+"/v0")
	// The sandbox is accepted, then Blaxel reports it FAILED.
	f.boxes["sbx-bad"] = map[string]any{"metadata": map[string]any{"name": "sbx-bad", "labels": map[string]any{}}, "status": "FAILED", "errors": []any{map[string]any{"message": "out of memory"}}}
	_, err := a.Create(context.Background(), adapter.CreateRequest{ID: "sbx_BAD", Installation: "inst_1"})
	if !api.IsCode(err, adapter.CodeSandboxFailed) || !strings.Contains(err.Error(), "out of memory") {
		t.Fatalf("got %v", err)
	}
	if _, err := a.Create(context.Background(), adapter.CreateRequest{ID: "sbx_2", Template: "bad/image"}); !api.IsCode(err, api.CodeInvalidRequest) {
		t.Fatalf("bad template image: %v", err)
	}
	if _, err := a.Create(context.Background(), adapter.CreateRequest{ID: "sbx_3", FromSnapshot: "snap"}); !api.IsCode(err, api.CodeUnsupportedOperation) {
		t.Fatalf("restore from snapshot: %v", err)
	}
}

func TestBlaxelConfigAndCapabilities(t *testing.T) {
	if _, err := FromConfig(map[string]string{}, "k"); err == nil {
		t.Fatal("a pool without a workspace must be refused")
	}
	a := newAdapter(t, DefaultAPIURL)
	caps, _ := a.Capabilities(context.Background())
	got := adapter.EffectiveFeatures(a, caps.Features)
	for _, f := range []string{adapter.FeatureExec, adapter.FeatureExecStream, adapter.FeatureFiles, adapter.FeaturePorts} {
		if !contains(got, f) {
			t.Errorf("missing %s in %v", f, got)
		}
	}
	for _, f := range []string{adapter.FeaturePauseResume, adapter.FeatureStopStart, adapter.FeatureSnapshotDisk, adapter.FeatureSnapshotMem, adapter.FeatureFork} {
		if contains(got, f) {
			t.Errorf("%s is advertised but Blaxel has no API for it", f)
		}
	}
	if a.Info().Trust != api.TrustExternal {
		t.Fatal("blaxel is an external provider")
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
