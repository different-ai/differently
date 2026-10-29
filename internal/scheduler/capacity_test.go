package scheduler

import (
	"testing"
	"time"

	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/pkg/api"
)

func TestVictimsOrder(t *testing.T) {
	now := time.Now()
	ago := func(d time.Duration) *time.Time { v := now.Add(-d); return &v }
	rec := func(id string, active, running time.Duration, prio int, preemptible bool) store.SandboxRecord {
		return store.SandboxRecord{BackendRef: "ref-" + id, RunningSince: ago(running), Sandbox: api.Sandbox{ID: id, LastActivityAt: ago(active), Priority: prio, Preemptible: preemptible}}
	}
	running := []store.SandboxRecord{
		rec("busy", time.Second, time.Hour, 0, true),
		rec("young", time.Hour, time.Minute, 0, true),    // under minRunSeconds
		rec("guarded", 3*time.Hour, time.Hour, 0, false), // not preemptible
		rec("idle-high", 2*time.Hour, time.Hour, 5, true),
		rec("idle-low", 2*time.Hour, time.Hour, 0, true),
		rec("oldest", 4*time.Hour, 5*time.Hour, 9, true),
	}
	ids := func(rs []store.SandboxRecord) (out []string) {
		for _, r := range rs {
			out = append(out, r.ID)
		}
		return out
	}
	lru := ids(Victims(running, api.Preemption{Mode: api.PreemptLRU}, now))
	if want := []string{"oldest", "idle-low", "idle-high", "busy"}; !equal(lru, want) {
		t.Fatalf("lru victims %v, want %v", lru, want)
	}
	idle := ids(Victims(running, api.Preemption{Mode: api.PreemptIdle, IdleSeconds: 3600}, now))
	if want := []string{"oldest", "idle-low", "idle-high", "young"}; !equal(idle, want) {
		t.Fatalf("idle victims %v, want %v", idle, want)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
