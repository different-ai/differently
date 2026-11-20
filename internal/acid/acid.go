// Package acid is the sandbox acid test: one suite, getting harder level by
// level, that any execution adapter can be run through. It drives the
// adapter contract (pkg/adapter) directly, so every built-in adapter runs it,
// and any other backend runs it behind the remote HTTP protocol.
//
// docs/acid-test.md is the design. This package holds levels 0–2 (contract,
// commands, files); cmd/differently-acid runs it against a configured pool,
// and internal/acid/acidtest runs it from an adapter's own tests.
package acid

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// Suite names this version of the tests. Results are only comparable within
// one suite version; once published, a version's tests are frozen.
const Suite = "acid/1"

// Outcome is what one test found.
type Outcome string

const (
	// Pass: declared (or required) and works.
	Pass Outcome = "pass"
	// Unsupported: not declared, and the backend says so.
	Unsupported Outcome = "unsupported"
	// Fail: declared or required, and does not work.
	Fail Outcome = "fail"
	// Dishonest: claims to work but silently does something else, such as
	// output cut short without saying so.
	Dishonest Outcome = "dishonest"
	// Skipped: not run, because something it needs failed first.
	Skipped Outcome = "skipped"
)

// Test is one check.
type Test struct {
	ID    string // "L1.04"
	Level int
	Title string
	// Feature, when set, is the adapter feature the test exercises. When the
	// adapter does not declare it, the test is not run and reports
	// Unsupported; L0.07 checks that such operations answer
	// unsupported_operation.
	Feature string
	Run     func(t *T)
}

// Options configures a run. Zero values take the defaults in the design.
type Options struct {
	// Installation scopes every instance the run creates; List and the final
	// sweep use it. Default: "acid-" and a random suffix.
	Installation string
	Resources    api.Resources // default 1 CPU, 1024 MiB, 5 GiB
	Template     string
	MaxLevel     int      // highest level to run; 0 runs them all (use Only for level 0 alone)
	Only         []string // test IDs; empty runs every test up to MaxLevel
	// Seed makes sandbox IDs and payloads repeatable (for recordings).
	// Zero picks a random seed, recorded in the report.
	Seed uint64
	// TestTimeout bounds one test; ReadyTimeout bounds the wait for a new
	// instance to run.
	TestTimeout  time.Duration // default 15 min
	ReadyTimeout time.Duration // default 5 min
	// LongCommand is how long L1.07's command runs (default 10 min).
	LongCommand time.Duration
	// LargeOutputBytes is L1.06's output size (default 10 MiB);
	// LargeFileBytes is L2.05's file size (default 100 MiB).
	LargeOutputBytes int
	LargeFileBytes   int
	// BadCredentials builds the same adapter with a wrong credential and
	// returns it with that credential's value, so L0.01 can check it is
	// refused and L0.06 that the value is never echoed. Nil skips both
	// checks (adapters without credentials).
	BadCredentials func() (adapter.ExecutionAdapter, string, error)
	// Progress, when set, is called as each test finishes.
	Progress func(Result)
}

func (o *Options) defaults() {
	if o.Seed == 0 {
		o.Seed = rand.Uint64()
	}
	if o.Installation == "" {
		o.Installation = "acid-" + randomName(rand.New(rand.NewPCG(o.Seed, 1)), 10)
	}
	if o.Resources == (api.Resources{}) {
		o.Resources = api.Resources{CPU: 1, MemoryMiB: 1024, DiskGiB: 5}
	}
	if o.MaxLevel <= 0 || o.MaxLevel > MaxLevel || len(o.Only) > 0 {
		o.MaxLevel = MaxLevel
	}
	if o.TestTimeout == 0 {
		o.TestTimeout = 15 * time.Minute
	}
	if o.ReadyTimeout == 0 {
		o.ReadyTimeout = 5 * time.Minute
	}
	if o.LongCommand == 0 {
		o.LongCommand = 10 * time.Minute
	}
	if o.LargeOutputBytes == 0 {
		o.LargeOutputBytes = 10 << 20
	}
	if o.LargeFileBytes == 0 {
		o.LargeFileBytes = 100 << 20
	}
}

// MaxLevel is the highest level this suite version implements.
const MaxLevel = 2

// Result is one test's outcome.
type Result struct {
	ID       string             `json:"id"`
	Level    int                `json:"level"`
	Title    string             `json:"title"`
	Feature  string             `json:"feature,omitempty"`
	Outcome  Outcome            `json:"outcome"`
	Evidence string             `json:"evidence,omitempty"`
	Seconds  float64            `json:"seconds"`
	Metrics  map[string]float64 `json:"metrics,omitempty"`
}

// Report is one run, in the acid/1 result format.
type Report struct {
	Suite     string    `json:"suite"`
	StartedAt time.Time `json:"startedAt"`
	Seconds   float64   `json:"seconds"`
	// Pool and Environment are filled in by whoever runs the suite (the
	// CLI records the pool name, commit and runner).
	Pool        string            `json:"pool,omitempty"`
	Adapter     adapter.Info      `json:"adapter"`
	Features    []string          `json:"features"`
	Run         RunSettings       `json:"run"`
	Environment map[string]string `json:"environment,omitempty"`
	// Level is the highest level whose every test passed or was honestly
	// unsupported; -1 when level 0 did not.
	Level   int      `json:"level"`
	Passed  int      `json:"passed"`
	Total   int      `json:"total"`
	Results []Result `json:"results"`
	// Leaked lists instances still listed for the run's installation after
	// every test cleaned up and the final sweep deleted what was left.
	Leaked []string `json:"leaked,omitempty"`
}

// RunSettings records what makes runs comparable.
type RunSettings struct {
	Installation       string        `json:"installation"`
	Seed               uint64        `json:"seed"`
	MaxLevel           int           `json:"maxLevel"`
	Only               []string      `json:"only,omitempty"`
	Resources          api.Resources `json:"resources"`
	Template           string        `json:"template,omitempty"`
	LongCommandSeconds float64       `json:"longCommandSeconds"`
	LargeOutputBytes   int           `json:"largeOutputBytes"`
	LargeFileBytes     int           `json:"largeFileBytes"`
}

// Tests returns every test in the suite, in order.
func Tests() []Test {
	all := slices.Concat(level0(), level1(), level2())
	slices.SortFunc(all, func(a, b Test) int { return strings.Compare(a.ID, b.ID) })
	return all
}

// Run executes the suite against a and returns the report. It always tries
// to delete every instance it created, also when ctx ends.
func Run(ctx context.Context, a adapter.ExecutionAdapter, opts Options) Report {
	opts.defaults()
	start := time.Now()
	r := &runner{a: a, opts: opts, rng: rand.New(rand.NewPCG(opts.Seed, 2)), live: map[string]string{}}
	rep := Report{
		Suite: Suite, StartedAt: start.UTC(), Adapter: a.Info(),
		Run: RunSettings{
			Installation: opts.Installation, Seed: opts.Seed, MaxLevel: opts.MaxLevel, Only: opts.Only,
			Resources: opts.Resources, Template: opts.Template, LongCommandSeconds: opts.LongCommand.Seconds(),
			LargeOutputBytes: opts.LargeOutputBytes, LargeFileBytes: opts.LargeFileBytes,
		},
	}
	if caps, err := a.Capabilities(ctx); err == nil {
		r.caps = caps
		r.features = adapter.EffectiveFeatures(a, caps.Features)
		r.declared = caps.Features
	} else {
		r.capsErr = err
	}
	rep.Features = r.features

	for _, test := range Tests() {
		if test.Level > opts.MaxLevel || (len(opts.Only) > 0 && !slices.Contains(opts.Only, test.ID)) {
			continue
		}
		res := r.run(ctx, test)
		rep.Results = append(rep.Results, res)
		if opts.Progress != nil {
			opts.Progress(res)
		}
	}
	r.closeShared()
	rep.Leaked = r.sweep()
	rep.Level, rep.Passed, rep.Total = score(rep.Results, opts.MaxLevel)
	rep.Seconds = time.Since(start).Seconds()
	return rep
}

func score(results []Result, maxLevel int) (level, passed, total int) {
	level = -1
	ok := map[int]bool{}
	seen := map[int]bool{}
	for _, r := range results {
		seen[r.Level] = true
		if _, set := ok[r.Level]; !set {
			ok[r.Level] = true
		}
		if r.Outcome != Pass && r.Outcome != Unsupported {
			ok[r.Level] = false
		}
		total++
		if r.Outcome == Pass {
			passed++
		}
	}
	for l := 0; l <= maxLevel && seen[l] && ok[l]; l++ {
		level = l
	}
	return level, passed, total
}

// runner holds one run's state.
type runner struct {
	a        adapter.ExecutionAdapter
	opts     Options
	caps     adapter.Capabilities
	capsErr  error
	features []string // effective: declared and implemented
	declared []string

	mu   sync.Mutex
	rng  *rand.Rand
	live map[string]string // ref -> installation, everything not yet deleted

	shared    string // a sandbox L1 and L2 tests share
	sharedErr error
}

func (r *runner) has(feature string) bool { return slices.Contains(r.features, feature) }

// stop ends a test early; T's outcome methods panic with it.
type stop struct{}

func (r *runner) run(parent context.Context, test Test) (res Result) {
	res = Result{ID: test.ID, Level: test.Level, Title: test.Title, Feature: test.Feature, Outcome: Pass}
	if test.Feature != "" && !r.has(test.Feature) {
		res.Outcome, res.Evidence = Unsupported, test.Feature+" is not declared"
		return res
	}
	ctx, cancel := context.WithTimeout(parent, r.opts.TestTimeout)
	defer cancel()
	t := &T{ctx: ctx, r: r, res: &res}
	start := time.Now()
	func() {
		defer func() {
			if p := recover(); p != nil {
				if _, ok := p.(stop); !ok {
					buf := make([]byte, 4096)
					buf = buf[:runtime.Stack(buf, false)]
					t.set(Fail, fmt.Sprintf("test panicked: %v\n%s", p, buf))
				}
			}
		}()
		test.Run(t)
	}()
	res.Seconds = time.Since(start).Seconds()
	t.cleanup()
	if ctx.Err() != nil && parent.Err() == nil && res.Outcome == Pass {
		res.Outcome, res.Evidence = Fail, fmt.Sprintf("did not finish within %s", r.opts.TestTimeout)
	}
	if parent.Err() != nil && res.Outcome == Pass {
		res.Outcome, res.Evidence = Skipped, "run cancelled"
	}
	return res
}

// track and untrack keep the set of instances to delete at the end.
func (r *runner) track(ref, installation string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.live[ref] = installation
}

func (r *runner) untrack(ref string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.live, ref)
}

func (r *runner) tracked() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]string, len(r.live))
	for k, v := range r.live {
		out[k] = v
	}
	return out
}

func (r *runner) closeShared() {
	if r.shared != "" {
		r.deleteRef(r.shared)
		r.shared = ""
	}
}

func (r *runner) deleteRef(ref string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := r.a.Delete(ctx, ref); err == nil || api.IsCode(err, api.CodeNotFound) {
		r.untrack(ref)
	}
}

// sweep deletes whatever the run still tracks or the backend still lists
// for the run's installations, and returns what remains listed.
func (r *runner) sweep() []string {
	for ref := range r.tracked() {
		r.deleteRef(ref)
	}
	installs := map[string]bool{r.opts.Installation: true}
	for _, in := range r.tracked() {
		installs[in] = true
	}
	installs[otherInstallation(r.opts.Installation)] = true
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var leaked []string
	for in := range installs {
		list, err := r.a.List(ctx, in)
		if err != nil {
			continue
		}
		for _, i := range list {
			r.deleteRef(i.Ref)
		}
		if list, err = r.a.List(ctx, in); err == nil {
			for _, i := range list {
				leaked = append(leaked, i.Ref)
			}
		}
	}
	slices.Sort(leaked)
	return leaked
}

func otherInstallation(installation string) string { return installation + "-other" }

// randomName is lowercase letters and digits: valid in every provider's
// labels, metadata and names that the adapters use.
func randomName(rng *rand.Rand, n int) string {
	const alphabet = "abcdefghijkmnpqrstuvwxyz23456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[rng.IntN(len(alphabet))]
	}
	return string(b)
}

// codeOf returns an error's Differently code, or "" for an unclassified one.
func codeOf(err error) string {
	var e *api.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func describe(err error) string {
	if c := codeOf(err); c != "" {
		return fmt.Sprintf("%s (%v)", c, err)
	}
	return fmt.Sprintf("unclassified error (%v)", err)
}
