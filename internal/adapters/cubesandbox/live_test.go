//go:build live

package cubesandbox

// Live check against a CubeSandbox install (not yet run; see
// docs/providers/cubesandbox.md):
//
//	CUBE_API_URL=http://<node>:3000 CUBE_TEMPLATE_ID=<template> \
//	CUBE_SANDBOX_URL=http://<node>:80 go test -tags live -v -run TestLiveCubeSandbox ./internal/adapters/cubesandbox/
//
// Settings: CUBE_API_KEY (when CubeAPI has auth on), CUBE_SANDBOX_URL
// (CubeProxy, for header routing; without it, host routing through
// CUBE_DOMAIN and CUBE_PROXY_SCHEME), CUBE_CA_FILE.

import (
	"bytes"
	"context"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

func TestLiveCubeSandbox(t *testing.T) {
	base := os.Getenv("CUBE_API_URL")
	if base == "" {
		t.Skip("set CUBE_API_URL and CUBE_TEMPLATE_ID")
	}
	o, err := FromConfig(map[string]string{
		"apiURL": base, "templateId": os.Getenv("CUBE_TEMPLATE_ID"), "sandboxURL": os.Getenv("CUBE_SANDBOX_URL"),
		"domain": os.Getenv("CUBE_DOMAIN"), "proxyScheme": os.Getenv("CUBE_PROXY_SCHEME"), "caFile": os.Getenv("CUBE_CA_FILE"),
	}, api.TrustPrivate, os.Getenv("CUBE_API_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	fatal := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	fatal("health", a.Health(ctx))
	start := time.Now()
	in, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_live_cube", Installation: "inst_live", Env: map[string]string{"GREETING": "hello"}, TTLSeconds: 900})
	fatal("create", err)
	t.Logf("create took %s", time.Since(start).Round(time.Millisecond))
	defer a.Delete(context.Background(), in.Ref)
	again, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_live_cube", Installation: "inst_live"})
	if err != nil || again.Ref != in.Ref {
		t.Fatalf("create is not idempotent: %+v %v", again, err)
	}
	list, err := a.List(ctx, "inst_live")
	if err != nil || !slices.ContainsFunc(list, func(i adapter.Instance) bool { return i.Ref == in.Ref }) {
		t.Fatalf("list %+v %v", list, err)
	}
	res, err := a.Exec(ctx, in.Ref, api.ExecRequest{Command: `echo "$GREETING $WHO"; printf err >&2; exit 7`, Env: map[string]string{"WHO": "world"}})
	if err != nil || res.ExitCode != 7 || res.Stdout != "hello world\n" || res.Stderr != "err" {
		t.Fatalf("exec %+v %v", res, err)
	}
	res, err = a.Exec(ctx, in.Ref, api.ExecRequest{Command: "tr a-z A-Z", Stdin: "hi\n"})
	if err != nil || res.Stdout != "HI\n" {
		t.Fatalf("stdin %+v %v", res, err)
	}
	res, err = a.Exec(ctx, in.Ref, api.ExecRequest{Command: "sleep 30", TimeoutSeconds: 1})
	if err != nil || res.ExitCode != 124 {
		t.Fatalf("timeout %+v %v", res, err)
	}
	data := bytes.Repeat([]byte{0, 7, 255}, 3000)
	fatal("write", a.WriteFile(ctx, in.Ref, "/tmp/live/rec.bin", data))
	back, err := a.ReadFile(ctx, in.Ref, "/tmp/live/rec.bin")
	if err != nil || !bytes.Equal(back, data) {
		t.Fatalf("read %d bytes %v", len(back), err)
	}
	files, err := a.ListFiles(ctx, in.Ref, "/tmp/live")
	if err != nil || len(files) != 1 || files[0].Size != int64(len(data)) {
		t.Fatalf("list files %+v %v", files, err)
	}
	fatal("pause", a.Pause(ctx, in.Ref))
	fatal("resume", a.Resume(ctx, in.Ref))
	if back, err := a.ReadFile(ctx, in.Ref, "/tmp/live/rec.bin"); err != nil || !bytes.Equal(back, data) {
		t.Fatalf("file lost across pause/resume: %v", err)
	}
	snap, err := a.Snapshot(ctx, in.Ref, "memory")
	fatal("snapshot", err)
	fork, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_live_cube_fork", Installation: "inst_live", FromSnapshot: snap.Ref})
	fatal("fork", err)
	if back, err := a.ReadFile(ctx, fork.Ref, "/tmp/live/rec.bin"); err != nil || !bytes.Equal(back, data) {
		t.Fatalf("fork does not carry the file: %v", err)
	}
	fatal("delete fork", a.Delete(ctx, fork.Ref))
	fatal("delete snapshot", a.DeleteSnapshot(ctx, snap.Ref))
	fatal("set expiry", a.SetExpiry(ctx, in.Ref, time.Now().Add(20*time.Minute)))
	fatal("delete", a.Delete(ctx, in.Ref))
	if _, err := a.Get(ctx, in.Ref); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	t.Logf("scenario passed in %s", time.Since(start).Round(time.Second))
}
