//go:build live

package freestyle

// Live check against a real Freestyle account. It creates at most three
// small VMs at a time, each with a short provider TTL, and deletes every VM
// and snapshot it made, including on failure:
//
//	FREESTYLE_API_KEY=… go test -tags live -v -timeout 30m -run TestLiveFreestyle ./internal/adapters/freestyle/
//
// Settings:
//
//	FREESTYLE_API_KEY   API key (required; the test is skipped without it)
//	FREESTYLE_API_URL   API base URL (default https://api.freestyle.sh)
//	FREESTYLE_SNAPSHOT  base snapshot for new VMs (default: the platform's)
//	FREESTYLE_DOMAIN    domain for exposed ports (default style.dev)

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/different-ai/differently/internal/id"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

func TestLiveFreestyle(t *testing.T) {
	key := os.Getenv("FREESTYLE_API_KEY")
	if key == "" {
		t.Skip("set FREESTYLE_API_KEY to run against a live Freestyle account")
	}
	cfg := map[string]string{
		"apiURL":   os.Getenv("FREESTYLE_API_URL"),
		"snapshot": os.Getenv("FREESTYLE_SNAPSHOT"),
		"domain":   os.Getenv("FREESTYLE_DOMAIN"),
		"egress":   "none",
	}
	closed := New(FromConfig(cfg, key))
	cfg["egress"] = "public"
	open := New(FromConfig(cfg, key))
	installation := id.New("inst")
	r := newLiveRun(t)
	ctx := context.Background()

	r.step("health", func() error { return closed.Health(ctx) })
	r.step("capabilities", func() error {
		caps, err := closed.Capabilities(ctx)
		if err == nil && !caps.Has(adapter.FeatureEgressControl) {
			err = fmt.Errorf("egress control not advertised: %v", caps.Features)
		}
		return err
	})

	// A: the main sandbox, deny-all egress, asking for a desktop-sized VM.
	reqA := adapter.CreateRequest{
		ID: id.New("sbx"), Installation: installation, TTLSeconds: 1800,
		Resources: api.Resources{CPU: 2, MemoryMiB: 4096, DiskGiB: 8},
		Env:       map[string]string{"DLY_GREETING": "hello from differently"},
	}
	var a adapter.Instance
	if !r.step("create (2 vCPU / 4 GiB / 8 GiB)", func() (err error) {
		a, err = closed.Create(ctx, reqA)
		if a.Ref != "" {
			r.cleanupVM(closed, a.Ref)
		}
		if err == nil && (a.State != adapter.StateRunning || a.SandboxID != reqA.ID) {
			err = fmt.Errorf("instance %+v", a)
		}
		return err
	}) {
		t.FailNow()
	}
	r.step("size after create", func() error {
		v, err := closed.get(ctx, a.Ref)
		if err != nil {
			return err
		}
		r.note("size after create", fmt.Sprintf("%d vCPU / %d MiB / %d MiB disk", v.Resources.CPU, v.Resources.Memory, v.Resources.Storage))
		if v.Resources.CPU < 2 || v.Resources.Memory < 4096 || v.Resources.Storage < 8192 {
			return fmt.Errorf("VM is %+v, smaller than requested", v.Resources)
		}
		return nil
	})
	r.step("idempotent create", func() error {
		again, err := closed.Create(ctx, reqA)
		if err == nil && again.Ref != a.Ref {
			err = fmt.Errorf("replayed create made a second VM %s (first %s)", again.Ref, a.Ref)
			r.cleanupVM(closed, again.Ref)
		}
		return err
	})
	exec := func(req api.ExecRequest) (api.ExecResult, error) { return closed.Exec(ctx, a.Ref, req) }
	r.step("guest", func() error {
		res, err := exec(api.ExecRequest{Command: "head -2 /etc/os-release; uname -m; nproc; free -m | sed -n 2p; df -h / | tail -1; for c in curl wget python3 busybox; do command -v $c; done"})
		r.note("guest", strings.ReplaceAll(strings.TrimSpace(res.Stdout), "\n", "; "))
		return err
	})
	r.step("exec stdout/stderr/exit code", func() error {
		res, err := exec(api.ExecRequest{Command: "echo out; echo err >&2; exit 3"})
		if err == nil && (res.ExitCode != 3 || strings.TrimSpace(res.Stdout) != "out" || strings.TrimSpace(res.Stderr) != "err") {
			err = fmt.Errorf("got %+v", res)
		}
		return err
	})
	r.step("exec args", func() error {
		res, err := exec(api.ExecRequest{Args: []string{"printf", "%s|", "a b", "it's"}})
		if err == nil && res.Stdout != "a b|it's|" {
			err = fmt.Errorf("got %q", res.Stdout)
		}
		return err
	})
	r.step("exec env (sandbox and request)", func() error {
		res, err := exec(api.ExecRequest{Command: `echo "$DLY_GREETING|$REQ_VAR"`, Env: map[string]string{"REQ_VAR": "per request"}})
		if err == nil && strings.TrimSpace(res.Stdout) != "hello from differently|per request" {
			err = fmt.Errorf("got %+v", res)
		}
		return err
	})
	r.step("exec cwd", func() error {
		res, err := exec(api.ExecRequest{Command: "pwd", Cwd: "/tmp"})
		if err == nil && strings.TrimSpace(res.Stdout) != "/tmp" {
			err = fmt.Errorf("got %+v", res)
		}
		return err
	})
	r.step("exec timeout", func() error {
		start := time.Now()
		res, err := exec(api.ExecRequest{Command: "sleep 30", TimeoutSeconds: 2})
		took := time.Since(start)
		r.note("exec timeout", fmt.Sprintf("exit %d after %s", res.ExitCode, took.Round(100*time.Millisecond)))
		if err == nil && (res.ExitCode != 124 || took > 20*time.Second) {
			err = fmt.Errorf("got %+v after %s", res, took)
		}
		return err
	})
	r.step("exec stdin", func() error {
		res, err := exec(api.ExecRequest{Command: "cat; echo", Stdin: "piped input"})
		if err == nil && strings.TrimSpace(res.Stdout) != "piped input" {
			err = fmt.Errorf("got %+v", res)
		}
		return err
	})
	r.step("env file is private", func() error {
		res, err := exec(api.ExecRequest{Command: "stat -c '%a %U' " + envFile + "; id -un"})
		r.note("env file is private", strings.ReplaceAll(strings.TrimSpace(res.Stdout), "\n", ", exec user "))
		if f := strings.Fields(res.Stdout); err == nil && (len(f) != 3 || f[0] != "600" || f[1] != f[2]) {
			err = fmt.Errorf("env file %q", strings.TrimSpace(res.Stdout+res.Stderr))
		}
		return err
	})
	r.step("overwritten file stays readable", func() error {
		for _, s := range []string{"first\n", "second\n"} {
			if err := closed.WriteFile(ctx, a.Ref, "/tmp/dly-twice/f.txt", []byte(s)); err != nil {
				return err
			}
		}
		owners, _ := exec(api.ExecRequest{Command: "stat -c '%U' /tmp/dly-twice /tmp/dly-twice/f.txt"})
		r.note("overwritten file stays readable", "owners dir, file: "+strings.ReplaceAll(strings.TrimSpace(owners.Stdout), "\n", ", "))
		res, err := exec(api.ExecRequest{Command: "cat /tmp/dly-twice/f.txt"})
		if err == nil && (res.ExitCode != 0 || res.Stdout != "second\n") {
			err = fmt.Errorf("exec reads %s", oneLine(res))
		}
		return err
	})
	r.step("files upload/download/list", func() error {
		if err := closed.WriteFile(ctx, a.Ref, "/tmp/dly/hello.txt", []byte("hello file\n")); err != nil {
			return err
		}
		b, err := closed.ReadFile(ctx, a.Ref, "/tmp/dly/hello.txt")
		if err != nil {
			return err
		}
		if string(b) != "hello file\n" {
			return fmt.Errorf("read back %q", b)
		}
		items, err := closed.ListFiles(ctx, a.Ref, "/tmp/dly")
		if err != nil {
			return err
		}
		if len(items) != 1 || items[0].Name != "hello.txt" || items[0].Path != "/tmp/dly/hello.txt" || items[0].IsDir {
			return fmt.Errorf("list %+v", items)
		}
		if _, err := closed.ReadFile(ctx, a.Ref, "/tmp/dly/missing.txt"); !api.IsCode(err, api.CodeNotFound) {
			return fmt.Errorf("missing file: %v", err)
		}
		return nil
	})
	r.step("egress none blocks outbound", func() error {
		res, err := exec(api.ExecRequest{Command: outboundCheck, TimeoutSeconds: 30})
		r.note("egress none blocks outbound", oneLine(res))
		if err == nil && res.ExitCode == 0 {
			err = fmt.Errorf("outbound request succeeded with egress none: %s", oneLine(res))
		}
		return err
	})
	r.step("ports expose (public host)", func() error {
		token := "dly-" + strings.ToLower(id.New("tok"))
		if _, err := exec(api.ExecRequest{Command: httpServer(8080, token)}); err != nil {
			return err
		}
		u, err := closed.ExposePort(ctx, a.Ref, 8080)
		if err != nil {
			return err
		}
		r.note("ports expose (public host)", u)
		return fetchUntil(u, token, 3*time.Minute)
	})
	r.step("ports expose again", func() error {
		_, err := closed.ExposePort(ctx, a.Ref, 8080)
		return err
	})

	// B: public egress.
	reqB := adapter.CreateRequest{ID: id.New("sbx"), Installation: installation, TTLSeconds: 900}
	var b adapter.Instance
	r.step("create with egress public", func() (err error) {
		b, err = open.Create(ctx, reqB)
		if b.Ref != "" {
			r.cleanupVM(open, b.Ref)
		}
		return err
	})
	if b.Ref != "" {
		r.step("egress public allows outbound", func() error {
			res, err := open.Exec(ctx, b.Ref, api.ExecRequest{Command: outboundCheck, TimeoutSeconds: 30})
			if err == nil && res.ExitCode != 0 {
				err = fmt.Errorf("outbound request failed with egress public: %s", oneLine(res))
			}
			return err
		})
	}
	r.step("list for the reconciler", func() error {
		list, err := closed.List(ctx, installation)
		if err != nil {
			return err
		}
		want := map[string]string{a.Ref: reqA.ID}
		if b.Ref != "" {
			want[b.Ref] = reqB.ID
		}
		for _, in := range list {
			if want[in.Ref] == in.SandboxID {
				delete(want, in.Ref)
			}
		}
		if len(want) > 0 {
			return fmt.Errorf("list %+v is missing %v", list, want)
		}
		if other, err := closed.List(ctx, id.New("inst")); err != nil || len(other) != 0 {
			return fmt.Errorf("another installation sees %d VMs (%v)", len(other), err)
		}
		return nil
	})
	if b.Ref != "" {
		r.step("delete (repeatable)", func() error {
			for i := 0; i < 2; i++ {
				if err := open.Delete(ctx, b.Ref); err != nil {
					return fmt.Errorf("delete #%d: %w", i+1, err)
				}
			}
			if _, err := open.Get(ctx, b.Ref); !api.IsCode(err, api.CodeNotFound) {
				return fmt.Errorf("get after delete: %v", err)
			}
			return nil
		})
	}

	// Pause keeps memory: a process started before it is still there after.
	pid := func() string {
		res, err := exec(api.ExecRequest{Command: "nohup setsid sleep 3600 >/dev/null 2>&1 & echo $!"})
		if err != nil {
			t.Errorf("start a process: %v", err)
		}
		return strings.TrimSpace(res.Stdout)
	}
	alive := func(p string) bool {
		res, err := exec(api.ExecRequest{Command: "kill -0 " + p})
		return err == nil && res.ExitCode == 0
	}
	r.step("pause/resume keeps memory", func() error {
		p := pid()
		if err := closed.Pause(ctx, a.Ref); err != nil {
			return fmt.Errorf("pause: %w", err)
		}
		if in, err := closed.Get(ctx, a.Ref); err != nil || in.State != adapter.StatePaused {
			return fmt.Errorf("after pause: %+v %v", in, err)
		}
		if err := closed.Resume(ctx, a.Ref); err != nil {
			return fmt.Errorf("resume: %w", err)
		}
		if !alive(p) {
			return fmt.Errorf("process %s did not survive pause", p)
		}
		return nil
	})
	r.step("stop/start keeps disk", func() error {
		if _, err := exec(api.ExecRequest{Command: "echo kept > /var/tmp/persist.txt && sync"}); err != nil {
			return err
		}
		p := pid()
		if err := closed.Stop(ctx, a.Ref); err != nil {
			return fmt.Errorf("stop: %w", err)
		}
		// With automatic restart on, Freestyle boots a powered-off VM again
		// about 5 s later; it must stay stopped.
		time.Sleep(10 * time.Second)
		if in, err := closed.Get(ctx, a.Ref); err != nil || in.State != adapter.StateStopped {
			return fmt.Errorf("10 s after stop: %+v %v", in, err)
		}
		if err := closed.Start(ctx, a.Ref); err != nil {
			return fmt.Errorf("start: %w", err)
		}
		got, err := closed.ReadFile(ctx, a.Ref, "/var/tmp/persist.txt")
		if err != nil || string(got) != "kept\n" {
			return fmt.Errorf("file after restart: %q %v", got, err)
		}
		if alive(p) {
			return fmt.Errorf("process %s survived a stop; the VM was not stopped", p)
		}
		return nil
	})

	// Memory snapshot and a fork from it.
	var snap adapter.Snapshot
	p := pid()
	r.step("memory snapshot", func() (err error) {
		snap, err = closed.Snapshot(ctx, a.Ref, "memory")
		if snap.Ref != "" {
			r.cleanupSnapshot(closed, snap.Ref)
		}
		if err == nil && snap.Kind != "memory" {
			err = fmt.Errorf("snapshot %+v", snap)
		}
		return err
	})
	if snap.Ref != "" {
		reqC := adapter.CreateRequest{ID: id.New("sbx"), Installation: installation, TTLSeconds: 900, FromSnapshot: snap.Ref}
		r.step("fork from the snapshot", func() error {
			c, err := closed.Create(ctx, reqC)
			if c.Ref != "" {
				r.cleanupVM(closed, c.Ref)
			}
			if err != nil {
				return err
			}
			res, err := closed.Exec(ctx, c.Ref, api.ExecRequest{Command: "cat /var/tmp/persist.txt; kill -0 " + p + " && echo alive; echo \"$DLY_GREETING\""})
			if err == nil && res.Stdout != "kept\nalive\nhello from differently\n" {
				err = fmt.Errorf("fork state %q", res.Stdout)
			}
			if derr := closed.Delete(ctx, c.Ref); err == nil {
				err = derr
			}
			return err
		})
		r.step("delete snapshot (repeatable)", func() error {
			for i := 0; i < 2; i++ {
				if err := closed.DeleteSnapshot(ctx, snap.Ref); err != nil {
					return fmt.Errorf("delete #%d: %w", i+1, err)
				}
			}
			return nil
		})
	}
	r.step("delete", func() error {
		if err := closed.Delete(ctx, a.Ref); err != nil {
			return err
		}
		if err := closed.Delete(ctx, a.Ref); err != nil {
			return fmt.Errorf("second delete: %w", err)
		}
		if _, err := closed.Get(ctx, a.Ref); !api.IsCode(err, api.CodeNotFound) {
			return fmt.Errorf("get after delete: %v", err)
		}
		return nil
	})
	r.step("list after delete", func() error {
		list, err := closed.List(ctx, installation)
		if err == nil && len(list) != 0 {
			err = fmt.Errorf("still listed: %+v", list)
		}
		return err
	})
}

func (r *liveRun) cleanupVM(a *Adapter, ref string) {
	r.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := a.Delete(ctx, ref); err != nil {
			r.t.Errorf("cleanup: delete VM %s: %v", ref, err)
		}
	})
}

func (r *liveRun) cleanupSnapshot(a *Adapter, ref string) {
	r.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := a.DeleteSnapshot(ctx, ref); err != nil {
			r.t.Errorf("cleanup: delete snapshot %s: %v", ref, err)
		}
	})
}

// outboundCheck exits 0 only if an HTTPS request to the public internet works.
const outboundCheck = `if command -v curl >/dev/null; then curl -sS -m 10 -o /dev/null -w '%{http_code}\n' https://example.com;
elif command -v wget >/dev/null; then wget -q -T 10 -O /dev/null https://example.com && echo ok;
else python3 -c 'import urllib.request; print(urllib.request.urlopen("https://example.com", timeout=10).status)'; fi`

// httpServer serves token on port in the background.
func httpServer(port int, token string) string {
	return fmt.Sprintf(`mkdir -p /tmp/www && echo %s > /tmp/www/index.html && cd /tmp/www &&
if command -v python3 >/dev/null; then nohup setsid python3 -m http.server %d >/tmp/www.log 2>&1 &
else nohup setsid busybox httpd -f -p %d -h /tmp/www >/tmp/www.log 2>&1 & fi
sleep 1`, token, port, port)
}

func fetchUntil(u, token string, within time.Duration) error {
	deadline := time.Now().Add(within)
	last := ""
	for time.Now().Before(deadline) {
		resp, err := http.Get(u)
		if err == nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if resp.StatusCode == 200 && strings.Contains(string(b), token) {
				return nil
			}
			last = fmt.Sprintf("%d %q", resp.StatusCode, firstLine(string(b)))
		} else {
			last = err.Error()
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("%s never served the page; last: %s", u, last)
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

func oneLine(res api.ExecResult) string {
	return fmt.Sprintf("exit %d: %s", res.ExitCode, firstLine(res.Stdout+" "+res.Stderr))
}

// liveRun records each step's outcome and duration and prints a summary.
type liveRun struct {
	t     *testing.T
	lines []string
	notes map[string]string
}

func newLiveRun(t *testing.T) *liveRun {
	r := &liveRun{t: t, notes: map[string]string{}}
	t.Cleanup(func() {
		t.Log("summary:\n" + strings.Join(r.lines, "\n"))
	})
	return r
}

func (r *liveRun) note(step, s string) { r.notes[step] = s }

func (r *liveRun) step(name string, fn func() error) bool {
	r.t.Helper()
	start := time.Now()
	err := fn()
	took := time.Since(start).Round(100 * time.Millisecond)
	result := "pass"
	if err != nil {
		result = "FAIL: " + err.Error()
		r.t.Errorf("%s: %v", name, err)
	}
	line := fmt.Sprintf("  %-34s %-8s %s", name, took, result)
	if n := r.notes[name]; n != "" {
		line += " (" + n + ")"
	}
	r.lines = append(r.lines, line)
	r.t.Log(line)
	return err == nil
}
