//go:build live

package opensandbox

// Live check against a real OpenSandbox server (test/opensandbox/compose.yaml):
//
//	OPENSANDBOX_URL=http://localhost:8090/v1 go test -tags live -v ./internal/adapters/opensandbox/

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

func TestLiveOpenSandbox(t *testing.T) {
	url := os.Getenv("OPENSANDBOX_URL")
	if url == "" {
		t.Skip("set OPENSANDBOX_URL to a running OpenSandbox lifecycle API")
	}
	key := os.Getenv("OPENSANDBOX_API_KEY")
	if key == "" {
		key = "osb-live-test-key-0123456789" // test/opensandbox/config.toml
	}
	image := os.Getenv("OPENSANDBOX_IMAGE")
	if image == "" {
		image = "debian:bookworm-slim"
	}
	lineEndings := os.Getenv("OPENSANDBOX_LINE_ENDINGS")
	if lineEndings == "" {
		lineEndings = "kept" // test/opensandbox/config.toml runs execd release-1.2.0
	}
	o, err := FromConfig(map[string]string{"apiURL": url, "image": image, "isolation": "container", "lineEndings": lineEndings}, api.TrustPrivate, key)
	if err != nil {
		t.Fatal(err)
	}
	a := New(o)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	id := "sbx_live_" + time.Now().Format("150405")

	start := time.Now()
	inst, err := a.Create(ctx, adapter.CreateRequest{ID: id, Installation: "inst_live", Resources: api.Resources{CPU: 1, MemoryMiB: 512}, Env: map[string]string{"GREETING": "hello"}, TTLSeconds: 600})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("created %s in %s", inst.Ref, time.Since(start).Round(time.Millisecond))
	defer a.Delete(context.Background(), inst.Ref)

	again, err := a.Create(ctx, adapter.CreateRequest{ID: id, Installation: "inst_live"})
	if err != nil || again.Ref != inst.Ref {
		t.Fatalf("create not idempotent against the real server: %+v %v", again, err)
	}
	res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: `echo "$GREETING $WHO from $(uname -s)"; echo oops >&2`, Env: map[string]string{"WHO": "world"}})
	if err != nil || !strings.Contains(res.Stdout, "hello world from Linux") || !strings.Contains(res.Stderr, "oops") || res.ExitCode != 0 {
		t.Fatalf("exec %+v %v", res, err)
	}
	if err := a.SetExpiry(ctx, inst.Ref, time.Now().Add(2*time.Hour)); err != nil {
		t.Fatalf("renew-expiration: %v", err)
	}
	if res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: `printf 'a\nb\n'`}); err != nil || res.Stdout != "a\nb\n" {
		t.Fatalf("multi-line output %q %v", res.Stdout, err)
	}
	if res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: `printf 'a\n\nno newline'`}); lineEndings == "kept" && (err != nil || res.Stdout != "a\n\nno newline") {
		t.Fatalf("output is not exact with lineEndings: kept: %q %v", res.Stdout, err)
	}
	if res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "exit 3"}); err != nil || res.ExitCode != 3 {
		t.Fatalf("exit code: %+v %v", res, err)
	}
	if res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Args: []string{"sh", "-c", "pwd"}, Cwd: "/tmp"}); err != nil || strings.TrimSpace(res.Stdout) != "/tmp" {
		t.Fatalf("argv + cwd: %+v %v", res, err)
	}
	if err := a.WriteFile(ctx, inst.Ref, "/tmp/work/a.txt", []byte("data\x00\xff")); err != nil {
		t.Fatal(err)
	}
	if b, err := a.ReadFile(ctx, inst.Ref, "/tmp/work/a.txt"); err != nil || string(b) != "data\x00\xff" {
		t.Fatalf("read back %q %v", b, err)
	}
	items, err := a.ListFiles(ctx, inst.Ref, "/tmp/work")
	if err != nil || len(items) != 1 || items[0].Name != "a.txt" {
		t.Fatalf("list %+v %v", items, err)
	}
	list, err := a.List(ctx, "inst_live")
	if err != nil || len(list) == 0 || list[0].SandboxID != id {
		t.Fatalf("list by installation %+v %v", list, err)
	}
	if err := a.Pause(ctx, inst.Ref); err != nil {
		t.Logf("pause not available on this runtime: %v", err)
	} else if err := a.Resume(ctx, inst.Ref); err != nil {
		t.Fatalf("resume: %v", err)
	} else if res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "cat /tmp/work/a.txt | wc -c"}); err != nil || strings.TrimSpace(res.Stdout) != "6" {
		t.Fatalf("state after resume: %+v %v", res, err)
	}
	if snap, err := a.Snapshot(ctx, inst.Ref, "disk"); err != nil {
		t.Logf("snapshot not available on this runtime: %v", err)
	} else {
		t.Logf("snapshot %s", snap.Ref)
		_ = a.DeleteSnapshot(context.Background(), snap.Ref)
	}
	if err := a.Delete(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
}
