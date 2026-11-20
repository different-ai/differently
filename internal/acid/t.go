package acid

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"

	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// T is what a test works with: the adapter, sandboxes that are always
// cleaned up, and ways to record its outcome.
type T struct {
	ctx      context.Context
	r        *runner
	res      *Result
	evidence []string
	cleanups []func()
}

// Ctx is the test's context; it ends at the test timeout.
func (t *T) Ctx() context.Context { return t.ctx }

// Adapter is the adapter under test.
func (t *T) Adapter() adapter.ExecutionAdapter { return t.r.a }

// Has reports whether the adapter declares and implements a feature.
func (t *T) Has(feature string) bool { return t.r.has(feature) }

// Options are the run's options.
func (t *T) Options() Options { return t.r.opts }

// Rand is the run's seeded source, so payloads repeat with the seed.
func (t *T) Rand() *rand.Rand { return t.r.rng }

// Name returns a fresh lowercase name, unique within the run.
func (t *T) Name(prefix string) string {
	t.r.mu.Lock()
	defer t.r.mu.Unlock()
	return prefix + "-" + randomName(t.r.rng, 12)
}

// Note adds evidence without changing the outcome.
func (t *T) Note(format string, args ...any) {
	t.evidence = append(t.evidence, fmt.Sprintf(format, args...))
	t.res.Evidence = strings.Join(t.evidence, "; ")
}

// Measure records a number (seconds unless the name says otherwise).
func (t *T) Measure(name string, v float64) {
	if t.res.Metrics == nil {
		t.res.Metrics = map[string]float64{}
	}
	t.res.Metrics[name] = v
}

func (t *T) set(o Outcome, msg string) {
	t.res.Outcome = o
	if msg != "" {
		t.evidence = append(t.evidence, msg)
	}
	t.res.Evidence = strings.Join(t.evidence, "; ")
}

// Fail ends the test as failed.
func (t *T) Fail(format string, args ...any) {
	t.set(Fail, fmt.Sprintf(format, args...))
	panic(stop{})
}

// Dishonest ends the test as dishonest: it looked like it worked and did not.
func (t *T) Dishonest(format string, args ...any) {
	t.set(Dishonest, fmt.Sprintf(format, args...))
	panic(stop{})
}

// Unsupported ends the test as honestly unsupported.
func (t *T) Unsupported(format string, args ...any) {
	t.set(Unsupported, fmt.Sprintf(format, args...))
	panic(stop{})
}

// Skip ends the test as not run.
func (t *T) Skip(format string, args ...any) {
	t.set(Skipped, fmt.Sprintf(format, args...))
	panic(stop{})
}

// Cleanup registers f to run when the test ends, last registered first.
func (t *T) Cleanup(f func()) { t.cleanups = append(t.cleanups, f) }

func (t *T) cleanup() {
	for i := len(t.cleanups) - 1; i >= 0; i-- {
		t.cleanups[i]()
	}
}

// Request returns a create request for id with the run's settings.
func (t *T) Request(id string) adapter.CreateRequest {
	return adapter.CreateRequest{
		ID: id, Installation: t.r.opts.Installation, Template: t.r.opts.Template,
		Resources: t.r.opts.Resources, TTLSeconds: 3600,
		Labels: map[string]string{"differently.dev/acid": "true"},
	}
}

// Create creates an instance, waits for it to run and deletes it when the
// test ends. It fails the test if the instance does not run.
func (t *T) Create(req adapter.CreateRequest) adapter.Instance {
	inst, err := t.r.a.Create(t.ctx, req)
	if err != nil {
		t.Fail("create %s: %s", req.ID, describe(err))
	}
	t.Owned(inst.Ref, req.Installation)
	inst, err = t.WaitRunning(inst)
	if err != nil {
		t.Fail("%v", err)
	}
	return inst
}

// Owned tracks a ref so it is deleted when the test ends (and swept at the
// end of the run if that fails).
func (t *T) Owned(ref, installation string) {
	if ref == "" {
		return
	}
	t.r.track(ref, installation)
	t.Cleanup(func() { t.r.deleteRef(ref) })
}

// WaitRunning polls Get until the instance runs, fails or the ready timeout
// passes.
func (t *T) WaitRunning(inst adapter.Instance) (adapter.Instance, error) {
	deadline := time.Now().Add(t.r.opts.ReadyTimeout)
	for {
		switch inst.State {
		case adapter.StateRunning:
			return inst, nil
		case adapter.StateError:
			return inst, fmt.Errorf("instance %s is in error: %s", inst.Ref, inst.Message)
		}
		if time.Now().After(deadline) {
			return inst, fmt.Errorf("instance %s still %q after %s", inst.Ref, inst.State, t.r.opts.ReadyTimeout)
		}
		select {
		case <-t.ctx.Done():
			return inst, t.ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		got, err := t.r.a.Get(t.ctx, inst.Ref)
		if err != nil {
			return inst, fmt.Errorf("get %s while waiting: %s", inst.Ref, describe(err))
		}
		inst = got
	}
}

// Shared returns a running sandbox shared by the tests of levels 1 and 2,
// created on first use and deleted at the end of the run. Tests that change
// its state must use their own. A failed creation skips every test after.
func (t *T) Shared() string {
	r := t.r
	if r.shared != "" {
		return r.shared
	}
	if r.sharedErr != nil {
		t.Skip("no shared sandbox: %v", r.sharedErr)
	}
	req := t.Request(t.Name("acid-shared"))
	inst, err := r.a.Create(t.ctx, req)
	if err == nil {
		r.track(inst.Ref, req.Installation)
		inst, err = t.WaitRunning(inst)
	}
	if err != nil {
		r.sharedErr = fmt.Errorf("create %s: %s", req.ID, describe(err))
		if inst.Ref != "" {
			r.deleteRef(inst.Ref)
		}
		t.Skip("no shared sandbox: %v", r.sharedErr)
	}
	r.shared = inst.Ref
	return r.shared
}

// Exec runs a request, failing the test on a transport or backend error.
func (t *T) Exec(ref string, req api.ExecRequest) api.ExecResult {
	res, err := t.r.a.Exec(t.ctx, ref, req)
	if err != nil {
		t.Fail("exec %s: %s", summary(req), describe(err))
	}
	return res
}

// Sh runs a shell command and fails the test unless it exits 0.
func (t *T) Sh(ref, command string) api.ExecResult {
	res := t.Exec(ref, api.ExecRequest{Command: command, TimeoutSeconds: 120})
	if res.ExitCode != 0 {
		t.Fail("%q exited %d: %s", command, res.ExitCode, clip(res.Stderr+res.Stdout))
	}
	return res
}

func summary(req api.ExecRequest) string {
	if req.Command != "" {
		return clip(req.Command)
	}
	return clip(strings.Join(req.Args, " "))
}

// clip quotes s for evidence, kept short and printable.
func clip(s string) string {
	const max = 120
	if len(s) > max {
		s = s[:max] + "…"
	}
	return strconv.Quote(s)
}
