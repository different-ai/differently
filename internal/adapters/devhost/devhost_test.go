package devhost

import (
	"context"
	"strings"
	"testing"

	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

func TestLifecycleAndSnapshotRestore(t *testing.T) {
	a, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	inst, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_1", Installation: "inst", Env: map[string]string{"NAME": "world"}})
	if err != nil {
		t.Fatal(err)
	}
	again, _ := a.Create(ctx, adapter.CreateRequest{ID: "sbx_1", Installation: "inst"})
	if again.Ref != inst.Ref {
		t.Fatal("create is not idempotent")
	}
	res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "echo hello $NAME > greeting && cat greeting"})
	if err != nil || strings.TrimSpace(res.Stdout) != "hello world" {
		t.Fatalf("exec %+v %v", res, err)
	}
	if err := a.Stop(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "true"}); !api.IsCode(err, api.CodeInvalidState) {
		t.Fatalf("exec on stopped sandbox: %v", err)
	}
	if err := a.Start(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	snap, err := a.Snapshot(ctx, inst.Ref, "disk")
	if err != nil {
		t.Fatal(err)
	}
	fork, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_2", Installation: "inst", FromSnapshot: snap.Ref})
	if err != nil {
		t.Fatal(err)
	}
	data, err := a.ReadFile(ctx, fork.Ref, "/greeting")
	if err != nil || strings.TrimSpace(string(data)) != "hello world" {
		t.Fatalf("fork lost disk state: %q %v", data, err)
	}
	items, _ := a.List(ctx, "inst")
	if len(items) != 2 {
		t.Fatalf("list %v", items)
	}
	_ = a.Delete(ctx, inst.Ref)
	if _, err := a.Get(ctx, inst.Ref); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("deleted sandbox still present: %v", err)
	}
}

func TestRejectsRefsOutsideRoot(t *testing.T) {
	root := t.TempDir()
	a, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, ref := range []string{"", ".", "..", "../x", "a/b"} {
		if _, err := a.Create(ctx, adapter.CreateRequest{ID: ref}); err == nil {
			t.Errorf("Create(%q) succeeded", ref)
		}
		if _, err := a.Get(ctx, ref); err == nil {
			t.Errorf("Get(%q) succeeded", ref)
		}
		if err := a.Delete(ctx, ref); err == nil {
			t.Errorf("Delete(%q) succeeded", ref)
		}
		if err := a.DeleteSnapshot(ctx, ref); err == nil {
			t.Errorf("DeleteSnapshot(%q) succeeded", ref)
		}
	}
	if _, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_ok", FromSnapshot: ".."}); err == nil {
		t.Error("Create from snapshot \"..\" succeeded")
	}
}
