package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/different-ai/differently/pkg/api"
	"github.com/different-ai/differently/pkg/client"
)

func TestReportErrorNeverSilent(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"usage", errors.New("unknown flag: --org"), "error: unknown flag: --org\n"},
		{"network", fmt.Errorf("get /v1/me: %w", errors.New("connection refused")), "error: get /v1/me: connection refused\n"},
		{"api", api.Errorf(api.CodePermissionDenied, "operator (admin) key required").With("scope", "admin"),
			"error [permission_denied]: operator (admin) key required\n  scope: admin\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			reportError(&out, tc.err)
			if out.String() != tc.want {
				t.Fatalf("got %q, want %q", out.String(), tc.want)
			}
		})
	}
}

// The dev operator token goes to this machine only.
func TestDevTokenOnlyForLoopback(t *testing.T) {
	for raw, want := range map[string]bool{
		"http://localhost:8080":               true,
		"http://LOCALHOST:8080/":              true,
		"http://api.localhost:8080":           true,
		"http://127.0.0.1:18180":              true,
		"http://[::1]:8080":                   true,
		"https://localhost.example.com":       false,
		"https://differently.example.com":     false,
		"https://example.com/localhost":       false,
		"https://example.com/?next=localhost": false,
		"https://notlocalhost":                false,
		"http://10.0.0.5:8080":                false,
		"://bad":                              false,
	} {
		if got := isLoopbackURL(raw); got != want {
			t.Errorf("isLoopbackURL(%q) = %v, want %v", raw, got, want)
		}
	}
}

// fakeAPI answers the calls one run makes and records them in order.
type fakeAPI struct {
	mu       sync.Mutex
	calls    []string
	created  api.CreateSandboxRequest
	status   api.SandboxStatus // what the wait call reports
	exitCode int
}

func (f *fakeAPI) serve(t *testing.T) *client.Client {
	t.Helper()
	mux := http.NewServeMux()
	record := func(r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
	}
	mux.HandleFunc("POST /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		_ = json.NewDecoder(r.Body).Decode(&f.created)
		_ = json.NewEncoder(w).Encode(api.Sandbox{ID: "sbx_1", Status: api.StatusQueued})
	})
	mux.HandleFunc("GET /v1/sandboxes/sbx_1/wait", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		_ = json.NewEncoder(w).Encode(api.Sandbox{ID: "sbx_1", Status: f.status, Pool: "local", Trust: "private", StatusReason: "no room"})
	})
	mux.HandleFunc("POST /v1/sandboxes/sbx_1/exec", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: output\ndata: {\"stream\":\"stdout\",\"data\":\"hello\\n\"}\n\n")
		fmt.Fprintf(w, "event: output\ndata: {\"stream\":\"stderr\",\"data\":\"warn\\n\"}\n\n")
		fmt.Fprintf(w, "event: exit\ndata: {\"exitCode\":%d}\n\n", f.exitCode)
	})
	mux.HandleFunc("DELETE /v1/sandboxes/sbx_1", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return client.New(srv.URL, "dly_test_key")
}

func (f *fakeAPI) deleted() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls) > 0 && f.calls[len(f.calls)-1] == "DELETE /v1/sandboxes/sbx_1"
}

// One run is one sandbox: created, used and deleted, with the command's
// output on the matching streams and its exit code returned.
func TestRunCreatesExecsAndDeletes(t *testing.T) {
	f := &fakeAPI{status: api.StatusRunning, exitCode: 3}
	var stdout, stderr strings.Builder
	code, err := runOnce(context.Background(), f.serve(t), api.CreateSandboxRequest{ProjectID: "prj_default"},
		execRequest([]string{"echo", "hello"}, "", nil), false, &stdout, &stderr)
	if err != nil || code != 3 {
		t.Fatalf("code %d, err %v; want the command's exit code 3", code, err)
	}
	if stdout.String() != "hello\n" {
		t.Errorf("stdout %q holds more than the command's output", stdout.String())
	}
	if !strings.Contains(stderr.String(), "warn\n") || !strings.Contains(stderr.String(), "sbx_1 running on pool local (private)") {
		t.Errorf("stderr %q", stderr.String())
	}
	if f.created.ProjectID != "prj_default" || f.created.Profile != "" {
		t.Errorf("create request %+v: want the project and the server's default profile", f.created)
	}
	if !f.deleted() {
		t.Errorf("calls %v: the sandbox was not deleted last", f.calls)
	}
}

func TestRunKeepLeavesTheSandbox(t *testing.T) {
	f := &fakeAPI{status: api.StatusRunning}
	var stdout, stderr strings.Builder
	if _, err := runOnce(context.Background(), f.serve(t), api.CreateSandboxRequest{}, execRequest([]string{"true"}, "", nil), true, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if f.deleted() || !strings.Contains(stderr.String(), "kept sbx_1") {
		t.Errorf("calls %v, stderr %q: want no delete and the kept ID", f.calls, stderr.String())
	}
}

// A sandbox that never starts is still cleaned up, and nothing is executed.
func TestRunDeletesASandboxThatFailedToStart(t *testing.T) {
	f := &fakeAPI{status: api.StatusFailed}
	var stdout, stderr strings.Builder
	_, err := runOnce(context.Background(), f.serve(t), api.CreateSandboxRequest{}, execRequest([]string{"true"}, "", nil), false, &stdout, &stderr)
	if !api.IsCode(err, api.CodeInvalidState) {
		t.Fatalf("err %v, want invalid_state", err)
	}
	for _, c := range f.calls {
		if strings.HasSuffix(c, "/exec") {
			t.Errorf("calls %v: ran a command in a sandbox that is not running", f.calls)
		}
	}
	if !f.deleted() {
		t.Errorf("calls %v: the failed sandbox was left behind", f.calls)
	}
}

func TestExecRequestUsesTheShellOnlyForShellSyntax(t *testing.T) {
	if r := execRequest([]string{"echo hi | wc -c"}, "", nil); r.Command != "echo hi | wc -c" || r.Args != nil {
		t.Errorf("one argument with shell syntax: %+v", r)
	}
	if r := execRequest([]string{"echo", "a b"}, "/work", []string{"K=V"}); r.Command != "" || len(r.Args) != 2 || r.Cwd != "/work" || r.Env["K"] != "V" {
		t.Errorf("an argument list: %+v", r)
	}
}

// The hint appears only when nothing answers on this machine.
func TestStartHint(t *testing.T) {
	refused := fmt.Errorf("post: %w", syscall.ECONNREFUSED)
	if h := startHint(refused, "http://localhost:8080"); !strings.Contains(h, "differently-server --dev") {
		t.Errorf("hint %q", h)
	}
	if h := startHint(refused, "https://differently.example.com"); h != "" {
		t.Errorf("remote server: hint %q", h)
	}
	if h := startHint(errors.New("unknown flag"), "http://localhost:8080"); h != "" {
		t.Errorf("unrelated error: hint %q", h)
	}
}
