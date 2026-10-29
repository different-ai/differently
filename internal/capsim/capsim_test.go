package capsim

import "testing"

// The findings docs/scheduling/capacity.md reports, pinned so a model change
// that overturns them fails here.
func TestStrategiesCompare(t *testing.T) {
	run := func(s Strategy) Result { return Average(Workbot(s, 4, 12), 5) }
	hold, release, idle, lru := run(Hold), run(Release), run(PreemptIdle), run(PreemptLRU)

	if release.PrivateUsers < 2*hold.PrivateUsers || release.ExternalHours > 0.4*hold.ExternalHours {
		t.Errorf("releasing paused slots should at least double the users the pool serves and halve overflow: hold %+v release %+v", hold, release)
	}
	if release.Utilization < 1.5*hold.Utilization {
		t.Errorf("releasing should raise utilization: hold %.3f release %.3f", hold.Utilization, release.Utilization)
	}
	if release.WaitP95 <= hold.WaitP95 {
		t.Errorf("the price of releasing is waiting for a slot: hold p95 %.0f release p95 %.0f", hold.WaitP95, release.WaitP95)
	}
	if idle.WaitP95 >= release.WaitP95 || idle.InterruptionsPerDay != 0 {
		t.Errorf("idle preemption should shorten the wait without interrupting sessions: release %+v idle %+v", release, idle)
	}
	if lru.InterruptionsPerDay == 0 {
		t.Errorf("lru preemption interrupts sessions: %+v", lru)
	}
}

func TestDeterministic(t *testing.T) {
	a, b := Run(Workbot(Release, 4, 8)), Run(Workbot(Release, 4, 8))
	if a != b {
		t.Fatalf("same seed, different results: %+v %+v", a, b)
	}
}
