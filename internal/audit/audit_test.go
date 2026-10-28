package audit

import (
	"testing"
	"time"

	"github.com/different-ai/differently/pkg/api"
)

func chain(n int) []api.AuditEvent {
	var out []api.AuditEvent
	prev := ""
	for i := 1; i <= n; i++ {
		ev := Seal(prev, api.AuditEvent{Seq: int64(i), Time: time.Unix(int64(i), 0).UTC(), Actor: "key_a", Action: "sandbox.create", Resource: "sbx_1", Details: map[string]string{"b": "2", "a": "1"}})
		out = append(out, ev)
		prev = ev.Hash
	}
	return out
}

func TestVerifyDetectsTampering(t *testing.T) {
	events := chain(5)
	if res := Verify(events); !res.OK || res.HeadHash != events[4].Hash {
		t.Fatalf("intact chain failed: %+v", res)
	}

	edited := chain(5)
	edited[2].Actor = "someone-else"
	if res := Verify(edited); res.OK || res.BrokenAt != 3 {
		t.Fatalf("edit not detected: %+v", res)
	}

	deleted := append(chain(5)[:2], chain(5)[3:]...)
	if res := Verify(deleted); res.OK {
		t.Fatalf("deletion not detected: %+v", res)
	}
}
