//go:build experimental_adapters

package gcpsandboxes

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/different-ai/differently/internal/adapters/shellcmd"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

const engine = "projects/proj-1/locations/us-central1/reasoningEngines/re-1"

// fakeAgentPlatform follows the aiplatform v1 sandboxEnvironments methods:
// long-running operations for create, pause, resume, snapshot and delete,
// and :execute proxying a JSON chunk to the shell container's /exec, which
// runs the command with the host's /bin/sh.
type fakeAgentPlatform struct {
	t     *testing.T
	mu    sync.Mutex
	n     int
	boxes map[string]map[string]any // id -> sandbox
	order []string
	ops   map[string]map[string]any
	dir   string
	srv   *httptest.Server

	templateReads int
}

func gerr(w http.ResponseWriter, code int, status, msg string) {
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": msg, "status": status}})
}

func newFake(t *testing.T) *fakeAgentPlatform {
	f := &fakeAgentPlatform{t: t, boxes: map[string]map[string]any{}, ops: map[string]map[string]any{}, dir: t.TempDir()}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAgentPlatform) op(response any) map[string]any {
	f.n++
	op := map[string]any{"name": fmt.Sprintf("%s/operations/op%d", engine, f.n), "done": false, "response": response}
	f.ops[op["name"].(string)] = op
	return map[string]any{"name": op["name"], "done": false}
}

func (f *fakeAgentPlatform) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/token" {
		r.ParseForm()
		if r.PostForm.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" || r.PostForm.Get("assertion") == "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "ya29.test", "expires_in": 3599, "token_type": "Bearer"})
		return
	}
	if r.Header.Get("Authorization") != "Bearer ya29.test" {
		gerr(w, 401, "UNAUTHENTICATED", "no token")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/v1/")
	list := engine + "/sandboxEnvironments"
	switch {
	case strings.Contains(p, "/operations/"):
		op := f.ops[p]
		op["done"] = true
		json.NewEncoder(w).Encode(op)
	case strings.HasPrefix(p, engine+"/sandboxEnvironmentTemplates/") && r.Method == http.MethodGet:
		// "shell" has no egress configuration (internet off); "open" allows it.
		tmpl := map[string]any{"name": p, "displayName": lastSegment(p)}
		switch lastSegment(p) {
		case "shell", "closed":
		case "open":
			tmpl["egressControlConfig"] = map[string]any{"internetAccess": true}
		default:
			gerr(w, 404, "NOT_FOUND", "no such template")
			return
		}
		f.templateReads++
		json.NewEncoder(w).Encode(tmpl)
	case p == list && r.Method == http.MethodGet:
		var items []any
		for _, id := range f.order {
			items = append(items, f.boxes[id])
		}
		i, _ := strconv.Atoi(r.URL.Query().Get("pageToken"))
		page := map[string]any{"sandboxEnvironments": items[min(i, len(items)):min(i+1, len(items))]}
		if i+1 < len(items) {
			page["nextPageToken"] = strconv.Itoa(i + 1)
		}
		json.NewEncoder(w).Encode(page)
	case p == list && r.Method == http.MethodPost:
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["sandboxEnvironmentTemplate"] == nil && body["sandboxEnvironmentSnapshot"] == nil {
			gerr(w, 400, "INVALID_ARGUMENT", "a template or snapshot is required")
			return
		}
		id := fmt.Sprintf("sb%d", len(f.order)+1)
		box := map[string]any{"name": list + "/" + id, "displayName": body["displayName"], "owner": body["owner"], "state": "STATE_PROVISIONING",
			"ttl": body["ttl"], "sandboxEnvironmentTemplate": body["sandboxEnvironmentTemplate"], "sandboxEnvironmentSnapshot": body["sandboxEnvironmentSnapshot"]}
		f.boxes[id], f.order = box, append(f.order, id)
		json.NewEncoder(w).Encode(f.op(box))
	case strings.HasPrefix(p, engine+"/sandboxEnvironmentSnapshots/") && r.Method == http.MethodDelete:
		json.NewEncoder(w).Encode(f.op(map[string]any{}))
	case strings.HasPrefix(p, list+"/"):
		id, verb, _ := strings.Cut(strings.TrimPrefix(p, list+"/"), ":")
		box, ok := f.boxes[id]
		if !ok || box["state"] == "STATE_DELETED" {
			gerr(w, 404, "NOT_FOUND", "sandbox not found")
			return
		}
		switch {
		case r.Method == http.MethodGet && verb == "":
			if box["state"] == "STATE_PROVISIONING" {
				box["state"] = "STATE_RUNNING"
			}
			json.NewEncoder(w).Encode(box)
		case r.Method == http.MethodDelete:
			if box["state"] == "STATE_DEPROVISIONING" {
				gerr(w, 400, "FAILED_PRECONDITION", "sandbox is being deleted")
				return
			}
			box["state"] = "STATE_DEPROVISIONING"
			json.NewEncoder(w).Encode(f.op(map[string]any{}))
		case verb == "pause":
			box["state"] = "STATE_PAUSED"
			json.NewEncoder(w).Encode(f.op(box))
		case verb == "resume":
			if box["state"] != "STATE_PAUSED" {
				gerr(w, 400, "FAILED_PRECONDITION", "sandbox is not paused")
				return
			}
			box["state"] = "STATE_RUNNING"
			json.NewEncoder(w).Encode(f.op(box))
		case verb == "snapshot":
			json.NewEncoder(w).Encode(f.op(map[string]any{"name": engine + "/sandboxEnvironmentSnapshots/snap-1", "sizeBytes": "2048"}))
		case verb == "execute":
			f.execute(w, r, id)
		default:
			gerr(w, 404, "NOT_FOUND", "no route "+p)
		}
	default:
		gerr(w, 404, "NOT_FOUND", "no route "+p)
	}
}

func (f *fakeAgentPlatform) execute(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Inputs []struct {
			MimeType string `json:"mimeType"`
			Data     []byte `json:"data"`
		} `json:"inputs"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	if len(body.Inputs) != 3 || string(body.Inputs[0].Data) != "/exec" || string(body.Inputs[1].Data) != "8080" || body.Inputs[2].MimeType != "application/json" {
		gerr(w, 400, "INVALID_ARGUMENT", "unexpected chunks")
		return
	}
	var req struct {
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	json.Unmarshal(body.Inputs[2].Data, &req)
	env := filepath.Join(f.dir, id, "env")
	cmdline := strings.ReplaceAll(req.Command, shellcmd.EnvFile, env)
	cmdline = strings.ReplaceAll(cmdline, filepath.Dir(shellcmd.EnvFile), filepath.Dir(env))
	ctx := r.Context()
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.Timeout)*time.Second)
		defer cancel()
	}
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", cmdline)
	cmd.Stdout, cmd.Stderr = &out, &errb
	var ee *exec.ExitError
	if err := cmd.Run(); err != nil && !errors.As(err, &ee) {
		f.t.Errorf("sh: %v", err)
	}
	result, _ := json.Marshal(map[string]any{"stdout": out.String(), "stderr": errb.String(), "returncode": cmd.ProcessState.ExitCode(), "duration_ms": 7})
	json.NewEncoder(w).Encode(map[string]any{"outputs": []any{map[string]any{"mimeType": "application/json", "data": base64.StdEncoding.EncodeToString(result)}}})
}

func keyJSON(t *testing.T, tokenURI string) string {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	b, _ := json.Marshal(map[string]string{"type": "service_account", "client_email": "dly@proj-1.iam.gserviceaccount.com",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), "token_uri": tokenURI})
	return string(b)
}

func newAdapter(t *testing.T, url string) *Adapter {
	o, err := FromConfig(map[string]string{"project": "proj-1", "reasoningEngine": "re-1", "template": "shell", "apiURL": url}, "us-central1", keyJSON(t, url+"/token"))
	if err != nil {
		t.Fatal(err)
	}
	o.PollEvery = time.Millisecond
	return New(o)
}

func TestAgentPlatformAdapter(t *testing.T) {
	f := newFake(t)
	a := newAdapter(t, f.srv.URL)
	ctx := context.Background()
	if err := a.Health(ctx); err != nil {
		t.Fatal(err)
	}
	req := adapter.CreateRequest{ID: "sbx_1", Installation: "inst_1", TTLSeconds: 3600, Env: map[string]string{"NAME": "differently"}}
	inst, err := a.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Ref != "sb1" || inst.SandboxID != "sbx_1" || inst.State != adapter.StateRunning || inst.Region != "us-central1" {
		t.Fatalf("instance %+v", inst)
	}
	box := f.boxes["sb1"]
	if box["ttl"] != "3900s" || box["owner"] != "inst_1" || box["sandboxEnvironmentTemplate"] != engine+"/sandboxEnvironmentTemplates/shell" {
		t.Fatalf("create body %v", box)
	}
	if again, err := a.Create(ctx, req); err != nil || again.Ref != inst.Ref || len(f.order) != 1 {
		t.Fatalf("replayed create %+v %v", again, err)
	}

	res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: `echo "hi $NAME"; cat; exit 3`, Stdin: "piped", Cwd: f.dir})
	if err != nil || res.ExitCode != 3 || res.Stdout != "hi differently\npiped" || res.DurationMs != 7 {
		t.Fatalf("exec %+v %v", res, err)
	}
	path := filepath.Join(f.dir, "w", "f.bin")
	if err := a.WriteFile(ctx, inst.Ref, path, []byte{0, 1, 2}); err != nil {
		t.Fatal(err)
	}
	if b, err := a.ReadFile(ctx, inst.Ref, path); err != nil || !bytes.Equal(b, []byte{0, 1, 2}) {
		t.Fatalf("read %v %v", b, err)
	}

	if err := a.Stop(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.Get(ctx, inst.Ref); got.State != adapter.StateStopped {
		t.Fatalf("paused sandbox state %+v", got)
	}
	if err := a.Start(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	if err := a.Start(ctx, inst.Ref); !api.IsCode(err, api.CodeInvalidState) {
		t.Fatalf("resume of a running sandbox: %v", err)
	}
	snap, err := a.Snapshot(ctx, inst.Ref, "disk")
	if err != nil || snap.Ref != "snap-1" || snap.Kind != "memory" || snap.SizeBytes != 2048 {
		t.Fatalf("snapshot %+v %v", snap, err)
	}
	fork, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_2", Installation: "inst_1", FromSnapshot: snap.Ref})
	if err != nil || f.boxes[fork.Ref]["sandboxEnvironmentSnapshot"] != engine+"/sandboxEnvironmentSnapshots/snap-1" {
		t.Fatalf("fork %+v %v", fork, err)
	}
	if f.boxes[fork.Ref]["ttl"] != fmt.Sprintf("%ds", defaultTTL) {
		t.Fatalf("no TTL should use the default, got %v", f.boxes[fork.Ref]["ttl"])
	}
	if err := a.DeleteSnapshot(ctx, snap.Ref); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_3", Installation: "inst_2"}); err != nil {
		t.Fatal(err)
	}
	if list, err := a.List(ctx, "inst_1"); err != nil || len(list) != 2 {
		t.Fatalf("paged list %+v %v", list, err)
	}
	if err := a.Delete(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	if list, _ := a.List(ctx, "inst_1"); len(list) != 1 {
		t.Fatalf("a deprovisioning sandbox is not listed: %+v", list)
	}
	if _, err := a.Get(ctx, inst.Ref); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	if err := a.Delete(ctx, inst.Ref); err != nil {
		t.Fatalf("a delete while deprovisioning must succeed: %v", err)
	}
	f.boxes["sb1"]["state"] = "STATE_DELETED"
	if err := a.Delete(ctx, inst.Ref); err != nil {
		t.Fatalf("second delete must succeed: %v", err)
	}
}

func TestAgentPlatformErrors(t *testing.T) {
	var failOp bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			json.NewEncoder(w).Encode(map[string]any{"access_token": "ya29.test", "expires_in": 3599})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/sandboxEnvironments"):
			json.NewEncoder(w).Encode(map[string]any{})
		case failOp:
			json.NewEncoder(w).Encode(map[string]any{"name": engine + "/operations/x", "done": true, "error": map[string]any{"code": 8, "message": "quota"}})
		default:
			gerr(w, 429, "RESOURCE_EXHAUSTED", "Quota exceeded for sandbox write requests per minute")
		}
	}))
	defer srv.Close()
	a := newAdapter(t, srv.URL)
	ctx := context.Background()
	_, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_1", Installation: "inst_1"})
	var ae *api.Error
	if !api.IsCode(err, adapter.CodeInsufficientCapacity) || !errors.As(err, &ae) || ae.Details["providerCode"] != "RESOURCE_EXHAUSTED" {
		t.Fatalf("429: %v", err)
	}
	failOp = true
	if _, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_1", Installation: "inst_1"}); !api.IsCode(err, adapter.CodeInsufficientCapacity) {
		t.Fatalf("operation error code 8: %v", err)
	}
	if err := opError("x", &rpcStatus{Code: 13, Message: "internal"}); !api.IsCode(err, adapter.CodeSandboxFailed) {
		t.Fatalf("a failed operation is sandbox_failed: %v", err)
	}
	if _, err := FromConfig(map[string]string{"project": "p"}, "us-central1", "{}"); err == nil {
		t.Fatal("engine and template are required")
	}
	if _, err := FromConfig(map[string]string{"project": "p", "reasoningEngine": "r", "template": "t"}, "us-central1", `{"type":"authorized_user"}`); err == nil {
		t.Fatal("user credentials are not accepted")
	}
}

// A pool on GKE Workload Identity or a Compute Engine VM takes its token
// from the metadata server, with no key in the pool's credential.
func TestAgentPlatformMetadataServer(t *testing.T) {
	f := newFake(t)
	meta := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" || !strings.HasSuffix(r.URL.Path, "/service-accounts/default/token") {
			w.WriteHeader(403)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "ya29.test", "expires_in": 3599, "token_type": "Bearer"})
	}))
	defer meta.Close()
	o, err := FromConfig(map[string]string{"project": "proj-1", "reasoningEngine": "re-1", "template": "shell", "apiURL": f.srv.URL},
		"us-central1", `{"type":"metadata_server","metadataHost":"`+meta.URL+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := New(o).Health(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Egress is a property of the template: a pool advertises egress control
// only when its template has internet access off, and then does not let a
// request swap in a template that allows it.
func TestAgentPlatformEgressControl(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	adapterFor := func(template string) *Adapter {
		o, err := FromConfig(map[string]string{"project": "proj-1", "reasoningEngine": "re-1", "template": template, "apiURL": f.srv.URL}, "us-central1", keyJSON(t, f.srv.URL+"/token"))
		if err != nil {
			t.Fatal(err)
		}
		o.PollEvery = time.Millisecond
		return New(o)
	}

	closed := adapterFor("shell")
	for range 2 {
		if caps, _ := closed.Capabilities(ctx); !caps.Has(adapter.FeatureEgressControl) {
			t.Fatalf("a template without internet access is egress-controlled: %v", caps.Features)
		}
	}
	if f.templateReads != 1 {
		t.Fatalf("the template is read once: %d reads", f.templateReads)
	}
	if _, err := closed.Create(ctx, adapter.CreateRequest{ID: "sbx_1", Installation: "inst_1", Template: "open"}); !api.IsCode(err, api.CodeInvalidRequest) || len(f.order) != 0 {
		t.Fatalf("a request may not pick a template with internet access on an egress-controlled pool: %v", err)
	}
	if _, err := closed.Create(ctx, adapter.CreateRequest{ID: "sbx_1", Installation: "inst_1", Template: "closed"}); err != nil {
		t.Fatalf("another template without internet access is fine: %v", err)
	}

	open := adapterFor("open")
	if caps, _ := open.Capabilities(ctx); caps.Has(adapter.FeatureEgressControl) {
		t.Fatalf("a template with internet access is not egress-controlled: %v", caps.Features)
	}
	if _, err := open.Create(ctx, adapter.CreateRequest{ID: "sbx_2", Installation: "inst_1", Template: "closed"}); err != nil {
		t.Fatalf("an open pool takes any template: %v", err)
	}
	if caps, err := adapterFor("missing").Capabilities(ctx); err != nil || caps.Has(adapter.FeatureEgressControl) {
		t.Fatalf("an unreadable template leaves the feature out: %v %v", caps.Features, err)
	}
}
