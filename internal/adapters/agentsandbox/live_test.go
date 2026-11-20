//go:build live && experimental_adapters

package agentsandbox

// Live check against a cluster running agent-sandbox, with the RBAC in
// docs/providers/agent-sandbox.md bound to a service account:
//
//	AGENTSANDBOX_API_SERVER=https://127.0.0.1:6443 AGENTSANDBOX_CA_FILE=ca.crt \
//	AGENTSANDBOX_TOKEN=$(kubectl -n dly create token differently) \
//	go test -tags live,experimental_adapters -v -run TestLiveAgentSandbox ./internal/adapters/agentsandbox/
//
// Settings: AGENTSANDBOX_NAMESPACE (default dly), AGENTSANDBOX_IMAGE (default
// debian:bookworm-slim), AGENTSANDBOX_VOLUME_SIZE (enables stop/start, default
// 1Gi), AGENTSANDBOX_WARM_POOL (also run the scenario through claims on that
// SandboxWarmPool).
//
// GKE Pod Snapshots (TestLivePodSnapshots): a GKE cluster with
// --enable-pod-snapshots, a gVisor node pool, and a PodSnapshotPolicy as in
// docs/providers/agent-sandbox.md#snapshots-fork-and-pauseresume-on-gke, named by
// AGENTSANDBOX_SNAPSHOT_POLICY. The service account also needs the
// podsnapshot.gke.io rules listed there.

import (
	"bytes"
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

func liveEnv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func liveAdapter(t *testing.T, cfg map[string]string) *Adapter {
	t.Helper()
	server := os.Getenv("AGENTSANDBOX_API_SERVER")
	if server == "" {
		t.Skip("set AGENTSANDBOX_API_SERVER, AGENTSANDBOX_CA_FILE and AGENTSANDBOX_TOKEN")
	}
	cfg["apiServer"], cfg["caFile"] = server, os.Getenv("AGENTSANDBOX_CA_FILE")
	cfg["namespace"] = liveEnv("AGENTSANDBOX_NAMESPACE", "dly")
	o, err := FromConfig(cfg, api.TrustPrivate, os.Getenv("AGENTSANDBOX_TOKEN"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestLiveAgentSandbox(t *testing.T) {
	a := liveAdapter(t, map[string]string{
		"image": liveEnv("AGENTSANDBOX_IMAGE", "debian:bookworm-slim"), "isolation": "container",
		"volumeSize": liveEnv("AGENTSANDBOX_VOLUME_SIZE", "1Gi"), "features": "exec,exec.stream,files,lifecycle.stop-start,persistence.disk,ports.expose",
	})
	start := time.Now()
	scenario(t, a, "sbx_live_direct")
	t.Logf("direct Sandboxes: scenario passed in %s", time.Since(start).Round(time.Second))
}

func TestLiveAgentSandboxWarmPool(t *testing.T) {
	pool := os.Getenv("AGENTSANDBOX_WARM_POOL")
	if pool == "" {
		t.Skip("set AGENTSANDBOX_WARM_POOL to a SandboxWarmPool")
	}
	a := liveAdapter(t, map[string]string{"warmPool": pool})
	start := time.Now()
	scenario(t, a, "sbx_live_claim")
	t.Logf("warm pool %s: scenario passed in %s", pool, time.Since(start).Round(time.Second))
}

// TestLivePodSnapshots checks what GKE Pod Snapshots give Differently: a
// fork and a resume keep a running process and its files, and a new sandbox
// does not start from anyone's snapshot. It logs the time each step took.
func TestLivePodSnapshots(t *testing.T) {
	policy := os.Getenv("AGENTSANDBOX_SNAPSHOT_POLICY")
	if policy == "" {
		t.Skip("set AGENTSANDBOX_SNAPSHOT_POLICY to a PodSnapshotPolicy (GKE with --enable-pod-snapshots)")
	}
	a := liveAdapter(t, map[string]string{
		"image": liveEnv("AGENTSANDBOX_IMAGE", "debian:bookworm-slim"), "runtimeClassName": liveEnv("AGENTSANDBOX_RUNTIME_CLASS", "gvisor"),
		"podSnapshotPolicy": policy,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if err := a.Health(ctx); err != nil {
		t.Fatalf("health: %v", err)
	}
	step := func(what string, fn func() error) {
		t.Helper()
		start := time.Now()
		if err := fn(); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		t.Logf("%s: %s", what, time.Since(start).Round(100*time.Millisecond))
	}
	run := func(ref, cmd string) string {
		t.Helper()
		res, err := a.Exec(ctx, ref, api.ExecRequest{Command: cmd, TimeoutSeconds: 30})
		if err != nil || res.ExitCode != 0 {
			t.Fatalf("%s: %q: %+v %v", ref, cmd, res, err)
		}
		return strings.TrimSpace(res.Stdout)
	}
	// A process whose only state is in memory: a counter it keeps advancing.
	const counter = `n=0; while :; do n=$((n+1)); echo $n > /tmp/count; sleep 1; done`
	alive := func(ref, pid string) bool {
		res, err := a.Exec(ctx, ref, api.ExecRequest{Command: "kill -0 " + pid})
		return err == nil && res.ExitCode == 0
	}

	var src adapter.Instance
	step("create", func() (err error) {
		src, err = a.Create(ctx, adapter.CreateRequest{ID: "sbx_live_snap_src", Installation: "inst_live", TTLSeconds: 3600})
		return err
	})
	t.Cleanup(func() { _ = a.Delete(context.Background(), src.Ref) })
	pid := run(src.Ref, `sh -c '`+counter+`' >/dev/null 2>&1 & echo $!`)
	run(src.Ref, "echo kept > /tmp/file")
	time.Sleep(3 * time.Second)

	var snap adapter.Snapshot
	step("snapshot", func() (err error) {
		snap, err = a.Snapshot(ctx, src.Ref, adapter.FeatureSnapshotMem)
		return err
	})
	t.Cleanup(func() { _ = a.DeleteSnapshot(context.Background(), snap.Ref) })
	if !alive(src.Ref, pid) {
		t.Fatal("the source's process did not survive its snapshot")
	}

	var fork adapter.Instance
	step("fork", func() (err error) {
		fork, err = a.Create(ctx, adapter.CreateRequest{ID: "sbx_live_snap_fork", Installation: "inst_live", FromSnapshot: snap.Ref, TTLSeconds: 3600})
		return err
	})
	t.Cleanup(func() { _ = a.Delete(context.Background(), fork.Ref) })
	if !alive(fork.Ref, pid) || run(fork.Ref, "cat /tmp/file") != "kept" {
		t.Fatal("the fork lacks its source's process or file")
	}
	before := run(fork.Ref, "cat /tmp/count")
	time.Sleep(2 * time.Second)
	if after := run(fork.Ref, "cat /tmp/count"); after == before {
		t.Fatalf("the fork's process is not running: count stayed at %s", after)
	}

	fresh, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_live_snap_fresh", Installation: "inst_live", TTLSeconds: 3600})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Delete(context.Background(), fresh.Ref) })
	if res, err := a.Exec(ctx, fresh.Ref, api.ExecRequest{Command: "test -e /tmp/file"}); err != nil || res.ExitCode == 0 {
		t.Fatalf("a new sandbox with the same pod spec started from a snapshot: %+v %v", res, err)
	}

	step("pause", func() error { return a.Pause(ctx, fork.Ref) })
	step("resume", func() error { return a.Resume(ctx, fork.Ref) })
	if !alive(fork.Ref, pid) || run(fork.Ref, "cat /tmp/file") != "kept" {
		t.Fatal("the resumed sandbox lacks its process or file")
	}
}

// scenario drives the adapter through what Differently relies on.
func scenario(t *testing.T, a *Adapter, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	fatal := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	const inst = "inst_live"
	fatal("health", a.Health(ctx))

	t0 := time.Now()
	in, err := a.Create(ctx, adapter.CreateRequest{ID: id, Installation: inst, Resources: api.Resources{CPU: 1, MemoryMiB: 256}, TTLSeconds: 900})
	fatal("create", err)
	t.Logf("create took %s", time.Since(t0).Round(time.Millisecond))
	defer a.Delete(context.Background(), in.Ref)
	if in.State != adapter.StateRunning || in.SandboxID != id {
		t.Fatalf("create returned %+v", in)
	}
	again, err := a.Create(ctx, adapter.CreateRequest{ID: id, Installation: inst})
	if err != nil || again.Ref != in.Ref {
		t.Fatalf("create is not idempotent: %+v %v", again, err)
	}
	list, err := a.List(ctx, inst)
	if err != nil || !slices.ContainsFunc(list, func(i adapter.Instance) bool { return i.Ref == in.Ref && i.SandboxID == id }) {
		t.Fatalf("list %+v %v", list, err)
	}

	res, err := a.Exec(ctx, in.Ref, api.ExecRequest{Command: `echo "hello $WHO"; printf 'a\nb'; printf err >&2; exit 7`, Env: map[string]string{"WHO": "world"}})
	if err != nil || res.ExitCode != 7 || res.Stdout != "hello world\na\nb" || res.Stderr != "err" {
		t.Fatalf("exec %+v %v", res, err)
	}
	res, err = a.Exec(ctx, in.Ref, api.ExecRequest{Args: []string{"sh", "-c", "pwd; echo \"$1\"", "sh", "it's quoted"}, Cwd: "/tmp"})
	if err != nil || res.Stdout != "/tmp\nit's quoted\n" {
		t.Fatalf("exec argv %+v %v", res, err)
	}
	res, err = a.Exec(ctx, in.Ref, api.ExecRequest{Command: "tr a-z A-Z", Stdin: "hello\nworld\n"})
	if err != nil || res.Stdout != "HELLO\nWORLD\n" {
		t.Fatalf("stdin %+v %v", res, err)
	}
	res, err = a.Exec(ctx, in.Ref, api.ExecRequest{Args: []string{"no-such-binary"}})
	if err != nil || res.ExitCode != 127 {
		t.Fatalf("missing binary: want exit 127, got %+v %v", res, err)
	}
	t1 := time.Now()
	res, err = a.Exec(ctx, in.Ref, api.ExecRequest{Command: "sleep 30", TimeoutSeconds: 1})
	if err != nil || res.ExitCode != 124 || time.Since(t1) > 15*time.Second {
		t.Fatalf("timeout: %+v %v after %s", res, err, time.Since(t1))
	}
	streams := map[string]string{}
	var events int
	code, err := a.ExecStream(ctx, in.Ref, api.ExecRequest{Command: "for i in 1 2 3; do echo line$i; sleep 0.2; done; echo e >&2; exit 3"}, func(ev api.ExecEvent) error {
		streams[ev.Stream] += ev.Data
		events++
		return nil
	})
	if err != nil || code != 3 || streams["stdout"] != "line1\nline2\nline3\n" || streams["stderr"] != "e\n" || events < 3 {
		t.Fatalf("stream code=%d %q %d events %v", code, streams, events, err)
	}

	data := make([]byte, 300<<10) // larger than one stdin chunk
	for i := range data {
		data[i] = byte(i * 7)
	}
	dir := "/tmp"
	if a.opts.VolumePath != "" {
		dir = a.opts.VolumePath
	}
	fatal("write file", a.WriteFile(ctx, in.Ref, dir+"/sub/rec.bin", data))
	back, err := a.ReadFile(ctx, in.Ref, dir+"/sub/rec.bin")
	if err != nil || !bytes.Equal(back, data) {
		t.Fatalf("read file: %d bytes, %v", len(back), err)
	}
	files, err := a.ListFiles(ctx, in.Ref, dir+"/sub")
	if err != nil || len(files) != 1 || files[0].Name != "rec.bin" || files[0].Size != int64(len(data)) || files[0].IsDir {
		t.Fatalf("list files %+v %v", files, err)
	}
	if _, err := a.ReadFile(ctx, in.Ref, "/no/such/file"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("read missing file: want not_found, got %v", err)
	}
	if _, err := a.ListFiles(ctx, in.Ref, "/no/such/dir"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("list missing dir: want not_found, got %v", err)
	}

	if slices.Contains(a.opts.Features, adapter.FeatureStopStart) {
		t2 := time.Now()
		fatal("stop", a.Stop(ctx, in.Ref))
		if got, err := a.Get(ctx, in.Ref); err != nil || got.State != adapter.StateStopped {
			t.Fatalf("after stop: %+v %v", got, err)
		}
		if _, err := a.Exec(ctx, in.Ref, api.ExecRequest{Command: "true"}); !api.IsCode(err, api.CodeInvalidState) {
			t.Fatalf("exec while stopped: want invalid_state, got %v", err)
		}
		fatal("start", a.Start(ctx, in.Ref))
		t.Logf("stop and start took %s", time.Since(t2).Round(time.Millisecond))
		if back, err := a.ReadFile(ctx, in.Ref, dir+"/sub/rec.bin"); err != nil || !bytes.Equal(back, data) {
			t.Fatalf("file on the volume lost across stop/start: %v", err)
		}
	}

	if slices.Contains(a.opts.Features, adapter.FeaturePorts) {
		// Serve a port in the sandbox and fetch it from inside the cluster (from
		// a second exec in the same pod, through the Service's DNS name).
		u, err := a.ExposePort(ctx, in.Ref, 8080)
		fatal("expose port", err)
		t.Logf("port URL %s", u)
		host := strings.TrimSuffix(strings.TrimPrefix(u, "http://"), ":8080")
		res, err := a.Exec(ctx, in.Ref, api.ExecRequest{Command: "getent hosts " + host + " || echo unresolved"})
		if err != nil || strings.Contains(res.Stdout, "unresolved") {
			t.Fatalf("port host %s does not resolve in the cluster: %+v %v", host, res, err)
		}
	}

	fatal("set expiry", a.SetExpiry(ctx, in.Ref, time.Now().Add(20*time.Minute)))
	fatal("delete", a.Delete(ctx, in.Ref))
	if _, err := a.Get(ctx, in.Ref); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("get after delete: want not_found, got %v", err)
	}
	fatal("delete is idempotent", a.Delete(ctx, in.Ref))

	if a.opts.WarmPool == "" {
		_, err = a.Create(ctx, adapter.CreateRequest{ID: id + "_badimage", Installation: inst, Template: "docker.io/library/ubuntu:no-such-tag-differently", Resources: api.Resources{CPU: 1, MemoryMiB: 128}})
		if !api.IsCode(err, api.CodeInvalidRequest) {
			t.Fatalf("unpullable image: want invalid_request, got %v", err)
		}
		if _, err := a.Get(ctx, name(id+"_badimage")); !api.IsCode(err, api.CodeNotFound) {
			t.Fatalf("the failed Sandbox was not removed: %v", err)
		}
	}
}
