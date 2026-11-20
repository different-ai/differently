// Package acidtest runs the acid test from an adapter's own tests, the way
// storetest runs the store conformance suite: against a fake of the
// provider's API, so a change to the adapter that breaks the contract fails
// `go test` before any live run.
package acidtest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/different-ai/differently/internal/acid"
	"github.com/different-ai/differently/pkg/adapter"
)

// Quick are options for tests: small payloads, a short long command and a
// fixed seed. The report records them, so quick runs are never mistaken for
// comparable ones.
func Quick() acid.Options {
	return acid.Options{
		Seed: 1, LongCommand: 2 * time.Second, LargeOutputBytes: 1 << 20, LargeFileBytes: 4 << 20,
		TestTimeout: time.Minute, ReadyTimeout: 30 * time.Second,
	}
}

// Check runs the suite against a and fails t for every test whose outcome
// differs from want (tests not in want are reported, not checked), and for
// any instance left behind. It returns the report.
func Check(t *testing.T, a adapter.ExecutionAdapter, opts acid.Options, want map[string]acid.Outcome) acid.Report {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	rep := acid.Run(ctx, a, opts)
	var b strings.Builder
	acid.WriteMarkdown(&b, rep)
	t.Log("\n" + b.String())
	for _, r := range rep.Results {
		if w, ok := want[r.ID]; ok && r.Outcome != w {
			t.Errorf("%s %s: %s, want %s (%s)", r.ID, r.Title, r.Outcome, w, r.Evidence)
		}
	}
	for id := range want {
		found := false
		for _, r := range rep.Results {
			found = found || r.ID == id
		}
		if !found {
			t.Errorf("%s did not run", id)
		}
	}
	if len(rep.Leaked) > 0 {
		t.Errorf("instances left behind: %v", rep.Leaked)
	}
	return rep
}

// All returns want with every test of the given levels expected to be o,
// for adapters that should pass a level outright.
func All(o acid.Outcome, levels ...int) map[string]acid.Outcome {
	want := map[string]acid.Outcome{}
	for _, t := range acid.Tests() {
		for _, l := range levels {
			if t.Level == l {
				want[t.ID] = o
			}
		}
	}
	return want
}
