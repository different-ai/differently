package azuresessions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// fakePool follows the dynamic sessions data-plane spec
// (2025-10-02-preview) for a Python code-interpreter pool: sessions are
// allocated on first use, executions run the code with the host's python3.
type fakePool struct {
	t        *testing.T
	py       string
	mu       sync.Mutex
	sessions map[string]bool
	files    map[string][]byte // identifier + "/" + path
	lastExec map[string]any
	srv      *httptest.Server
}

func newFake(t *testing.T) *fakePool {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed; the fake runs the adapter's Python programs")
	}
	f := &fakePool{t: t, py: py, sessions: map[string]bool{}, files: map[string][]byte{}}
	notFound := func(w http.ResponseWriter) {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "SessionWithIdentifierNotFound", "message": "session not found"}})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /tenant-1/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.PostForm.Get("scope") != Scope {
			t.Errorf("scope %q", r.PostForm.Get("scope"))
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3599})
	})
	mux.HandleFunc("GET /metadata", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"runtimes":[],"preInstalledPackages":[]}`))
	})
	mux.HandleFunc("POST /executions", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		id := r.URL.Query().Get("identifier")
		f.mu.Lock()
		f.sessions[id] = true // allocated on first use
		f.lastExec = body
		f.mu.Unlock()
		if body["codeInputType"] != "Inline" || body["executionType"] != "Synchronous" {
			t.Errorf("execution body %v", body)
		}
		var out, errb bytes.Buffer
		cmd := exec.Command(f.py, "-I", "-c", body["code"].(string))
		cmd.Stdout, cmd.Stderr = &out, &errb
		_ = cmd.Run()
		json.NewEncoder(w).Encode(map[string]any{"id": "exec-1", "identifier": id, "executionType": "Synchronous", "status": "Succeeded",
			"result": map[string]any{"stdout": out.String(), "stderr": errb.String(), "executionResult": "", "executionTimeInMilliseconds": 4}})
	})
	mux.HandleFunc("GET /session", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("identifier")
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.sessions[id] {
			notFound(w)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"identifier": id, "createdAt": "2026-10-28T10:00:00Z", "expireAt": "2026-10-28T11:00:00Z"})
	})
	mux.HandleFunc("DELETE /session", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("identifier")
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.sessions[id] {
			notFound(w)
			return
		}
		delete(f.sessions, id)
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /listSessions", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		ids := make([]string, 0, len(f.sessions))
		for id := range f.sessions {
			ids = append(ids, id)
		}
		f.mu.Unlock()
		sort.Strings(ids)
		skip := 0
		fmt.Sscan(r.URL.Query().Get("skip"), &skip)
		page := map[string]any{"sessions": []any{}}
		if skip < len(ids) {
			page["sessions"] = []any{map[string]string{"identifier": ids[skip]}}
		}
		if skip+1 < len(ids) {
			page["nextLink"] = fmt.Sprintf("%s/listSessions?api-version=%s&skip=%d", f.srv.URL, DefaultAPIVersion, skip+1)
		}
		json.NewEncoder(w).Encode(page)
	})
	mux.HandleFunc("POST /files", func(w http.ResponseWriter, r *http.Request) {
		file, hdr, err := r.FormFile("file")
		if err != nil {
			t.Errorf("multipart: %v", err)
			w.WriteHeader(400)
			return
		}
		b, _ := io.ReadAll(file)
		key := r.URL.Query().Get("identifier") + "/" + filepath.Join(r.URL.Query().Get("path"), hdr.Filename)
		f.mu.Lock()
		f.files[key] = b
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"name": hdr.Filename, "type": "File", "sizeInBytes": len(b)})
	})
	mux.HandleFunc("GET /files/{name}/content", func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("identifier") + "/" + filepath.Join(r.URL.Query().Get("path"), r.PathValue("name"))
		f.mu.Lock()
		defer f.mu.Unlock()
		b, ok := f.files[key]
		if !ok {
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "FileNotFound", "message": "no such file"}})
			return
		}
		w.Write(b)
	})
	mux.HandleFunc("GET /files", func(w http.ResponseWriter, r *http.Request) {
		prefix := r.URL.Query().Get("identifier") + "/" + r.URL.Query().Get("path")
		f.mu.Lock()
		defer f.mu.Unlock()
		var value []any
		for k, b := range f.files {
			if dir, name := filepath.Split(k); strings.TrimSuffix(dir, "/") == strings.TrimSuffix(prefix, "/") {
				value = append(value, map[string]any{"name": name, "type": "File", "sizeInBytes": len(b), "lastModifiedAt": "2026-10-28T10:00:00Z"})
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"value": value})
	})
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/token") {
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(401)
				return
			}
			if r.URL.Query().Get("api-version") != DefaultAPIVersion {
				t.Errorf("%s %s without api-version", r.Method, r.URL)
			}
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newAdapter(t *testing.T, srv *httptest.Server) *Adapter {
	o, err := FromConfig(map[string]string{"endpoint": srv.URL}, "eastus", `{"tenantId":"tenant-1","clientId":"app","clientSecret":"s","authorityHost":"`+srv.URL+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	o.EnvFile = filepath.Join(t.TempDir(), "env.json")
	return New(o)
}

func TestAzureSessionsAdapter(t *testing.T) {
	f := newFake(t)
	a := newAdapter(t, f.srv)
	ctx := context.Background()
	if err := a.Health(ctx); err != nil {
		t.Fatal(err)
	}
	req := adapter.CreateRequest{ID: "sbx_01jb2c", Installation: "inst_01jaaa", Env: map[string]string{"GREETING": "hello"}}
	inst, err := a.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Ref != "dly-inst-01jaaa--sbx-01jb2c" || inst.SandboxID != "sbx_01jb2c" || inst.State != adapter.StateRunning {
		t.Fatalf("instance %+v", inst)
	}
	if again, err := a.Create(ctx, req); err != nil || again.Ref != inst.Ref || len(f.sessions) != 1 {
		t.Fatalf("replayed create %+v %v", again, err)
	}
	if got, err := a.Get(ctx, inst.Ref); err != nil || got.SandboxID != "sbx_01jb2c" {
		t.Fatalf("get %+v %v", got, err)
	}

	res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: `echo "$GREETING"; cat; echo e >&2; exit 5`, Stdin: "x"})
	if err != nil || res.ExitCode != 5 || res.Stdout != "hello\nx" || res.Stderr != "e\n" {
		t.Fatalf("exec %+v %v", res, err)
	}
	if f.lastExec["timeoutInSeconds"] != float64(maxExecSeconds) || f.lastExec["outputStreamsMaxLength"] != float64(defaultOutputLimit) {
		t.Fatalf("execution limits %v", f.lastExec)
	}

	if err := a.WriteFile(ctx, inst.Ref, "/mnt/data/out/r.csv", []byte("a,b")); err != nil {
		t.Fatal(err)
	}
	if b, err := a.ReadFile(ctx, inst.Ref, "/mnt/data/out/r.csv"); err != nil || string(b) != "a,b" {
		t.Fatalf("read %q %v", b, err)
	}
	if list, err := a.ListFiles(ctx, inst.Ref, "/mnt/data/out"); err != nil || len(list) != 1 || list[0].Path != "/mnt/data/out/r.csv" {
		t.Fatalf("list files %+v %v", list, err)
	}
	if err := a.WriteFile(ctx, inst.Ref, "/etc/passwd", nil); !api.IsCode(err, api.CodeInvalidRequest) {
		t.Fatalf("paths outside /mnt/data: %v", err)
	}

	for _, id := range []string{"sbx_01jb2d", "sbx_01jb2e"} {
		if _, err := a.Create(ctx, adapter.CreateRequest{ID: id, Installation: "inst_01jbbb"}); err != nil {
			t.Fatal(err)
		}
	}
	if list, err := a.List(ctx, "inst_01jaaa"); err != nil || len(list) != 1 || list[0].SandboxID != "sbx_01jb2c" {
		t.Fatalf("list %+v %v", list, err)
	}
	if list, err := a.List(ctx, "inst_01jbbb"); err != nil || len(list) != 2 {
		t.Fatalf("paged list %+v %v", list, err)
	}
	if err := a.Delete(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	if err := a.Delete(ctx, inst.Ref); err != nil {
		t.Fatalf("second delete must succeed: %v", err)
	}
	if _, err := a.Get(ctx, inst.Ref); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("SessionWithIdentifierNotFound is not_found: %v", err)
	}
}

func TestAzureSessionsFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/token"):
			json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3599})
		case r.URL.Query().Get("identifier") == "dly-inst-x--sbx-busy":
			w.WriteHeader(429)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "TooManyRequests", "message": "slow down"}})
		default:
			// A program the interpreter killed: no exit line on stderr.
			json.NewEncoder(w).Encode(map[string]any{"status": "Succeeded", "result": map[string]any{"stdout": "", "stderr": "KeyboardInterrupt"}})
		}
	}))
	defer srv.Close()
	a := newAdapter(t, srv)
	ctx := context.Background()
	if _, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_busy", Installation: "inst_x"}); !api.IsCode(err, adapter.CodeInsufficientCapacity) {
		t.Fatalf("429: %v", err)
	}
	if _, err := a.Exec(ctx, "dly-inst-x--sbx-y", api.ExecRequest{Command: "sleep 999"}); !api.IsCode(err, adapter.CodeSandboxFailed) {
		t.Fatalf("killed program: %v", err)
	}
	if _, err := FromConfig(map[string]string{}, "", ""); err == nil {
		t.Fatal("endpoint is required")
	}
}
