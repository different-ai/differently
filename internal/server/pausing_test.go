package server_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/different-ai/differently/internal/adapters/fake"
	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/pkg/api"
)

// holdingPool is a one-slot checkpointing pool whose pauses take until the
// returned function runs, like a Fast Sandbox checkpoint, with one sandbox
// running on it.
func holdingPool(t *testing.T, h *harness, pre *api.Preemption, cancels bool) (token string, pin *api.Placement, sb api.Sandbox, a *fake.Adapter, finish func()) {
	t.Helper()
	token, pin, a = checkpointBackend(t, h, pre)
	hold := make(chan struct{})
	a.HoldPauses, a.PauseCancels = hold, cancels
	sb = h.create(token, api.CreateSandboxRequest{Profile: "small", Placement: pin})
	h.tick()
	var once bool
	finish = func() {
		if !once {
			once = true
			close(hold)
		}
	}
	t.Cleanup(finish)
	return token, pin, sb, a, finish
}

// pause asks for a pause and returns once the sandbox is pausing, with a
// channel that carries the pause request's HTTP status.
func (h *harness) pause(token, id string) <-chan int {
	h.t.Helper()
	done := make(chan int, 1)
	go func() { done <- h.call("POST", "/v1/sandboxes/"+id+"/pause", token, nil, nil) }()
	h.waitFor(token, id, api.StatusPausing)
	return done
}

func (h *harness) waitFor(token, id string, want api.SandboxStatus) api.Sandbox {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := h.get(token, id)
		if got.Status == want {
			return got
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("sandbox stayed %s, want %s", got.Status, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *harness) wrote(token, id string) int {
	return h.call("PUT", "/v1/sandboxes/"+id+"/files/content?path=/work", token, []byte("work"), nil)
}

func (h *harness) ran(token, id string) int {
	return h.call("POST", "/v1/sandboxes/"+id+"/exec", token, api.ExecRequest{Command: "echo work"}, nil)
}

// While a checkpoint uploads the VM still answers, but whatever it does next
// is gone when the checkpoint is restored. So nothing new may start on it, a
// caller can see why, and it keeps its slot until the pause is done.
func TestPausingSandboxRefusesWork(t *testing.T) {
	h := newHarness(t, api.PlacementPolicy{})
	token, pin, sb, _, finish := holdingPool(t, h, nil, false)
	pausing := h.pause(token, sb.ID)

	if got := h.ran(token, sb.ID); got != 409 {
		t.Fatalf("exec during a pause: got %d, want 409", got)
	}
	if got := h.wrote(token, sb.ID); got != 409 {
		t.Fatalf("a file write during a pause: got %d, want 409", got)
	}
	if got := h.call("POST", "/v1/sandboxes/"+sb.ID+"/keepalive", token, nil, nil); got != 409 {
		t.Fatalf("keepalive on a caller's pause: got %d, want 409", got)
	}
	var again api.Sandbox
	h.must(202, "POST", "/v1/sandboxes/"+sb.ID+"/pause", token, nil, &again)
	if again.Status != api.StatusPausing {
		t.Fatalf("a repeated pause should report the one under way: %s", again.Status)
	}
	next := h.create(token, api.CreateSandboxRequest{Profile: "small", Placement: pin})
	h.tick()
	if got := h.get(token, next.ID); got.Status != api.StatusQueued {
		t.Fatalf("a pausing sandbox still holds its slot: %s", got.Status)
	}

	finish()
	if got := <-pausing; got != 200 {
		t.Fatalf("pause answered %d", got)
	}
	got := h.get(token, sb.ID)
	if got.Status != api.StatusPaused || !got.SlotReleased || got.PausedAt == nil {
		t.Fatalf("the pause should finish as usual: %+v", got)
	}
	h.tick()
	if got := h.get(token, next.ID); got.Status != api.StatusRunning {
		t.Fatalf("the freed slot should go to the waiting create: %s", got.Status)
	}
}

// Someone coming back during a pause wants the sandbox they left. It is still
// up, so the resume calls the pause off instead of waiting minutes to restore
// a copy of it, and the run clock carries on.
func TestResumeCallsOffAPause(t *testing.T) {
	h := newHarness(t, api.PlacementPolicy{})
	token, _, sb, _, _ := holdingPool(t, h, nil, true)
	time.Sleep(20 * time.Millisecond)
	timed := pauseCount(t, "ckpt")
	pausing := h.pause(token, sb.ID)

	var back api.Sandbox
	h.must(200, "POST", "/v1/sandboxes/"+sb.ID+"/resume", token, nil, &back)
	if back.Status != api.StatusRunning || back.PausedAt != nil || back.SlotReleased {
		t.Fatalf("resume should call the pause off: %+v", back)
	}
	if rec, err := h.store.GetSandbox(context.Background(), sb.ID); err != nil || rec.RunSeconds == 0 {
		t.Fatalf("the run clock should carry on across a called-off pause: %v %v", rec.RunSeconds, err)
	}
	if got := <-pausing; got != 409 {
		t.Fatalf("the pause's caller should hear it was called off: %d", got)
	}
	if got := pauseCount(t, "ckpt"); got != timed {
		t.Fatalf("a called-off pause should not count as a pause's length: %d before, %d after", timed, got)
	}
	if got := h.ran(token, sb.ID); got != 200 {
		t.Fatalf("exec after a called-off pause: %d", got)
	}
}

// Where the backend cannot call a pause off, a resume joins the queue and runs
// once the pause has finished, without holding a request open meanwhile.
func TestResumeWaitsForAPauseItCannotCallOff(t *testing.T) {
	h := newHarness(t, api.PlacementPolicy{})
	token, _, sb, _, finish := holdingPool(t, h, nil, false)
	pausing := h.pause(token, sb.ID)

	var queued api.Sandbox
	h.must(202, "POST", "/v1/sandboxes/"+sb.ID+"/resume", token, nil, &queued)
	if queued.Status != api.StatusPausing || queued.ResumeRequestedAt == nil || !strings.Contains(queued.StatusReason, "pause finishes") {
		t.Fatalf("resume should wait for the pause: %+v", queued)
	}
	finish()
	<-pausing
	h.tick()
	if got := h.get(token, sb.ID); got.Status != api.StatusRunning || got.ResumeRequestedAt != nil {
		t.Fatalf("the queue should resume it after the pause: %+v", got)
	}
}

func TestFailedPauseGoesBackToRunning(t *testing.T) {
	h := newHarness(t, api.PlacementPolicy{})
	token, pin, a := checkpointBackend(t, h, nil)
	a.FailPauses = 1
	sb := h.create(token, api.CreateSandboxRequest{Profile: "small", Placement: pin})
	h.tick()
	timed := pauseCount(t, "ckpt")
	if got := h.call("POST", "/v1/sandboxes/"+sb.ID+"/pause", token, nil, nil); got < 500 {
		t.Fatalf("a failed pause should answer an error: %d", got)
	}
	if got := pauseCount(t, "ckpt"); got != timed {
		t.Fatalf("a failed pause should not count as a pause's length: %d before, %d after", timed, got)
	}
	if got := h.get(token, sb.ID); got.Status != api.StatusRunning {
		t.Fatalf("a failed pause leaves the sandbox running: %s", got.Status)
	}
	if got := h.ran(token, sb.ID); got != 200 {
		t.Fatalf("exec after a failed pause: %d", got)
	}
}

// Deleting a sandbox mid-pause stops the wait for it.
func TestDeleteDuringAPause(t *testing.T) {
	h := newHarness(t, api.PlacementPolicy{})
	token, _, sb, _, _ := holdingPool(t, h, nil, false)
	pausing := h.pause(token, sb.ID)
	h.must(202, "DELETE", "/v1/sandboxes/"+sb.ID, token, nil, nil)
	if got := <-pausing; got != 409 {
		t.Fatalf("the pause's caller should hear the sandbox changed: %d", got)
	}
	h.tick()
	if got := h.get(token, sb.ID); got.Status != api.StatusTerminated {
		t.Fatalf("deleted: %s", got.Status)
	}
}

// backdate makes a sandbox look unused for d, like a computer resumed that long
// ago and left alone.
func (h *harness) backdate(id string, d time.Duration) {
	h.t.Helper()
	at := time.Now().UTC().Add(-d)
	if _, err := h.store.UpdateSandbox(context.Background(), id, func(r *store.SandboxRecord) error {
		r.LastActivityAt = &at
		return nil
	}); err != nil {
		h.t.Fatal(err)
	}
}

// A sandbox in use is not idle, whether or not it has an idle timeout of its
// own: pausing it to make room would lose the work under way.
func TestPreemptionSparesASandboxInUse(t *testing.T) {
	h := newHarness(t, api.PlacementPolicy{})
	token, pin := checkpointPool(t, h, &api.Preemption{Mode: api.PreemptIdle, IdleSeconds: 60})
	busy := h.create(token, api.CreateSandboxRequest{Profile: "small", Placement: pin})
	h.tick()
	h.backdate(busy.ID, 2*time.Minute)
	if got := h.ran(token, busy.ID); got != 200 {
		t.Fatalf("exec: %d", got)
	}
	h.create(token, api.CreateSandboxRequest{Profile: "small", Placement: pin})
	h.tick()
	if got := h.get(token, busy.ID); got.Status != api.StatusRunning {
		t.Fatalf("a sandbox that just ran a command is not idle: %s", got.Status)
	}
	h.backdate(busy.ID, 2*time.Minute)
	h.tick()
	if got := h.get(token, busy.ID); got.Status != api.StatusPaused {
		t.Fatalf("once idle it makes room: %s", got.Status)
	}
}

// A command that outlasts the idle window is activity for as long as it runs.
func TestRunningCommandCountsAsActivity(t *testing.T) {
	h := newHarness(t, api.PlacementPolicy{})
	h.api.ActivityBeat = 20 * time.Millisecond
	token, pin, a := checkpointBackend(t, h, &api.Preemption{Mode: api.PreemptIdle, IdleSeconds: 60})
	busy := h.create(token, api.CreateSandboxRequest{Profile: "small", Placement: pin})
	h.tick()
	h.backdate(busy.ID, 2*time.Minute)
	hold := make(chan struct{})
	a.HoldExecs = hold
	running := make(chan int, 1)
	go func() { running <- h.ran(token, busy.ID) }()
	defer func() {
		close(hold)
		if got := <-running; got != 200 {
			t.Errorf("exec: %d", got)
		}
	}()
	h.backdate(busy.ID, 2*time.Minute) // after the exec's own start counted
	deadline := time.Now().Add(5 * time.Second)
	for got := h.get(token, busy.ID); time.Since(*got.LastActivityAt) > time.Minute; got = h.get(token, busy.ID) {
		if time.Now().After(deadline) {
			t.Fatal("a running command should keep counting as activity")
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.create(token, api.CreateSandboxRequest{Profile: "small", Placement: pin})
	h.tick()
	if got := h.get(token, busy.ID); got.Status != api.StatusRunning {
		t.Fatalf("a sandbox in the middle of a command is not idle: %s", got.Status)
	}
}

// A sandbox being paused to make room keeps the idle contract: the next call
// brings it back, and here that means it never stopped.
func TestCallWakesASandboxBeingPreempted(t *testing.T) {
	h := newHarness(t, api.PlacementPolicy{})
	token, pin, sb, _, _ := holdingPool(t, h, &api.Preemption{Mode: api.PreemptLRU, MinRunSeconds: 60}, true)
	later := time.Now().UTC().Add(5 * time.Minute)
	h.sched.Now = func() time.Time { return later }
	h.create(token, api.CreateSandboxRequest{Profile: "small", Placement: pin})
	ctx := context.Background()
	h.sched.Tick(ctx)
	h.sched.Tick(ctx)
	parked := h.waitFor(token, sb.ID, api.StatusPausing)
	if parked.IdledAt == nil || !strings.Contains(parked.StatusReason, "make room") {
		t.Fatalf("a preempted sandbox should say why: %+v", parked)
	}
	if got := h.ran(token, sb.ID); got != 200 {
		t.Fatalf("a call should wake a sandbox being preempted: %d", got)
	}
	if got := h.get(token, sb.ID); got.Status != api.StatusRunning || got.IdledAt != nil {
		t.Fatalf("called off: %+v", got)
	}
}

func TestIdleSweepShowsItsPause(t *testing.T) {
	h := newHarness(t, api.PlacementPolicy{})
	_, _, token := h.tenant("a", api.Quota{})
	hold := make(chan struct{})
	h.fc.HoldPauses = hold
	sb := h.create(token, api.CreateSandboxRequest{IdleTimeoutSeconds: 60})
	h.tick()
	h.sched.Now = func() time.Time { return time.Now().UTC().Add(2 * time.Minute) }
	swept := make(chan struct{})
	go func() {
		defer close(swept)
		h.sched.SweepIdle(context.Background())
	}()
	if got := h.waitFor(token, sb.ID, api.StatusPausing); got.IdledAt == nil {
		t.Fatalf("an idle pause should be marked idled from the start: %+v", got)
	}
	close(hold)
	<-swept
	if got := h.get(token, sb.ID); got.Status != api.StatusPaused || got.IdledAt == nil {
		t.Fatalf("idle pause: %+v", got)
	}
}

// The replica that asked for a pause can stop while the backend goes on: the
// reconciler settles what it left behind.
func TestReconcilerSettlesAPauseWithNoWaiter(t *testing.T) {
	h := newHarness(t, api.PlacementPolicy{})
	token, pin, a := checkpointBackend(t, h, nil)
	ctx := context.Background()
	orphan := func() api.Sandbox {
		sb := h.create(token, api.CreateSandboxRequest{Profile: "small", Placement: pin})
		h.tick()
		if _, err := h.store.UpdateSandbox(ctx, sb.ID, func(r *store.SandboxRecord) error {
			r.Status = api.StatusPausing
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return sb
	}

	finished := orphan()
	if err := a.Pause(ctx, a.RefOf(finished.ID)); err != nil {
		t.Fatal(err)
	}
	h.sched.Reconcile(ctx)
	if got := h.get(token, finished.ID); got.Status != api.StatusPaused || !got.SlotReleased {
		t.Fatalf("a finished pause should be followed: %+v", got)
	}
	if _, err := h.store.UpdateSandbox(ctx, finished.ID, func(r *store.SandboxRecord) error {
		r.Status = api.StatusTerminated
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	stuck := orphan()
	h.sched.Reconcile(ctx)
	if got := h.get(token, stuck.ID); got.Status != api.StatusPausing {
		t.Fatalf("a retrying pause reads as running between attempts: %s", got.Status)
	}
	h.sched.Now = func() time.Time { return time.Now().UTC().Add(time.Hour) }
	h.sched.Reconcile(ctx)
	if got := h.get(token, stuck.ID); got.Status != api.StatusRunning {
		t.Fatalf("a pause nobody is waiting for that never happened: %s", got.Status)
	}
}
