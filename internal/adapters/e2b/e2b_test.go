package e2b

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// fakeE2B serves the control plane and, on the same host, envd as reached
// through E2B's shared sandbox host.
type fakeE2B struct {
	mu        sync.Mutex
	next      int
	creates   []map[string]any
	boxes     map[string]map[string]any
	tokens    map[string]string
	files     map[string][]byte
	starts    []map[string]any
	stdin     chan string
	timeouts  []map[string]any
	snapshots map[string]bool
	srv       *httptest.Server
}

func e2bError(w http.ResponseWriter, status int, msg, code string) {
	w.WriteHeader(status)
	body := map[string]any{"code": status, "message": msg}
	if code != "" {
		body["error_code"] = code
	}
	json.NewEncoder(w).Encode(body)
}

func connectEnvelope(w io.Writer, flags byte, v any) {
	b, _ := json.Marshal(v)
	frame := envelope(b)
	frame[0] = flags
	w.Write(frame)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func newFake(t *testing.T) *fakeE2B {
	f := &fakeE2B{boxes: map[string]map[string]any{}, tokens: map[string]string{}, files: map[string][]byte{}, snapshots: map[string]bool{}, stdin: make(chan string, 1)}
	mux := http.NewServeMux()
	cp := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-API-Key") != "e2b_test" || r.Header.Get("Authorization") != "" {
				e2bError(w, 401, "Invalid API key", "")
				return
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			h(w, r)
		}
	}
	// envd checks the sandbox's own token and never sees the API key.
	envd := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("E2b-Sandbox-Id")
			f.mu.Lock()
			box, tok := f.boxes[id], f.tokens[id]
			f.mu.Unlock()
			switch {
			case r.Header.Get("E2b-Sandbox-Port") != "49983" || r.Header.Get("X-API-Key") != "":
				t.Errorf("envd request headers %v", r.Header)
			case box == nil || box["state"] != "running":
				w.WriteHeader(502)
				w.Write([]byte("<html>Sandbox not found</html>"))
				return
			case r.Header.Get("X-Access-Token") != tok:
				e2bError(w, 401, "unauthorized access, please provide a valid access token", "")
				return
			}
			h(w, r)
		}
	}
	sandboxJSON := func(b map[string]any, detail bool) map[string]any {
		out := map[string]any{"sandboxID": b["sandboxID"], "templateID": b["templateID"], "metadata": b["metadata"], "state": b["state"], "domain": nil}
		if detail {
			out["envdAccessToken"] = f.tokens[b["sandboxID"].(string)]
		}
		return out
	}
	mux.HandleFunc("POST /v2/sandboxes", cp(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch body["templateID"] {
		case "missing":
			e2bError(w, 404, "template 'missing' not found", "")
			return
		case "private":
			e2bError(w, 403, "you don't have access to template 'private'", "")
			return
		case "full":
			e2bError(w, 503, "no capacity", "sandbox_capacity_unavailable")
			return
		case "broken":
			e2bError(w, 500, "failed to create sandbox", "sandbox_create_failed")
			return
		}
		if _, ok := body["timeout"].(float64); !ok {
			e2bError(w, 400, "timeout must be an integer", "")
			return
		}
		f.creates = append(f.creates, body)
		f.next++
		id := fmt.Sprintf("i%019d", f.next)
		f.boxes[id] = map[string]any{"sandboxID": id, "templateID": body["templateID"], "metadata": body["metadata"], "state": "running"}
		f.tokens[id] = "tok-" + id
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{"sandboxID": id, "templateID": body["templateID"], "envdVersion": "0.6.10", "envdAccessToken": f.tokens[id], "domain": nil})
	}))
	mux.HandleFunc("GET /v2/sandboxes", cp(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != "running,paused" {
			t.Errorf("state filter %q", q.Get("state"))
		}
		// As the server does: unescape, split into pairs, unescape each.
		want := map[string]string{}
		raw, _ := url.QueryUnescape(q.Get("metadata"))
		for _, pair := range strings.Split(raw, "&") {
			if k, v, ok := strings.Cut(pair, "="); ok {
				k, _ = url.QueryUnescape(k)
				v, _ = url.QueryUnescape(v)
				want[k] = v
			}
		}
		var match []map[string]any
		for i := 1; i <= f.next; i++ {
			b := f.boxes[fmt.Sprintf("i%019d", i)]
			if b == nil {
				continue
			}
			meta, _ := b["metadata"].(map[string]any)
			ok := true
			for k, v := range want {
				if meta[k] != v {
					ok = false
				}
			}
			if ok {
				match = append(match, sandboxJSON(b, false))
			}
		}
		start := 0
		fmt.Sscan(q.Get("nextToken"), &start)
		out := []map[string]any{}
		if start < len(match) {
			out = match[start : start+1] // one per page to exercise paging
			if start+1 < len(match) {
				w.Header().Set("X-Next-Token", fmt.Sprint(start+1))
			}
		}
		json.NewEncoder(w).Encode(out)
	}))
	mux.HandleFunc("GET /sandboxes/{id}", cp(func(w http.ResponseWriter, r *http.Request) {
		b := f.boxes[r.PathValue("id")]
		if b == nil {
			e2bError(w, 404, "sandbox doesn't exist", "")
			return
		}
		json.NewEncoder(w).Encode(sandboxJSON(b, true))
	}))
	mux.HandleFunc("DELETE /sandboxes/{id}", cp(func(w http.ResponseWriter, r *http.Request) {
		if f.boxes[r.PathValue("id")] == nil {
			e2bError(w, 404, "sandbox doesn't exist", "")
			return
		}
		delete(f.boxes, r.PathValue("id"))
		w.WriteHeader(204)
	}))
	mux.HandleFunc("POST /sandboxes/{id}/pause", cp(func(w http.ResponseWriter, r *http.Request) {
		b := f.boxes[r.PathValue("id")]
		if b["state"] == "paused" {
			e2bError(w, 409, "sandbox is already paused", "")
			return
		}
		b["state"] = "paused"
		w.WriteHeader(204)
	}))
	mux.HandleFunc("POST /v2/sandboxes/{id}/connect", cp(func(w http.ResponseWriter, r *http.Request) {
		b := f.boxes[r.PathValue("id")]
		b["state"] = "running"
		f.tokens[r.PathValue("id")] += "-resumed"
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{"sandboxID": r.PathValue("id"), "envdAccessToken": f.tokens[r.PathValue("id")], "domain": nil})
	}))
	mux.HandleFunc("POST /sandboxes/{id}/timeout", cp(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.timeouts = append(f.timeouts, body)
		w.WriteHeader(204)
	}))
	mux.HandleFunc("POST /sandboxes/{id}/snapshots", cp(func(w http.ResponseWriter, r *http.Request) {
		id := "team-slug/" + r.PathValue("id") + ":default"
		f.snapshots[id] = true
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{"snapshotID": id, "names": []string{id}})
	}))
	mux.HandleFunc("DELETE /templates/{id}", cp(func(w http.ResponseWriter, r *http.Request) {
		if !f.snapshots[r.PathValue("id")] {
			e2bError(w, 404, "template not found", "")
			return
		}
		delete(f.snapshots, r.PathValue("id"))
		w.WriteHeader(204)
	}))

	mux.HandleFunc("POST /process.Process/Start", envd(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/connect+json" || r.Header.Get("Connect-Protocol-Version") != "1" {
			w.WriteHeader(415)
			return
		}
		flags, msg, err := readEnvelope(r.Body)
		if err != nil || flags != 0 {
			t.Errorf("request envelope: %v %d", err, flags)
			return
		}
		var req struct {
			Process struct {
				Cmd  string            `json:"cmd"`
				Args []string          `json:"args"`
				Envs map[string]string `json:"envs"`
				Cwd  string            `json:"cwd"`
			} `json:"process"`
			Stdin *bool `json:"stdin"`
		}
		_ = json.Unmarshal(msg, &req)
		if req.Stdin == nil || req.Process.Cmd != "/bin/bash" || len(req.Process.Args) != 3 || req.Process.Args[1] != "-c" {
			t.Errorf("start request %s", msg)
		}
		var raw map[string]any
		_ = json.Unmarshal(msg, &raw)
		f.mu.Lock()
		f.starts = append(f.starts, raw)
		f.mu.Unlock()
		cmd := req.Process.Args[2]
		w.Header().Set("Content-Type", "application/connect+json")
		w.WriteHeader(200)
		data := func(stream, s string) map[string]any {
			return map[string]any{"event": map[string]any{"data": map[string]any{stream: base64.StdEncoding.EncodeToString([]byte(s))}}}
		}
		connectEnvelope(w, 0, map[string]any{"event": map[string]any{"start": map[string]any{"pid": 42}}})
		switch {
		case *req.Stdin:
			select {
			case in := <-f.stdin:
				connectEnvelope(w, 0, data("stdout", in))
			case <-time.After(2 * time.Second):
				t.Error("stdin never closed")
			}
			connectEnvelope(w, 0, map[string]any{"event": map[string]any{"end": map[string]any{"exited": true, "status": "exit status 0"}}})
		case strings.HasPrefix(cmd, "sleep"):
			if r.Header.Get("Connect-Timeout-Ms") != "1000" {
				t.Errorf("timeout header %q", r.Header.Get("Connect-Timeout-Ms"))
			}
			connectEnvelope(w, 0, map[string]any{"event": map[string]any{"end": map[string]any{"exitCode": -1, "status": "signal: killed"}}})
			connectEnvelope(w, 2, map[string]any{"error": map[string]any{"code": "deadline_exceeded", "message": "deadline exceeded"}})
			return
		case cmd == "bad-cwd":
			connectEnvelope(w, 2, map[string]any{"error": map[string]any{"code": "invalid_argument", "message": "cwd '/nope' does not exist"}})
			return
		default:
			connectEnvelope(w, 0, map[string]any{"event": map[string]any{"keepalive": map[string]any{}}})
			connectEnvelope(w, 0, data("stdout", "ran: "+cmd+"\n"))
			connectEnvelope(w, 0, data("stderr", "cwd="+req.Process.Cwd+" A="+req.Process.Envs["A"]))
			connectEnvelope(w, 0, map[string]any{"event": map[string]any{"end": map[string]any{"exitCode": 7, "exited": true, "status": "exit status 7"}}})
		}
		connectEnvelope(w, 2, map[string]any{})
	}))
	var input []byte
	mux.HandleFunc("POST /process.Process/SendInput", envd(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Process struct {
				PID int `json:"pid"`
			} `json:"process"`
			Input struct {
				Stdin string `json:"stdin"`
			} `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if r.Header.Get("Content-Type") != "application/json" || req.Process.PID != 42 {
			t.Errorf("SendInput %+v", req)
		}
		input, _ = base64.StdEncoding.DecodeString(req.Input.Stdin)
		w.Write([]byte(`{}`))
	}))
	mux.HandleFunc("POST /process.Process/CloseStdin", envd(func(w http.ResponseWriter, r *http.Request) {
		f.stdin <- string(input)
		w.Write([]byte(`{}`))
	}))
	mux.HandleFunc("POST /files", envd(func(w http.ResponseWriter, r *http.Request) {
		mr, err := r.MultipartReader()
		if err != nil {
			e2bError(w, 400, "expected multipart", "")
			return
		}
		part, err := mr.NextPart()
		if err != nil || part.FormName() != "file" {
			e2bError(w, 400, "missing file", "")
			return
		}
		b, _ := io.ReadAll(part)
		path := r.URL.Query().Get("path")
		if cd := part.Header.Get("Content-Disposition"); !strings.Contains(cd, `filename="`+path+`"`) {
			t.Errorf("part %q for %q", cd, path)
		}
		f.mu.Lock()
		f.files[path] = b
		f.mu.Unlock()
		json.NewEncoder(w).Encode([]any{map[string]any{"path": path, "name": path[strings.LastIndex(path, "/")+1:], "type": "file"}})
	}))
	mux.HandleFunc("GET /files", envd(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		b, ok := f.files[r.URL.Query().Get("path")]
		f.mu.Unlock()
		if !ok {
			e2bError(w, 404, "path not found", "")
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(b)
	}))
	mux.HandleFunc("POST /filesystem.Filesystem/ListDir", envd(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["path"] != "/home/user" {
			w.WriteHeader(404)
			w.Write([]byte(`{"code":"not_found","message":"directory not found"}`))
			return
		}
		w.Write([]byte(`{"entries":[{"name":"src","type":"FILE_TYPE_DIRECTORY","path":"/home/user/src","mode":2147484141,"modifiedTime":"2026-10-28T10:00:00.5Z"},
			{"name":"a.txt","type":"FILE_TYPE_FILE","path":"/home/user/a.txt","size":"5","mode":420,"modifiedTime":"2026-10-28T10:00:00Z"}]}`))
	}))
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func newAdapter(f *fakeE2B, cfg map[string]string) *Adapter {
	if cfg == nil {
		cfg = map[string]string{}
	}
	cfg["apiURL"] = f.srv.URL
	o := FromConfig(cfg, "e2b_test")
	o.SandboxURL = f.srv.URL
	return New(o)
}

func TestE2BLifecycle(t *testing.T) {
	f := newFake(t)
	a := newAdapter(f, map[string]string{"template": "my-template", "maxLifetimeSeconds": "86400", "ports": "public"})
	ctx := context.Background()
	req := adapter.CreateRequest{ID: "sbx_1", Installation: "inst_1", TTLSeconds: 600, Env: map[string]string{"TOKEN": "s"}, Labels: map[string]string{adapter.LabelProject: "prj_1"}}
	inst, err := a.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != adapter.StateRunning || inst.SandboxID != "sbx_1" {
		t.Fatalf("instance %+v", inst)
	}
	c := f.creates[0]
	meta := c["metadata"].(map[string]any)
	if c["templateID"] != "my-template" || c["timeout"] != float64(600) || c["autoPause"] != false || c["envVars"].(map[string]any)["TOKEN"] != "s" ||
		meta[metaInstallation] != "inst_1" || meta[metaProject] != "prj_1" || c["network"].(map[string]any)["allowPublicTraffic"] != true {
		t.Fatalf("create body %v", c)
	}
	if again, err := a.Create(ctx, req); err != nil || again.Ref != inst.Ref || len(f.creates) != 1 {
		t.Fatalf("replayed create made %d sandboxes (%v)", len(f.creates), err)
	}

	res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Args: []string{"echo", "hi there"}, Cwd: "/home/user", Env: map[string]string{"A": "1"}})
	if err != nil || res.ExitCode != 7 || res.Stdout != "ran: echo 'hi there'\n" || res.Stderr != "cwd=/home/user A=1" {
		t.Fatalf("exec %+v %v", res, err)
	}
	if f.starts[0]["stdin"] != false {
		t.Fatalf("stdin must be sent as false: %v", f.starts[0])
	}
	if res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "cat", Stdin: "piped input"}); err != nil || res.Stdout != "piped input" || res.ExitCode != 0 {
		t.Fatalf("stdin exec %+v %v", res, err)
	}
	if res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "sleep 9", TimeoutSeconds: 1}); err != nil || res.ExitCode != 124 {
		t.Fatalf("timeout %+v %v", res, err)
	}
	if _, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "bad-cwd"}); !api.IsCode(err, api.CodeInvalidRequest) {
		t.Fatalf("connect error: %v", err)
	}
	var events []api.ExecEvent
	if code, err := a.ExecStream(ctx, inst.Ref, api.ExecRequest{Command: "make"}, func(e api.ExecEvent) error { events = append(events, e); return nil }); err != nil || code != 7 || len(events) != 2 || events[1].Stream != "stderr" {
		t.Fatalf("stream %v %d %v", events, code, err)
	}

	if err := a.WriteFile(ctx, inst.Ref, "/home/user/src/a.bin", []byte("x\x00y")); err != nil {
		t.Fatal(err)
	}
	if b, err := a.ReadFile(ctx, inst.Ref, "/home/user/src/a.bin"); err != nil || string(b) != "x\x00y" {
		t.Fatalf("read %q %v", b, err)
	}
	if _, err := a.ReadFile(ctx, inst.Ref, "/nope"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("missing file: %v", err)
	}
	ls, err := a.ListFiles(ctx, inst.Ref, "/home/user")
	if err != nil || len(ls) != 2 || !ls[0].IsDir || ls[0].Mode != "0755" || ls[1].Size != 5 || ls[1].Mode != "0644" || ls[0].ModTime.IsZero() {
		t.Fatalf("list files %+v %v", ls, err)
	}
	if _, err := a.ListFiles(ctx, inst.Ref, "/nope"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("missing dir: %v", err)
	}

	if err := a.Pause(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	if err := a.Pause(ctx, inst.Ref); err != nil {
		t.Fatalf("pausing a paused sandbox: %v", err)
	}
	if got, _ := a.Get(ctx, inst.Ref); got.State != adapter.StatePaused {
		t.Fatalf("after pause %+v", got)
	}
	if _, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "true"}); !api.IsCode(err, api.CodeInvalidState) {
		t.Fatalf("exec on a paused sandbox: %v", err)
	}
	if err := a.Resume(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	// The resumed sandbox has a new envd token; exec must use it.
	if res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "true"}); err != nil || res.ExitCode != 7 {
		t.Fatalf("exec after resume %+v %v", res, err)
	}
	// A fresh adapter (another replica) fetches the token itself.
	if res, err := newAdapter(f, nil).Exec(ctx, inst.Ref, api.ExecRequest{Command: "true"}); err != nil || res.ExitCode != 7 {
		t.Fatalf("exec from a fresh adapter %+v %v", res, err)
	}
	// Another replica resumed it and the token changed: a refused token is
	// fetched again once.
	f.mu.Lock()
	f.tokens[inst.Ref] += "-elsewhere"
	f.mu.Unlock()
	if res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "true"}); err != nil || res.ExitCode != 7 {
		t.Fatalf("exec with a stale token %+v %v", res, err)
	}
	if err := a.SetExpiry(ctx, inst.Ref, time.Now().Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if s := f.timeouts[0]["timeout"].(float64); s < 1790 || s > 1800 {
		t.Fatalf("timeout %v", s)
	}

	snap, err := a.Snapshot(ctx, inst.Ref, "disk")
	if err != nil || snap.Kind != "memory" || !strings.Contains(snap.Ref, "/") {
		t.Fatalf("snapshot %+v %v", snap, err)
	}
	restored, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_2", Installation: "inst_1", FromSnapshot: snap.Ref})
	if err != nil || f.creates[1]["templateID"] != snap.Ref || f.creates[1]["timeout"] != float64(86400) {
		t.Fatalf("restore %v %v", f.creates[1], err)
	}
	if err := a.DeleteSnapshot(ctx, snap.Ref); err != nil || len(f.snapshots) != 0 {
		t.Fatalf("delete snapshot %v (%v)", err, f.snapshots)
	}
	if err := a.DeleteSnapshot(ctx, snap.Ref); err != nil {
		t.Fatalf("idempotent snapshot delete: %v", err)
	}
	if u, err := a.ExposePort(ctx, inst.Ref, 3000); err != nil || u != "https://3000-"+inst.Ref+".e2b.app" {
		t.Fatalf("port %q %v", u, err)
	}

	if list, err := a.List(ctx, "inst_1"); err != nil || len(list) != 2 || list[1].SandboxID != "sbx_2" {
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
	if !caps.Has(adapter.FeaturePorts) || caps.Has(adapter.FeatureStopStart) || len(adapter.EffectiveFeatures(a, caps.Features)) != len(caps.Features) {
		t.Fatalf("features %v", caps.Features)
	}
}

func TestE2BDefaults(t *testing.T) {
	f := newFake(t)
	a := newAdapter(f, nil)
	ctx := context.Background()
	inst, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_1", Installation: "i", TTLSeconds: 7200})
	if err != nil {
		t.Fatal(err)
	}
	c := f.creates[0]
	// The TTL backstop never exceeds the plan's maximum runtime, ports stay private.
	if c["templateID"] != "base" || c["timeout"] != float64(3600) || c["network"].(map[string]any)["allowPublicTraffic"] != false || c["allow_internet_access"] != true {
		t.Fatalf("create body %v", c)
	}
	caps, _ := a.Capabilities(ctx)
	if caps.Has(adapter.FeaturePorts) {
		t.Fatal("private ports must not advertise ports.expose")
	}
	if _, err := a.ExposePort(ctx, inst.Ref, 80); !api.IsCode(err, api.CodeUnsupportedOperation) {
		t.Fatalf("expose with private ports: %v", err)
	}
	if _, err := newAdapter(f, map[string]string{"egress": "none"}).Create(ctx, adapter.CreateRequest{ID: "sbx_2", Installation: "i"}); err != nil || f.creates[1]["allow_internet_access"] != false {
		t.Fatalf("egress none: %v %v", f.creates[1], err)
	}
}

func TestE2BErrorClassification(t *testing.T) {
	f := newFake(t)
	a := newAdapter(f, nil)
	ctx := context.Background()
	for template, want := range map[string]string{
		"missing": api.CodeInvalidRequest,
		"private": api.CodeInvalidRequest,
		"full":    adapter.CodeInsufficientCapacity,
		"broken":  adapter.CodeSandboxFailed,
	} {
		if _, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_" + template, Installation: "i", Template: template}); !api.IsCode(err, want) {
			t.Errorf("template %s: got %v, want %s", template, err, want)
		}
	}
	cases := []struct {
		status int
		want   string
	}{
		{429, adapter.CodeInsufficientCapacity},
		{503, adapter.CodeInsufficientCapacity},
		{401, api.CodeProviderUnavailable},
		{500, api.CodeProviderUnavailable},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { e2bError(w, c.status, "nope", "") }))
		err := New(Options{APIURL: srv.URL, APIKey: "e2b_secret"}).Health(ctx)
		if !api.IsCode(err, c.want) || strings.Contains(err.Error(), "e2b_secret") {
			t.Errorf("%d: got %v, want %s", c.status, err, c.want)
		}
		srv.Close()
	}
}
