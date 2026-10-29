package azuresandboxes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

const group = "/subscriptions/sub-1/resourceGroups/rg-1/sandboxGroups/grp-1"

// fakeAzure follows the Container Apps Sandboxes data-plane spec
// (2026-09-01-preview) for the operations the adapter uses, plus an Entra
// ID token endpoint.
type fakeAzure struct {
	t         *testing.T
	mu        sync.Mutex
	n         int
	tokens    int
	boxes     map[string]map[string]any
	files     map[string][]byte
	lifecycle map[string]any
	ports     map[string][]map[string]any
	srv       *httptest.Server
}

func newFake(t *testing.T) *fakeAzure {
	f := &fakeAzure{t: t, boxes: map[string]map[string]any{}, files: map[string][]byte{}, ports: map[string][]map[string]any{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /tenant-1/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.PostForm.Get("scope") != Scope || r.PostForm.Get("client_secret") != "s3cret" {
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client"})
			return
		}
		f.mu.Lock()
		f.tokens++
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"token_type": "Bearer", "expires_in": 3599, "access_token": "tok"})
	})
	mux.HandleFunc("GET "+group+"/sandboxes/count", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]int{"count": len(f.boxes)})
	})
	mux.HandleFunc("POST "+group+"/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.n++
		id := fmt.Sprintf("box%d", f.n)
		box := map[string]any{"id": id, "labels": body["labels"], "state": "Creating", "region": "westus3", "createdAt": "2026-10-28T10:00:00Z",
			"sourcesRef": body["sourcesRef"], "resources": body["resources"], "lifecycle": body["lifecycle"], "egressPolicy": body["egressPolicy"]}
		f.boxes[id] = box
		w.Header().Set("Location", f.srv.URL+group+"/sandboxes/"+id)
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(box)
	})
	mux.HandleFunc("GET "+group+"/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		k, v, _ := strings.Cut(r.URL.Query().Get("labels"), "=")
		f.mu.Lock()
		defer f.mu.Unlock()
		var match []any
		for i := 1; i <= f.n; i++ {
			b, ok := f.boxes[fmt.Sprintf("box%d", i)]
			if !ok {
				continue
			}
			if labels, _ := b["labels"].(map[string]any); labels[k] == v {
				match = append(match, b)
			}
		}
		// One per page, linking to the next with skipToken.
		skip := 0
		fmt.Sscan(r.URL.Query().Get("skipToken"), &skip)
		page := map[string]any{"value": match[min(skip, len(match)):min(skip+1, len(match))]}
		if skip+1 < len(match) {
			q := r.URL.Query()
			q.Set("skipToken", fmt.Sprint(skip+1))
			page["nextLink"] = f.srv.URL + r.URL.Path + "?" + q.Encode()
		}
		json.NewEncoder(w).Encode(page)
	})
	box := func(w http.ResponseWriter, r *http.Request) map[string]any {
		b, ok := f.boxes[r.PathValue("id")]
		if !ok {
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "SandboxNotFound", "message": "sandbox not found"}})
		}
		return b
	}
	mux.HandleFunc("GET "+group+"/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if b := box(w, r); b != nil {
			if b["state"] == "Creating" {
				b["state"] = "Running"
			}
			json.NewEncoder(w).Encode(b)
		}
	})
	mux.HandleFunc("DELETE "+group+"/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if box(w, r) != nil {
			delete(f.boxes, r.PathValue("id"))
			w.WriteHeader(204)
		}
	})
	mux.HandleFunc("POST "+group+"/sandboxes/{id}/executeShellCommand", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		var out, errb bytes.Buffer
		cmd := exec.Command("/bin/sh", "-c", body["command"].(string))
		cmd.Stdout, cmd.Stderr = &out, &errb
		var ee *exec.ExitError
		if err := cmd.Run(); err != nil && !errors.As(err, &ee) {
			t.Errorf("sh: %v", err)
		}
		json.NewEncoder(w).Encode(map[string]any{"exitCode": cmd.ProcessState.ExitCode(), "stdout": out.String(), "stderr": errb.String(), "executionTimeMs": 5})
	})
	mux.HandleFunc("POST "+group+"/sandboxes/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/octet-stream" || r.URL.Query().Get("createDirs") != "true" {
			t.Errorf("upload headers %v %v", r.Header, r.URL.Query())
		}
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.files[r.URL.Query().Get("path")] = b
		f.mu.Unlock()
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{"success": true, "bytesWritten": len(b)})
	})
	mux.HandleFunc("GET "+group+"/sandboxes/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		b, ok := f.files[r.URL.Query().Get("path")]
		if !ok {
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "FileNotFound", "message": "no such file"}})
			return
		}
		w.Write(b)
	})
	mux.HandleFunc("GET "+group+"/sandboxes/{id}/files/list", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"path": r.URL.Query().Get("path"), "entries": []map[string]any{
			{"name": "a.txt", "path": "/work/a.txt", "size": 3, "mode": 0o644, "isDir": false, "isSymlink": false, "symlinkTarget": "", "modifiedTime": 1761645600},
			{"name": "sub", "path": "/work/sub", "size": 4096, "mode": 0o755, "isDir": true, "isSymlink": false, "symlinkTarget": "", "modifiedTime": 1761645600000},
		}})
	})
	action := func(name string, fn func(b map[string]any) any) {
		mux.HandleFunc("POST "+group+"/sandboxes/{id}/"+name, func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if b := box(w, r); b != nil {
				json.NewEncoder(w).Encode(fn(b))
			}
		})
	}
	action("stop", func(b map[string]any) any {
		b["state"] = "Stopped"
		return map[string]any{"id": "snap-stop", "sandboxId": b["id"], "createdAtUtc": "2026-10-28T10:05:00Z"}
	})
	action("resume", func(b map[string]any) any { b["state"] = "Creating"; return b })
	action("snapshot", func(b map[string]any) any {
		return map[string]any{"id": "snap-1", "sandboxId": b["id"], "createdAtUtc": "2026-10-28T10:05:00Z", "sizeInMB": 2}
	})
	mux.HandleFunc("POST "+group+"/sandboxes/{id}/lifecycle", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.lifecycle = body
		f.mu.Unlock()
		w.Write([]byte(`{}`))
	})
	mux.HandleFunc("POST "+group+"/sandboxes/{id}/ports/add", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		id := r.PathValue("id")
		for _, p := range f.ports[id] {
			if p["port"] == body["port"] {
				w.WriteHeader(409)
				json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "PortExists", "message": "port already added"}})
				return
			}
		}
		if auth, _ := body["auth"].(map[string]any); auth["anonymous"] != true {
			t.Errorf("port auth %v", body["auth"])
		}
		f.ports[id] = append(f.ports[id], map[string]any{"port": body["port"], "url": fmt.Sprintf("https://%s-%v.example.azurecontainerapps.io", id, body["port"])})
		json.NewEncoder(w).Encode(map[string]any{"ports": f.ports[id]})
	})
	mux.HandleFunc("GET "+group+"/sandboxes/{id}/ports", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"ports": f.ports[r.PathValue("id")]})
	})
	mux.HandleFunc("DELETE "+group+"/snapshots/{id}", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/subscriptions/") {
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

func newAdapter(t *testing.T, srv *httptest.Server, secret string, cfg map[string]string) *Adapter {
	base := map[string]string{"endpoint": srv.URL, "subscriptionId": "sub-1", "resourceGroup": "rg-1", "sandboxGroup": "grp-1", "diskImage": "ubuntu-24.04"}
	for k, v := range cfg {
		base[k] = v
	}
	o, err := FromConfig(base, "westus3", `{"tenantId":"tenant-1","clientId":"app","clientSecret":"`+secret+`","authorityHost":"`+srv.URL+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	return New(o)
}

func TestAzureSandboxesAdapter(t *testing.T) {
	f := newFake(t)
	a := newAdapter(t, f.srv, "s3cret", nil)
	ctx := context.Background()
	if err := a.Health(ctx); err != nil {
		t.Fatal(err)
	}
	req := adapter.CreateRequest{ID: "sbx_1", Installation: "inst_1", TTLSeconds: 3600, Resources: api.Resources{CPU: 2, MemoryMiB: 4096, DiskGiB: 20}, Env: map[string]string{"A": "b"}}
	inst, err := a.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != adapter.StateRunning || inst.SandboxID != "sbx_1" || inst.Region != "westus3" {
		t.Fatalf("instance %+v", inst)
	}
	if again, err := a.Create(ctx, req); err != nil || again.Ref != inst.Ref || f.n != 1 {
		t.Fatalf("replayed create: %+v %v (%d sandboxes)", again, err, f.n)
	}
	b := f.boxes[inst.Ref]
	res, _ := json.Marshal(b["resources"])
	lc, _ := json.Marshal(b["lifecycle"])
	src, _ := json.Marshal(b["sourcesRef"])
	eg, _ := json.Marshal(b["egressPolicy"])
	if string(res) != `{"cpu":"2","disk":"20Gi","memory":"4096Mi"}` ||
		string(lc) != `{"autoDeletePolicy":{"deleteIntervalInSeconds":3900,"enabled":true,"trigger":"AfterCreation"},"autoSuspendPolicy":{"enabled":false}}` ||
		string(src) != `{"diskImage":{"isPublic":true,"name":"ubuntu-24.04"}}` || string(eg) != `{"defaultAction":"Deny"}` {
		t.Fatalf("create body: %s %s %s %s", res, lc, src, eg)
	}

	r, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "cat; echo e >&2; exit 2", Stdin: "hello"})
	if err != nil || r.ExitCode != 2 || r.Stdout != "hello" || r.Stderr != "e\n" || r.DurationMs != 5 {
		t.Fatalf("exec %+v %v", r, err)
	}

	if err := a.WriteFile(ctx, inst.Ref, "/work/a.txt", []byte("abc")); err != nil {
		t.Fatal(err)
	}
	if got, err := a.ReadFile(ctx, inst.Ref, "/work/a.txt"); err != nil || string(got) != "abc" {
		t.Fatalf("read %q %v", got, err)
	}
	if _, err := a.ReadFile(ctx, inst.Ref, "/missing"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("missing file: %v", err)
	}
	list, err := a.ListFiles(ctx, inst.Ref, "/work")
	if err != nil || len(list) != 2 || !list[1].IsDir || list[0].Mode != "644" || !list[0].ModTime.Equal(list[1].ModTime) {
		t.Fatalf("list files %+v %v", list, err)
	}

	if err := a.Stop(ctx, inst.Ref); err != nil || f.boxes[inst.Ref]["state"] != "Stopped" {
		t.Fatalf("stop: %v", err)
	}
	if err := a.Start(ctx, inst.Ref); err != nil || f.boxes[inst.Ref]["state"] != "Running" {
		t.Fatalf("start: %v", err)
	}
	if err := a.SetExpiry(ctx, inst.Ref, time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if del := f.lifecycle["autoDeletePolicy"].(map[string]any); del["deleteIntervalInSeconds"] != float64(7200) {
		t.Fatalf("expiry moved to %v", del)
	}
	snap, err := a.Snapshot(ctx, inst.Ref, "disk")
	if err != nil || snap.Ref != "snap-1" || snap.SizeBytes != 2<<20 {
		t.Fatalf("snapshot %+v %v", snap, err)
	}
	if _, err := a.Snapshot(ctx, inst.Ref, "memory"); !api.IsCode(err, api.CodeUnsupportedOperation) {
		t.Fatalf("memory snapshots are not offered: %v", err)
	}
	if err := a.DeleteSnapshot(ctx, snap.Ref); err != nil {
		t.Fatal(err)
	}
	fork, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_2", Installation: "inst_1", FromSnapshot: snap.Ref})
	if err != nil || fork.Ref == inst.Ref {
		t.Fatalf("fork %+v %v", fork, err)
	}
	if src, _ := json.Marshal(f.boxes[fork.Ref]["sourcesRef"]); string(src) != `{"snapshot":{"id":"snap-1"}}` {
		t.Fatalf("restore source %s", src)
	}
	u, err := a.ExposePort(ctx, inst.Ref, 8080)
	if err != nil || u != "https://box1-8080.example.azurecontainerapps.io" {
		t.Fatalf("port %q %v", u, err)
	}
	if again, err := a.ExposePort(ctx, inst.Ref, 8080); err != nil || again != u {
		t.Fatalf("exposing again %q %v", again, err)
	}

	if _, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_9", Installation: "inst_2"}); err != nil {
		t.Fatal(err)
	}
	all, err := a.List(ctx, "inst_1")
	if err != nil || len(all) != 2 {
		t.Fatalf("paged list %+v %v", all, err)
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
	if f.tokens != 1 {
		t.Fatalf("the Entra token should be reused, fetched %d times", f.tokens)
	}
}

func TestAzureSandboxesErrors(t *testing.T) {
	f := newFake(t)
	if err := newAdapter(t, f.srv, "wrong", nil).Health(context.Background()); !api.IsCode(err, api.CodeProviderUnavailable) || strings.Contains(err.Error(), "wrong") {
		t.Fatalf("rejected secret: %v", err)
	}
	for status, want := range map[int]string{429: adapter.CodeInsufficientCapacity, 400: api.CodeInvalidRequest, 403: api.CodeProviderUnavailable, 500: api.CodeProviderUnavailable} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/token") {
				json.NewEncoder(w).Encode(map[string]any{"access_token": "tok", "expires_in": 3600})
				return
			}
			if r.Method == http.MethodGet {
				json.NewEncoder(w).Encode(map[string]any{"value": []any{}})
				return
			}
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "Code" + fmt.Sprint(status), "message": "failed"}})
		}))
		_, err := newAdapter(t, srv, "s3cret", nil).Create(context.Background(), adapter.CreateRequest{ID: "sbx_1", Installation: "inst_1"})
		srv.Close()
		if !api.IsCode(err, want) {
			t.Errorf("%d: got %v, want %s", status, err, want)
		}
		var ae *api.Error
		if errors.As(err, &ae) && ae.Details["providerCode"] != "Code"+fmt.Sprint(status) {
			t.Errorf("%d: provider code %v", status, ae.Details)
		}
	}
	if _, err := FromConfig(map[string]string{"endpoint": "https://x"}, "", `{}`); err == nil {
		t.Fatal("missing group settings must fail")
	}
}
