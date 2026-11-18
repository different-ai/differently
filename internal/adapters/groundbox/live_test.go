//go:build live && experimental_adapters

package groundbox

// Live check against a real Groundbox account. It creates at most four
// small sandboxes at a time (1 vCPU, 1 GB, 5 GB), each with a short auto-stop
// time and tagged with an installation ID of its own, and deletes every one
// it made, including on failure. Other sandboxes in the account are left
// alone:
//
//	GROUNDBOX_API_KEY=… go test -tags live,experimental_adapters -v -timeout 20m -run TestLiveGroundbox ./internal/adapters/groundbox/
//
// Settings:
//
//	GROUNDBOX_API_KEY   API key with the sandboxes:read and sandboxes:write scopes (required; the test is skipped without it)
//	GROUNDBOX_API_URL   API base URL (default https://api.groundbox.dev/v1)
//	GROUNDBOX_TEMPLATE  template for new sandboxes (default: Groundbox's own image)
//	GROUNDBOX_IMAGE     a Docker image to also start one sandbox from, such as python:3.12-slim (default: that step is skipped)

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/different-ai/differently/internal/id"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

func TestLiveGroundbox(t *testing.T) {
	key := os.Getenv("GROUNDBOX_API_KEY")
	if key == "" {
		t.Skip("set GROUNDBOX_API_KEY to run against a live Groundbox account")
	}
	build := func(extra map[string]string) *Adapter {
		cfg := map[string]string{"apiURL": os.Getenv("GROUNDBOX_API_URL"), "template": os.Getenv("GROUNDBOX_TEMPLATE")}
		for k, v := range extra {
			cfg[k] = v
		}
		o, err := FromConfig(cfg, key)
		if err != nil {
			t.Fatal(err)
		}
		return New(o)
	}
	closed := build(map[string]string{"egress": "none"})
	open := build(nil)
	public := build(map[string]string{"egress": "none", "ports": "public"})
	listed := build(map[string]string{"egress": "allowlist", "egressAllow": "example.com, *.wikipedia.org, example.org", "egressDeny": "example.org"})
	installation := id.New("inst")
	small := api.Resources{CPU: 1, MemoryMiB: 1024, DiskGiB: 5}
	r := newLiveRun(t)
	ctx := context.Background()

	r.step("health", func() error { return closed.Health(ctx) })
	r.step("health with a wrong key", func() error {
		err := New(Options{APIURL: os.Getenv("GROUNDBOX_API_URL"), APIKey: "gbx_not_a_key"}).Health(ctx)
		if !api.IsCode(err, api.CodeProviderUnavailable) || strings.Contains(err.Error(), "gbx_not_a_key") {
			return fmt.Errorf("got %v", err)
		}
		return nil
	})
	r.step("capabilities", func() error {
		caps, err := closed.Capabilities(ctx)
		if err == nil && len(caps.Missing([]string{adapter.FeatureExecStream, adapter.FeatureStopStart, adapter.FeaturePauseResume, adapter.FeatureFork, adapter.FeaturePorts, adapter.FeatureEgressControl})) > 0 {
			err = fmt.Errorf("features %v", caps.Features)
		}
		return err
	})

	// A: the main sandbox, no egress.
	reqA := adapter.CreateRequest{
		ID: id.New("sbx"), Installation: installation, TTLSeconds: 900, Resources: small,
		Env:    map[string]string{"DLY_GREETING": "hello from differently"},
		Labels: map[string]string{adapter.LabelOrg: "org_live", adapter.LabelProject: "prj_live"},
	}
	var a adapter.Instance
	if !r.step("create (1 vCPU / 1 GiB / 5 GiB)", func() (err error) {
		a, err = closed.Create(ctx, reqA)
		if a.Ref != "" {
			r.cleanup(closed, a.Ref)
		}
		if err == nil && (a.State != adapter.StateRunning || a.SandboxID != reqA.ID) {
			err = fmt.Errorf("instance %+v", a)
		}
		return err
	}) {
		t.FailNow()
	}
	r.step("idempotent create", func() error {
		again, err := closed.Create(ctx, reqA)
		if err == nil && again.Ref != a.Ref {
			err = fmt.Errorf("replayed create made a second sandbox %s (first %s)", again.Ref, a.Ref)
			r.cleanup(closed, again.Ref)
		}
		return err
	})
	exec := func(req api.ExecRequest) (api.ExecResult, error) { return closed.Exec(ctx, a.Ref, req) }
	r.step("guest", func() error {
		res, err := exec(api.ExecRequest{Command: "head -1 /etc/os-release; uname -m; echo $(nproc) vCPU; free -m | awk 'NR==2{print $2\" MiB\"}'; id -un"})
		r.note("guest", strings.ReplaceAll(strings.TrimSpace(res.Stdout), "\n", "; "))
		if err == nil && !strings.Contains(res.Stdout, "1 vCPU") {
			err = fmt.Errorf("not the size asked for: %s", oneLine(res))
		}
		return err
	})
	r.step("exec stdout/stderr/exit code", func() error {
		res, err := exec(api.ExecRequest{Command: "echo out; echo err >&2; exit 3"})
		if err == nil && (res.ExitCode != 3 || strings.TrimSpace(res.Stdout) != "out" || strings.TrimSpace(res.Stderr) != "err") {
			err = fmt.Errorf("got %+v", res)
		}
		return err
	})
	r.step("exec args (no shell)", func() error {
		res, err := exec(api.ExecRequest{Args: []string{"printf", "%s|", "a b", "it's", "$HOME", "*"}})
		if err == nil && res.Stdout != "a b|it's|$HOME|*|" {
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
		res, err := exec(api.ExecRequest{Command: "cat; echo; echo err >&2; exit 4", Stdin: "piped input"})
		if err == nil && (strings.TrimSpace(res.Stdout) != "piped input" || strings.TrimSpace(res.Stderr) != "err" || res.ExitCode != 4) {
			err = fmt.Errorf("got %+v", res)
		}
		return err
	})
	r.step("exec stdin (bytes that are not text)", func() error {
		res, err := exec(api.ExecRequest{Command: "od -An -tx1 | tr -d ' \n'", Stdin: "a\x00b\xff\n"})
		if err == nil && res.Stdout != "610062ff0a" {
			err = fmt.Errorf("got %+v", res)
		}
		return err
	})
	r.step("exec stdin (1 MiB, over the stream)", func() error {
		res, err := exec(api.ExecRequest{Command: "wc -c; echo err >&2; exit 6", Stdin: strings.Repeat("0123456789abcde\n", 1<<16)})
		if err == nil && (strings.TrimSpace(res.Stdout) != "1048576" || strings.TrimSpace(res.Stderr) != "err" || res.ExitCode != 6) {
			err = fmt.Errorf("got %+v", res)
		}
		return err
	})
	r.step("exec stream", func() error {
		var events []api.ExecEvent
		var first, last time.Time
		code, err := closed.ExecStream(ctx, a.Ref, api.ExecRequest{Command: `echo "$DLY_GREETING"; sleep 2; echo late; echo err >&2; exit 5`}, func(e api.ExecEvent) error {
			if first.IsZero() {
				first = time.Now()
			}
			last = time.Now()
			events = append(events, e)
			return nil
		})
		if err != nil {
			return err
		}
		var stdout, stderr string
		for _, e := range events {
			if e.Stream == "stderr" {
				stderr += e.Data
			} else {
				stdout += e.Data
			}
		}
		r.note("exec stream", fmt.Sprintf("%d events, %s between the first and the last", len(events), last.Sub(first).Round(100*time.Millisecond)))
		if code != 5 || stdout != "hello from differently\nlate\n" || stderr != "err\n" || last.Sub(first) < time.Second {
			return fmt.Errorf("exit %d, stdout %q, stderr %q, %s apart", code, stdout, stderr, last.Sub(first))
		}
		return nil
	})
	r.step("exec stream timeout", func() error {
		code, err := closed.ExecStream(ctx, a.Ref, api.ExecRequest{Command: "sleep 30", TimeoutSeconds: 2}, func(api.ExecEvent) error { return nil })
		if err == nil && code != 124 {
			err = fmt.Errorf("exit %d", code)
		}
		return err
	})
	r.step("cancelled stream kills the command", func() error {
		cctx, cancel := context.WithCancel(ctx)
		started := make(chan struct{}, 1)
		go func() {
			<-started
			time.Sleep(500 * time.Millisecond)
			cancel()
		}()
		_, err := closed.ExecStream(cctx, a.Ref, api.ExecRequest{Command: "echo up; exec sleep 4242"}, func(api.ExecEvent) error {
			select {
			case started <- struct{}{}:
			default:
			}
			return nil
		})
		cancel()
		if err == nil {
			return fmt.Errorf("the stream ended on its own")
		}
		deadline := time.Now().Add(15 * time.Second)
		for {
			res, err := exec(api.ExecRequest{Command: "pgrep -a -x sleep | grep -q ' 4242$' && echo alive || echo gone"})
			if err != nil {
				return err
			}
			if strings.TrimSpace(res.Stdout) == "gone" {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("the command still runs 15 s after its stream was cancelled")
			}
			time.Sleep(time.Second)
		}
	})
	r.step("files upload/download/list", func() error {
		for _, s := range []string{"first\n", "hello file\n"} {
			if err := closed.WriteFile(ctx, a.Ref, "/tmp/dly/deep/hello.txt", []byte(s)); err != nil {
				return err
			}
		}
		b, err := closed.ReadFile(ctx, a.Ref, "/tmp/dly/deep/hello.txt")
		if err != nil {
			return err
		}
		if string(b) != "hello file\n" {
			return fmt.Errorf("read back %q", b)
		}
		items, err := closed.ListFiles(ctx, a.Ref, "/tmp/dly/deep")
		if err != nil {
			return err
		}
		if len(items) != 1 || items[0].Name != "hello.txt" || items[0].Path != "/tmp/dly/deep/hello.txt" || items[0].IsDir || items[0].Size != 11 || items[0].ModTime.IsZero() {
			return fmt.Errorf("list %+v", items)
		}
		if dirs, err := closed.ListFiles(ctx, a.Ref, "/tmp/dly"); err != nil || len(dirs) != 1 || !dirs[0].IsDir {
			return fmt.Errorf("list of the parent: %+v %v", dirs, err)
		}
		if _, err := closed.ReadFile(ctx, a.Ref, "/tmp/dly/missing.txt"); !api.IsCode(err, api.CodeNotFound) {
			return fmt.Errorf("missing file: %v", err)
		}
		res, err := exec(api.ExecRequest{Command: "cat /tmp/dly/deep/hello.txt; stat -c '%U' /tmp/dly/deep/hello.txt; id -un"})
		r.note("files upload/download/list", "owner, exec user: "+strings.Join(strings.Fields(strings.TrimPrefix(res.Stdout, "hello file\n")), ", "))
		if err == nil && !strings.HasPrefix(res.Stdout, "hello file\n") {
			err = fmt.Errorf("exec reads %s", oneLine(res))
		}
		return err
	})
	r.step("egress none blocks outbound", func() error {
		res, err := exec(api.ExecRequest{Command: outboundCheck("example.com"), TimeoutSeconds: 30})
		r.note("egress none blocks outbound", oneLine(res))
		if err == nil && res.ExitCode == 0 {
			err = fmt.Errorf("outbound request succeeded with egress none: %s", oneLine(res))
		}
		return err
	})
	token := "dly-" + strings.ToLower(id.New("tok"))
	r.step("ports expose (private share link)", func() error {
		if _, err := exec(api.ExecRequest{Command: httpServer(8080, token)}); err != nil {
			return err
		}
		u, err := closed.ExposePort(ctx, a.Ref, 8080)
		if err != nil {
			return err
		}
		bare, _, hasToken := strings.Cut(u, "?")
		if !hasToken {
			return fmt.Errorf("a private preview should come with a share link, got %s", bare)
		}
		r.note("ports expose (private share link)", bare+"?gb_token=…")
		if err := fetchUntil(u, token, 2*time.Minute); err != nil {
			return err
		}
		// Without the token the same host must not serve the page.
		if body, status, err := fetch(bare); err == nil && strings.Contains(body, token) {
			return fmt.Errorf("the private preview served the page without its token (status %d)", status)
		}
		again, err := closed.ExposePort(ctx, a.Ref, 8080)
		if err == nil && again != u {
			err = fmt.Errorf("asking again returned another link")
		}
		return err
	})
	r.step("ports expose (public)", func() error {
		if _, err := exec(api.ExecRequest{Command: httpServer(8081, token)}); err != nil {
			return err
		}
		u, err := public.ExposePort(ctx, a.Ref, 8081)
		if err != nil {
			return err
		}
		r.note("ports expose (public)", u)
		if strings.Contains(u, "?") {
			return fmt.Errorf("a public preview has no token, got a link with a query")
		}
		return fetchUntil(u, token, 2*time.Minute)
	})

	// B: open egress. C: an allowlist.
	reqB := adapter.CreateRequest{ID: id.New("sbx"), Installation: installation, TTLSeconds: 600, Resources: small}
	var b adapter.Instance
	r.step("create with egress public", func() (err error) {
		b, err = open.Create(ctx, reqB)
		if b.Ref != "" {
			r.cleanup(open, b.Ref)
		}
		return err
	})
	if b.Ref != "" {
		r.step("egress public allows outbound", func() error {
			res, err := open.Exec(ctx, b.Ref, api.ExecRequest{Command: outboundCheck("example.com"), TimeoutSeconds: 30})
			if err == nil && res.ExitCode != 0 {
				err = fmt.Errorf("outbound request failed with egress public: %s", oneLine(res))
			}
			return err
		})
	}
	reqC := adapter.CreateRequest{ID: id.New("sbx"), Installation: installation, TTLSeconds: 600, Resources: small}
	var c adapter.Instance
	r.step("create with an egress allowlist", func() (err error) {
		c, err = listed.Create(ctx, reqC)
		if c.Ref != "" {
			r.cleanup(listed, c.Ref)
		}
		return err
	})
	if c.Ref != "" {
		r.step("allowlist, wildcard and deny list", func() error {
			// example.com is listed, www.wikipedia.org is under a listed
			// wildcard, example.org is listed and denied, example.net is not listed.
			var notes []string
			for host, reachable := range map[string]bool{"example.com": true, "www.wikipedia.org": true, "example.org": false, "example.net": false} {
				res, err := listed.Exec(ctx, c.Ref, api.ExecRequest{Command: outboundCheck(host), TimeoutSeconds: 30})
				if err != nil {
					return err
				}
				notes = append(notes, host+" "+oneLine(res))
				if (res.ExitCode == 0) != reachable {
					return fmt.Errorf("%s: %s, want reachable %v", host, oneLine(res), reachable)
				}
			}
			slices.Sort(notes)
			r.note("allowlist, wildcard and deny list", strings.Join(notes, "; "))
			return nil
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
		if c.Ref != "" {
			want[c.Ref] = reqC.ID
		}
		if len(list) != len(want) {
			return fmt.Errorf("list has %d instances, want %d: %+v", len(list), len(want), list)
		}
		for _, in := range list {
			if want[in.Ref] == in.SandboxID && in.State == adapter.StateRunning {
				delete(want, in.Ref)
			}
		}
		if len(want) > 0 {
			return fmt.Errorf("list %+v is missing %v", list, want)
		}
		if other, err := closed.List(ctx, id.New("inst")); err != nil || len(other) != 0 {
			return fmt.Errorf("another installation sees %d sandboxes (%v)", len(other), err)
		}
		return nil
	})
	for _, extra := range []struct {
		name string
		a    *Adapter
		ref  string
	}{{"delete B (repeatable)", open, b.Ref}, {"delete C (repeatable)", listed, c.Ref}} {
		if extra.ref == "" {
			continue
		}
		r.step(extra.name, func() error {
			for i := 0; i < 2; i++ {
				if err := extra.a.Delete(ctx, extra.ref); err != nil {
					return fmt.Errorf("delete #%d: %w", i+1, err)
				}
			}
			if _, err := extra.a.Get(ctx, extra.ref); !api.IsCode(err, api.CodeNotFound) {
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
	alive := func(a *Adapter, ref, p string) bool {
		res, err := a.Exec(ctx, ref, api.ExecRequest{Command: "kill -0 " + p})
		return err == nil && res.ExitCode == 0
	}
	r.step("pause/resume keeps memory", func() error {
		p := pid()
		start := time.Now()
		if err := closed.Pause(ctx, a.Ref); err != nil {
			return fmt.Errorf("pause: %w", err)
		}
		paused := time.Since(start)
		if in, err := closed.Get(ctx, a.Ref); err != nil || in.State != adapter.StatePaused {
			return fmt.Errorf("after pause: %+v %v", in, err)
		}
		if list, err := closed.List(ctx, installation); err != nil || len(list) != 1 || list[0].State != adapter.StatePaused {
			return fmt.Errorf("list while paused: %+v %v", list, err)
		}
		if _, err := exec(api.ExecRequest{Command: "true"}); !api.IsCode(err, api.CodeInvalidState) {
			return fmt.Errorf("exec on a paused sandbox: %v", err)
		}
		if _, err := closed.ExecStream(ctx, a.Ref, api.ExecRequest{Command: "true"}, func(api.ExecEvent) error { return nil }); !api.IsCode(err, api.CodeInvalidState) {
			return fmt.Errorf("streamed exec on a paused sandbox: %v", err)
		}
		start = time.Now()
		if err := closed.Resume(ctx, a.Ref); err != nil {
			return fmt.Errorf("resume: %w", err)
		}
		r.note("pause/resume keeps memory", fmt.Sprintf("pause %s, resume %s", paused.Round(100*time.Millisecond), time.Since(start).Round(100*time.Millisecond)))
		if !alive(closed, a.Ref, p) {
			return fmt.Errorf("process %s did not survive pause", p)
		}
		return nil
	})
	r.step("stop/start keeps disk", func() error {
		if _, err := exec(api.ExecRequest{Command: "echo kept > /var/tmp/disk.txt && sync"}); err != nil {
			return err
		}
		p := pid()
		start := time.Now()
		if err := closed.Stop(ctx, a.Ref); err != nil {
			return fmt.Errorf("stop: %w", err)
		}
		stopped := time.Since(start)
		if in, err := closed.Get(ctx, a.Ref); err != nil || in.State != adapter.StateStopped {
			return fmt.Errorf("after stop: %+v %v", in, err)
		}
		if list, err := closed.List(ctx, installation); err != nil || len(list) != 1 || list[0].State != adapter.StateStopped {
			return fmt.Errorf("list while stopped: %+v %v", list, err)
		}
		if _, err := exec(api.ExecRequest{Command: "true"}); !api.IsCode(err, api.CodeInvalidState) {
			return fmt.Errorf("exec on a stopped sandbox: %v", err)
		}
		if again, err := closed.Create(ctx, reqA); err != nil || again.Ref != a.Ref || again.State != adapter.StateStopped {
			return fmt.Errorf("replayed create of a stopped sandbox: %+v %v", again, err)
		}
		start = time.Now()
		if err := closed.Start(ctx, a.Ref); err != nil {
			return fmt.Errorf("start: %w", err)
		}
		r.note("stop/start keeps disk", fmt.Sprintf("stop %s, start %s", stopped.Round(100*time.Millisecond), time.Since(start).Round(100*time.Millisecond)))
		got, err := closed.ReadFile(ctx, a.Ref, "/var/tmp/disk.txt")
		if err != nil || string(got) != "kept\n" {
			return fmt.Errorf("file after start: %q %v", got, err)
		}
		if alive(closed, a.Ref, p) {
			return fmt.Errorf("process %s survived a stop; the sandbox was not shut down", p)
		}
		res, err := exec(api.ExecRequest{Command: `echo "$DLY_GREETING"`})
		if err == nil && strings.TrimSpace(res.Stdout) != "hello from differently" {
			err = fmt.Errorf("sandbox env after start: %s", oneLine(res))
		}
		return err
	})
	r.step("move the auto-stop time", func() error {
		var before, after struct {
			ExpiresAt time.Time `json:"expires_at"`
		}
		if err := closed.call(ctx, http.MethodGet, closed.path(a.Ref), nil, "", &before); err != nil {
			return err
		}
		// More than a day ahead, then back to a short one.
		at := time.Now().Add(26 * time.Hour)
		if err := closed.SetExpiry(ctx, a.Ref, at); err != nil {
			return err
		}
		if err := closed.call(ctx, http.MethodGet, closed.path(a.Ref), nil, "", &after); err != nil {
			return err
		}
		r.note("move the auto-stop time", fmt.Sprintf("from %s to %s from now", time.Until(before.ExpiresAt).Round(time.Minute), time.Until(after.ExpiresAt).Round(time.Minute)))
		if after.ExpiresAt.Before(at.Add(-time.Minute)) || after.ExpiresAt.After(at.Add(2*time.Minute)) {
			return fmt.Errorf("auto-stop at %s, asked for %s", after.ExpiresAt, at)
		}
		return closed.SetExpiry(ctx, a.Ref, time.Now().Add(20*time.Minute))
	})

	// A kept snapshot, a fork started from it, and a sandbox started from it
	// after its source is gone.
	var snap adapter.Snapshot
	p := pid()
	r.step("memory snapshot (kept)", func() (err error) {
		if _, err := exec(api.ExecRequest{Command: "echo kept > /var/tmp/persist.txt && sync"}); err != nil {
			return err
		}
		start := time.Now()
		snap, err = closed.Snapshot(ctx, a.Ref, "memory")
		if snap.Ref != "" {
			r.t.Cleanup(func() {
				if err := closed.DeleteSnapshot(context.Background(), snap.Ref); err != nil {
					r.t.Errorf("cleanup: delete snapshot %s: %v", snap.Ref, err)
				}
			})
		}
		if err == nil && (snap.Kind != "memory" || !strings.HasPrefix(snap.Ref, "snap_")) {
			err = fmt.Errorf("snapshot %+v", snap)
		}
		r.note("memory snapshot (kept)", fmt.Sprintf("%d MiB in %s", snap.SizeBytes>>20, time.Since(start).Round(100*time.Millisecond)))
		return err
	})
	reqD := adapter.CreateRequest{ID: id.New("sbx"), Installation: installation, TTLSeconds: 600, FromSnapshot: snap.Ref, Resources: small, Env: reqA.Env}
	if snap.Ref != "" {
		r.step("fork from the snapshot", func() error {
			// Changed after the snapshot: the fork must not see it.
			if _, err := exec(api.ExecRequest{Command: "echo later > /var/tmp/persist.txt && sync"}); err != nil {
				return err
			}
			start := time.Now()
			d, err := closed.Create(ctx, reqD)
			if d.Ref != "" {
				r.cleanup(closed, d.Ref)
			}
			if err != nil {
				return err
			}
			took := time.Since(start)
			if d.SandboxID != reqD.ID || d.State != adapter.StateRunning || d.Ref == a.Ref {
				return fmt.Errorf("fork %+v", d)
			}
			if again, err := closed.Create(ctx, reqD); err != nil || again.Ref != d.Ref {
				return fmt.Errorf("replayed fork: %+v %v (first %s)", again, err, d.Ref)
			}
			res, err := closed.Exec(ctx, d.Ref, api.ExecRequest{Command: "cat /var/tmp/persist.txt; kill -0 " + p + " && echo alive; echo \"env: $DLY_GREETING\""})
			if err != nil {
				return err
			}
			r.note("fork from the snapshot", fmt.Sprintf("%s; ready in %s", strings.ReplaceAll(strings.TrimSpace(res.Stdout), "\n", "; "), took.Round(100*time.Millisecond)))
			if res.Stdout != "kept\nalive\nenv: hello from differently\n" {
				err = fmt.Errorf("fork state %q", res.Stdout)
			}
			if derr := closed.Delete(ctx, d.Ref); err == nil {
				err = derr
			}
			return err
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
		if err := closed.Delete(ctx, "sbx_neverexisted0000"); err != nil {
			return fmt.Errorf("delete of a sandbox that never existed: %w", err)
		}
		return nil
	})
	if snap.Ref != "" {
		r.step("a snapshot outlives its sandbox", func() error {
			req := adapter.CreateRequest{ID: id.New("sbx"), Installation: installation, TTLSeconds: 600, FromSnapshot: snap.Ref, Env: reqA.Env}
			e, err := closed.Create(ctx, req)
			if e.Ref != "" {
				r.cleanup(closed, e.Ref)
			}
			if err != nil {
				return fmt.Errorf("a sandbox from the snapshot of a deleted sandbox: %w", err)
			}
			res, err := closed.Exec(ctx, e.Ref, api.ExecRequest{Command: "cat /var/tmp/persist.txt; kill -0 " + p + " && echo alive"})
			if err == nil && res.Stdout != "kept\nalive\n" {
				err = fmt.Errorf("state %q", res.Stdout)
			}
			if derr := closed.Delete(ctx, e.Ref); err == nil {
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
			req := adapter.CreateRequest{ID: id.New("sbx"), Installation: installation, TTLSeconds: 600, FromSnapshot: snap.Ref}
			e, err := closed.Create(ctx, req)
			if e.Ref != "" {
				r.cleanup(closed, e.Ref)
			}
			if !api.IsCode(err, api.CodeInvalidRequest) {
				return fmt.Errorf("a sandbox from a deleted snapshot: %+v %v", e, err)
			}
			return nil
		})
	}
	if image := os.Getenv("GROUNDBOX_IMAGE"); image != "" {
		// The first sandbox from an image waits, queued, while Groundbox
		// turns the image into a disk: Create reports a lack of capacity and
		// the next call finds the same sandbox, as the scheduler's retry does.
		r.step("create from a Docker image", func() error {
			req := adapter.CreateRequest{ID: id.New("sbx"), Installation: installation, TTLSeconds: 600, Resources: small, Template: image}
			var in adapter.Instance
			var err error
			waits := 0
			for deadline := time.Now().Add(10 * time.Minute); ; waits++ {
				in, err = open.Create(ctx, req)
				if !api.IsCode(err, adapter.CodeInsufficientCapacity) || time.Now().After(deadline) {
					break
				}
			}
			if list, lerr := open.List(ctx, installation); lerr == nil {
				for _, l := range list {
					r.cleanup(open, l.Ref)
				}
			}
			if err != nil {
				return err
			}
			res, err := open.Exec(ctx, in.Ref, api.ExecRequest{Command: "head -1 /etc/os-release; python3 --version; id -un"})
			r.note("create from a Docker image", fmt.Sprintf("%s: %s; %d capacity waits", image, strings.ReplaceAll(strings.TrimSpace(res.Stdout), "\n", "; "), waits))
			if err == nil && res.ExitCode != 0 {
				err = fmt.Errorf("exec in the image: %s", oneLine(res))
			}
			if err == nil { // a streamed command too
				var out strings.Builder
				code, serr := open.ExecStream(ctx, in.Ref, api.ExecRequest{Command: "echo one; sleep 1; echo two"}, func(e api.ExecEvent) error {
					out.WriteString(e.Data)
					return nil
				})
				if err = serr; err == nil && (code != 0 || out.String() != "one\ntwo\n") {
					err = fmt.Errorf("streamed exec in the image: exit %d, %q", code, out.String())
				}
			}
			if derr := open.Delete(ctx, in.Ref); err == nil {
				err = derr
			}
			return err
		})
	}
	// Groundbox's own idle pause (NativeIdler): a sandbox with a 30 s idle
	// timeout is paused once nothing has reached it for that long (never in
	// its first minute), keeps its memory, and the next exec wakes it.
	r.step("idle pause and wake on exec", func() error {
		req := adapter.CreateRequest{ID: id.New("sbx"), Installation: installation, TTLSeconds: 900, Resources: small, IdleTimeoutSeconds: 30}
		in, err := closed.Create(ctx, req)
		if in.Ref != "" {
			r.cleanup(closed, in.Ref)
		}
		if err != nil {
			return err
		}
		res, err := closed.Exec(ctx, in.Ref, api.ExecRequest{Command: "nohup setsid sleep 3600 >/dev/null 2>&1 & echo $!"})
		if err != nil {
			return fmt.Errorf("start a process: %w", err)
		}
		p := strings.TrimSpace(res.Stdout)
		start := time.Now()
		for {
			got, err := closed.Get(ctx, in.Ref) // reading the sandbox is not activity
			if err != nil {
				return err
			}
			if got.State == adapter.StatePaused {
				break
			}
			if time.Since(start) > 6*time.Minute {
				return fmt.Errorf("still %s after %s", got.State, time.Since(start).Round(time.Second))
			}
			time.Sleep(5 * time.Second)
		}
		idled := time.Since(start)
		var raw struct {
			Status         string `json:"status"`
			PauseReason    string `json:"pause_reason"`
			WakesOnRequest bool   `json:"wakes_on_request"`
		}
		for raw.Status != "paused" && time.Since(start) < 8*time.Minute {
			if err := closed.call(ctx, http.MethodGet, closed.path(in.Ref), nil, "", &raw); err != nil {
				return err
			}
			if raw.Status != "paused" {
				time.Sleep(2 * time.Second)
			}
		}
		if raw.Status != "paused" || raw.PauseReason != "idle" || !raw.WakesOnRequest {
			return fmt.Errorf("paused as %+v", raw)
		}
		start = time.Now()
		if !alive(closed, in.Ref, p) {
			return fmt.Errorf("process %s did not survive the idle pause and wake", p)
		}
		woke := time.Since(start)
		if got, err := closed.Get(ctx, in.Ref); err != nil || got.State != adapter.StateRunning {
			return fmt.Errorf("after the waking exec: %+v %v", got, err)
		}
		r.note("idle pause and wake on exec", fmt.Sprintf("paused with reason idle %s after the last exec; the exec that woke it took %s; process kept", idled.Round(time.Second), woke.Round(100*time.Millisecond)))
		return closed.Delete(ctx, in.Ref)
	})
	r.step("list after delete", func() error {
		list, err := closed.List(ctx, installation)
		if err == nil && len(list) != 0 {
			err = fmt.Errorf("still listed: %+v", list)
		}
		return err
	})
}

func (r *liveRun) cleanup(a *Adapter, ref string) {
	r.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := a.Delete(ctx, ref); err != nil {
			r.t.Errorf("cleanup: delete sandbox %s: %v", ref, err)
		}
	})
}

// outboundCheck exits 0 only if an HTTPS request to host works.
func outboundCheck(host string) string {
	return "curl -sS -m 10 -o /dev/null -w '%{http_code}\\n' https://" + host
}

// httpServer serves token on port in the background.
func httpServer(port int, token string) string {
	return fmt.Sprintf(`mkdir -p /tmp/www && echo %s > /tmp/www/index.html && cd /tmp/www &&
nohup setsid python3 -m http.server %d >/tmp/www-%d.log 2>&1 &
sleep 1`, token, port, port)
}

func fetch(u string) (string, int, error) {
	resp, err := http.Get(u)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return string(b), resp.StatusCode, nil
}

func fetchUntil(u, token string, within time.Duration) error {
	deadline := time.Now().Add(within)
	last := ""
	for time.Now().Before(deadline) {
		body, status, err := fetch(u)
		if err == nil && status == 200 && strings.Contains(body, token) {
			return nil
		}
		if err != nil {
			last = "request failed"
		} else {
			last = fmt.Sprintf("%d %q", status, firstLine(body))
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("the preview never served the page; last: %s", last)
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
	line := fmt.Sprintf("  %-40s %-8s %s", name, took, result)
	if n := r.notes[name]; n != "" {
		line += " (" + n + ")"
	}
	r.lines = append(r.lines, line)
	r.t.Log(line)
	return err == nil
}
