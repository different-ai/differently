package agentcore

import (
	"bytes"
	"context"
	"encoding/json"
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

	"github.com/different-ai/differently/internal/adapters/cloudhttp"
	"github.com/different-ai/differently/internal/adapters/shellcmd"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// fakeAgentCore follows the AgentCore data-plane model: sessions under one
// code interpreter, a paged list, and tools/invoke answering with an event
// stream. executeCommand runs the command with the host's /bin/sh, with
// shellcmd's env file moved into a temp dir.
type fakeAgentCore struct {
	t        *testing.T
	mu       sync.Mutex
	starts   int
	sessions map[string]map[string]any // id -> session
	tokens   map[string]string         // clientToken -> id
	dir      string
	srv      *httptest.Server
}

func newFake(t *testing.T) *fakeAgentCore {
	f := &fakeAgentCore{t: t, sessions: map[string]map[string]any{}, tokens: map[string]string{}, dir: t.TempDir()}
	base := "/code-interpreters/aws.codeinterpreter.v1"
	mux := http.NewServeMux()
	mux.HandleFunc("PUT "+base+"/sessions/start", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		tok, _ := body["clientToken"].(string)
		if len(tok) < 33 {
			fail(w, 400, "ValidationException", "clientToken too short")
			return
		}
		id, ok := f.tokens[tok]
		if !ok {
			f.starts++
			id = fmt.Sprintf("s%02d", f.starts)
			f.tokens[tok] = id
			f.sessions[id] = map[string]any{"sessionId": id, "name": body["name"], "status": "READY", "sessionTimeoutSeconds": body["sessionTimeoutSeconds"]}
		}
		json.NewEncoder(w).Encode(map[string]any{"codeInterpreterIdentifier": "aws.codeinterpreter.v1", "sessionId": id, "createdAt": "2026-10-28T00:00:00Z"})
	})
	mux.HandleFunc("POST "+base+"/sessions/list", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			MaxResults int    `json:"maxResults"`
			NextToken  string `json:"nextToken"`
			Status     string `json:"status"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		var items []map[string]any
		for i := 1; i <= f.starts; i++ {
			s := f.sessions[fmt.Sprintf("s%02d", i)]
			if body.Status == "" || s["status"] == body.Status {
				items = append(items, s)
			}
		}
		start, _ := strconv.Atoi(body.NextToken)
		end := min(start+2, len(items)) // two per page, to exercise paging
		page := map[string]any{"items": items[start:end]}
		if end < len(items) {
			page["nextToken"] = strconv.Itoa(end)
		}
		json.NewEncoder(w).Encode(page)
	})
	mux.HandleFunc("GET "+base+"/sessions/get", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		s, ok := f.sessions[r.URL.Query().Get("sessionId")]
		if !ok {
			fail(w, 404, "ResourceNotFoundException", "no such session")
			return
		}
		json.NewEncoder(w).Encode(s)
	})
	mux.HandleFunc("PUT "+base+"/sessions/stop", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		s, ok := f.sessions[r.URL.Query().Get("sessionId")]
		switch {
		case !ok:
			fail(w, 404, "ResourceNotFoundException", "no such session")
		case s["status"] == "TERMINATED":
			fail(w, 409, "ConflictException", "session already stopped")
		default:
			s["status"] = "TERMINATED"
			json.NewEncoder(w).Encode(map[string]any{"sessionId": s["sessionId"]})
		}
	})
	mux.HandleFunc("POST "+base+"/tools/invoke", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		id := r.Header.Get("x-amzn-code-interpreter-session-id")
		f.mu.Lock()
		s, ok := f.sessions[id]
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		if !ok || s["status"] != "READY" {
			w.Write(cloudhttp.EncodeEvent(map[string]string{":message-type": "exception", ":exception-type": "resourceNotFoundException", ":content-type": "application/json"}, []byte(`{"message":"session not found"}`)))
			return
		}
		if body.Name != "executeCommand" {
			fail(w, 400, "ValidationException", "unexpected tool "+body.Name)
			return
		}
		stdout, stderr, code := f.sh(id, body.Arguments["command"].(string))
		payload, _ := json.Marshal(map[string]any{
			"content":           []map[string]any{{"type": "text", "text": stdout}},
			"structuredContent": map[string]any{"stdout": stdout, "stderr": stderr, "exitCode": code, "executionTime": 3},
			"isError":           code != 0,
		})
		w.Write(cloudhttp.EncodeEvent(map[string]string{":message-type": "event", ":event-type": "result", ":content-type": "application/json"}, payload))
	})
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIDTEST/") || !strings.Contains(auth, "/us-east-1/bedrock-agentcore/aws4_request") || r.Header.Get("X-Amz-Date") == "" {
			fail(w, 403, "AccessDeniedException", "bad signature")
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func fail(w http.ResponseWriter, status int, kind, msg string) {
	w.Header().Set("X-Amzn-ErrorType", kind+":http://internal.amazon.com/coral/com.amazonaws.bedrockagentcore/")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"message": msg})
}

func (f *fakeAgentCore) sh(id, command string) (string, string, int) {
	env := filepath.Join(f.dir, id, "env")
	command = strings.ReplaceAll(command, shellcmd.EnvFile, env)
	command = strings.ReplaceAll(command, filepath.Dir(shellcmd.EnvFile), filepath.Dir(env))
	var out, errb bytes.Buffer
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		f.t.Errorf("sh: %v", err)
	}
	return out.String(), errb.String(), cmd.ProcessState.ExitCode()
}

func newAdapter(t *testing.T, url string) *Adapter {
	o, err := FromConfig(map[string]string{"apiURL": url}, "us-east-1", `{"accessKeyId":"AKIDTEST","secretAccessKey":"s3cret"}`)
	if err != nil {
		t.Fatal(err)
	}
	return New(o)
}

func TestAgentCoreAdapter(t *testing.T) {
	f := newFake(t)
	a := newAdapter(t, f.srv.URL)
	ctx := context.Background()
	if err := a.Health(ctx); err != nil {
		t.Fatal(err)
	}
	req := adapter.CreateRequest{ID: "sbx_1", Installation: "inst_1", TTLSeconds: 3600, Env: map[string]string{"GREETING": "hi there"}}
	inst, err := a.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != adapter.StateRunning || inst.SandboxID != "sbx_1" || inst.Region != "us-east-1" {
		t.Fatalf("instance %+v", inst)
	}
	if again, err := a.Create(ctx, req); err != nil || again.Ref != inst.Ref || f.starts != 1 {
		t.Fatalf("replayed create: %+v %v (%d sessions)", again, err, f.starts)
	}
	if f.sessions[inst.Ref]["sessionTimeoutSeconds"] != float64(3600+backstopGrace) {
		t.Fatalf("ttl backstop %v", f.sessions[inst.Ref]["sessionTimeoutSeconds"])
	}

	res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: `echo "$GREETING $X"; cat; echo err >&2; exit 4`, Env: map[string]string{"X": "y"}, Stdin: "in"})
	if err != nil || res.ExitCode != 4 || res.Stdout != "hi there y\nin" || res.Stderr != "err\n" {
		t.Fatalf("exec %+v %v", res, err)
	}

	path := filepath.Join(f.dir, "work", "a.txt")
	if err := a.WriteFile(ctx, inst.Ref, path, []byte("data\x00")); err != nil {
		t.Fatal(err)
	}
	if b, err := a.ReadFile(ctx, inst.Ref, path); err != nil || string(b) != "data\x00" {
		t.Fatalf("read %q %v", b, err)
	}
	if list, err := a.ListFiles(ctx, inst.Ref, filepath.Dir(path)); err != nil || len(list) != 1 || list[0].Name != "a.txt" {
		t.Fatalf("list files %+v %v", list, err)
	}

	// Sessions of another installation, and enough of them to page.
	for _, id := range []string{"sbx_2", "sbx_3"} {
		if _, err := a.Create(ctx, adapter.CreateRequest{ID: id, Installation: "inst_2"}); err != nil {
			t.Fatal(err)
		}
	}
	if f.sessions["s02"]["sessionTimeoutSeconds"] != float64(maxSessionSeconds) {
		t.Fatalf("no TTL should use the 8-hour maximum, got %v", f.sessions["s02"]["sessionTimeoutSeconds"])
	}
	list, err := a.List(ctx, "inst_1")
	if err != nil || len(list) != 1 || list[0].SandboxID != "sbx_1" {
		t.Fatalf("list %+v %v", list, err)
	}
	if list, _ := a.List(ctx, "inst_2"); len(list) != 2 {
		t.Fatalf("paged list %+v", list)
	}

	if err := a.Delete(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	if err := a.Delete(ctx, inst.Ref); err != nil {
		t.Fatalf("second delete must succeed: %v", err)
	}
	if err := a.Delete(ctx, "nope"); err != nil {
		t.Fatalf("deleting an unknown session must succeed: %v", err)
	}
	if _, err := a.Get(ctx, inst.Ref); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("stopped session should be not_found: %v", err)
	}
	if _, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "true"}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("exception event should classify: %v", err)
	}
	if list, _ := a.List(ctx, "inst_1"); len(list) != 0 {
		t.Fatalf("stopped sessions are not listed: %+v", list)
	}
	caps, _ := a.Capabilities(ctx)
	if caps.Has(adapter.FeatureStopStart) || caps.Has(adapter.FeatureSnapshotDisk) || !caps.Has(adapter.FeatureFiles) || caps.MaxCPU != 2 {
		t.Fatalf("capabilities %+v", caps)
	}
}

func TestAgentCoreErrorClassification(t *testing.T) {
	cases := []struct {
		status int
		kind   string
		want   string
	}{
		{402, "ServiceQuotaExceededException", adapter.CodeInsufficientCapacity},
		{429, "ThrottlingException", adapter.CodeInsufficientCapacity},
		{400, "ValidationException", api.CodeInvalidRequest},
		{403, "AccessDeniedException", api.CodeProviderUnavailable},
		{500, "InternalServerException", api.CodeProviderUnavailable},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fail(w, c.status, c.kind, "nope") }))
		a := newAdapter(t, srv.URL)
		_, err := a.Create(context.Background(), adapter.CreateRequest{ID: "sbx_x", Installation: "inst_x"})
		srv.Close()
		if !api.IsCode(err, c.want) || strings.Contains(err.Error(), "s3cret") {
			t.Errorf("%s: got %v, want %s", c.kind, err, c.want)
		}
	}
	if _, err := FromConfig(map[string]string{}, "", `{"accessKeyId":"a","secretAccessKey":"b"}`); err == nil {
		t.Fatal("a region is required")
	}
	if _, err := FromConfig(map[string]string{"region": "us-east-1"}, "", `AKID:hunter2value`); err == nil || strings.Contains(err.Error(), "hunter2value") {
		t.Fatalf("bad credential: %v", err)
	}
}

func TestAgentCoreStreamException(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(cloudhttp.EncodeEvent(map[string]string{":message-type": "exception", ":exception-type": "throttlingException"}, []byte(`{"message":"slow down"}`)))
	}))
	defer srv.Close()
	a := newAdapter(t, srv.URL)
	if _, err := a.Exec(context.Background(), "s1", api.ExecRequest{Command: "true"}); !api.IsCode(err, adapter.CodeInsufficientCapacity) || !strings.Contains(err.Error(), "slow down") {
		t.Fatalf("throttling inside the stream: %v", err)
	}
}
