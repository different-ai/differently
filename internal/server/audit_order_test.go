package server_test

import (
	"fmt"
	"testing"

	"github.com/different-ai/differently/pkg/api"
)

// The audit views ask for the newest events. Without order=desc a page holds
// the oldest ones, so a busy installation never showed recent activity.
func TestAuditEventsNewestFirst(t *testing.T) {
	h := newHarness(t, api.PlacementPolicy{})
	for i := 0; i < 4; i++ {
		h.must(201, "POST", "/v1/admin/organizations", operatorToken, map[string]string{"name": fmt.Sprintf("org-%d", i)}, nil)
	}
	list := func(query string) api.List[api.AuditEvent] {
		t.Helper()
		var out api.List[api.AuditEvent]
		h.must(200, "GET", "/v1/audit-events?"+query, operatorToken, nil, &out)
		return out
	}
	all := list("limit=1000")
	total := int64(len(all.Items))
	if total < 4 || all.Items[0].Seq != 1 {
		t.Fatalf("default order should start at the oldest event: %d events, first seq %d", total, all.Items[0].Seq)
	}
	newest := list("order=desc&limit=2")
	if len(newest.Items) != 2 || newest.Items[0].Seq != total || newest.Items[1].Seq != total-1 {
		t.Fatalf("order=desc should return the two newest events (%d, %d): %+v", total, total-1, newest.Items)
	}
	if newest.NextCursor != fmt.Sprint(total-1) {
		t.Fatalf("nextCursor %q, want the last sequence %d", newest.NextCursor, total-1)
	}
	older := list("order=desc&limit=2&beforeSeq=" + newest.NextCursor)
	if len(older.Items) != 2 || older.Items[0].Seq != total-2 {
		t.Fatalf("the next page should continue at %d: %+v", total-2, older.Items)
	}
	if code := h.call("GET", "/v1/audit-events?order=sideways", operatorToken, nil, nil); code != 400 {
		t.Fatalf("an unknown order answered %d, want 400", code)
	}
}
