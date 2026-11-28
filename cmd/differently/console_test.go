package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOrchestrator answers the three calls the local admin makes and records
// what reached it.
type fakeOrchestrator struct {
	*httptest.Server
	mu   sync.Mutex
	seen []*http.Request
}

func newFakeOrchestrator(t *testing.T, key string) *fakeOrchestrator {
	t.Helper()
	f := &fakeOrchestrator{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seen = append(f.seen, r.Clone(context.Background()))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/version" {
			io.WriteString(w, `{"version":"9.9.9","installation":"inst_1"}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+key {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"code":"unauthenticated","message":"bad key"}}`)
			return
		}
		switch r.URL.Path {
		case "/v1/me":
			io.WriteString(w, `{"keyId":"key_1","scopes":["admin"],"operator":true}`)
		case "/v1/capabilities":
			io.WriteString(w, `{"items":[{"pool":"private","trust":"private","health":"healthy"}]}`)
		default:
			io.WriteString(w, `{"path":"`+r.URL.Path+`","method":"`+r.Method+`"}`)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeOrchestrator) last() *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[len(f.seen)-1]
}

// startLocalAdmin serves a local admin on a free port and returns it with
// its address.
func startLocalAdmin(t *testing.T, orch settings) (*localAdmin, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a, err := newLocalAdmin(orch, ln.Addr())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serveLocalAdmin(ctx, ln, a) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the local admin did not stop")
		}
	})
	return a, "http://" + ln.Addr().String()
}

// signedIn is a browser that opened the printed address.
func signedIn(t *testing.T, a *localAdmin, base string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	res, err := c.Get(base + "/?token=" + a.token)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || res.Request.URL.RawQuery != "" {
		t.Fatalf("opening the printed address: status %d, ended at %s", res.StatusCode, res.Request.URL)
	}
	return c
}

// body reads a response: body(t)(http.Get(...)) gives its status and text.
func body(t *testing.T) func(*http.Response, error) (int, string) {
	return func(res *http.Response, err error) (int, string) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(b)
	}
}

// Only the browser that opened the printed address gets in. The local admin
// acts with the CLI's key, so every other caller must get nothing: no page,
// no discovery, no proxy.
func TestLocalAdminLetsInOnlyItsOwnSession(t *testing.T) {
	fakeTools(t, nil, nil)
	orch := newFakeOrchestrator(t, "secret-key")
	a, base := startLocalAdmin(t, settings{URL: orch.URL, APIKey: "secret-key"})
	before := len(orch.seen)

	for _, path := range []string{"/", "/local.js", "/local/discover", "/local/orchestrator", "/dashboard/", "/v1/me", "/version", "/v1/admin/pools"} {
		if code, _ := body(t)(http.Get(base + path)); code != http.StatusForbidden {
			t.Errorf("GET %s with no session: status %d, want 403", path, code)
		}
	}
	if code, _ := body(t)(http.Post(base+"/v1/sandboxes", "text/plain", strings.NewReader("{}"))); code != http.StatusForbidden {
		t.Errorf("POST with no session: status %d, want 403", code)
	}
	if code, _ := body(t)(http.Get(base + "/?token=wrong")); code != http.StatusForbidden {
		t.Errorf("a wrong token: status %d, want 403", code)
	}
	// The token opens the page only: it is not a password for the API.
	if code, _ := body(t)(http.Get(base + "/v1/me?token=" + a.token)); code != http.StatusForbidden {
		t.Errorf("token on an API path: status %d, want 403", code)
	}

	// Another name that resolves to this machine, even with the session.
	c := signedIn(t, a, base)
	req, _ := http.NewRequest("GET", base+"/local/orchestrator", nil)
	req.Host = "evil.example:80"
	for _, ck := range c.Jar.Cookies(req.URL) {
		req.AddCookie(ck)
	}
	if code, _ := body(t)(http.DefaultClient.Do(req)); code != http.StatusForbidden {
		t.Errorf("another Host with the session cookie: status %d, want 403", code)
	}
	if got := len(orch.seen) - before; got != 0 {
		t.Errorf("%d refused requests reached the orchestrator", got)
	}

	// The session cookie is not readable by scripts; the marker is, and names
	// the port.
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(base, "http://"))
	seen := map[string]*http.Cookie{}
	res, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Get(base + "/?token=" + a.token)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	for _, ck := range res.Cookies() {
		seen[ck.Name] = ck
	}
	if ck := seen["dly_local_session_"+port]; ck == nil || !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode || ck.Value != a.token {
		t.Errorf("session cookie = %+v", ck)
	}
	if ck := seen["dly_local"]; ck == nil || ck.HttpOnly || ck.Value != port {
		t.Errorf("marker cookie = %+v", ck)
	}
}

func TestLocalAdminServesItsPageDiscoveryAndTheDashboard(t *testing.T) {
	fakeTools(t, []string{"docker"}, map[string]string{
		"docker --version":                        "Docker version 27.3.1, build ce12230\n",
		"docker info --format {{.ServerVersion}}": "29.5.2\n",
	})
	orch := newFakeOrchestrator(t, "secret-key")
	a, base := startLocalAdmin(t, settings{URL: orch.URL, APIKey: "secret-key"})
	c := signedIn(t, a, base)

	code, page := body(t)(c.Get(base + "/"))
	if code != 200 || !strings.Contains(page, "local admin") || !strings.Contains(page, `src="local.js"`) {
		t.Errorf("page: status %d\n%s", code, page)
	}
	for _, path := range []string{"/local.js", "/local.css", "/dashboard/", "/dashboard/app.js", "/dashboard/style.css"} {
		if code, _ := body(t)(c.Get(base + path)); code != 200 {
			t.Errorf("GET %s: status %d", path, code)
		}
	}

	code, out := body(t)(c.Get(base + "/local/discover"))
	var d discovery
	if err := json.Unmarshal([]byte(out), &d); code != 200 || err != nil {
		t.Fatalf("discover: status %d, %v\n%s", code, err, out)
	}
	if len(d.Tools) != 6 || len(d.Found) != 1 || d.Possible[0].What != "Run the whole stack on this Docker host" {
		t.Errorf("discover = %+v", d)
	}

	code, out = body(t)(c.Get(base + "/local/orchestrator"))
	var st orchestratorStatus
	if err := json.Unmarshal([]byte(out), &st); code != 200 || err != nil {
		t.Fatalf("orchestrator: status %d, %v\n%s", code, err, out)
	}
	if !st.Reachable || !st.Authenticated || st.Version != "9.9.9" || st.KeyID != "key_1" || len(st.Pools) != 1 || st.Pools[0].Pool != "private" {
		t.Errorf("orchestrator = %+v", st)
	}
	// The key is for the server side only.
	if strings.Contains(out, "secret-key") || strings.Contains(page, "secret-key") {
		t.Error("the key reached the browser")
	}
}

// The proxy adds the CLI's key and passes nothing the browser used to
// identify itself: not its session cookie, not an Authorization header.
func TestLocalAdminProxyAddsTheKeyAndDropsTheBrowsersIdentity(t *testing.T) {
	fakeTools(t, nil, nil)
	orch := newFakeOrchestrator(t, "secret-key")
	a, base := startLocalAdmin(t, settings{URL: orch.URL, APIKey: "secret-key"})
	c := signedIn(t, a, base)

	req, _ := http.NewRequest("POST", base+"/v1/sandboxes?x=1", strings.NewReader(`{"image":"debian"}`))
	req.Header.Set("Authorization", "Bearer ")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", base)
	code, out := body(t)(c.Do(req))
	if code != 200 || !strings.Contains(out, `"path":"/v1/sandboxes"`) || !strings.Contains(out, `"method":"POST"`) {
		t.Fatalf("proxied call: status %d, %s", code, out)
	}
	got := orch.last()
	if got.Header.Get("Authorization") != "Bearer secret-key" {
		t.Errorf("Authorization at the orchestrator = %q", got.Header.Get("Authorization"))
	}
	if got.Header.Get("Cookie") != "" || got.Header.Get("Origin") != "" {
		t.Errorf("the browser's cookie or origin reached the orchestrator: cookie %q, origin %q", got.Header.Get("Cookie"), got.Header.Get("Origin"))
	}
	if got.URL.RawQuery != "x=1" {
		t.Errorf("query = %q", got.URL.RawQuery)
	}
	// Only the API is forwarded.
	if code, _ := body(t)(c.Get(base + "/metrics")); code != http.StatusNotFound {
		t.Errorf("GET /metrics: status %d, want 404", code)
	}
}

// No orchestrator is a normal state: the page and discovery still work, and
// the status says what is missing.
func TestLocalAdminWithoutAnOrchestrator(t *testing.T) {
	fakeTools(t, nil, nil)
	dead := httptest.NewServer(http.NotFoundHandler())
	gone := dead.URL
	dead.Close()
	a, base := startLocalAdmin(t, settings{URL: gone})
	c := signedIn(t, a, base)

	var st orchestratorStatus
	_, out := body(t)(c.Get(base + "/local/orchestrator"))
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatal(err)
	}
	if st.Reachable || st.Authenticated || st.Pools == nil || !strings.Contains(st.Problem, "nothing answers") {
		t.Errorf("status = %+v", st)
	}
	if code, _ := body(t)(c.Get(base + "/local/discover")); code != 200 {
		t.Errorf("discover without an orchestrator: status %d", code)
	}
	if code, out := body(t)(c.Get(base + "/v1/me")); code != http.StatusBadGateway || !strings.Contains(out, "does not answer") {
		t.Errorf("proxy without an orchestrator: status %d, %s", code, out)
	}

	// One that answers but has no key saved, and one that refuses the key.
	orch := newFakeOrchestrator(t, "secret-key")
	for key, want := range map[string]string{"": "no key is saved", "wrong": "refused the saved key"} {
		a, base := startLocalAdmin(t, settings{URL: orch.URL, APIKey: key})
		_, out := body(t)(signedIn(t, a, base).Get(base + "/local/orchestrator"))
		var st orchestratorStatus
		_ = json.Unmarshal([]byte(out), &st)
		if !st.Reachable || st.Authenticated || !strings.Contains(st.Problem, want) {
			t.Errorf("key %q: status = %+v", key, st)
		}
	}
}

func TestConsoleCommandPrintsItsAddressAndStops(t *testing.T) {
	fakeTools(t, nil, nil)
	t.Setenv("DIFFERENTLY_URL", "http://127.0.0.1:1")
	opened := make(chan string, 1)
	open := openBrowser
	openBrowser = func(address string) error { opened <- address; return nil }
	t.Cleanup(func() { openBrowser = open })

	root := newRoot()
	t.Cleanup(func() { flagURL, flagKey, flagProject, flagOutput = "", "", "", "" })
	ctx, cancel := context.WithCancel(context.Background())
	var out strings.Builder
	root.SetOut(&out)
	root.SetArgs([]string{"console", "--port", "0"})
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()

	var address string
	select {
	case address = <-opened:
	case <-time.After(5 * time.Second):
		t.Fatal("no browser was opened")
	}
	if !strings.HasPrefix(address, "http://127.0.0.1:") || !strings.Contains(address, "/?token=") {
		t.Errorf("address = %q", address)
	}
	jar, _ := cookiejar.New(nil)
	if code, page := body(t)((&http.Client{Jar: jar}).Get(address)); code != 200 || !strings.Contains(page, "local admin") {
		t.Errorf("the opened address: status %d", code)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("console returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("console did not stop when its context ended")
	}
	if !strings.Contains(out.String(), "Local admin: "+address) {
		t.Errorf("output = %q", out.String())
	}
}

// localAdminOnRealServer is a local admin whose orchestrator is the real
// control plane in this process, with a setup file beside it.
func localAdminOnRealServer(t *testing.T, file string) (*localAdmin, *http.Client, string) {
	t.Helper()
	realServer(t)
	a, base := startLocalAdmin(t, settings{URL: os.Getenv("DIFFERENTLY_URL"), APIKey: os.Getenv("DIFFERENTLY_API_KEY")})
	a.setupFile = file
	return a, signedIn(t, a, base), base
}

func getSetupStatus(t *testing.T, c *http.Client, base string) setupStatus {
	t.Helper()
	code, out := body(t)(c.Get(base + "/local/setup"))
	var st setupStatus
	if err := json.Unmarshal([]byte(out), &st); code != 200 || err != nil {
		t.Fatalf("GET /local/setup: status %d, %v\n%s", code, err, out)
	}
	return st
}

func postApply(t *testing.T, c *http.Client, base, plan string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", base+"/local/setup/apply", strings.NewReader(`{"plan":"`+plan+`"}`))
	req.Header.Set("Content-Type", "application/json")
	code, out := body(t)(c.Do(req))
	var got map[string]any
	_ = json.Unmarshal([]byte(out), &got)
	return code, got
}

// The page's plan is the CLI's plan, and its Apply applies exactly the plan
// it showed: after it the server matches, and a plan that is no longer the
// current one is refused before anything changes.
func TestLocalAdminShowsAndAppliesTheSetupFile(t *testing.T) {
	file := setup(t, firstSetup)
	_, c, base := localAdminOnRealServer(t, file)

	st := getSetupStatus(t, c, base)
	if !st.Exists || st.Problem != "" || st.ToApply != 4 || st.Blocked != 0 || len(st.Changes) != 4 || st.Plan == "" {
		t.Fatalf("setup = %+v", st)
	}
	if st.Changes[0].Kind != "policy" || st.Changes[1].Action != actCreate || st.Changes[1].Name != "private-a" {
		t.Errorf("changes = %+v", st.Changes)
	}
	// What the CLI prints for the same file is the same plan.
	if out := mustCLI(t, "diff", "-f", file); !strings.Contains(out, "4 to apply.") {
		t.Errorf("the CLI's diff:\n%s", out)
	}

	// A plan the page never showed is refused, and nothing is created.
	code, got := postApply(t, c, base, "0000")
	if code != http.StatusConflict || !strings.Contains(got["problem"].(string), "look again") {
		t.Fatalf("a stale plan: status %d, %+v", code, got)
	}
	if out := mustCLI(t, "diff", "-f", file); !strings.Contains(out, "4 to apply.") {
		t.Fatalf("a refused apply changed the server:\n%s", out)
	}

	code, got = postApply(t, c, base, st.Plan)
	if code != 200 || got["applied"].(float64) != 4 || got["problem"] != nil {
		t.Fatalf("apply: status %d, %+v", code, got)
	}
	after := getSetupStatus(t, c, base)
	if after.ToApply != 0 || !strings.Contains(after.Summary, "The server matches") {
		t.Errorf("after apply = %+v", after)
	}
	if out := mustCLI(t, "diff", "-f", file, "--exit-code"); !strings.Contains(out, "The server matches") {
		t.Errorf("the CLI's diff after the page applied:\n%s", out)
	}

	// The file changes after the page read it: the old plan no longer applies.
	if err := os.WriteFile(file, []byte(strings.Replace(firstSetup, "maxConcurrency: 20", "maxConcurrency: 9", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _ := postApply(t, c, base, after.Plan); code != http.StatusConflict {
		t.Errorf("applying a plan from before the file changed: status %d", code)
	}
	changed := getSetupStatus(t, c, base)
	if changed.ToApply != 1 || changed.Plan == after.Plan {
		t.Errorf("after the file changed = %+v", changed)
	}
}

// The button never deletes, and never applies around something that cannot
// be applied.
func TestLocalAdminApplyNeverDeletesAndStopsOnWhatCannotBeApplied(t *testing.T) {
	file := setup(t, firstSetup)
	_, c, base := localAdminOnRealServer(t, file)
	st := getSetupStatus(t, c, base)
	postApply(t, c, base, st.Plan)

	// A file that names less: the rest is listed, never planned for deletion.
	os.WriteFile(file, []byte("pools:\n  - {name: private-a, adapter: fake, trust: private}\n"), 0o600)
	less := getSetupStatus(t, c, base)
	if less.ToApply != 0 || less.Extra != 2 {
		t.Fatalf("a file that names less = %+v", less)
	}
	for _, ch := range less.Changes {
		if ch.Action == actDelete {
			t.Errorf("the page plans a deletion: %+v", ch)
		}
	}

	// Something that cannot be applied, beside something that could.
	os.WriteFile(file, []byte(strings.Replace(strings.Replace(firstSetup, "trust: private", "trust: external", 1), "maxConcurrency: 5", "maxConcurrency: 6", 1)), 0o600)
	blocked := getSetupStatus(t, c, base)
	if blocked.Blocked != 1 || blocked.ToApply != 1 {
		t.Fatalf("blocked = %+v", blocked)
	}
	code, got := postApply(t, c, base, blocked.Plan)
	if code != http.StatusConflict || !strings.Contains(got["problem"].(string), "nothing was changed") {
		t.Errorf("apply with something that cannot be applied: status %d, %+v", code, got)
	}
	if again := getSetupStatus(t, c, base); again.ToApply != 1 {
		t.Errorf("the change beside it was applied: %+v", again)
	}
}

// No file, a file with a mistake, and an orchestrator that does not answer
// are states the page shows, not failures. And the apply endpoint is behind
// the same session as the rest, with a JSON body on top.
func TestLocalAdminSetupStatesAndGuard(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "differently.yaml")
	a, c, base := localAdminOnRealServer(t, missing)
	if st := getSetupStatus(t, c, base); st.Exists || st.Problem != "" || st.File != missing {
		t.Errorf("no file = %+v", st)
	}
	os.WriteFile(missing, []byte("pools:\n  - name: a\n"), 0o600)
	if st := getSetupStatus(t, c, base); !st.Exists || !strings.Contains(st.Problem, "has no adapter") || st.Plan != "" {
		t.Errorf("a file with a mistake = %+v", st)
	}

	// Without the session: nothing, for reading and for applying.
	if code, _ := body(t)(http.Get(base + "/local/setup")); code != http.StatusForbidden {
		t.Errorf("GET /local/setup with no session: status %d", code)
	}
	if code, _ := body(t)(http.Post(base+"/local/setup/apply", "application/json", strings.NewReader(`{"plan":"x"}`))); code != http.StatusForbidden {
		t.Errorf("POST apply with no session: status %d", code)
	}
	// With the session, but as a form another site could post: refused.
	if code, _ := body(t)(c.Post(base+"/local/setup/apply", "text/plain", strings.NewReader(`{"plan":"x"}`))); code != http.StatusUnsupportedMediaType {
		t.Errorf("POST apply without a JSON content type: status %d", code)
	}
	if code, _ := body(t)(c.Post(base+"/local/setup/apply", "application/json", strings.NewReader(`{}`))); code != http.StatusBadRequest {
		t.Errorf("POST apply with no plan: status %d", code)
	}
	if code, _ := body(t)(c.Get(base + "/local/setup/apply")); code == 200 {
		t.Error("GET on the apply endpoint answered 200")
	}

	// An orchestrator that does not answer.
	os.WriteFile(missing, []byte(firstSetup), 0o600)
	a.orch.URL = "http://127.0.0.1:1"
	if st := getSetupStatus(t, c, base); !st.Exists || !strings.Contains(st.Problem, "read the pools") {
		t.Errorf("no orchestrator = %+v", st)
	}
}
