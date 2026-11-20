package acid

import (
	"slices"
	"strings"
	"time"

	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// Level 0: does the backend behave like an API a scheduler can rely on?
// The conformance checklist in docs/adapters/specification.md §11, made
// executable.
func level0() []Test {
	return []Test{
		{ID: "L0.01", Level: 0, Title: "Health", Run: health},
		{ID: "L0.02", Level: 0, Title: "Create and get", Run: createAndGet},
		{ID: "L0.03", Level: 0, Title: "Idempotent create", Run: idempotentCreate},
		{ID: "L0.04", Level: 0, Title: "List ownership", Run: listOwnership},
		{ID: "L0.05", Level: 0, Title: "Idempotent delete", Run: idempotentDelete},
		{ID: "L0.06", Level: 0, Title: "Error classes", Run: errorClasses},
		{ID: "L0.07", Level: 0, Title: "Honest features", Run: honestFeatures},
	}
}

func health(t *T) {
	a := t.Adapter()
	if p := a.Info().Protocol; p != adapter.ProtocolVersion {
		t.Fail("Info reports protocol %q, want %q", p, adapter.ProtocolVersion)
	}
	if t.r.capsErr != nil {
		t.Fail("Capabilities: %s", describe(t.r.capsErr))
	}
	start := time.Now()
	if err := a.Health(t.Ctx()); err != nil {
		t.Fail("Health with valid credentials: %s", describe(err))
	}
	t.Measure("healthSeconds", time.Since(start).Seconds())
	bad := t.Options().BadCredentials
	if bad == nil {
		t.Note("no credential to get wrong: bad-credential check not run")
		return
	}
	b, secret, err := bad()
	if err != nil {
		t.Fail("could not build the adapter with a wrong credential: %v", err)
	}
	err = b.Health(t.Ctx())
	switch {
	case err == nil:
		t.Fail("Health succeeded with a wrong credential")
	case secret != "" && strings.Contains(err.Error(), secret):
		t.Fail("the error for a wrong credential contains the credential")
	case codeOf(err) != api.CodeProviderUnavailable:
		t.Fail("a wrong credential answers %s, want provider_unavailable", describe(err))
	}
	t.Note("a wrong credential is refused as provider_unavailable")
}

func createAndGet(t *T) {
	a := t.Adapter()
	req := t.Request(t.Name("acid"))
	start := time.Now()
	inst := t.Create(req)
	t.Measure("createSeconds", time.Since(start).Seconds())
	if inst.Ref == "" {
		t.Fail("Create returned no ref")
	}
	got, err := a.Get(t.Ctx(), inst.Ref)
	if err != nil {
		t.Fail("Get %s: %s", inst.Ref, describe(err))
	}
	if got.State != adapter.StateRunning {
		t.Fail("Get reports %q after the instance ran", got.State)
	}
	// Time to a first command is what users feel; it is measured here but
	// only level 1 judges exec.
	if res, err := a.Exec(t.Ctx(), inst.Ref, api.ExecRequest{Command: "true", TimeoutSeconds: 60}); err == nil && res.ExitCode == 0 {
		t.Measure("firstExecSeconds", time.Since(start).Seconds())
	}
	t.Note("running in %.2fs", time.Since(start).Seconds())
}

func idempotentCreate(t *T) {
	a := t.Adapter()
	req := t.Request(t.Name("acid"))
	first := t.Create(req)
	second, err := a.Create(t.Ctx(), req)
	if err != nil {
		t.Fail("second create of the same ID: %s", describe(err))
	}
	t.Owned(second.Ref, req.Installation)
	if second.Ref != first.Ref {
		t.Fail("creating %s twice made two instances (%s and %s)", req.ID, first.Ref, second.Ref)
	}
	list, err := a.List(t.Ctx(), req.Installation)
	if err != nil {
		t.Fail("List: %s", describe(err))
	}
	n := 0
	for _, i := range list {
		if i.SandboxID == req.ID || i.Ref == first.Ref {
			n++
		}
	}
	if n != 1 {
		t.Fail("List shows %d instances for one ID", n)
	}
	t.Note("same ref twice, one instance listed")
}

func listOwnership(t *T) {
	a := t.Adapter()
	mine := map[string]string{} // ref -> sandbox ID
	for range 2 {
		req := t.Request(t.Name("acid"))
		inst := t.Create(req)
		mine[inst.Ref] = req.ID
	}
	otherReq := t.Request(t.Name("acid"))
	otherReq.Installation = otherInstallation(otherReq.Installation)
	other := t.Create(otherReq)

	list, err := a.List(t.Ctx(), t.Options().Installation)
	if err != nil {
		t.Fail("List: %s", describe(err))
	}
	known := t.r.tracked()
	found := 0
	for _, i := range list {
		if i.Ref == other.Ref || i.SandboxID == otherReq.ID {
			t.Fail("List for one installation returned another installation's instance %s", i.Ref)
		}
		if _, ok := known[i.Ref]; !ok {
			t.Fail("List returned %s, which this run did not create (or already deleted)", i.Ref)
		}
		if id, ok := mine[i.Ref]; ok {
			if i.SandboxID != id {
				t.Fail("List reports sandboxId %q for %s, want %q", i.SandboxID, i.Ref, id)
			}
			found++
		}
	}
	if found != len(mine) {
		t.Fail("List returned %d of the %d instances just created", found, len(mine))
	}
	otherList, err := a.List(t.Ctx(), otherReq.Installation)
	if err != nil {
		t.Fail("List other installation: %s", describe(err))
	}
	if !slices.ContainsFunc(otherList, func(i adapter.Instance) bool { return i.Ref == other.Ref }) {
		t.Fail("List for the second installation misses its instance")
	}
	for _, i := range otherList {
		if _, ok := mine[i.Ref]; ok {
			t.Fail("List for the second installation returned the first's instance %s", i.Ref)
		}
	}
	t.Note("each installation lists exactly its own instances, with sandbox IDs")
}

func idempotentDelete(t *T) {
	a := t.Adapter()
	req := t.Request(t.Name("acid"))
	inst := t.Create(req)
	start := time.Now()
	if err := a.Delete(t.Ctx(), inst.Ref); err != nil {
		t.Fail("Delete: %s", describe(err))
	}
	t.Measure("deleteSeconds", time.Since(start).Seconds())
	if err := a.Delete(t.Ctx(), inst.Ref); err != nil {
		t.Fail("second Delete: %s, want success", describe(err))
	}
	// Some backends keep a deleted instance visible for a moment.
	wait := min(60*time.Second, t.Options().ReadyTimeout)
	deadline := time.Now().Add(wait)
	for {
		got, err := a.Get(t.Ctx(), inst.Ref)
		if api.IsCode(err, api.CodeNotFound) {
			break
		}
		if err != nil {
			t.Fail("Get after Delete: %s, want not_found", describe(err))
		}
		if time.Now().After(deadline) {
			t.Fail("Get still finds the instance (%q) %s after Delete", got.State, wait)
		}
		time.Sleep(time.Second)
	}
	list, err := a.List(t.Ctx(), req.Installation)
	if err != nil {
		t.Fail("List: %s", describe(err))
	}
	if slices.ContainsFunc(list, func(i adapter.Instance) bool { return i.Ref == inst.Ref }) {
		t.Fail("List still shows the deleted instance")
	}
	t.Note("second delete succeeds; Get answers not_found")
}

func errorClasses(t *T) {
	a := t.Adapter()
	// A ref that never existed.
	missing := t.Name("acid-missing")
	if _, err := a.Get(t.Ctx(), missing); !api.IsCode(err, api.CodeNotFound) {
		t.Fail("Get of a ref that never existed: %s, want not_found", describeOrNil(err))
	}
	if err := a.Delete(t.Ctx(), missing); err != nil && !api.IsCode(err, api.CodeNotFound) {
		t.Fail("Delete of a ref that never existed: %s, want success", describe(err))
	}

	// A request no backend can serve. The control plane refuses it first
	// when the adapter declares limits; otherwise the backend must, with a
	// class the scheduler can act on (not provider_unavailable, which would
	// mark the whole pool unhealthy).
	huge := api.Resources{CPU: 4096, MemoryMiB: 16 << 20, DiskGiB: 65536}
	if ok, why := t.r.caps.Accepts(huge, ""); !ok {
		t.Note("an impossible size is refused by the declared limits (%s)", why)
	} else {
		req := t.Request(t.Name("acid-huge"))
		req.Resources = huge
		inst, err := a.Create(t.Ctx(), req)
		t.Owned(inst.Ref, req.Installation)
		switch codeOf(err) {
		case api.CodeInvalidRequest, adapter.CodeInsufficientCapacity, adapter.CodeSandboxFailed:
			t.Note("an impossible size answers %s", codeOf(err))
		case "":
			if err == nil {
				t.Fail("Create accepted 4096 CPUs and 16 TiB of memory (got %s)", inst.Ref)
			}
			t.Fail("an impossible size answers %s", describe(err))
		default:
			t.Fail("an impossible size answers %s; want invalid_request, insufficient_capacity or sandbox_failed", describe(err))
		}
		if list, err := a.List(t.Ctx(), req.Installation); err == nil {
			for _, i := range list {
				if i.SandboxID == req.ID {
					t.Owned(i.Ref, req.Installation)
					t.Fail("a refused create left instance %s behind", i.Ref)
				}
			}
		}
	}

	if bad := t.Options().BadCredentials; bad != nil {
		b, secret, err := bad()
		if err != nil {
			t.Fail("could not build the adapter with a wrong credential: %v", err)
		}
		req := t.Request(t.Name("acid-badcred"))
		inst, err := b.Create(t.Ctx(), req)
		t.Owned(inst.Ref, req.Installation)
		switch {
		case err == nil:
			t.Fail("Create succeeded with a wrong credential")
		case secret != "" && strings.Contains(err.Error(), secret):
			t.Fail("the create error for a wrong credential contains the credential")
		case codeOf(err) != api.CodeProviderUnavailable:
			t.Fail("Create with a wrong credential answers %s, want provider_unavailable", describe(err))
		}
		t.Note("a wrong credential's error does not echo it")
	}
}

func describeOrNil(err error) string {
	if err == nil {
		return "success"
	}
	return describe(err)
}

// honestFeatures calls every optional operation the adapter implements but
// does not declare, and expects unsupported_operation from each.
func honestFeatures(t *T) {
	a := t.Adapter()
	for _, f := range t.r.declared {
		if !slices.Contains(t.r.features, f) {
			t.Note("declares %s without implementing it (Differently ignores the claim)", f)
		}
	}
	type probe struct {
		feature string
		call    func(ref string) error
		// own: the call may change the sandbox's state, so it gets one of
		// its own.
		own bool
	}
	var probes []probe
	if s, ok := a.(adapter.Streamer); ok && !t.Has(adapter.FeatureExecStream) {
		probes = append(probes, probe{adapter.FeatureExecStream, func(ref string) error {
			_, err := s.ExecStream(t.Ctx(), ref, api.ExecRequest{Command: "true"}, func(api.ExecEvent) error { return nil })
			return err
		}, false})
	}
	if fs, ok := a.(adapter.FileSystem); ok && !t.Has(adapter.FeatureFiles) {
		probes = append(probes, probe{adapter.FeatureFiles, func(ref string) error {
			_, err := fs.ListFiles(t.Ctx(), ref, "/")
			return err
		}, false})
	}
	if p, ok := a.(adapter.PortExposer); ok && !t.Has(adapter.FeaturePorts) {
		probes = append(probes, probe{adapter.FeaturePorts, func(ref string) error {
			_, err := p.ExposePort(t.Ctx(), ref, 8080)
			return err
		}, false})
	}
	if s, ok := a.(adapter.Snapshotter); ok && !t.Has(adapter.FeatureSnapshotDisk) && !t.Has(adapter.FeatureSnapshotMem) && !t.Has(adapter.FeatureFork) {
		probes = append(probes, probe{adapter.FeatureSnapshotDisk, func(ref string) error {
			snap, err := s.Snapshot(t.Ctx(), ref, "disk")
			if err == nil {
				_ = s.DeleteSnapshot(t.Ctx(), snap.Ref)
			}
			return err
		}, false})
	}
	if p, ok := a.(adapter.Pauser); ok && !t.Has(adapter.FeaturePauseResume) {
		probes = append(probes, probe{adapter.FeaturePauseResume, func(ref string) error { return p.Pause(t.Ctx(), ref) }, true})
	}
	if l, ok := a.(adapter.Lifecycle); ok && !t.Has(adapter.FeatureStopStart) {
		probes = append(probes, probe{adapter.FeatureStopStart, func(ref string) error { return l.Stop(t.Ctx(), ref) }, true})
	}
	if len(probes) == 0 {
		t.Note("every optional operation it implements is declared")
		return
	}
	var shared string
	var honest, quiet []string
	for _, p := range probes {
		ref := shared
		if p.own {
			ref = t.Create(t.Request(t.Name("acid"))).Ref
		} else if ref == "" {
			shared = t.Create(t.Request(t.Name("acid"))).Ref
			ref = shared
		}
		err := p.call(ref)
		switch {
		case api.IsCode(err, api.CodeUnsupportedOperation):
			honest = append(honest, p.feature)
		case err == nil:
			quiet = append(quiet, p.feature)
		default:
			t.Fail("undeclared %s answers %s, want unsupported_operation", p.feature, describe(err))
		}
	}
	if len(honest) > 0 {
		t.Note("undeclared and refused as unsupported_operation: %s", strings.Join(honest, ", "))
	}
	if len(quiet) > 0 {
		t.Note("works although undeclared: %s", strings.Join(quiet, ", "))
	}
}
