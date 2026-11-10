package daytona

import (
	"context"
	"testing"

	"github.com/different-ai/differently/pkg/adapter"
)

// Daytona's deletion deadline counts from the create request and Differently's
// TTL from the moment the sandbox is ready, so the deadline is sent with a
// grace: Differently's own, audited expiry must run first.
func TestDaytonaBackstopOutlastsTheTTL(t *testing.T) {
	if got := ttlMinutes(600); got != 15 {
		t.Fatalf("ttlMinutes %d for a 600 s TTL, want 15 (the TTL plus a 5-minute grace)", got)
	}
	if got := ttlMinutes(1); got != 6 {
		t.Fatalf("ttlMinutes %d for a 1 s TTL, want 6 (rounded up, plus the grace)", got)
	}
	f := newFake(t)
	a := New(Options{APIURL: f.srv.URL + "/api", APIKey: "k", Snapshot: "daytona-small"})
	if _, err := a.Create(context.Background(), adapter.CreateRequest{ID: "sbx_ttl", Installation: "inst_1", TTLSeconds: 600}); err != nil {
		t.Fatal(err)
	}
}
