package localexec

import (
	"context"
	"strings"
	"testing"

	"github.com/different-ai/differently/pkg/api"
)

func TestRunnerExecAndFiles(t *testing.T) {
	r := &Runner{Root: t.TempDir(), BaseEnv: map[string]string{"GREETING": "hi"}}
	ctx := context.Background()

	res, err := r.Exec(ctx, api.ExecRequest{Command: `echo "$GREETING $WHO"; echo oops >&2; exit 3`, Env: map[string]string{"WHO": "there"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 3 || strings.TrimSpace(res.Stdout) != "hi there" || strings.TrimSpace(res.Stderr) != "oops" {
		t.Fatalf("unexpected result %+v", res)
	}

	if err := r.WriteFile("/work/a.txt", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	data, err := r.ReadFile("work/a.txt")
	if err != nil || string(data) != "hello" {
		t.Fatalf("read back %q, %v", data, err)
	}
	items, err := r.ListFiles("/work")
	if err != nil || len(items) != 1 || items[0].Path != "/work/a.txt" {
		t.Fatalf("list %+v, %v", items, err)
	}
	if _, err := r.ReadFile("/missing"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("want not_found, got %v", err)
	}
	// Paths cannot climb out of the root.
	full, err := r.Resolve("../../etc/passwd")
	if err != nil || !strings.HasPrefix(full, r.Root) {
		t.Fatalf("resolve escaped root: %s %v", full, err)
	}
}

func TestRunnerTimeoutKillsProcessGroup(t *testing.T) {
	r := &Runner{Root: t.TempDir()}
	res, err := r.Exec(context.Background(), api.ExecRequest{Command: "sleep 30 & sleep 30", TimeoutSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode == 0 || res.DurationMs > 10_000 {
		t.Fatalf("timeout not enforced: %+v", res)
	}
}
