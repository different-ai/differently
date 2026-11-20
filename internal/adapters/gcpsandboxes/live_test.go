//go:build live && experimental_adapters

package gcpsandboxes

// Live check against Google Cloud Agent Platform sandboxes, set up as in
// docs/providers/gcp-sandboxes.md (an engine, a shell template, a service
// account with roles/aiplatform.user):
//
//	GCP_SANDBOXES_PROJECT=my-project GCP_SANDBOXES_LOCATION=us-central1 \
//	GCP_SANDBOXES_ENGINE=1234567890 GCP_SANDBOXES_TEMPLATE=9876543210 \
//	GCP_SANDBOXES_CREDENTIAL_FILE=./service-account.json \
//	go test -tags live,experimental_adapters -v -run TestLiveGCPSandboxes ./internal/adapters/gcpsandboxes/
//
// The credential is a service-account key or a workload identity federation
// file; on Compute Engine or GKE, leave GCP_SANDBOXES_CREDENTIAL_FILE unset to
// use the metadata server. Every sandbox and snapshot it makes is deleted at
// the end. Besides what Differently relies on, it reports two things Google
// does not document: whether a process survives :pause and :resume, and
// whether one survives a snapshot into a fork.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

func liveAdapter(t *testing.T) *Adapter {
	t.Helper()
	cfg := map[string]string{
		"project": os.Getenv("GCP_SANDBOXES_PROJECT"), "location": os.Getenv("GCP_SANDBOXES_LOCATION"),
		"reasoningEngine": os.Getenv("GCP_SANDBOXES_ENGINE"), "template": os.Getenv("GCP_SANDBOXES_TEMPLATE"),
	}
	for k, v := range cfg {
		if v == "" {
			t.Skipf("set GCP_SANDBOXES_PROJECT, _LOCATION, _ENGINE and _TEMPLATE (config.%s is missing)", k)
		}
	}
	cred := `{"type":"metadata_server"}`
	if f := os.Getenv("GCP_SANDBOXES_CREDENTIAL_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		cred = string(b)
	}
	o, err := FromConfig(cfg, "", cred)
	if err != nil {
		t.Fatal(err)
	}
	return New(o)
}

func TestLiveGCPSandboxes(t *testing.T) {
	a := liveAdapter(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	step := func(what string, fn func() error) {
		t.Helper()
		start := time.Now()
		if err := fn(); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		t.Logf("%s: %s", what, time.Since(start).Round(100*time.Millisecond))
	}
	exec := func(ref string, req api.ExecRequest) api.ExecResult {
		t.Helper()
		res, err := a.Exec(ctx, ref, req)
		if err != nil {
			t.Fatalf("exec %q: %v", req.Command, err)
		}
		return res
	}
	// alive reports whether the process that wrote /tmp/live.pid still runs.
	alive := func(ref string) bool {
		res, err := a.Exec(ctx, ref, api.ExecRequest{Command: `kill -0 "$(cat /tmp/live.pid)"`})
		return err == nil && res.ExitCode == 0
	}

	step("health", func() error { return a.Health(ctx) })
	caps, err := a.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("capabilities: %v", caps.Features)

	inst := "inst_live_gcp"
	var src adapter.Instance
	step("create", func() (err error) {
		src, err = a.Create(ctx, adapter.CreateRequest{ID: "sbx_live_gcp_src", Installation: inst, TTLSeconds: 3600})
		return err
	})
	t.Cleanup(func() { _ = a.Delete(context.Background(), src.Ref) })
	again, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_live_gcp_src", Installation: inst, TTLSeconds: 3600})
	if err != nil || again.Ref != src.Ref {
		t.Fatalf("a replayed create made another sandbox: %+v %v", again, err)
	}

	if res := exec(src.Ref, api.ExecRequest{Command: `echo "$GREETING from $(pwd)"; exit 7`, Env: map[string]string{"GREETING": "hello"}, Cwd: "/tmp"}); res.ExitCode != 7 || strings.TrimSpace(res.Stdout) != "hello from /tmp" {
		t.Fatalf("exec with env, cwd and an exit code: %+v", res)
	}
	if res := exec(src.Ref, api.ExecRequest{Command: "cat", Stdin: "piped"}); res.Stdout != "piped" {
		t.Fatalf("stdin: %+v", res)
	}
	data := []byte(strings.Repeat("0123456789abcdef", 8192)) // 128 KiB: several 64 KiB pieces
	step("write and read 128 KiB", func() error {
		if err := a.WriteFile(ctx, src.Ref, "/tmp/live/data.txt", data); err != nil {
			return err
		}
		got, err := a.ReadFile(ctx, src.Ref, "/tmp/live/data.txt")
		if err == nil && string(got) != string(data) {
			t.Fatalf("read back %d bytes, wrote %d", len(got), len(data))
		}
		return err
	})
	if files, err := a.ListFiles(ctx, src.Ref, "/tmp/live"); err != nil || len(files) != 1 {
		t.Fatalf("list files: %+v %v", files, err)
	}
	if _, err := a.ReadFile(ctx, src.Ref, "/tmp/live/missing"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("a missing file: %v", err)
	}
	if list, err := a.List(ctx, inst); err != nil || len(list) != 1 || list[0].Ref != src.Ref {
		t.Fatalf("list: %+v %v", list, err)
	}

	// A process with nothing but memory state, to see what keeps it.
	exec(src.Ref, api.ExecRequest{Command: `nohup sh -c 'while :; do sleep 1; done' >/dev/null 2>&1 & echo $! > /tmp/live.pid`})
	if !alive(src.Ref) {
		t.Log("a background process does not outlive the command that started it: the process checks below say nothing")
	}

	step("stop (:pause)", func() error { return a.Stop(ctx, src.Ref) })
	if got, err := a.Get(ctx, src.Ref); err != nil || got.State != adapter.StateStopped {
		t.Fatalf("after stop: %+v %v", got, err)
	}
	step("start (:resume)", func() error { return a.Start(ctx, src.Ref) })
	got, err := a.ReadFile(ctx, src.Ref, "/tmp/live/data.txt")
	if err != nil || string(got) != string(data) {
		t.Fatalf("the file did not survive stop and start: %d bytes, %v", len(got), err)
	}
	t.Logf("a process survives :pause and :resume: %v", alive(src.Ref))

	var snap adapter.Snapshot
	step("snapshot", func() (err error) {
		snap, err = a.Snapshot(ctx, src.Ref, adapter.FeatureSnapshotMem)
		return err
	})
	t.Cleanup(func() { _ = a.DeleteSnapshot(context.Background(), snap.Ref) })
	var fork adapter.Instance
	step("fork", func() (err error) {
		fork, err = a.Create(ctx, adapter.CreateRequest{ID: "sbx_live_gcp_fork", Installation: inst, FromSnapshot: snap.Ref, TTLSeconds: 3600})
		return err
	})
	t.Cleanup(func() { _ = a.Delete(context.Background(), fork.Ref) })
	if got, err := a.ReadFile(ctx, fork.Ref, "/tmp/live/data.txt"); err != nil || string(got) != string(data) {
		t.Fatalf("the fork lacks its source's file: %d bytes, %v", len(got), err)
	}
	t.Logf("a process survives a snapshot into a fork: %v", alive(fork.Ref))

	step("delete snapshot", func() error { return a.DeleteSnapshot(ctx, snap.Ref) })
	step("delete", func() error {
		if err := a.Delete(ctx, fork.Ref); err != nil {
			return err
		}
		return a.Delete(ctx, src.Ref)
	})
	if _, err := a.Get(ctx, src.Ref); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("a deleted sandbox: %v", err)
	}
	if err := a.Delete(ctx, src.Ref); err != nil {
		t.Fatalf("deleting twice: %v", err)
	}
}
