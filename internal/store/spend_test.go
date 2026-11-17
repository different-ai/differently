package store

import (
	"math"
	"testing"
	"time"

	"github.com/different-ai/differently/pkg/api"
)

// MonthSpend prices this month's run time per pool and never counts time
// from before the month began.
func TestMonthSpend(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	hour := func(h float64) time.Time { return now.Add(time.Duration(h * float64(time.Hour))) }
	at := func(tm time.Time) *time.Time { return &tm }
	two := api.Resources{CPU: 2, MemoryMiB: 4096}
	rec := func(pool string, created time.Time, ended, runningSince *time.Time, runSeconds float64) SandboxRecord {
		return SandboxRecord{Sandbox: api.Sandbox{Pool: pool, Resources: two, CreatedAt: created, EndedAt: ended}, RunningSince: runningSince, RunSeconds: runSeconds}
	}
	costs := map[string]api.CostModel{
		"vendor": {CPUHour: 0.05, MemoryGiBHour: 0.0125}, // 2 CPU + 4 GiB = 0.15 an hour
		"flat":   {SandboxHour: 1},
	}
	recs := []SandboxRecord{
		rec("vendor", hour(-10), at(hour(-8)), nil, 7200),                         // ran 2 h this month, ended
		rec("vendor", hour(-3), nil, at(hour(-1)), 3600),                          // 1 h banked + 1 h in the current stretch
		rec("flat", hour(-24*40), nil, at(hour(-24*40)), 0),                       // running since before the month: only this month counts
		rec("flat", hour(-24*60), at(hour(-24*30)), nil, 99999),                   // ended before the month began
		rec("", hour(-1), nil, nil, 3600),                                         // never placed
		rec("unpriced", hour(-5), nil, at(hour(-5)), 0),                           // no cost model: costs nothing
		rec("vendor", hour(-24*20), at(MonthStart(now).Add(time.Hour)), nil, 1e9), // a total far larger than its hour in the month
	}
	got := MonthSpend(recs, costs, now)
	sinceMonthStart := now.Sub(MonthStart(now)).Hours()
	want := map[string]float64{
		"vendor": 0.15 * (2 + 2 + 1), // the last record is capped at the hour it lived this month
		"flat":   sinceMonthStart,
	}
	for pool, w := range want {
		if math.Abs(got[pool]-w) > 1e-9 {
			t.Errorf("%s: spend %.6f, want %.6f", pool, got[pool], w)
		}
	}
	if got["unpriced"] != 0 || got[""] != 0 {
		t.Errorf("unpriced or unplaced work was charged: %v", got)
	}
	if start := MonthStart(now); !start.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("month start %v", start)
	}
}
