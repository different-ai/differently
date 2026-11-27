package file

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/internal/store/storetest"
	"github.com/different-ai/differently/pkg/api"
)

// state is everything a store holds, as bytes that are equal when two
// stores hold the same.
func state(s *Store) string {
	b, _ := json.Marshal(append(s.mem.Dump(), s.mem.DumpAudit()...))
	return string(b)
}

func open(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// The conformance suite, with one more check after every one of its tests:
// the directory, opened again, holds exactly what the store held in memory.
// That is what catches a change that was made and never written down.
func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		dir := t.TempDir()
		s := open(t, dir)
		t.Cleanup(func() {
			want := state(s)
			if err := s.Close(); err != nil {
				t.Errorf("close: %v", err)
			}
			again := open(t, dir)
			defer again.Close()
			if got := state(again); got != want {
				t.Errorf("after a restart the store holds something else:\n got %s\nwant %s", got, want)
			}
		})
		return s
	})
}

// The same suite with the snapshot rewritten after every write, so each
// test also runs across many compactions.
func TestConformanceCompactingAfterEveryWrite(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		dir := t.TempDir()
		s := open(t, dir)
		s.compactAt = 1
		t.Cleanup(func() {
			want := state(s)
			s.Close()
			again := open(t, dir)
			defer again.Close()
			if got := state(again); got != want {
				t.Errorf("after a restart the store holds something else:\n got %s\nwant %s", got, want)
			}
		})
		return s
	})
}

func fill(t *testing.T, s store.Store, from, to int) {
	t.Helper()
	ctx := context.Background()
	for i := from; i < to; i++ {
		id := fmt.Sprintf("org_%04d", i)
		if err := s.CreateOrg(ctx, api.Organization{ID: id, Name: id}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AppendAudit(ctx, api.AuditEvent{Action: "org.create", Resource: id}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRestartKeepsEverythingAndTheAuditChainGoesOn(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)
	inst, _ := s.Installation(ctx)
	fill(t, s, 0, 5)
	if err := s.CreateProject(ctx, api.Project{ID: "prj_1", OrgID: "org_0000", Name: "p"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(ctx, "prj_1"); err != nil {
		t.Fatal(err)
	}
	want := state(s)
	s.Close()

	s = open(t, dir)
	defer s.Close()
	if got := state(s); got != want {
		t.Fatalf("state after restart:\n got %s\nwant %s", got, want)
	}
	if again, _ := s.Installation(ctx); again != inst {
		t.Errorf("installation changed from %s to %s", inst, again)
	}
	if _, err := s.GetProject(ctx, "prj_1"); err == nil {
		t.Error("the deleted project came back")
	}
	ev, err := s.AppendAudit(ctx, api.AuditEvent{Action: "next"})
	if err != nil || ev.Seq != 6 {
		t.Errorf("next audit event: seq %d, err %v", ev.Seq, err)
	}
	events, _ := s.ListAudit(ctx, store.AuditFilter{})
	for i, e := range events {
		if e.Seq != int64(i+1) {
			t.Fatalf("audit sequence %d at position %d", e.Seq, i)
		}
		if i > 0 && e.PrevHash != events[i-1].Hash {
			t.Fatalf("audit event %d does not follow event %d", e.Seq, events[i-1].Seq)
		}
	}
	// Files and the directory are the owner's alone: they hold key hashes
	// and sealed credentials.
	for name, want := range map[string]os.FileMode{"": 0o700, snapshotName: 0o600, journalName: 0o600, auditName: 0o600} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
		} else if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s has mode %o, want %o", filepath.Join(dir, name), got, want)
		}
	}
}

// A write cut short by a crash leaves a last line with no newline. It was
// never reported as done, so it is dropped and the rest is kept.
func TestAnUnfinishedLastWriteIsCutOff(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	fill(t, s, 0, 3)
	want := state(s)
	s.Close()
	for _, name := range []string{journalName, auditName} {
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(`{"c":[{"t":"org","id":"org_torn","v":{"id":"org_to`)
		f.Close()
	}
	s = open(t, dir)
	if got := state(s); got != want {
		t.Errorf("state after an unfinished write:\n got %s\nwant %s", got, want)
	}
	// And the store goes on from there.
	fill(t, s, 3, 4)
	want = state(s)
	s.Close()
	s = open(t, dir)
	defer s.Close()
	if got := state(s); got != want {
		t.Errorf("state after going on:\n got %s\nwant %s", got, want)
	}
}

// A damaged file is refused. Starting with part of what was kept, or with
// nothing, would look like working and be data loss.
func TestDamagedFilesAreRefused(t *testing.T) {
	for _, tc := range []struct{ name, file, damage, want string }{
		{"a journal line that is not JSON", journalName, "this is not json\n", "journal.jsonl line"},
		{"a journal line for an unknown table", journalName, `{"c":[{"t":"nope","id":"x","v":{}}]}` + "\n", "journal.jsonl line"},
		{"an audit event out of sequence", auditName, `{"seq":99,"action":"x"}` + "\n", "audit.jsonl line"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s := open(t, dir)
			s.compactAt = 1 << 40 // keep the journal
			fill(t, s, 0, 2)
			// Close without the compaction a clean start would do next time.
			s.Close()
			f, err := os.OpenFile(filepath.Join(dir, tc.file), os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			f.WriteString(tc.damage)
			f.Close()
			got, err := Open(dir)
			if err == nil {
				got.Close()
				t.Fatal("a damaged file was opened")
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "damaged") {
				t.Errorf("error = %v", err)
			}
		})
	}
	t.Run("a snapshot that is not JSON", func(t *testing.T) {
		dir := t.TempDir()
		s := open(t, dir)
		fill(t, s, 0, 2)
		s.Close()
		open(t, dir).Close() // a start compacts into the snapshot
		if err := os.WriteFile(filepath.Join(dir, snapshotName), []byte("{half"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got, err := Open(dir); err == nil {
			got.Close()
			t.Fatal("a damaged snapshot was opened")
		} else if !strings.Contains(err.Error(), "snapshot.json cannot be read") {
			t.Errorf("error = %v", err)
		}
	})
	t.Run("a snapshot from another version", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, snapshotName), []byte(`{"version":2,"changes":[]}`), 0o600)
		if got, err := Open(dir); err == nil {
			got.Close()
			t.Fatal("a snapshot of another version was opened")
		} else if !strings.Contains(err.Error(), "version 2") {
			t.Errorf("error = %v", err)
		}
	})
}

// If the server stops after the new snapshot is in place and before the
// journal is emptied, the old journal is replayed over the new snapshot. The
// result must be the same state.
func TestStoppingBetweenSnapshotAndEmptyingTheJournal(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)
	s.compactAt = 1 << 40
	fill(t, s, 0, 4)
	if _, err := s.UpdateOrg(ctx, "org_0001", func(o *api.Organization) error { o.Name = "renamed"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject(ctx, api.Project{ID: "prj_1", OrgID: "org_0000"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(ctx, "prj_1"); err != nil {
		t.Fatal(err)
	}
	journal, err := os.ReadFile(filepath.Join(dir, journalName))
	if err != nil || len(journal) == 0 {
		t.Fatalf("journal: %d bytes, %v", len(journal), err)
	}
	s.mu.Lock()
	err = s.compact()
	s.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	want := state(s)
	s.Close()
	// Put the journal back as it was before it was emptied.
	if err := os.WriteFile(filepath.Join(dir, journalName), journal, 0o600); err != nil {
		t.Fatal(err)
	}
	s = open(t, dir)
	defer s.Close()
	if got := state(s); got != want {
		t.Errorf("state:\n got %s\nwant %s", got, want)
	}
}

func TestCompactionEmptiesTheJournalAndLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	s.compactAt = 2000
	fill(t, s, 0, 200)
	want := state(s)
	fi, err := os.Stat(filepath.Join(dir, journalName))
	if err != nil || fi.Size() >= 4000 {
		t.Errorf("journal is %d bytes after 200 writes with compaction at 2000 (%v)", fi.Size(), err)
	}
	if _, err := os.Stat(filepath.Join(dir, snapshotName+".tmp")); !os.IsNotExist(err) {
		t.Errorf("a temporary snapshot was left behind: %v", err)
	}
	s.Close()
	// A snapshot left half-written by a crash is ignored and removed.
	os.WriteFile(filepath.Join(dir, snapshotName+".tmp"), []byte("{half"), 0o600)
	s = open(t, dir)
	defer s.Close()
	if got := state(s); got != want {
		t.Error("state changed across compaction and restart")
	}
	if _, err := os.Stat(filepath.Join(dir, snapshotName+".tmp")); !os.IsNotExist(err) {
		t.Error("the half-written snapshot was not removed")
	}
}

func TestOneServerPerDirectory(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if second, err := Open(dir); err == nil {
		second.Close()
		t.Fatal("a second store opened a directory that is in use")
	} else if !strings.Contains(err.Error(), "in use by another differently-server") {
		t.Errorf("error = %v", err)
	}
	s.Close()
	open(t, dir).Close()
}

// When a write to disk fails, the call fails and the store stops answering:
// memory is ahead of the disk, and anything it said would be gone after a
// restart.
func TestAFailedWriteStopsTheStore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)
	fill(t, s, 0, 2)
	want := state(s)
	s.journal.Close() // the disk goes away

	err := s.CreateOrg(ctx, api.Organization{ID: "org_lost", Name: "lost"})
	if err == nil || !strings.Contains(err.Error(), "has stopped answering") {
		t.Fatalf("a write that could not be kept returned %v", err)
	}
	if _, err := s.GetOrg(ctx, "org_lost"); err == nil || !strings.Contains(err.Error(), "has stopped answering") {
		t.Errorf("a read after the failure returned %v", err)
	}
	if err := s.CreateOrg(ctx, api.Organization{ID: "org_after"}); err == nil {
		t.Error("a write after the failure succeeded")
	}
	if ok, err := s.TryLeader(ctx); ok || err == nil {
		t.Errorf("TryLeader after the failure = %v, %v", ok, err)
	}
	s.Close()
	s = open(t, dir)
	defer s.Close()
	if got := state(s); got != want {
		t.Errorf("after a restart:\n got %s\nwant %s", got, want)
	}
}

// Writers at once: every call that returned without error is on disk, and
// writes share syncs without one being reported before its sync.
func TestConcurrentWritersAllKept(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := open(t, dir)
	s.compactAt = 4000
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				id := fmt.Sprintf("org_%d_%03d", w, i)
				if err := s.CreateOrg(ctx, api.Organization{ID: id, Name: id}); err != nil {
					t.Error(err)
					return
				}
				if _, err := s.AppendAudit(ctx, api.AuditEvent{Action: "org.create", Resource: id}); err != nil {
					t.Error(err)
					return
				}
				if _, err := s.GetOrg(ctx, id); err != nil {
					t.Error(err)
				}
			}
		}(w)
	}
	wg.Wait()
	want := state(s)
	s.Close()
	s = open(t, dir)
	defer s.Close()
	if got := state(s); got != want {
		t.Error("state differs after a restart")
	}
	orgs, _ := s.ListOrgs(ctx)
	events, _ := s.ListAudit(ctx, store.AuditFilter{})
	if len(orgs) != 320 || len(events) != 320 {
		t.Errorf("kept %d organizations and %d audit events, want 320 of each", len(orgs), len(events))
	}
}

// A server killed outright, with no chance to close anything, has on disk
// every write it reported as done.
func TestKilledProcessKeepsEveryAcknowledgedWrite(t *testing.T) {
	if dir := os.Getenv("DIFFERENTLY_FILE_STORE_CHILD"); dir != "" {
		// The child: write, report each write once it returned, never stop.
		s, err := Open(dir)
		if err != nil {
			fmt.Println("ERR", err)
			os.Exit(1)
		}
		s.compactAt = 3000
		ctx := context.Background()
		for i := 0; ; i++ {
			id := fmt.Sprintf("org_%06d", i)
			if err := s.CreateOrg(ctx, api.Organization{ID: id, Name: id}); err != nil {
				fmt.Println("ERR", err)
				os.Exit(1)
			}
			if _, err := s.AppendAudit(ctx, api.AuditEvent{Action: "org.create", Resource: id}); err != nil {
				fmt.Println("ERR", err)
				os.Exit(1)
			}
			fmt.Println("ACK", id)
		}
	}

	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestKilledProcessKeepsEveryAcknowledgedWrite$")
	cmd.Env = append(os.Environ(), "DIFFERENTLY_FILE_STORE_CHILD="+dir)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var acked []string
	lines := bufio.NewScanner(out)
	deadline := time.Now().Add(20 * time.Second)
	for len(acked) < 60 && time.Now().Before(deadline) && lines.Scan() {
		line := lines.Text()
		if strings.HasPrefix(line, "ERR") {
			t.Fatalf("child: %s", line)
		}
		if id, ok := strings.CutPrefix(line, "ACK "); ok {
			acked = append(acked, id)
		}
	}
	cmd.Process.Kill() // SIGKILL: no deferred calls, no Close
	cmd.Wait()
	if len(acked) < 60 {
		t.Fatalf("the child reported only %d writes", len(acked))
	}

	s := open(t, dir)
	defer s.Close()
	ctx := context.Background()
	for _, id := range acked {
		if _, err := s.GetOrg(ctx, id); err != nil {
			t.Fatalf("%s was reported as written and is gone after the kill: %v", id, err)
		}
	}
	events, _ := s.ListAudit(ctx, store.AuditFilter{})
	if len(events) < len(acked) {
		t.Errorf("%d audit events kept for %d reported writes", len(events), len(acked))
	}
	for i, e := range events {
		if e.Seq != int64(i+1) {
			t.Fatalf("audit sequence %d at position %d", e.Seq, i)
		}
	}
	// The lock died with the process, and the store goes on.
	fill(t, s, 900000, 900001)
}

func TestDriverIsRegistered(t *testing.T) {
	d, ok := store.Lookup("file")
	if !ok || !d.Durable || d.Shared {
		t.Fatalf("driver = %+v, registered %v", d, ok)
	}
	if _, err := store.Open(context.Background(), "file", store.Options{}); err == nil {
		t.Error("opened with no data directory")
	}
	dir := t.TempDir()
	s, err := store.Open(context.Background(), "file", store.Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := os.Stat(filepath.Join(dir, "store", lockName)); err != nil {
		t.Errorf("the store is not under <dataDir>/store: %v", err)
	}
}
