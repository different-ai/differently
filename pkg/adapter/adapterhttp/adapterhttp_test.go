package adapterhttp_test

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/different-ai/differently/internal/adapters/fake"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/adapter/adapterhttp"
	"github.com/different-ai/differently/pkg/api"
)

func serve(t *testing.T, a adapter.ExecutionAdapter, token string) *adapterhttp.Client {
	t.Helper()
	srv := httptest.NewServer(adapterhttp.Handler(a, token))
	t.Cleanup(srv.Close)
	return adapterhttp.NewClient(srv.URL, token)
}

// The client and handler round-trip every operation, keep error codes, and
// stay idempotent where the contract requires it.
func TestRoundTrip(t *testing.T) {
	ctx := context.Background()
	backend := fake.New(fake.Options{Trust: api.TrustPrivate, ExecFunc: func(req api.ExecRequest) api.ExecResult {
		return api.ExecResult{Stdout: "out:" + req.Command, Stderr: "err", ExitCode: 3}
	}})
	c := serve(t, backend, "plugin-token")

	info, err := c.Discover(ctx)
	if err != nil || info.Info.Protocol != adapter.ProtocolVersion || !slices.Contains(info.Capabilities.Features, adapter.FeatureFiles) {
		t.Fatalf("discover %+v %v", info, err)
	}
	req := adapter.CreateRequest{ID: "sbx_1", Installation: "inst"}
	in, err := c.Create(ctx, req)
	if err != nil || in.SandboxID != "sbx_1" {
		t.Fatalf("create %+v %v", in, err)
	}
	if again, err := c.Create(ctx, req); err != nil || again.Ref != in.Ref {
		t.Fatalf("create is not idempotent: %+v %v", again, err)
	}
	if list, err := c.List(ctx, "inst"); err != nil || len(list) != 1 || list[0].Ref != in.Ref {
		t.Fatalf("list %+v %v", list, err)
	}
	if list, err := c.List(ctx, "other"); err != nil || len(list) != 0 {
		t.Fatalf("list leaked another installation's instances: %+v %v", list, err)
	}

	res, err := c.Exec(ctx, in.Ref, api.ExecRequest{Command: "x"})
	if err != nil || res.Stdout != "out:x" || res.Stderr != "err" || res.ExitCode != 3 {
		t.Fatalf("exec %+v %v", res, err)
	}
	var streamed []string
	code, err := c.ExecStream(ctx, in.Ref, api.ExecRequest{Command: "y"}, func(ev api.ExecEvent) error {
		streamed = append(streamed, ev.Stream+":"+ev.Data)
		return nil
	})
	if err != nil || code != 3 || !slices.Contains(streamed, "stdout:out:y") {
		t.Fatalf("stream code=%d %v %v", code, streamed, err)
	}

	data := []byte{0, 1, 2, 0xff, '\n'}
	if err := c.WriteFile(ctx, in.Ref, "/work/bin", data); err != nil {
		t.Fatal(err)
	}
	if got, err := c.ReadFile(ctx, in.Ref, "/work/bin"); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read %v %v", got, err)
	}
	if files, err := c.ListFiles(ctx, in.Ref, "/work"); err != nil || len(files) != 1 {
		t.Fatalf("list files %+v %v", files, err)
	}

	for _, step := range []func(context.Context, string) error{c.Pause, c.Resume, c.Stop, c.Start} {
		if err := step(ctx, in.Ref); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := c.Snapshot(ctx, in.Ref, "disk")
	if err != nil || snap.Ref == "" {
		t.Fatalf("snapshot %+v %v", snap, err)
	}
	if err := c.DeleteSnapshot(ctx, snap.Ref); err != nil {
		t.Fatal(err)
	}

	if err := c.Delete(ctx, in.Ref); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, in.Ref); err != nil {
		t.Fatalf("delete is not idempotent: %v", err)
	}
	if _, err := c.Get(ctx, in.Ref); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("get after delete should be not_found, got %v", err)
	}
}

func TestRejectsWrongToken(t *testing.T) {
	srv := httptest.NewServer(adapterhttp.Handler(fake.New(fake.Options{}), "right"))
	t.Cleanup(srv.Close)
	if _, err := adapterhttp.NewClient(srv.URL, "wrong").Discover(context.Background()); !api.IsCode(err, api.CodeUnauthenticated) {
		t.Fatalf("want unauthenticated, got %v", err)
	}
}

// A backend without the optional interfaces answers unsupported_operation
// instead of failing in some other way.
func TestUnsupportedOptionalOperations(t *testing.T) {
	ctx := context.Background()
	minimal := struct{ adapter.ExecutionAdapter }{fake.New(fake.Options{})}
	c := serve(t, minimal, "")
	in, err := c.Create(ctx, adapter.CreateRequest{ID: "sbx_1", Installation: "inst"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReadFile(ctx, in.Ref, "/etc/hostname"); !api.IsCode(err, api.CodeUnsupportedOperation) {
		t.Fatalf("read file: want unsupported_operation, got %v", err)
	}
	if err := c.Pause(ctx, in.Ref); !api.IsCode(err, api.CodeUnsupportedOperation) {
		t.Fatalf("pause: want unsupported_operation, got %v", err)
	}
}

// One sandbox's failure keeps its code across the protocol, so the scheduler
// does not mistake it for an outage of the plugin's backend.
func TestSandboxFailedSurvivesTheProtocol(t *testing.T) {
	backend := fake.New(fake.Options{})
	backend.FailCreates, backend.FailWith = 1, adapter.SandboxFailed(errors.New("pod not ready"))
	c := serve(t, backend, "")
	if _, err := c.Create(context.Background(), adapter.CreateRequest{ID: "sbx_1", Installation: "inst"}); !api.IsCode(err, adapter.CodeSandboxFailed) {
		t.Fatalf("want sandbox_failed, got %v", err)
	}
}
