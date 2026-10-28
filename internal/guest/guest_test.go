package guest

import (
	"context"
	"net"
	"net/http/httptest"
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

func TestClientOverHandler(t *testing.T) {
	srv := httptest.NewServer(Handler(&Runner{Root: t.TempDir()}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	c := NewClient(func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	})
	ctx := context.Background()
	if err := c.Health(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteFile(ctx, "/x/y", []byte("data")); err != nil {
		t.Fatal(err)
	}
	if got, err := c.ReadFile(ctx, "/x/y"); err != nil || string(got) != "data" {
		t.Fatalf("got %q %v", got, err)
	}
	var chunks []string
	code, err := c.ExecStream(ctx, api.ExecRequest{Command: "printf a; printf b >&2"}, func(ev api.ExecEvent) error {
		chunks = append(chunks, ev.Stream+":"+ev.Data)
		return nil
	})
	if err != nil || code != 0 || len(chunks) != 2 {
		t.Fatalf("stream code=%d chunks=%v err=%v", code, chunks, err)
	}
	if _, err := c.ReadFile(ctx, "/nope"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("want not_found over the wire, got %v", err)
	}
}
