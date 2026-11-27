package sqlite

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/internal/store/storetest"
	"github.com/different-ai/differently/pkg/api"
)

func open(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(context.Background()); err != nil {
		s.Close()
		t.Fatal(err)
	}
	return s
}

func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store {
		s := open(t, filepath.Join(t.TempDir(), "differently.db"))
		t.Cleanup(func() { s.Close() })
		return s
	})
}

func TestRestartKeepsEverythingAndTheAuditChainGoesOn(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "differently.db")
	s := open(t, path)
	inst, err := s.Installation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOrg(ctx, api.Organization{ID: "org_1", Name: "one"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject(ctx, api.Project{ID: "prj_1", OrgID: "org_1", Name: "p"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := s.AppendAudit(ctx, api.AuditEvent{Action: "test", Resource: "r"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = open(t, path) // migrating again changes nothing
	defer s.Close()
	if again, _ := s.Installation(ctx); again != inst {
		t.Errorf("installation changed from %s to %s", inst, again)
	}
	if o, err := s.GetOrg(ctx, "org_1"); err != nil || o.Name != "one" {
		t.Errorf("organization after restart: %+v, %v", o, err)
	}
	if _, err := s.GetProject(ctx, "prj_1"); err != nil {
		t.Errorf("project after restart: %v", err)
	}
	ev, err := s.AppendAudit(ctx, api.AuditEvent{Action: "next"})
	if err != nil || ev.Seq != 4 {
		t.Fatalf("next audit event: seq %d, err %v", ev.Seq, err)
	}
	events, _ := s.ListAudit(ctx, store.AuditFilter{})
	for i, e := range events {
		if e.Seq != int64(i+1) || (i > 0 && e.PrevHash != events[i-1].Hash) {
			t.Fatalf("audit chain broken at position %d: seq %d", i, e.Seq)
		}
	}
	// The files are the owner's alone: they hold key hashes and sealed
	// credentials.
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		fi, _ := e.Info()
		if got := fi.Mode().Perm(); got&0o077 != 0 {
			t.Errorf("%s has mode %o; others can read it", e.Name(), got)
		}
	}
}

func TestOneServerPerFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "differently.db")
	s := open(t, path)
	if second, err := Open(context.Background(), path); err == nil {
		second.Close()
		t.Fatal("a second store opened a file that is in use")
	} else if !strings.Contains(err.Error(), "in use by another differently-server") {
		t.Errorf("error = %v", err)
	}
	s.Close()
	open(t, path).Close()
}

func TestDuplicatesAreConflictsAndOtherErrorsAreNot(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "differently.db"))
	defer s.Close()
	if err := s.CreateOrg(ctx, api.Organization{ID: "org_1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateOrg(ctx, api.Organization{ID: "org_1"}); !api.IsCode(err, api.CodeConflict) {
		t.Errorf("a second organization with the same ID: %v", err)
	}
	// A project in an organization that is not there breaks a foreign key,
	// which is not a duplicate and must not be reported as one.
	if err := s.CreateProject(ctx, api.Project{ID: "prj_x", OrgID: "org_missing"}); err == nil || api.IsCode(err, api.CodeConflict) {
		t.Errorf("a project in a missing organization: %v", err)
	}
}

func TestDriverIsRegisteredAndPlacesItsFile(t *testing.T) {
	d, ok := store.Lookup("sqlite")
	if !ok || !d.Durable || d.Shared {
		t.Fatalf("driver = %+v, registered %v", d, ok)
	}
	if _, err := store.Open(context.Background(), "sqlite", store.Options{}); err == nil {
		t.Error("opened with no data directory and no file")
	}
	dir := t.TempDir()
	s, err := store.Open(context.Background(), "sqlite", store.Options{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.(store.Migrator); !ok {
		t.Error("the store does not migrate")
	}
	s.Close()
	if _, err := os.Stat(filepath.Join(dir, "sqlite", "differently.db")); err != nil {
		t.Errorf("the database is not at <dataDir>/sqlite/differently.db: %v", err)
	}
	// store.dsnEnv can name the file instead.
	named := filepath.Join(t.TempDir(), "elsewhere", "state.db")
	s, err = store.Open(context.Background(), "sqlite", store.Options{DataDir: dir, DSN: named})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := os.Stat(named); err != nil {
		t.Errorf("the database is not at the named path: %v", err)
	}
}

// A server killed outright, with no chance to close anything, has on disk
// every write it reported as done.
func TestKilledProcessKeepsEveryAcknowledgedWrite(t *testing.T) {
	if path := os.Getenv("DIFFERENTLY_SQLITE_STORE_CHILD"); path != "" {
		// The child: write, report each write once it returned, never stop.
		s, err := Open(context.Background(), path)
		if err == nil {
			err = s.Migrate(context.Background())
		}
		if err != nil {
			fmt.Println("ERR", err)
			os.Exit(1)
		}
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

	path := filepath.Join(t.TempDir(), "differently.db")
	cmd := exec.Command(os.Args[0], "-test.run=^TestKilledProcessKeepsEveryAcknowledgedWrite$")
	cmd.Env = append(os.Environ(), "DIFFERENTLY_SQLITE_STORE_CHILD="+path)
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

	s := open(t, path) // the lock died with the process
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
		if e.Seq != int64(i+1) || (i > 0 && e.PrevHash != events[i-1].Hash) {
			t.Fatalf("audit chain broken at position %d: seq %d", i, e.Seq)
		}
	}
	if _, err := s.AppendAudit(ctx, api.AuditEvent{Action: "after"}); err != nil {
		t.Errorf("the store does not go on after the kill: %v", err)
	}
}
