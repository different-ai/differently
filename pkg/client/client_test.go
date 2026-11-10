package client

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/different-ai/differently/pkg/api"
)

// A streamed exec lasts as long as its command. The client's overall request
// timeout (10 minutes by default) must not cut it off; the caller's context
// bounds it.
func TestExecStreamOutlivesTheRequestTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: stdout\ndata: {\"stream\":\"stdout\",\"data\":\"started\\n\"}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(400 * time.Millisecond) // the command is still running
		fmt.Fprint(w, "event: exit\ndata: {\"exitCode\":0}\n\n")
	}))
	defer srv.Close()
	c := New(srv.URL, "dly_test_key")
	c.HTTP.Timeout = 100 * time.Millisecond

	var out string
	code, err := c.ExecStream(context.Background(), "sbx_1", api.ExecRequest{Command: "sleep 1"}, func(ev api.ExecEvent) { out += ev.Data })
	if err != nil || code != 0 || out != "started\n" {
		t.Fatalf("stream cut short: code %d, output %q, err %v", code, out, err)
	}
	// Ordinary requests keep the timeout.
	if c.HTTP.Timeout != 100*time.Millisecond {
		t.Fatalf("the shared client's timeout changed to %v", c.HTTP.Timeout)
	}
}
