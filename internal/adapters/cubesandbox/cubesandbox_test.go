package cubesandbox

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/different-ai/differently/internal/config"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

const apiKey = "cube-key-0123456789"

// fakeCube mimics CubeAPI (from openapi.yml and CubeAPI's routes) and, behind
// CubeProxy's header routing, envd: Connect-streamed process.Process/Start,
// /files and the filesystem RPCs.
type fakeCube struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	creates   int
	boxes     map[string]map[string]any
	templates map[string]bool
	snaps     map[string]bool
	files     map[string][]byte
	dirs      map[string]bool
	pageSize  int
	lastProc  map[string]any
	lastHost  string
	timeouts  map[string]int
	lastBody  map[string]any
}

func newFake(t *testing.T) *fakeCube {
	f := &fakeCube{t: t, boxes: map[string]map[string]any{}, templates: map[string]bool{"tpl-python": true}, snaps: map[string]bool{},
		files: map[string][]byte{}, dirs: map[string]bool{"/tmp": true, "/": true}, timeouts: map[string]int{}}
	jsonOut := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	cubeErr := func(w http.ResponseWriter, code int, msg string) {
		jsonOut(w, code, map[string]any{"code": code, "message": msg})
	}
	connectErr := func(w http.ResponseWriter, status int, code, msg string) {
		jsonOut(w, status, map[string]string{"code": code, "message": msg})
	}

	cube := http.NewServeMux()
	authed := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get(APIKeyHeader) != apiKey {
				cubeErr(w, 401, "invalid API key")
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
			cubeErr(w, 404, "sandbox "+id+" not found")
		}
		return b
	}
	cube.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { jsonOut(w, 200, map[string]string{"status": "ok"}) })
	cube.HandleFunc("GET /templates/{id}", authed(func(w http.ResponseWriter, r *http.Request) {
		if !f.templates[r.PathValue("id")] {
			cubeErr(w, 404, "template not found")
			return
		}
		jsonOut(w, 200, map[string]string{"templateID": r.PathValue("id")})
	}))
	cube.HandleFunc("DELETE /templates/{id}", authed(func(w http.ResponseWriter, r *http.Request) {
		if !f.snaps[r.PathValue("id")] {
			cubeErr(w, 404, "template not found")
			return
		}
		delete(f.snaps, r.PathValue("id"))
		delete(f.templates, r.PathValue("id"))
		w.Header().Set("x-operation-id", "op-1")
		w.WriteHeader(204)
	}))
	cube.HandleFunc("POST /sandboxes", authed(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.lastBody = body
		tpl, _ := body["templateID"].(string)
		if !f.templates[tpl] {
			cubeErr(w, 404, "template "+tpl+" not found")
			return
		}
		f.creates++
		id := fmt.Sprintf("sbx-%d", f.creates)
		f.boxes[id] = map[string]any{"sandboxID": id, "templateID": tpl, "state": "unknown", "metadata": body["metadata"],
			"domain": "cube.test", "startedAt": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339), "envdVersion": "0.2.0"}
		jsonOut(w, 201, map[string]any{"sandboxID": id, "templateID": tpl, "clientID": "node-1", "envdVersion": "0.2.0", "domain": "cube.test", "envdAccessToken": nil})
	}))
	cube.HandleFunc("GET /v2/sandboxes", authed(func(w http.ResponseWriter, r *http.Request) {
		k, v, _ := strings.Cut(r.URL.Query().Get("metadata"), "=")
		var ids []string
		for id, b := range f.boxes {
			md, _ := b["metadata"].(map[string]any)
			if k == "" || md[k] == v {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
		if after, ok := strings.CutPrefix(r.URL.Query().Get("nextToken"), "sbx-after-"); ok {
			ids = slices.DeleteFunc(ids, func(id string) bool { return id <= after })
		}
		items := []any{}
		for _, id := range ids {
			if f.pageSize > 0 && len(items) == f.pageSize {
				w.Header().Set("x-next-token", "sbx-after-"+items[len(items)-1].(map[string]any)["sandboxID"].(string))
				break
			}
			items = append(items, f.boxes[id])
		}
		jsonOut(w, 200, items)
	}))
	cube.HandleFunc("GET /sandboxes/{id}", authed(func(w http.ResponseWriter, r *http.Request) {
		b := box(w, r.PathValue("id"))
		if b == nil {
			return
		}
		switch b["state"] { // transitions complete on the next read
		case "unknown":
			b["state"] = "running"
		case "pausing":
			b["state"] = "paused"
		}
		jsonOut(w, 200, b)
	}))
	cube.HandleFunc("DELETE /sandboxes/{id}", authed(func(w http.ResponseWriter, r *http.Request) {
		if box(w, r.PathValue("id")) == nil {
			return
		}
		delete(f.boxes, r.PathValue("id"))
		w.WriteHeader(204)
	}))
	cube.HandleFunc("POST /sandboxes/{id}/{action}", authed(func(w http.ResponseWriter, r *http.Request) {
		b := box(w, r.PathValue("id"))
		if b == nil {
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.PathValue("action") {
		case "pause":
			b["state"] = "pausing"
			w.WriteHeader(204)
		case "connect":
			b["state"] = "running"
			jsonOut(w, 200, map[string]any{"sandboxID": r.PathValue("id"), "domain": "cube.test"})
		case "timeout":
			f.timeouts[r.PathValue("id")] = int(body["timeout"].(float64))
			w.WriteHeader(204)
		case "snapshots":
			sid := "snap-" + r.PathValue("id")
			f.snaps[sid], f.templates[sid] = true, true
			jsonOut(w, 201, map[string]any{"snapshotID": sid, "names": []string{fmt.Sprint(body["name"])}})
		default:
			cubeErr(w, 404, "no route")
		}
	}))

	envd := http.NewServeMux()
	envd.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	envd.HandleFunc("GET /files", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		b, ok := f.files[r.URL.Query().Get("path")]
		f.mu.Unlock()
		if !ok {
			jsonOut(w, 404, map[string]any{"code": 404, "message": "file not found"})
			return
		}
		_, _ = w.Write(b)
	})
	envd.HandleFunc("POST /files", func(w http.ResponseWriter, r *http.Request) {
		file, _, err := r.FormFile("file")
		if err != nil || r.URL.Query().Get("username") != "root" {
			jsonOut(w, 400, map[string]any{"code": 400, "message": "want a multipart file and a user"})
			return
		}
		b, _ := io.ReadAll(file)
		p := r.URL.Query().Get("path")
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.dirs[p[:strings.LastIndex(p, "/")]] && strings.Count(p, "/") > 1 {
			jsonOut(w, 400, map[string]any{"code": 400, "message": "parent directory does not exist"})
			return
		}
		f.files[p] = b
		jsonOut(w, 200, []map[string]string{{"name": p, "type": "file", "path": p}})
	})
	rpc := func(h func(w http.ResponseWriter, body map[string]any)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Connect-Protocol-Version") != "1" || r.Header.Get("Content-Type") != "application/json" {
				connectErr(w, 415, "invalid_argument", "want Connect JSON")
				return
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			defer f.mu.Unlock()
			h(w, body)
		}
	}
	envd.HandleFunc("POST /filesystem.Filesystem/MakeDir", rpc(func(w http.ResponseWriter, body map[string]any) {
		p := body["path"].(string)
		if f.dirs[p] {
			connectErr(w, 409, "already_exists", "directory exists")
			return
		}
		f.dirs[p] = true
		jsonOut(w, 200, map[string]any{"entry": map[string]any{"name": p}})
	}))
	envd.HandleFunc("POST /filesystem.Filesystem/ListDir", rpc(func(w http.ResponseWriter, body map[string]any) {
		dir := strings.TrimRight(body["path"].(string), "/")
		if !f.dirs[dir] {
			connectErr(w, 404, "not_found", "path "+dir+" not found")
			return
		}
		entries := []any{}
		for p, b := range f.files {
			if p[:strings.LastIndex(p, "/")] == dir {
				entries = append(entries, map[string]any{"name": p[strings.LastIndex(p, "/")+1:], "type": "FILE_TYPE_FILE", "path": p,
					"size": fmt.Sprint(len(b)), "mode": 0o100644, "permissions": "-rw-r--r--", "modifiedTime": "2026-10-28T10:00:00.123Z"})
			}
		}
		jsonOut(w, 200, map[string]any{"entries": entries})
	}))
	envd.HandleFunc("POST /process.Process/Start", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/connect+json" || r.Header.Get("Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("root:")) {
			connectErr(w, 415, "invalid_argument", "want a Connect stream as root")
			return
		}
		var hdr [5]byte
		_, _ = io.ReadFull(r.Body, hdr[:])
		msg := make([]byte, binary.BigEndian.Uint32(hdr[1:]))
		_, _ = io.ReadFull(r.Body, msg)
		var req struct {
			Process map[string]any `json:"process"`
		}
		_ = json.Unmarshal(msg, &req)
		f.mu.Lock()
		f.lastProc = req.Process
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/connect+json")
		send := func(flags byte, v any) {
			b, _ := json.Marshal(v)
			var h [5]byte
			h[0] = flags
			binary.BigEndian.PutUint32(h[1:], uint32(len(b)))
			_, _ = w.Write(append(h[:], b...))
			w.(http.Flusher).Flush()
		}
		data := func(stream, s string) {
			send(0, map[string]any{"event": map[string]any{"data": map[string]string{stream: base64.StdEncoding.EncodeToString([]byte(s))}}})
		}
		end := func(e map[string]any) {
			send(0, map[string]any{"event": map[string]any{"end": e}})
			send(flagEndStream, map[string]any{})
		}
		send(0, map[string]any{"event": map[string]any{"start": map[string]int{"pid": 42}}})
		args, _ := req.Process["args"].([]any)
		script := ""
		if len(args) > 1 {
			script, _ = args[1].(string)
		}
		switch {
		case req.Process["cmd"] == "nope":
			send(flagEndStream, map[string]any{"error": map[string]string{"code": "internal", "message": `failed to start process: exec: "nope": executable file not found in $PATH`}})
		case strings.Contains(script, ".differently-stdin-"):
			file := strings.Trim(strings.Fields(script[strings.Index(script, "< ")+2:])[0], "';")
			f.mu.Lock()
			in := f.files[file]
			delete(f.files, file)
			f.mu.Unlock()
			data("stdout", strings.ToUpper(string(in)))
			end(map[string]any{"exited": true, "status": "exit status 0"})
		case strings.Contains(script, "sleep"):
			time.Sleep(50 * time.Millisecond)
			send(flagEndStream, map[string]any{"error": map[string]string{"code": "deadline_exceeded", "message": "deadline exceeded"}})
		case strings.Contains(script, "exit 3"):
			data("stdout", "hello\n")
			data("stderr", "warn\n")
			end(map[string]any{"exitCode": 3, "exited": true, "status": "exit status 3"})
		default:
			data("stdout", "ok\n")
			end(map[string]any{"exited": true, "status": "exit status 0"}) // proto3 JSON drops exitCode 0
		}
	})

	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.lastHost = r.Host
		f.mu.Unlock()
		if r.Header.Get("E2b-Sandbox-Id") != "" || strings.HasPrefix(r.Host, "49983-") {
			id := r.Header.Get("E2b-Sandbox-Id")
			if id == "" {
				id = strings.TrimPrefix(strings.Split(r.Host, ".")[0], "49983-")
			}
			f.mu.Lock()
			b, ok := f.boxes[id]
			running := ok && b["state"] == "running"
			f.mu.Unlock()
			if !running {
				http.Error(w, "sandbox not running", http.StatusBadGateway)
				return
			}
			envd.ServeHTTP(w, r)
			return
		}
		cube.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCube) adapter(t *testing.T, cfg map[string]string) *Adapter {
	t.Helper()
	full := map[string]string{"apiURL": f.srv.URL, "templateId": "tpl-python", "sandboxURL": f.srv.URL}
	for k, v := range cfg {
		full[k] = v
	}
	o, err := FromConfig(full, api.TrustPrivate, apiKey)
	if err != nil {
		t.Fatal(err)
	}
	o.ReadyTimeout = 3 * time.Second
	a, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestFromConfig(t *testing.T) {
	for cfg, want := range map[string]string{
		`{}`: "config.apiURL",
		`{"apiURL":"http://c","proxyScheme":"ftp"}`: "proxyScheme must be https or http",
		`{"apiURL":"http://c","sandboxURL":"c:80"}`: "sandboxURL must start with http",
	} {
		var m map[string]string
		_ = json.Unmarshal([]byte(cfg), &m)
		if _, err := FromConfig(m, api.TrustPrivate, ""); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", cfg, want, err)
		}
	}
	o, err := FromConfig(map[string]string{"apiURL": "http://c/"}, api.TrustPrivate, "")
	if err != nil || o.APIURL != "http://c" || o.ProxyScheme != "https" || o.User != "root" || !slices.Equal(o.Features, DefaultFeatures) {
		t.Fatalf("defaults %+v %v", o, err)
	}
	a, _ := New(o)
	caps, _ := a.Capabilities(context.Background())
	if got := adapter.EffectiveFeatures(a, caps.Features); !slices.Equal(got, []string{"exec", "exec.stream", "files", "fork", "isolation.microvm", "lifecycle.pause-resume", "snapshots.disk", "snapshots.memory"}) {
		t.Fatalf("effective features %v", got)
	}

	// The operator must say who runs CubeSandbox, and where its API is.
	for yaml, want := range map[string]string{
		"pools:\n  - {name: cube, adapter: cubesandbox, config: {apiURL: 'http://c:3000'}}\n": "set trust explicitly",
		"pools:\n  - {name: cube, adapter: cubesandbox, trust: private}\n":                    "needs config.apiURL",
	} {
		if _, err := config.Parse([]byte(yaml)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: want %q, got %v", yaml, want, err)
		}
	}
	if cfg, err := config.Parse([]byte("pools:\n  - {name: cube, adapter: cubesandbox, trust: private, config: {apiURL: 'http://c:3000'}}\n")); err != nil || cfg.Pools[0].Trust != api.TrustPrivate {
		t.Fatalf("explicit trust: %v", err)
	}
}

func TestHealth(t *testing.T) {
	f := newFake(t)
	if err := f.adapter(t, nil).Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := f.adapter(t, map[string]string{"templateId": "tpl-gone"}).Health(context.Background()); !api.IsCode(err, api.CodeProviderUnavailable) || !strings.Contains(err.Error(), "tpl-gone not found") {
		t.Fatalf("missing template: %v", err)
	}
	o, _ := FromConfig(map[string]string{"apiURL": f.srv.URL, "templateId": "tpl-python"}, api.TrustPrivate, "wrong-key-abcdef")
	a, _ := New(o)
	if err := a.Health(context.Background()); !api.IsCode(err, api.CodeProviderUnavailable) || strings.Contains(err.Error(), "wrong-key") {
		t.Fatalf("bad key: %v", err)
	}
}

func TestLifecycle(t *testing.T) {
	f := newFake(t)
	a := f.adapter(t, nil)
	ctx := context.Background()
	req := adapter.CreateRequest{ID: "sbx_01ABC", Installation: "inst_1", Env: map[string]string{"A": "1"}, TTLSeconds: 600}
	in, err := a.Create(ctx, req)
	if err != nil || in.Ref != "sbx-1" || in.SandboxID != "sbx_01ABC" || in.State != adapter.StateRunning {
		t.Fatalf("create %+v %v", in, err)
	}
	b, _ := json.Marshal(f.lastBody)
	if string(b) != `{"autoPause":false,"envVars":{"A":"1"},"metadata":{"differently.installation":"inst_1","differently.sandbox-id":"sbx_01ABC"},"templateID":"tpl-python","timeout":600}` {
		t.Fatalf("create body %s", b)
	}
	again, err := a.Create(ctx, req)
	if err != nil || again.Ref != in.Ref || f.creates != 1 {
		t.Fatalf("create is not idempotent: %+v %v (%d creates)", again, err, f.creates)
	}

	for _, id := range []string{"sbx_b", "sbx_c"} {
		if _, err := a.Create(ctx, adapter.CreateRequest{ID: id, Installation: "inst_1"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_other", Installation: "inst_2"}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.pageSize = 2
	f.mu.Unlock()
	list, err := a.List(ctx, "inst_1")
	if err != nil || len(list) != 3 || list[2].SandboxID != "sbx_c" {
		t.Fatalf("paged list %+v %v", list, err)
	}

	if err := a.Pause(ctx, in.Ref); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.Get(ctx, in.Ref); got.State != adapter.StatePaused {
		t.Fatalf("after pause %+v", got)
	}
	if err := a.Resume(ctx, in.Ref); err != nil {
		t.Fatal(err)
	}

	// The idle timeout is set from the start, so it cannot fire before the TTL.
	if err := a.SetExpiry(ctx, in.Ref, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := f.timeouts[in.Ref]; got < 3660 || got > 3662 {
		t.Fatalf("timeout %d, want an hour plus the minute since the start", got)
	}

	snap, err := a.Snapshot(ctx, in.Ref, "disk")
	if err != nil || snap.Ref != "snap-sbx-1" || snap.Kind != "memory" {
		t.Fatalf("snapshot %+v %v", snap, err)
	}
	fork, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_fork", Installation: "inst_1", FromSnapshot: snap.Ref})
	if err != nil || f.lastBody["templateID"] != snap.Ref {
		t.Fatalf("fork %+v %v (%v)", fork, err, f.lastBody)
	}
	if err := a.DeleteSnapshot(ctx, snap.Ref); err != nil {
		t.Fatal(err)
	}
	if err := a.DeleteSnapshot(ctx, snap.Ref); err != nil {
		t.Fatalf("delete snapshot is idempotent: %v", err)
	}

	if err := a.Delete(ctx, in.Ref); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Get(ctx, in.Ref); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	if err := a.Delete(ctx, in.Ref); err != nil {
		t.Fatalf("delete is idempotent: %v", err)
	}

	_, err = a.Create(ctx, adapter.CreateRequest{ID: "sbx_bad", Installation: "inst_1", Template: "tpl-missing"})
	if !api.IsCode(err, api.CodeInvalidRequest) {
		t.Fatalf("missing template: want invalid_request, got %v", err)
	}
}

func TestExec(t *testing.T) {
	f := newFake(t)
	a := f.adapter(t, nil)
	ctx := context.Background()
	in, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_x", Installation: "inst_1"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := a.Exec(ctx, in.Ref, api.ExecRequest{Command: "echo hi; exit 3", Env: map[string]string{"A": "1"}, Cwd: "/srv"})
	if err != nil || res.ExitCode != 3 || res.Stdout != "hello\n" || res.Stderr != "warn\n" {
		t.Fatalf("exec %+v %v", res, err)
	}
	b, _ := json.Marshal(f.lastProc)
	if string(b) != `{"args":["-c","echo hi; exit 3"],"cmd":"/bin/sh","cwd":"/srv","envs":{"A":"1"}}` {
		t.Fatalf("process %s", b)
	}
	if res, err := a.Exec(ctx, in.Ref, api.ExecRequest{Args: []string{"ls", "-l"}}); err != nil || res.ExitCode != 0 || res.Stdout != "ok\n" {
		t.Fatalf("exit 0 without an exitCode field: %+v %v", res, err)
	}
	if b, _ := json.Marshal(f.lastProc); string(b) != `{"args":["-l"],"cmd":"ls"}` {
		t.Fatalf("args run without a shell: %s", b)
	}
	if res, err := a.Exec(ctx, in.Ref, api.ExecRequest{Args: []string{"nope"}}); err != nil || res.ExitCode != 127 {
		t.Fatalf("missing binary: %+v %v", res, err)
	}
	if res, err := a.Exec(ctx, in.Ref, api.ExecRequest{Command: "sleep 30", TimeoutSeconds: 1}); err != nil || res.ExitCode != 124 {
		t.Fatalf("timeout: %+v %v", res, err)
	}
	res, err = a.Exec(ctx, in.Ref, api.ExecRequest{Command: "tr a-z A-Z", Stdin: "hello\n"})
	if err != nil || res.Stdout != "HELLO\n" {
		t.Fatalf("stdin %+v %v", res, err)
	}
	var events []api.ExecEvent
	code, err := a.ExecStream(ctx, in.Ref, api.ExecRequest{Command: "exit 3"}, func(ev api.ExecEvent) error { events = append(events, ev); return nil })
	if err != nil || code != 3 || len(events) != 2 || events[0] != (api.ExecEvent{Stream: "stdout", Data: "hello\n"}) {
		t.Fatalf("stream %d %+v %v", code, events, err)
	}
	if err := a.Pause(ctx, in.Ref); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Exec(ctx, in.Ref, api.ExecRequest{Command: "true"}); !api.IsCode(err, api.CodeProviderUnavailable) {
		t.Fatalf("exec on a paused sandbox: %v", err)
	}
}

func TestFiles(t *testing.T) {
	f := newFake(t)
	a := f.adapter(t, nil)
	ctx := context.Background()
	in, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_f", Installation: "inst_1"})
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte{0, 1, 2, 255}, 4096)
	if err := a.WriteFile(ctx, in.Ref, "/work/data.bin", data); err != nil {
		t.Fatal(err)
	}
	if err := a.WriteFile(ctx, in.Ref, "/work/b.txt", []byte("b")); err != nil {
		t.Fatalf("second file in an existing directory: %v", err)
	}
	back, err := a.ReadFile(ctx, in.Ref, "/work/data.bin")
	if err != nil || !bytes.Equal(back, data) {
		t.Fatalf("read %d bytes %v", len(back), err)
	}
	if _, err := a.ReadFile(ctx, in.Ref, "/work/none"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("read missing: %v", err)
	}
	files, err := a.ListFiles(ctx, in.Ref, "/work")
	slices.SortFunc(files, func(x, y api.FileInfo) int { return strings.Compare(x.Name, y.Name) })
	if err != nil || len(files) != 2 || files[1].Path != "/work/data.bin" || files[1].Size != int64(len(data)) || files[1].Mode != "644" || files[1].IsDir || files[1].ModTime.IsZero() {
		t.Fatalf("list %+v %v", files, err)
	}
	if _, err := a.ListFiles(ctx, in.Ref, "/nowhere"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("list missing: %v", err)
	}
}

func TestHostRoutingAndPorts(t *testing.T) {
	f := newFake(t)
	a := f.adapter(t, map[string]string{"sandboxURL": "", "proxyScheme": "http"})
	// Resolve every <port>-<id>.<domain> name to the fake, as wildcard DNS would.
	addr := f.srv.Listener.Addr().String()
	a.data.HTTP.Transport = &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	ctx := context.Background()
	in, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_h", Installation: "inst_1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Exec(ctx, in.Ref, api.ExecRequest{Command: "true"}); err != nil || f.lastHost != "49983-"+in.Ref+".cube.test" {
		t.Fatalf("host routing: %q %v", f.lastHost, err)
	}
	if u, err := a.ExposePort(ctx, in.Ref, 8080); err != nil || u != "http://8080-"+in.Ref+".cube.test" {
		t.Fatalf("host port %q %v", u, err)
	}
	if u, err := f.adapter(t, nil).ExposePort(ctx, in.Ref, 8080); err != nil || u != f.srv.URL+"/sandbox/"+in.Ref+"/8080" {
		t.Fatalf("path port %q %v", u, err)
	}
	if u, err := f.adapter(t, map[string]string{"sandboxURL": "", "domain": "sbx.example"}).ExposePort(ctx, in.Ref, 3000); err != nil || u != "https://3000-"+in.Ref+".sbx.example" {
		t.Fatalf("configured domain %q %v", u, err)
	}
}

// A request too large for one Connect message is refused before it is framed.
func TestEnvelopeRefusesOversizedRequest(t *testing.T) {
	if _, err := envelope(map[string]string{"cmd": strings.Repeat("x", maxMessage)}); !api.IsCode(err, api.CodeInvalidRequest) {
		t.Fatalf("oversized request: %v", err)
	}
	if _, err := envelope(map[string]string{"cmd": "ls"}); err != nil {
		t.Fatalf("small request: %v", err)
	}
}
