package opensandbox

// Recordings of real OpenSandbox servers, replayed without one.
//
// testdata/recordings/*.json hold every HTTP exchange the adapter had with a
// real server (the lifecycle API, and execd through the server's proxy)
// while it ran scenario below. TestReplayRecordings serves those bytes back
// from a local listener and runs the same scenario, so every `go test` checks
// the adapter against what real runtimes send, including how they fail.
//
// Record again against a live server with TestRecordOpenSandbox
// (live_test.go); test/opensandbox/README.md has the commands.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// recording is one scenario run against one real server.
type recording struct {
	Recorded  string            `json:"recorded"`
	Server    string            `json:"server"`
	Execd     string            `json:"execd"`
	Runtime   string            `json:"runtime"` // docker, kubernetes or fastsandbox
	Host      string            `json:"host,omitempty"`
	Config    map[string]string `json:"config"` // pool config, apiURL omitted
	Exchanges []exchange        `json:"exchanges"`
}

// exchange is one request and its response. Paths include the query;
// "{{host}}" in a body stands for the server's host:port.
type exchange struct {
	Step        string `json:"step"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	Request     string `json:"request,omitempty"` // small JSON bodies only
	Status      int    `json:"status"`
	ContentType string `json:"contentType,omitempty"`
	Body        string `json:"body,omitempty"`
	BodyBase64  string `json:"bodyBase64,omitempty"`
}

// recorder captures exchanges as they happen on the adapter's HTTP client.
type recorder struct {
	next    http.RoundTripper
	host    string // replaced by {{host}}
	gateway string // an ingress gateway reached directly, also {{host}}
	secret  string // never written

	mu        sync.Mutex
	step      string
	exchanges []exchange
}

func (r *recorder) mark(step string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.step = step
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	var sent []byte
	if req.Body != nil {
		sent, _ = io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(sent))
	}
	resp, err := r.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(got))
	if err != nil {
		return nil, err
	}
	clean := func(s string) string {
		if r.secret != "" {
			s = strings.ReplaceAll(s, r.secret, "{{redacted}}")
		}
		if r.gateway != "" {
			s = strings.ReplaceAll(s, r.gateway, "{{host}}")
		}
		return strings.ReplaceAll(s, r.host, "{{host}}")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e := exchange{Step: r.step, Method: req.Method, Path: req.URL.RequestURI(), Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type")}
	if strings.HasPrefix(req.Header.Get("Content-Type"), "application/json") && len(sent) <= 4096 {
		e.Request = clean(string(sent))
	}
	if utf8.Valid(got) {
		e.Body = clean(string(got))
	} else {
		e.BodyBase64 = base64.StdEncoding.EncodeToString(got)
	}
	r.exchanges = append(r.exchanges, e)
	return resp, nil
}

// replay serves a recording. Within each step, exchanges for the same
// method and path are served in order and the last one repeats, so the
// adapter may poll more or fewer times than it did when recording.
func replay(t *testing.T, rec recording) (*httptest.Server, func(step string)) {
	t.Helper()
	var (
		mu     sync.Mutex
		step   string
		served = map[string]int{}
	)
	queues := map[string][]exchange{}
	for _, e := range rec.Exchanges {
		k := e.Step + " " + e.Method + " " + timeless(e.Path)
		// Repeated identical polls (a Fast Sandbox pause polls for a minute)
		// collapse into one, so a replay sees each state change at once.
		if q := queues[k]; len(q) > 0 && q[len(q)-1] == e {
			continue
		}
		queues[k] = append(queues[k], e)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		k := step + " " + r.Method + " " + timeless(r.URL.RequestURI())
		q := queues[k]
		i := min(served[k], len(q)-1)
		served[k]++
		mu.Unlock()
		if len(q) == 0 {
			t.Errorf("step %q: request not in the recording: %s %s", step, r.Method, r.URL.RequestURI())
			http.Error(w, "not recorded", http.StatusTeapot)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		e := q[i]
		body := []byte(strings.ReplaceAll(e.Body, "{{host}}", r.Host))
		if e.BodyBase64 != "" {
			body, _ = base64.StdEncoding.DecodeString(e.BodyBase64)
		}
		if e.ContentType != "" {
			w.Header().Set("Content-Type", e.ContentType)
		}
		w.WriteHeader(e.Status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, func(s string) {
		mu.Lock()
		defer mu.Unlock()
		step = s
	}
}

// livePortCheck, set by the live test, fetches a signed port URL for real;
// a replay cannot, since the gateway is not part of the recording.
var livePortCheck func(t *testing.T, url string)

// timeless drops the one time-dependent request value, a signed port URL's
// expires=<unix seconds>, so a replay matches whenever it runs.
func timeless(path string) string { return expiresParam.ReplaceAllString(path, "expires=*") }

var expiresParam = regexp.MustCompile(`expires=\d+`)

// Fixed IDs keep the recorded paths and metadata queries stable.
const (
	recSandbox      = "sbx_rec_main"
	recInstallation = "inst_rec"
	recBadImage     = "docker.io/library/ubuntu:no-such-tag-differently"
	recBadTemplate  = "tpl-no-such-template-differently"
)

// scenario drives the adapter through what Differently relies on and checks
// the outcome for the runtime that served it. step names the exchanges that
// follow, for the recorder or the replay.
func scenario(t *testing.T, a *Adapter, runtime string, step func(string)) {
	t.Helper()
	// Fast Sandbox writes a full checkpoint for each pause and snapshot
	// (minutes for a desktop-sized template).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	fatal := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}

	step("health")
	fatal("health", a.Health(ctx))

	// Fast Sandbox templates fix the workload, so env goes with each exec.
	createEnv, execEnv := map[string]string{"GREETING": "hello"}, map[string]string{"WHO": "world"}
	if runtime == runtimeFastSandbox {
		createEnv, execEnv["GREETING"] = nil, "hello"
	}
	step("create")
	in, err := a.Create(ctx, adapter.CreateRequest{ID: recSandbox, Installation: recInstallation, Resources: api.Resources{CPU: 1, MemoryMiB: 512}, Env: createEnv, TTLSeconds: 900})
	fatal("create", err)
	if in.State != adapter.StateRunning || in.SandboxID != recSandbox {
		t.Fatalf("create returned %+v", in)
	}
	step("create-again")
	again, err := a.Create(ctx, adapter.CreateRequest{ID: recSandbox, Installation: recInstallation})
	if err != nil || again.Ref != in.Ref {
		t.Fatalf("create is not idempotent: %+v %v", again, err)
	}

	step("get-list")
	got, err := a.Get(ctx, in.Ref)
	if err != nil || got.State != adapter.StateRunning {
		t.Fatalf("get %+v %v", got, err)
	}
	list, err := a.List(ctx, recInstallation)
	if err != nil || !slices.ContainsFunc(list, func(i adapter.Instance) bool { return i.Ref == in.Ref && i.SandboxID == recSandbox }) {
		t.Fatalf("list %+v %v", list, err)
	}

	step("exec")
	res, err := a.Exec(ctx, in.Ref, api.ExecRequest{Command: `echo "$GREETING $WHO"; printf 'a\nb'; printf err >&2; exit 7`, Env: execEnv})
	if err != nil || res.ExitCode != 7 || res.Stdout != "hello world\na\nb" || res.Stderr != "err" {
		t.Fatalf("exec %+v %v", res, err)
	}
	step("exec-argv")
	res, err = a.Exec(ctx, in.Ref, api.ExecRequest{Args: []string{"sh", "-c", "pwd; echo \"$1\"", "sh", "it's quoted"}, Cwd: "/tmp"})
	if err != nil || res.Stdout != "/tmp\nit's quoted\n" {
		t.Fatalf("exec argv %+v %v", res, err)
	}
	step("exec-stdin")
	res, err = a.Exec(ctx, in.Ref, api.ExecRequest{Command: "tr a-z A-Z", Stdin: "hello\nworld\n"})
	if err != nil || res.Stdout != "HELLO\nWORLD\n" {
		t.Fatalf("stdin %+v %v", res, err)
	}
	res, err = a.Exec(ctx, in.Ref, api.ExecRequest{Command: "ls /tmp/.differently-stdin-* 2>/dev/null | wc -l"})
	if err != nil || strings.TrimSpace(res.Stdout) != "0" {
		t.Fatalf("the staged stdin file was not removed: %+v %v", res, err)
	}
	if a.opts.NetworkPolicy != nil {
		// egress: allowlist with egressAllow=pypi.org: only that host is reachable.
		step("egress")
		probe := func(host string) string {
			res, err := a.Exec(ctx, in.Ref, api.ExecRequest{Command: "timeout 8 bash -c 'exec 3<>/dev/tcp/" + host + "/443' 2>/dev/null && echo open || echo blocked"})
			if err != nil {
				t.Fatalf("egress probe %s: %v", host, err)
			}
			return strings.TrimSpace(res.Stdout)
		}
		if got := probe("pypi.org"); got != "open" {
			t.Fatalf("allowed host pypi.org: %s", got)
		}
		if got := probe("example.com"); got != "blocked" {
			t.Fatalf("host outside the allowlist example.com: %s", got)
		}
	}
	step("exec-timeout")
	start := time.Now()
	res, err = a.Exec(ctx, in.Ref, api.ExecRequest{Command: "sleep 30", TimeoutSeconds: 1})
	if err == nil && res.ExitCode == 0 || time.Since(start) > 20*time.Second {
		t.Fatalf("timeout not enforced: %+v %v after %s", res, err, time.Since(start))
	}
	step("exec-stream")
	// stdout and stderr are separate pipes: order holds within a stream only.
	streams := map[string]string{}
	code, err := a.ExecStream(ctx, in.Ref, api.ExecRequest{Command: "for i in 1 2 3; do echo line$i; done; echo e >&2; exit 3"}, func(ev api.ExecEvent) error {
		streams[ev.Stream] += ev.Data
		return nil
	})
	if err != nil || code != 3 || streams["stdout"] != "line1\nline2\nline3\n" || streams["stderr"] != "e\n" {
		t.Fatalf("stream code=%d %q %v", code, streams, err)
	}

	step("files")
	data := make([]byte, 8<<10)
	for i := range data {
		data[i] = byte(i * 7)
	}
	fatal("write file", a.WriteFile(ctx, in.Ref, "/tmp/rec.bin", data))
	back, err := a.ReadFile(ctx, in.Ref, "/tmp/rec.bin")
	if err != nil || !bytes.Equal(back, data) {
		t.Fatalf("read file: %d bytes, %v", len(back), err)
	}
	files, err := a.ListFiles(ctx, in.Ref, "/tmp")
	if err != nil || !slices.ContainsFunc(files, func(f api.FileInfo) bool { return strings.HasSuffix(f.Path, "/rec.bin") && f.Size == int64(len(data)) }) {
		t.Fatalf("list files %+v %v", files, err)
	}

	step("pause")
	err = a.Pause(ctx, in.Ref)
	switch runtime {
	case "kubernetes-registry": // pause keeps no memory there: stop/start instead
		if !api.IsCode(err, api.CodeUnsupportedOperation) {
			t.Fatalf("pause on the Kubernetes runtime: want unsupported_operation, got %v", err)
		}
		step("stop")
		fatal("stop", a.Stop(ctx, in.Ref))
		if got, err := a.Get(ctx, in.Ref); err != nil || got.State != adapter.StateStopped {
			t.Fatalf("after stop: %+v %v", got, err)
		}
		step("start")
		fatal("start", a.Start(ctx, in.Ref))
		if back, err := a.ReadFile(ctx, in.Ref, "/tmp/rec.bin"); err != nil || !bytes.Equal(back, data) {
			t.Fatalf("file lost across stop/start: %v", err)
		}
	case "kubernetes": // no snapshot registry: accepted, never started
		if !api.IsCode(err, api.CodeUnsupportedOperation) {
			t.Fatalf("pause on the Kubernetes runtime without a registry: want unsupported_operation, got %v", err)
		}
	default:
		fatal("pause", err)
		step("resume")
		fatal("resume", a.Resume(ctx, in.Ref))
		if back, err := a.ReadFile(ctx, in.Ref, "/tmp/rec.bin"); err != nil || !bytes.Equal(back, data) {
			t.Fatalf("file lost across pause/resume: %v", err)
		}
	}

	step("snapshot")
	snap, err := a.Snapshot(ctx, in.Ref, "disk")
	switch runtime {
	case "kubernetes": // no snapshot registry
		if !api.IsCode(err, api.CodeUnsupportedOperation) || !strings.Contains(err.Error(), "snapshot") {
			t.Fatalf("snapshot without a registry: want unsupported_operation, got %v", err)
		}
	default:
		fatal("snapshot", err)
		step("fork")
		fork, err := a.Create(ctx, adapter.CreateRequest{ID: recSandbox + "_fork", Installation: recInstallation, Resources: api.Resources{CPU: 1, MemoryMiB: 512}, FromSnapshot: snap.Ref, TTLSeconds: 900})
		fatal("fork", err)
		if back, err := a.ReadFile(ctx, fork.Ref, "/tmp/rec.bin"); err != nil || !bytes.Equal(back, data) {
			t.Fatalf("fork does not carry the parent's file: %v", err)
		}
		fatal("delete fork", a.Delete(ctx, fork.Ref))
		step("delete-snapshot")
		fatal("delete snapshot", a.DeleteSnapshot(ctx, snap.Ref))
	}

	step("port")
	// Direct endpoints are addresses only the runtime host can use, which is
	// why ports.expose is not a default feature.
	url, err := a.ExposePort(ctx, in.Ref, 8080)
	switch {
	case a.opts.SignedPorts:
		// A signed gateway URL: /{sandbox}/{port}/{expires}/{signature}.
		if err != nil || !strings.Contains(url, "/"+in.Ref+"/8080/") {
			t.Fatalf("signed port URL %q %v", url, err)
		}
		if livePortCheck != nil {
			livePortCheck(t, url)
		}
	case runtime == runtimeFastSandbox:
		// A route scope: <gateway>/f1.<namespace>.<sandbox>.<port>.<key>.<mac>.
		if err != nil || !strings.HasPrefix(url, a.opts.PortScheme+"://") || !strings.Contains(url, "/f1.") {
			t.Fatalf("Fast Sandbox port URL %q %v", url, err)
		}
	// Otherwise a pod IP (host:8080), or a gateway path (/{sandbox}/8080)
	// when OpenSandbox announces its ingress gateway.
	case err != nil || !strings.HasPrefix(url, "http://") || !strings.HasSuffix(url, ":8080") && !strings.HasSuffix(url, "/8080") && strings.HasPrefix(runtime, "kubernetes"):
		t.Fatalf("port endpoint %q %v", url, err)
	}

	step("expiry")
	fatal("set expiry", a.SetExpiry(ctx, in.Ref, time.Now().Add(20*time.Minute)))

	step("delete")
	fatal("delete", a.Delete(ctx, in.Ref))
	step("after-delete")
	// Fast Sandbox finishes a delete after answering it.
	_, err = a.Get(ctx, in.Ref)
	for deadline := time.Now().Add(time.Minute); runtime == runtimeFastSandbox && err == nil && time.Now().Before(deadline); {
		time.Sleep(2 * time.Second)
		_, err = a.Get(ctx, in.Ref)
	}
	if !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("get after delete: want not_found, got %v", err)
	}
	fatal("delete is idempotent", a.Delete(ctx, in.Ref))

	step("bad-image")
	bad, want := recBadImage, api.CodeInvalidRequest // the Docker runtime reports the failed pull at once
	switch {
	case strings.HasPrefix(runtime, "kubernetes"):
		want = adapter.CodeSandboxFailed // the pod just never becomes ready
	case runtime == runtimeFastSandbox:
		bad, want = recBadTemplate, api.CodeNotFound // a template ID, not an image
	}
	_, err = a.Create(ctx, adapter.CreateRequest{ID: "sbx_rec_badimage", Installation: recInstallation, Template: bad, Resources: api.Resources{CPU: 1, MemoryMiB: 256}, TTLSeconds: 300})
	if !api.IsCode(err, want) {
		t.Fatalf("unpullable image: want %s, got %v", want, err)
	}
}

func TestReplayRecordings(t *testing.T) {
	paths, _ := filepath.Glob("testdata/recordings/*.json")
	if len(paths) == 0 {
		t.Fatal("no recordings in testdata/recordings")
	}
	for _, path := range paths {
		t.Run(strings.TrimSuffix(filepath.Base(path), ".json"), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var rec recording
			if err := json.Unmarshal(raw, &rec); err != nil {
				t.Fatal(err)
			}
			srv, step := replay(t, rec)
			cfg := map[string]string{}
			for k, v := range rec.Config {
				cfg[k] = v
			}
			cfg["apiURL"] = srv.URL + "/v1"
			if cfg["portScheme"] != "" {
				cfg["portScheme"] = "http" // the replay server is plain HTTP
			}
			o, err := FromConfig(cfg, api.TrustPrivate, "replay-key")
			if err != nil {
				t.Fatal(err)
			}
			// Replayed responses arrive at once: bound waits so a mismatch
			// fails fast instead of polling for minutes.
			o.ReadyTimeout, o.PauseStartTimeout = 5*time.Second, time.Second
			scenario(t, New(o), rec.Runtime, step)
		})
	}
}

// writeRecording saves a recording made by TestRecordOpenSandbox.
func writeRecording(t *testing.T, name string, rec recording) {
	t.Helper()
	out, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "recordings", name+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s (%d exchanges)", path, len(rec.Exchanges))
}
