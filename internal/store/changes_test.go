package store

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"testing"

	"github.com/different-ai/differently/pkg/api"
)

// A driver that keeps the memory store's state relies on every write being
// recorded. A direct write to a table would work in memory and be missing
// after a restart, so the source is held to put, drop and note.
func TestMemoryWritesGoThroughPutAndDrop(t *testing.T) {
	src, err := os.ReadFile("memory.go")
	if err != nil {
		t.Fatal(err)
	}
	tables := `(orgs|projects|keys|pools|secrets|stacks|templates|policies|sandboxes|snapshots)`
	for name, re := range map[string]*regexp.Regexp{
		"assigns to a table":   regexp.MustCompile(`m\.` + tables + `\[[^\]]*\]\s*=[^=]`),
		"deletes from a table": regexp.MustCompile(`delete\(m\.` + tables),
	} {
		if loc := re.Find(src); loc != nil {
			t.Errorf("memory.go %s directly (%s); use put or drop so the change is recorded", name, loc)
		}
	}
	if n := len(regexp.MustCompile(`m\.audit = append`).FindAll(src, -1)); n != 1 {
		t.Errorf("memory.go appends to the audit chain in %d places; the one place must also call note", n)
	}
	if n := len(regexp.MustCompile(`m\.installation = `).FindAll(src, -1)); n != 1 {
		t.Errorf("memory.go sets the installation in %d places; the one place must also call note", n)
	}
}

func TestChangesRebuildTheSameState(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	m.TrackChanges()
	inst, _ := m.Installation(ctx)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(m.CreateOrg(ctx, api.Organization{ID: "org_1", Name: "one"}))
	must(m.CreateProject(ctx, api.Project{ID: "prj_1", OrgID: "org_1", Name: "p"}))
	must(m.CreateProject(ctx, api.Project{ID: "prj_2", OrgID: "org_1", Name: "gone"}))
	must(m.DeleteProject(ctx, "prj_2"))
	_, err := m.UpdateOrg(ctx, "org_1", func(o *api.Organization) error { o.Name = "renamed"; return nil })
	must(err)
	// A failed update changes nothing and records nothing.
	before := m.ChangeSeq()
	if _, err := m.UpdateOrg(ctx, "org_missing", func(*api.Organization) error { return nil }); err == nil {
		t.Fatal("updating a missing organization succeeded")
	}
	if m.ChangeSeq() != before {
		t.Error("a failed update recorded a change")
	}
	for i := 0; i < 3; i++ {
		_, err := m.AppendAudit(ctx, api.AuditEvent{Action: "test", Resource: "r"})
		must(err)
	}

	changes, seq := m.TakeChanges()
	if seq != m.ChangeSeq() || len(changes) != int(seq) {
		t.Fatalf("took %d changes, seq %d, ChangeSeq %d", len(changes), seq, m.ChangeSeq())
	}
	if more, _ := m.TakeChanges(); len(more) != 0 {
		t.Fatalf("a second take returned %d changes", len(more))
	}

	// Replaying the changes, and replaying a dump, both give the same state.
	replayed := NewMemory()
	must(replayed.Apply(changes))
	dumped := NewMemory()
	must(dumped.Apply(m.Dump()))
	must(dumped.Apply(m.DumpAudit()))
	want, _ := json.Marshal(append(m.Dump(), m.DumpAudit()...))
	for name, got := range map[string]*Memory{"replayed": replayed, "dumped": dumped} {
		b, _ := json.Marshal(append(got.Dump(), got.DumpAudit()...))
		if string(b) != string(want) {
			t.Errorf("%s state differs:\n got %s\nwant %s", name, b, want)
		}
		if id, _ := got.Installation(ctx); id != inst {
			t.Errorf("%s installation = %q, want %q", name, id, inst)
		}
		if _, err := got.GetProject(ctx, "prj_2"); err == nil {
			t.Errorf("%s still has the deleted project", name)
		}
		// The chain goes on from where it was.
		ev, err := got.AppendAudit(ctx, api.AuditEvent{Action: "next"})
		if err != nil || ev.Seq != 4 {
			t.Errorf("%s next audit event: seq %d, err %v", name, ev.Seq, err)
		}
	}

	// Applying table changes twice changes nothing; an audit event out of
	// place is refused.
	must(replayed.Apply(m.Dump()))
	if err := replayed.Apply(m.DumpAudit()[:1]); err == nil {
		t.Error("an audit event with the wrong sequence was applied")
	}
	// A store that does not track records nothing.
	plain := NewMemory()
	must(plain.CreateOrg(ctx, api.Organization{ID: "org_1"}))
	if c, seq := plain.TakeChanges(); len(c) != 0 || seq != 0 {
		t.Errorf("an untracked store recorded %d changes", len(c))
	}
}
