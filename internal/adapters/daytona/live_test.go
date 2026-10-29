//go:build live

package daytona

// Live check against a real Daytona account. It creates at most three small
// sandboxes at a time, each with a short provider TTL, and deletes every
// sandbox and snapshot it made, including on failure:
//
//	DAYTONA_API_KEY=… go test -tags live -v -timeout 30m -run TestLiveDaytona ./internal/adapters/daytona/
//
// Settings:
//
//	DAYTONA_API_KEY           API key (required; the test is skipped without it)
//	DAYTONA_API_URL           control plane URL (default https://app.daytona.io/api)
//	DAYTONA_TARGET            region (default us)
//	DAYTONA_SNAPSHOT          base snapshot for container sandboxes (default: Daytona's)
//	DAYTONA_VM_SNAPSHOT       snapshot for the linux-vm class (default daytona-vm-small)
//	DAYTONA_DESKTOP_SNAPSHOT  2 vCPU / 4 GiB / 8 GiB snapshot (default daytona-medium)

import (
	"context"
	"errors"
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

func TestLiveDaytona(t *testing.T) {
	key := os.Getenv("DAYTONA_API_KEY")
	if key == "" {
		t.Skip("set DAYTONA_API_KEY to run against a live Daytona account")
	}
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	cfg := map[string]string{"apiURL": os.Getenv("DAYTONA_API_URL"), "target": env("DAYTONA_TARGET", "us"), "snapshot": os.Getenv("DAYTONA_SNAPSHOT")}
	ct := New(FromConfig(cfg, key))
	installation := id.New("inst")
	r := newLiveRun(t)
	ctx := context.Background()

	r.step("health", func() error { return ct.Health(ctx) })
	r.step("capabilities", func() error {
		caps, err := ct.Capabilities(ctx)
		if err == nil && (caps.Has(adapter.FeaturePauseResume) || !caps.Has(adapter.FeatureSnapshotDisk)) {
			err = fmt.Errorf("container class features %v", caps.Features)
		}
		return err
	})

	reqA := adapter.CreateRequest{
		ID: id.New("sbx"), Installation: installation, TTLSeconds: 1800,
		Resources: api.Resources{CPU: 1, MemoryMiB: 1024, DiskGiB: 3},
		Env:       map[string]string{"DLY_GREETING": "hello from differently"},
	}
	var a adapter.Instance
	if !r.step("create (default snapshot)", func() (err error) {
		a, err = ct.Create(ctx, reqA)
		if a.Ref != "" {
			r.cleanupSandbox(ct, a.Ref)
		}
		if err == nil && (a.State != adapter.StateRunning || a.SandboxID != reqA.ID) {
			err = fmt.Errorf("instance %+v", a)
		}
		return err
	}) {
		t.FailNow()
	}
	r.step("idempotent create", func() error {
		again, err := ct.Create(ctx, reqA)
		if err == nil && again.Ref != a.Ref {
			r.cleanupSandbox(ct, again.Ref)
			err = fmt.Errorf("replayed create made a second sandbox %s (first %s)", again.Ref, a.Ref)
		}
		return err
	})
	exec := func(req api.ExecRequest) (api.ExecResult, error) { return ct.Exec(ctx, a.Ref, req) }
	r.step("guest", func() error {
		res, err := exec(api.ExecRequest{Command: guestInfo})
		r.note("guest", strings.ReplaceAll(strings.TrimSpace(res.Stdout), "\n", "; "))
		return err
	})
	r.step("exec output and exit code", func() error {
		res, err := exec(api.ExecRequest{Command: "sh -c 'echo out; echo err >&2; exit 3'"})
		r.note("exec output and exit code", fmt.Sprintf("stdout %q stderr %q", res.Stdout, res.Stderr))
		if err == nil && (res.ExitCode != 3 || !strings.Contains(res.Stdout, "out") || !strings.Contains(res.Stdout+res.Stderr, "err")) {
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
		res, err := exec(api.ExecRequest{Command: `sh -c 'echo "$DLY_GREETING|$REQ_VAR"'`, Env: map[string]string{"REQ_VAR": "per request"}})
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
	r.step("exec stdin is refused", func() error {
		if _, err := exec(api.ExecRequest{Command: "cat", Stdin: "x"}); !api.IsCode(err, api.CodeUnsupportedOperation) {
			return fmt.Errorf("got %v", err)
		}
		return nil
	})
	r.step("files upload/download/list", func() error {
		if err := ct.WriteFile(ctx, a.Ref, "/tmp/dly/hello.txt", []byte("hello file\n")); err != nil {
			return err
		}
		b, err := ct.ReadFile(ctx, a.Ref, "/tmp/dly/hello.txt")
		if err != nil {
			return err
		}
		if string(b) != "hello file\n" {
			return fmt.Errorf("read back %q", b)
		}
		items, err := ct.ListFiles(ctx, a.Ref, "/tmp/dly")
		if err != nil {
			return err
		}
		if len(items) != 1 || items[0].Name != "hello.txt" || items[0].IsDir || items[0].Size != 11 {
			return fmt.Errorf("list %+v", items)
		}
		r.note("files upload/download/list", fmt.Sprintf("entry %+v", items[0]))
		if _, err := ct.ReadFile(ctx, a.Ref, "/tmp/dly/missing.txt"); !api.IsCode(err, api.CodeNotFound) {
			return fmt.Errorf("missing file: %v", err)
		}
		return nil
	})
	r.step("ports expose (signed preview URL)", func() error {
		token := "dly-" + strings.ToLower(id.New("tok"))
		if _, err := exec(api.ExecRequest{Command: httpServer(8080, token)}); err != nil {
			return err
		}
		u, err := ct.ExposePort(ctx, a.Ref, 8080)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(u, "https://") {
			return fmt.Errorf("url %q", u)
		}
		return fetchUntil(u, token, 2*time.Minute)
	})
	r.step("list for the reconciler", func() error {
		list, err := ct.List(ctx, installation)
		if err != nil {
			return err
		}
		if len(list) != 1 || list[0].Ref != a.Ref || list[0].SandboxID != reqA.ID || list[0].State != adapter.StateRunning {
			return fmt.Errorf("list %+v", list)
		}
		if other, err := ct.List(ctx, id.New("inst")); err != nil || len(other) != 0 {
			return fmt.Errorf("another installation sees %d sandboxes (%v)", len(other), err)
		}
		return nil
	})
	r.step("stop/start keeps disk", func() error {
		if res, err := exec(api.ExecRequest{Command: "sh -c 'echo kept > /var/tmp/persist.txt && sync'"}); err != nil || res.ExitCode != 0 {
			return fmt.Errorf("write: %+v %v", res, err)
		}
		if err := ct.Stop(ctx, a.Ref); err != nil {
			return fmt.Errorf("stop: %w", err)
		}
		in, err := ct.Get(ctx, a.Ref)
		r.note("stop/start keeps disk", "state right after stop: "+in.State)
		if err != nil || in.State != adapter.StateStopped {
			return fmt.Errorf("after stop: %+v %v", in, err)
		}
		if err := ct.Start(ctx, a.Ref); err != nil {
			return fmt.Errorf("start: %w", err)
		}
		if in, err := ct.Get(ctx, a.Ref); err != nil || in.State != adapter.StateRunning {
			return fmt.Errorf("after start: %+v %v", in, err)
		}
		got, err := ct.ReadFile(ctx, a.Ref, "/var/tmp/persist.txt")
		if err != nil || string(got) != "kept\n" {
			return fmt.Errorf("file after restart: %q %v", got, err)
		}
		return nil
	})

	var snap adapter.Snapshot
	r.step("disk snapshot", func() (err error) {
		snap, err = ct.Snapshot(ctx, a.Ref, "disk")
		if snap.Ref != "" {
			r.cleanupSnapshot(ct, snap.Ref)
		}
		if err == nil {
			var in adapter.Instance
			in, err = ct.Get(ctx, a.Ref)
			if err == nil && in.State != adapter.StateRunning {
				err = fmt.Errorf("source sandbox is %s after the snapshot", in.State)
			}
		}
		return err
	})
	if snap.Ref != "" {
		r.step("create from the snapshot", func() error {
			req := adapter.CreateRequest{ID: id.New("sbx"), Installation: installation, TTLSeconds: 900, FromSnapshot: snap.Ref}
			in, err := ct.Create(ctx, req)
			if in.Ref != "" {
				r.cleanupSandbox(ct, in.Ref)
			}
			if err != nil {
				return err
			}
			got, err := ct.ReadFile(ctx, in.Ref, "/var/tmp/persist.txt")
			if err == nil && string(got) != "kept\n" {
				err = fmt.Errorf("restored file %q", got)
			}
			if derr := ct.Delete(ctx, in.Ref); err == nil {
				err = derr
			}
			return err
		})
		r.step("delete snapshot (repeatable)", func() error {
			for i := 0; i < 2; i++ {
				if err := ct.DeleteSnapshot(ctx, snap.Ref); err != nil {
					return fmt.Errorf("delete #%d: %w", i+1, err)
				}
			}
			return nil
		})
	}

	// The linux-vm class: pause and resume keep memory.
	vcfg := map[string]string{"apiURL": cfg["apiURL"], "target": cfg["target"], "class": "linux-vm", "snapshot": env("DAYTONA_VM_SNAPSHOT", "daytona-vm-small")}
	vm := New(FromConfig(vcfg, key))
	var v adapter.Instance
	r.step("linux-vm create", func() (err error) {
		v, err = vm.Create(ctx, adapter.CreateRequest{ID: id.New("sbx"), Installation: installation, TTLSeconds: 900})
		if v.Ref != "" {
			r.cleanupSandbox(vm, v.Ref)
		}
		if api.IsCode(err, api.CodeInvalidRequest) && strings.Contains(err.Error(), "not available in region") {
			return unavailable{err.Error()}
		}
		return err
	})
	if v.Ref != "" && v.State == adapter.StateRunning {
		r.step("linux-vm guest", func() error {
			res, err := vm.Exec(ctx, v.Ref, api.ExecRequest{Command: guestInfo})
			r.note("linux-vm guest", strings.ReplaceAll(strings.TrimSpace(res.Stdout), "\n", "; "))
			return err
		})
		r.step("linux-vm pause/resume keeps memory", func() error {
			res, err := vm.Exec(ctx, v.Ref, api.ExecRequest{Command: "sh -c 'nohup setsid sleep 3600 >/dev/null 2>&1 & echo $!'"})
			if err != nil {
				return err
			}
			p := strings.TrimSpace(res.Stdout)
			if err := vm.Pause(ctx, v.Ref); err != nil {
				return fmt.Errorf("pause: %w", err)
			}
			in, err := vm.Get(ctx, v.Ref)
			r.note("linux-vm pause/resume keeps memory", "state right after pause: "+in.State)
			if err != nil || in.State != adapter.StatePaused {
				return fmt.Errorf("after pause: %+v %v", in, err)
			}
			if err := vm.Resume(ctx, v.Ref); err != nil {
				return fmt.Errorf("resume: %w", err)
			}
			res, err = vm.Exec(ctx, v.Ref, api.ExecRequest{Command: "kill -0 " + p})
			if err != nil || res.ExitCode != 0 {
				return fmt.Errorf("process %s did not survive pause: %+v %v", p, res, err)
			}
			return nil
		})
		r.step("linux-vm memory snapshot", func() error {
			s, err := vm.Snapshot(ctx, v.Ref, "memory")
			if s.Ref != "" {
				r.cleanupSnapshot(vm, s.Ref)
				if derr := vm.DeleteSnapshot(ctx, s.Ref); err == nil {
					err = derr
				}
			}
			return err
		})
		r.step("linux-vm delete", func() error { return vm.Delete(ctx, v.Ref) })
	}

	// A desktop-sized sandbox: 2 vCPU / 4 GiB / 8 GiB comes from the snapshot.
	dcfg := map[string]string{"apiURL": cfg["apiURL"], "target": cfg["target"], "snapshot": env("DAYTONA_DESKTOP_SNAPSHOT", "daytona-medium")}
	desk := New(FromConfig(dcfg, key))
	r.step("create 2 vCPU / 4 GiB / 8 GiB", func() error {
		in, err := desk.Create(ctx, adapter.CreateRequest{ID: id.New("sbx"), Installation: installation, TTLSeconds: 900, Resources: api.Resources{CPU: 2, MemoryMiB: 4096, DiskGiB: 8}})
		if in.Ref != "" {
			r.cleanupSandbox(desk, in.Ref)
		}
		if err != nil {
			return err
		}
		// A container sees the host's CPUs and memory; its cgroup has the limits.
		res, err := desk.Exec(ctx, in.Ref, api.ExecRequest{Command: "sh -c 'cat /sys/fs/cgroup/cpu.max /sys/fs/cgroup/memory.max; df -BG --output=size / | tail -1'"})
		got := strings.Fields(res.Stdout)
		r.note("create 2 vCPU / 4 GiB / 8 GiB", "cpu.max, memory.max, disk: "+strings.Join(got, " "))
		if err == nil && (len(got) != 4 || got[0] != "200000" || got[1] != "100000" || got[2] != "4294967296" || got[3] != "8G") {
			err = fmt.Errorf("limits %q", res.Stdout)
		}
		if derr := desk.Delete(ctx, in.Ref); err == nil {
			err = derr
		}
		return err
	})

	r.step("delete (repeatable)", func() error {
		for i := 0; i < 2; i++ {
			if err := ct.Delete(ctx, a.Ref); err != nil {
				return fmt.Errorf("delete #%d: %w", i+1, err)
			}
		}
		if _, err := ct.Get(ctx, a.Ref); !api.IsCode(err, api.CodeNotFound) {
			return fmt.Errorf("get after delete: %v", err)
		}
		return nil
	})
	// Daytona's list lags a delete by a few seconds; the reconciler then
	// deletes the stale entry again, which is harmless.
	r.step("list after delete", func() error {
		start := time.Now()
		for {
			list, err := ct.List(ctx, installation)
			if err != nil {
				return err
			}
			if len(list) == 0 {
				r.note("list after delete", "empty after "+time.Since(start).Round(100*time.Millisecond).String())
				return nil
			}
			if time.Since(start) > time.Minute {
				return fmt.Errorf("still listed after a minute: %+v", list)
			}
			time.Sleep(time.Second)
		}
	})
}

const guestInfo = `sh -c 'head -1 /etc/os-release; id -un; uname -m; cat /sys/fs/cgroup/cpu.max /sys/fs/cgroup/memory.max; df -h / | tail -1; for c in curl python3; do command -v $c; done'`

func (r *liveRun) cleanupSandbox(a *Adapter, ref string) {
	r.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := a.Delete(ctx, ref); err != nil {
			r.t.Errorf("cleanup: delete sandbox %s: %v", ref, err)
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

// httpServer serves token on port in the background.
func httpServer(port int, token string) string {
	return fmt.Sprintf(`sh -c 'mkdir -p /tmp/www && echo %s > /tmp/www/index.html && cd /tmp/www && (nohup setsid python3 -m http.server %d >/tmp/www.log 2>&1 &) ; sleep 1'`, token, port)
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
	return fmt.Errorf("the port URL never served the page; last: %s", last)
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 120 {
		s = s[:120]
	}
	return s
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
	var u unavailable
	switch {
	case errors.As(err, &u):
		result = "unavailable on this account: " + u.reason
		err = nil
	case err != nil:
		result = "FAIL: " + err.Error()
		r.t.Errorf("%s: %v", name, err)
	}
	line := fmt.Sprintf("  %-38s %-8s %s", name, took, result)
	if n := r.notes[name]; n != "" {
		line += " (" + n + ")"
	}
	r.lines = append(r.lines, line)
	r.t.Log(line)
	return err == nil
}

// unavailable marks a step the account cannot run (a class or region it
// does not have): recorded, not failed.
type unavailable struct{ reason string }

func (u unavailable) Error() string { return u.reason }
