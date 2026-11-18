package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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

// fakeAPI answers the calls one run makes and records them in order. With
// me and projects set it also answers what login, org create and key create
// ask.
type fakeAPI struct {
	mu       sync.Mutex
	calls    []string
	created  api.CreateSandboxRequest
	status   api.SandboxStatus // what the wait call reports
	exitCode int

	me       whoami                  // GET /v1/me
	projects []api.Project           // what the key can see
	keyReq   api.CreateAPIKeyRequest // the last POST /v1/api-keys
}

func (f *fakeAPI) serve(t *testing.T) *client.Client {
	t.Helper()
	return client.New(f.start(t), "dly_test_key")
}

// start serves the fake and returns its URL.
func (f *fakeAPI) start(t *testing.T) string {
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
	mux.HandleFunc("GET /v1/me", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		_ = json.NewEncoder(w).Encode(f.me)
	})
	mux.HandleFunc("GET /v1/projects", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(api.List[api.Project]{Items: append([]api.Project{}, f.projects...)})
	})
	mux.HandleFunc("GET /v1/projects/{id}", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, p := range f.projects {
			if p.ID == r.PathValue("id") {
				_ = json.NewEncoder(w).Encode(p)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(api.ErrorEnvelope{Error: api.Errorf(api.CodeNotFound, "project %s not found", r.PathValue("id"))})
	})
	// Like the server, an organization comes with a project unless the
	// request leaves its name empty.
	mux.HandleFunc("POST /v1/admin/organizations", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		var req struct{ Name, DefaultProject string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		out := map[string]any{"organization": api.Organization{ID: "org_1", Name: req.Name}}
		if req.DefaultProject != "" {
			proj := api.Project{ID: "prj_1", OrgID: "org_1", Name: req.DefaultProject}
			f.mu.Lock()
			f.projects = append(f.projects, proj)
			f.mu.Unlock()
			out["project"] = proj
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("POST /v1/api-keys", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		_ = json.NewDecoder(r.Body).Decode(&f.keyReq)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(api.CreateAPIKeyResponse{
			APIKey: api.APIKey{ID: "newkey1", Name: f.keyReq.Name, OrgID: f.keyReq.OrgID, ProjectID: f.keyReq.ProjectID, Scopes: f.keyReq.Scopes},
			Token:  "dly_newkey1_secret",
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// called reports whether the fake saw a call, such as "POST /v1/api-keys".
func (f *fakeAPI) called(call string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.calls, call)
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

// isolate gives the CLI an empty home and clears the variables that override
// the config file, so a test reads and writes no real configuration.
func isolate(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	for _, k := range []string{"DIFFERENTLY_URL", "DIFFERENTLY_API_KEY", "DIFFERENTLY_PROJECT"} {
		t.Setenv(k, "")
	}
}

// devToken writes the token a local dev server leaves in the home directory.
func devToken(t *testing.T) {
	t.Helper()
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".differently", "dev")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("dly_dev_token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// runCLI runs one command line the way main does, with its output captured.
// The flags are package-level variables, so they are cleared when the test
// ends: a "-o json" left behind would change what other tests print.
func runCLI(t *testing.T, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	t.Cleanup(func() { flagURL, flagKey, flagProject, flagOutput = "", "", "", "" })
	root := newRoot()
	var out, errOut strings.Builder
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	err = root.Execute()
	return out.String(), errOut.String(), err
}

// privateConfig fails the test unless the config file and its directory are
// the owner's alone: the file can hold a key.
func privateConfig(t *testing.T) {
	t.Helper()
	for path, want := range map[string]os.FileMode{configPath(): 0o600, filepath.Dir(configPath()): 0o700} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s has mode %o, want %o", path, got, want)
		}
	}
}

// The path from a bootstrap operator token to a first sandbox: three commands
// and no ID copied from one into the next.
func TestFirstSandboxNeedsNoCopiedID(t *testing.T) {
	isolate(t)
	f := &fakeAPI{status: api.StatusRunning, me: whoami{KeyID: "bootop1", Operator: true}}
	url := f.start(t)

	out, _, err := runCLI(t, "", "login", "--url", url, "--api-key", "dly_bootop1_secret")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "differently admin org create NAME") {
		t.Errorf("login on an empty installation does not say how to make a project:\n%s", out)
	}
	if got := saved(); got != (settings{URL: url, APIKey: "dly_bootop1_secret"}) {
		t.Fatalf("after login the config holds %+v", got)
	}
	privateConfig(t)

	out, errOut, err := runCLI(t, "", "admin", "org", "create", "my-team")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"org_1", "prj_1", "my-team"} {
		if !strings.Contains(out, want) {
			t.Errorf("the summary leaves out %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, "{") {
		t.Errorf("table mode printed JSON:\n%s", out)
	}
	if !strings.Contains(errOut, "prj_1 is now the default project") || strings.Count(strings.TrimSpace(errOut), "\n") != 0 {
		t.Errorf("want one line saying the project was saved, got:\n%s", errOut)
	}
	if got := saved(); got != (settings{URL: url, APIKey: "dly_bootop1_secret", Project: "prj_1"}) {
		t.Fatalf("after org create the config holds %+v", got)
	}

	out, _, err = runCLI(t, "", "run", "--", "echo", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if out != "hello\n" || f.created.ProjectID != "prj_1" {
		t.Errorf("run printed %q in project %q; want hello in prj_1", out, f.created.ProjectID)
	}
}

// Org create saves a default project only when there is none, and only in a
// config that belongs to the server it talked to.
func TestOrgCreateRemembersOnlyAMissingDefault(t *testing.T) {
	const key = "dly_op_secret"
	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T, url string) // after isolate
		args   []string
		want   func(url string) settings
		stderr string
		check  func(t *testing.T)
	}{
		{
			name:   "one is saved already",
			setup:  func(t *testing.T, url string) { must(t, save(settings{URL: url, APIKey: key, Project: "prj_old"})) },
			want:   func(url string) settings { return settings{URL: url, APIKey: key, Project: "prj_old"} },
			stderr: "the default stays prj_old); use it with --project prj_1",
		},
		{
			name: "one is named in the environment",
			setup: func(t *testing.T, url string) {
				must(t, save(settings{URL: url, APIKey: key}))
				t.Setenv("DIFFERENTLY_PROJECT", "prj_env")
			},
			want:   func(url string) settings { return settings{URL: url, APIKey: key} },
			stderr: "the default stays prj_env",
		},
		{
			name:   "one is named with --project",
			setup:  func(t *testing.T, url string) { must(t, save(settings{URL: url, APIKey: key})) },
			args:   []string{"--project", "prj_flag"},
			want:   func(url string) settings { return settings{URL: url, APIKey: key} },
			stderr: "the default stays prj_flag",
		},
		{
			name: "the saved login is for another server",
			setup: func(t *testing.T, url string) {
				must(t, save(settings{URL: "https://other.example.com", APIKey: "dly_other_secret"}))
				t.Setenv("DIFFERENTLY_URL", url)
				t.Setenv("DIFFERENTLY_API_KEY", key)
			},
			want:   func(string) settings { return settings{URL: "https://other.example.com", APIKey: "dly_other_secret"} },
			stderr: "the saved config is for https://other.example.com",
		},
		{
			name: "the saved login is for another server and has a project",
			setup: func(t *testing.T, url string) {
				must(t, save(settings{URL: "https://other.example.com", APIKey: "dly_other_secret", Project: "prj_other"}))
				t.Setenv("DIFFERENTLY_URL", url)
				t.Setenv("DIFFERENTLY_API_KEY", key)
			},
			want: func(string) settings {
				return settings{URL: "https://other.example.com", APIKey: "dly_other_secret", Project: "prj_other"}
			},
			stderr: "the saved config is for https://other.example.com",
		},
		{
			name: "a project was saved for another server with no login",
			setup: func(t *testing.T, url string) {
				must(t, save(settings{Project: "prj_other", ProjectURL: "https://other.example.com"}))
				t.Setenv("DIFFERENTLY_URL", url)
				t.Setenv("DIFFERENTLY_API_KEY", key)
			},
			want:   func(string) settings { return settings{Project: "prj_other", ProjectURL: "https://other.example.com"} },
			stderr: "the saved config is for https://other.example.com",
		},
		{
			name: "the saved login names this server with a trailing slash",
			setup: func(t *testing.T, url string) {
				must(t, save(settings{URL: url + "/", APIKey: key}))
				t.Setenv("DIFFERENTLY_URL", url)
			},
			want:   func(url string) settings { return settings{URL: url + "/", APIKey: key, Project: "prj_1"} },
			stderr: "prj_1 is now the default project",
		},
		{
			name: "a local dev server has its own default",
			setup: func(t *testing.T, url string) {
				devToken(t)
				t.Setenv("DIFFERENTLY_URL", url) // loopback, and no key: the dev token and prj_default apply
			},
			want:   func(string) settings { return settings{} },
			stderr: "the default stays prj_default",
		},
		{
			name: "no login, only the environment",
			setup: func(t *testing.T, url string) {
				t.Setenv("DIFFERENTLY_URL", url)
				t.Setenv("DIFFERENTLY_API_KEY", key)
			},
			// The project is remembered with the server it belongs to. The key
			// from the environment is not written, and neither is the URL as
			// the CLI's default server.
			want:   func(url string) settings { return settings{Project: "prj_1", ProjectURL: url} },
			stderr: "prj_1 is now the default project",
			check:  privateConfig,
		},
		{
			name:   "no project was asked for",
			setup:  func(t *testing.T, url string) { must(t, save(settings{URL: url, APIKey: key})) },
			args:   []string{"--default-project", ""},
			want:   func(url string) settings { return settings{URL: url, APIKey: key} },
			stderr: "differently project create NAME --org org_1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			f := &fakeAPI{me: whoami{KeyID: "op", Operator: true}}
			url := f.start(t)
			tc.setup(t, url)
			_, errOut, err := runCLI(t, "", append([]string{"admin", "org", "create", "my-team"}, tc.args...)...)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := saved(), tc.want(url); got != want {
				t.Errorf("config holds %+v, want %+v", got, want)
			}
			if !strings.Contains(errOut, tc.stderr) {
				t.Errorf("stderr %q does not say %q", errOut, tc.stderr)
			}
			if tc.check != nil {
				tc.check(t)
			}
		})
	}
}

// A project saved without a login belongs to the server it was made on. It
// must not follow the CLI to a local dev server, whose own token and
// prj_default worked before anything was saved.
func TestSavedProjectDoesNotHideTheDevServers(t *testing.T) {
	isolate(t)
	devToken(t)
	prod := &fakeAPI{me: whoami{KeyID: "op", Operator: true}}
	prodURL := prod.start(t)
	t.Setenv("DIFFERENTLY_URL", prodURL)
	t.Setenv("DIFFERENTLY_API_KEY", "dly_prod_secret")
	if _, _, err := runCLI(t, "", "admin", "org", "create", "my-team"); err != nil {
		t.Fatal(err)
	}
	if got, want := saved(), (settings{Project: "prj_1", ProjectURL: prodURL}); got != want {
		t.Fatalf("config holds %+v, want %+v", got, want)
	}
	if got := load(); got.Project != "prj_1" {
		t.Errorf("on the server it was saved for, the project is %q", got.Project)
	}

	// Nothing names a server any more: the CLI is back on this machine, with
	// the dev server's token and project.
	t.Setenv("DIFFERENTLY_URL", "")
	t.Setenv("DIFFERENTLY_API_KEY", "")
	if got, want := load(), (settings{URL: defaultURL, APIKey: "dly_dev_token", Project: "prj_default"}); got != want {
		t.Errorf("with no server named the settings are %+v, want %+v", got, want)
	}

	// The same through a run, against a dev server on another local port.
	dev := &fakeAPI{status: api.StatusRunning}
	t.Setenv("DIFFERENTLY_URL", dev.start(t))
	out, _, err := runCLI(t, "", "run", "--", "echo", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if out != "hello\n" || dev.created.ProjectID != "prj_default" {
		t.Errorf("run printed %q in project %q; want hello in prj_default", out, dev.created.ProjectID)
	}
}

// A project saved by a login is used with that server only. On a second
// server it is not sent, and an organization made there does not replace it.
func TestSavedProjectStaysWithItsServer(t *testing.T) {
	isolate(t)
	first := &fakeAPI{status: api.StatusRunning, me: whoami{KeyID: "op", Operator: true}}
	firstURL := first.start(t)
	if _, _, err := runCLI(t, "", "login", "--url", firstURL, "--api-key", "dly_first_secret"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, "", "admin", "org", "create", "my-team"); err != nil {
		t.Fatal(err)
	}
	login := settings{URL: firstURL, APIKey: "dly_first_secret", Project: "prj_1"}
	if got := saved(); got != login {
		t.Fatalf("config holds %+v, want %+v", got, login)
	}

	second := &fakeAPI{status: api.StatusRunning, me: whoami{KeyID: "op2", Operator: true}}
	secondURL := second.start(t)
	if _, _, err := runCLI(t, "", "--url", secondURL, "--api-key", "dly_second_secret", "run", "--", "echo", "hello"); err != nil {
		t.Fatal(err)
	}
	if second.created.ProjectID != "" {
		t.Errorf("the first server's project %q was sent to the second server", second.created.ProjectID)
	}
	_, errOut, err := runCLI(t, "", "--url", secondURL, "--api-key", "dly_second_secret", "admin", "org", "create", "other-team")
	if err != nil {
		t.Fatal(err)
	}
	if got := saved(); got != login || !strings.Contains(errOut, "the saved config is for "+firstURL) {
		t.Errorf("config holds %+v, stderr %q: want the login untouched and the reason said", got, errOut)
	}

	// The first server still gets its project, with or without a trailing slash.
	if _, _, err := runCLI(t, "", "--url", firstURL+"/", "run", "--", "echo", "hello"); err != nil {
		t.Fatal(err)
	}
	if first.created.ProjectID != "prj_1" {
		t.Errorf("the first server got project %q, want prj_1", first.created.ProjectID)
	}
}

// A config file that cannot be parsed is left exactly as it is: writing the
// project over it would lose the URL and key it may hold.
func TestOrgCreateLeavesAnUnreadableConfigAlone(t *testing.T) {
	isolate(t)
	f := &fakeAPI{me: whoami{KeyID: "op", Operator: true}}
	t.Setenv("DIFFERENTLY_URL", f.start(t))
	t.Setenv("DIFFERENTLY_API_KEY", "dly_op_secret")
	broken := []byte(`{"url": "https://differently.example.com", "apiKey": "dly_saved_secret",`)
	must(t, os.MkdirAll(filepath.Dir(configPath()), 0o700))
	must(t, os.WriteFile(configPath(), broken, 0o600))

	out, errOut, err := runCLI(t, "", "admin", "org", "create", "my-team")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "prj_1") {
		t.Errorf("the organization was made but the summary is missing:\n%s", out)
	}
	if got, _ := os.ReadFile(configPath()); string(got) != string(broken) {
		t.Errorf("the config file was rewritten:\n%s", got)
	}
	for _, want := range []string{"could not save prj_1 as the default project", "not valid JSON", "--project prj_1"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr %q does not say %q", errOut, want)
		}
	}
}

// With -o json the response is printed as the API sent it and nothing else
// reaches stdout; the project is remembered all the same.
func TestOrgCreateJSON(t *testing.T) {
	isolate(t)
	f := &fakeAPI{me: whoami{KeyID: "op", Operator: true}}
	url := f.start(t)
	must(t, save(settings{URL: url, APIKey: "dly_op_secret"}))
	out, _, err := runCLI(t, "", "admin", "org", "create", "my-team", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Organization api.Organization `json:"organization"`
		Project      api.Project      `json:"project"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not one JSON value: %v\n%s", err, out)
	}
	if got.Organization.ID != "org_1" || got.Project.ID != "prj_1" || got.Project.OrgID != "org_1" {
		t.Errorf("response %+v", got)
	}
	if saved().Project != "prj_1" {
		t.Errorf("config holds %+v", saved())
	}
}

// Login saves the project a key is limited to, the one the caller names, or
// the only one the key can see. It never guesses between several.
func TestLoginSavesTheProject(t *testing.T) {
	a := api.Project{ID: "prj_a", OrgID: "org_1", Name: "default"}
	b := api.Project{ID: "prj_b", OrgID: "org_2", Name: "ci"}
	for _, tc := range []struct {
		name     string
		me       whoami
		projects []api.Project
		before   string // the project saved before this login
		args     []string
		want     string   // the project saved after
		says     []string // in the output
		code     string   // the error, if login must fail
	}{
		{name: "a key limited to one project", me: whoami{KeyID: "k", ProjectID: "prj_k"}, projects: []api.Project{a, b}, want: "prj_k", says: []string{"default project: prj_k"}},
		{name: "the only project", me: whoami{KeyID: "op", Operator: true}, projects: []api.Project{a}, want: "prj_a", says: []string{"default project: prj_a"}},
		{name: "no project, operator key", me: whoami{KeyID: "op", Operator: true}, says: []string{"differently admin org create NAME"}},
		{name: "no project, organization key", me: whoami{KeyID: "org"}, says: []string{"differently project create NAME"}},
		{name: "several projects", me: whoami{KeyID: "op", Operator: true}, projects: []api.Project{a, b},
			says: []string{"prj_a", "prj_b", "org_2", "--project ID", "DIFFERENTLY_PROJECT=ID"}},
		{name: "several projects and one named", me: whoami{KeyID: "op", Operator: true}, projects: []api.Project{a, b}, args: []string{"--project", "prj_b"}, want: "prj_b"},
		{name: "a named project the key cannot see", me: whoami{KeyID: "op", Operator: true}, projects: []api.Project{a}, args: []string{"--project", "prj_typo"}, code: api.CodeNotFound},
		{name: "the saved project is still visible", me: whoami{KeyID: "op", Operator: true}, projects: []api.Project{a, b}, before: "prj_b", want: "prj_b"},
		{name: "the saved project is from another server", me: whoami{KeyID: "op", Operator: true}, projects: []api.Project{a}, before: "prj_gone", want: "prj_a"},
		{name: "the saved project is gone and several remain", me: whoami{KeyID: "op", Operator: true}, projects: []api.Project{a, b}, before: "prj_gone", says: []string{"prj_a", "prj_b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			f := &fakeAPI{me: tc.me, projects: tc.projects}
			url := f.start(t)
			if tc.before != "" {
				must(t, save(settings{URL: url, APIKey: "dly_old_secret", Project: tc.before}))
			}
			out, _, err := runCLI(t, "", append([]string{"login", "--url", url, "--api-key", "dly_new_secret"}, tc.args...)...)
			if tc.code != "" {
				if !api.IsCode(err, tc.code) {
					t.Fatalf("err %v, want %s", err, tc.code)
				}
				if got := saved(); got.APIKey == "dly_new_secret" {
					t.Errorf("a failed login was saved: %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got, want := saved(), (settings{URL: url, APIKey: "dly_new_secret", Project: tc.want}); got != want {
				t.Errorf("config holds %+v, want %+v", got, want)
			}
			for _, s := range tc.says {
				if !strings.Contains(out, s) {
					t.Errorf("output does not say %q:\n%s", s, out)
				}
			}
		})
	}
}

// A local dev server's token and prj_default are what the CLI falls back to
// with no key. Logging in to another server on this machine with a typed key
// must not save that project.
func TestLoginDoesNotSaveTheDevProject(t *testing.T) {
	isolate(t)
	devToken(t)
	f := &fakeAPI{me: whoami{KeyID: "op", Operator: true}}
	url := f.start(t) // loopback, like a port-forward to a real installation
	out, _, err := runCLI(t, "dly_typed_secret\n", "login", "--url", url)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := saved(), (settings{URL: url, APIKey: "dly_typed_secret"}); got != want {
		t.Errorf("config holds %+v, want %+v", got, want)
	}
	if !strings.Contains(out, "API key: ") {
		t.Errorf("login did not ask for the key:\n%s", out)
	}
}

// Key create limits a key that only uses sandboxes to the default project
// and looks up the organization. Flags win, and a key with any other scope
// is not narrowed.
func TestKeyCreateUsesTheDefaultProject(t *testing.T) {
	projects := []api.Project{{ID: "prj_1", OrgID: "org_1"}, {ID: "prj_2", OrgID: "org_2"}}
	for _, tc := range []struct {
		name             string
		saved            string // the default project
		args             []string
		wantOrg, wantPrj string
		says             string
		noLookup         bool
		code             string
	}{
		{name: "no flags", saved: "prj_1", wantOrg: "org_1", wantPrj: "prj_1", says: "for project prj_1, the default project;"},
		{name: "a narrower set of sandbox scopes", saved: "prj_1", args: []string{"--scope", "sandboxes:read"}, wantOrg: "org_1", wantPrj: "prj_1", says: "the default project"},
		{name: "--project for this command", saved: "prj_1", args: []string{"--project", "prj_2"}, wantOrg: "org_2", wantPrj: "prj_2"},
		{name: "--org alone is the whole organization", saved: "prj_1", args: []string{"--org", "org_9"}, wantOrg: "org_9", says: "every project of organization org_9", noLookup: true},
		{name: "--key-project wins over the default", saved: "prj_1", args: []string{"--key-project", "prj_2"}, wantOrg: "org_2", wantPrj: "prj_2", says: "for project prj_2;"},
		{name: "both flags are sent as given", saved: "prj_1", args: []string{"--org", "org_x", "--key-project", "prj_y"}, wantOrg: "org_x", wantPrj: "prj_y", noLookup: true},
		{name: "an operator key is not narrowed", saved: "prj_1", args: []string{"--scope", "admin"}, says: "every organization (operator)", noLookup: true},
		{name: "an org:admin key is not narrowed", saved: "prj_1", args: []string{"--scope", "org:admin"}, noLookup: true},
		// A project-limited key cannot create or delete projects, so a key
		// asked to do that stays as wide as the server makes it.
		{name: "a projects:write key is not narrowed", saved: "prj_1", args: []string{"--scope", "projects:write"}, noLookup: true},
		{name: "one scope beyond sandboxes is enough", saved: "prj_1", args: []string{"--scope", "sandboxes:read,keys:write"}, noLookup: true},
		{name: "a projects:write key takes --key-project", saved: "prj_1", args: []string{"--scope", "projects:write", "--key-project", "prj_2"}, wantOrg: "org_2", wantPrj: "prj_2", says: "for project prj_2;"},
		{name: "no default project", noLookup: true},
		{name: "a default project the key cannot see", saved: "prj_gone", code: api.CodeNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			f := &fakeAPI{me: whoami{KeyID: "op", Operator: true}, projects: projects}
			url := f.start(t)
			must(t, save(settings{URL: url, APIKey: "dly_op_secret", Project: tc.saved}))
			out, _, err := runCLI(t, "", append([]string{"key", "create", "dev"}, tc.args...)...)
			if tc.code != "" {
				if !api.IsCode(err, tc.code) || f.called("POST /v1/api-keys") {
					t.Fatalf("err %v, calls %v: want %s and no key created", err, f.calls, tc.code)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if f.keyReq.OrgID != tc.wantOrg || f.keyReq.ProjectID != tc.wantPrj || f.keyReq.Name != "dev" {
				t.Errorf("request %+v, want organization %q and project %q", f.keyReq, tc.wantOrg, tc.wantPrj)
			}
			if tc.noLookup && len(f.calls) != 1 {
				t.Errorf("calls %v: want the create alone", f.calls)
			}
			if strings.Count(out, "dly_newkey1_secret") != 1 || !strings.Contains(out, tc.says) {
				t.Errorf("want the token once and %q, got:\n%s", tc.says, out)
			}
		})
	}
}

func TestKeyCreateJSON(t *testing.T) {
	isolate(t)
	f := &fakeAPI{projects: []api.Project{{ID: "prj_1", OrgID: "org_1"}}}
	must(t, save(settings{URL: f.start(t), APIKey: "dly_op_secret", Project: "prj_1"}))
	out, _, err := runCLI(t, "", "key", "create", "dev", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var got api.CreateAPIKeyResponse
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.Token != "dly_newkey1_secret" || got.ProjectID != "prj_1" {
		t.Errorf("stdout %q: %v", out, err)
	}
}

// The hint appears only for the error a key without a project gets.
func TestProjectHint(t *testing.T) {
	missing := api.Errorf(api.CodeInvalidRequest, "projectId is required for organization and operator keys")
	if h := projectHint(fmt.Errorf("create: %w", missing)); !strings.Contains(h, "differently admin org create NAME") {
		t.Errorf("hint %q", h)
	}
	for _, err := range []error{
		api.Errorf(api.CodeInvalidRequest, "name is required"),
		api.Errorf(api.CodeNotFound, "project prj_x not found"),
		errors.New("projectId is required"),
	} {
		if h := projectHint(err); h != "" {
			t.Errorf("%v: hint %q", err, h)
		}
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
