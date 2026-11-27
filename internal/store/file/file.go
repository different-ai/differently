// Package file is a store driver that keeps Differently's records in files
// in the server's data directory. It needs no database.
//
// The rules are the memory store's: this package holds a store.Memory and
// repeats none of them. What it adds is durability. After every call it
// takes the changes the memory store recorded and writes them to a journal,
// synced to disk before the call returns; at start it reads them back.
//
//	<dataDir>/store/snapshot.json  every table as of the last compaction
//	<dataDir>/store/journal.jsonl  changes since then, one line per write
//	<dataDir>/store/audit.jsonl    the audit chain, one event per line, only appended
//	<dataDir>/store/lock           held while a server has the directory open
//
// One server uses a directory at a time, and all state is also held in
// memory. For several replicas, use the postgres driver.
package file

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/pkg/api"
)

func init() {
	store.Register("file", store.Driver{
		Summary: "files in the data directory; durable, one replica, no database",
		Durable: true,
		Open: func(_ context.Context, o store.Options) (store.Store, error) {
			if o.DataDir == "" {
				return nil, errors.New("store.driver is file but the server has no data directory")
			}
			return Open(filepath.Join(o.DataDir, "store"))
		},
	})
}

const (
	snapshotName = "snapshot.json"
	journalName  = "journal.jsonl"
	auditName    = "audit.jsonl"
	lockName     = "lock"

	// snapshotVersion is written to the snapshot. A snapshot with another
	// version is refused: this code would not know what it holds.
	snapshotVersion = 1

	// defaultCompactAt is the journal size past which the snapshot is
	// rewritten and the journal emptied.
	defaultCompactAt = 8 << 20
)

var errLocked = errors.New("another process holds the lock")

// Store keeps a memory store's state in a directory.
type Store struct {
	mem *store.Memory
	dir string

	// mu serializes everything written to disk.
	mu        sync.Mutex
	lock      *os.File
	journal   *os.File
	audit     *os.File
	journalSz int64
	compactAt int64
	// durable is the memory store's ChangeSeq that is on disk.
	durable uint64
	closed  bool

	// failed is set when a write to disk fails. Memory is then ahead of the
	// disk, so nothing more is answered: a result that would be gone after a
	// restart is worse than an error.
	failed atomic.Pointer[error]
}

type snapshot struct {
	Version int            `json:"version"`
	Changes []store.Change `json:"changes"`
}

// journalLine is one write: the changes of the store calls it made durable.
type journalLine struct {
	Changes []store.Change `json:"c"`
}

// Open opens the store kept in dir, creating it when dir is empty. It
// refuses a directory another process has open, and files it cannot read in
// full: starting with less than was kept would look like data loss later.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// The files hold key hashes and sealed credentials: the directory is the
	// owner's alone, also when it was there before.
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, lockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(lock); err != nil {
		lock.Close()
		if errors.Is(err, errLocked) {
			return nil, fmt.Errorf("the store in %s is in use by another differently-server; the file driver supports one at a time", dir)
		}
		return nil, fmt.Errorf("lock %s: %w", dir, err)
	}
	s := &Store{mem: store.NewMemory(), dir: dir, lock: lock, compactAt: defaultCompactAt}
	if err := s.load(); err != nil {
		lock.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) path(name string) string { return filepath.Join(s.dir, name) }

func (s *Store) load() error {
	// A snapshot that was being written when the server stopped was never
	// renamed into place; the one before it is still whole.
	_ = os.Remove(s.path(snapshotName + ".tmp"))

	if b, err := os.ReadFile(s.path(snapshotName)); err == nil {
		var snap snapshot
		if err := json.Unmarshal(b, &snap); err != nil {
			return fmt.Errorf("%s cannot be read (%w); restore it from a backup, or move it aside to start from the journal alone", s.path(snapshotName), err)
		}
		if snap.Version != snapshotVersion {
			return fmt.Errorf("%s has version %d and this server reads version %d", s.path(snapshotName), snap.Version, snapshotVersion)
		}
		if err := s.mem.Apply(snap.Changes); err != nil {
			return fmt.Errorf("%s: %w", s.path(snapshotName), err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	var err error
	if s.journalSz, err = replay(s.path(journalName), func(line []byte) error {
		var l journalLine
		if err := json.Unmarshal(line, &l); err != nil {
			return err
		}
		return s.mem.Apply(l.Changes)
	}); err != nil {
		return err
	}
	if _, err = replay(s.path(auditName), func(line []byte) error {
		if !json.Valid(line) {
			return errors.New("not JSON")
		}
		return s.mem.Apply([]store.Change{{Table: store.TableAudit, Value: append(json.RawMessage(nil), line...)}})
	}); err != nil {
		return err
	}

	if s.journal, err = os.OpenFile(s.path(journalName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err != nil {
		return err
	}
	if s.audit, err = os.OpenFile(s.path(auditName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err != nil {
		s.journal.Close()
		return err
	}
	if err := syncDir(s.dir); err != nil {
		s.closeFiles()
		return err
	}
	s.mem.TrackChanges()
	// Start each run from a snapshot: the journal then holds only this run.
	if s.journalSz > 0 {
		if err := s.compact(); err != nil {
			s.closeFiles()
			return err
		}
	}
	return nil
}

// replay feeds each complete line of a log to apply and returns the log's
// length. A last line with no newline is a write that was cut short: it was
// never reported as done, so it is cut off. Any other line that cannot be
// applied means the file was damaged, and that is an error.
func replay(path string, apply func(line []byte) error) (int64, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	var good int64
	for n := 1; ; n++ {
		line, err := r.ReadBytes('\n')
		if err == io.EOF {
			if len(line) == 0 {
				return good, nil
			}
			if err := f.Truncate(good); err != nil {
				return 0, fmt.Errorf("%s: cut off an unfinished write: %w", path, err)
			}
			return good, f.Sync()
		}
		if err != nil {
			return 0, fmt.Errorf("%s: %w", path, err)
		}
		if err := apply(bytes.TrimSuffix(line, []byte("\n"))); err != nil {
			return 0, fmt.Errorf("%s line %d cannot be read (%w); the file is damaged, restore it from a backup", path, n, err)
		}
		good += int64(len(line))
	}
}

// after makes durable whatever the call that just ran wrote. before is the
// memory store's ChangeSeq read before the call.
func (s *Store) after(before uint64) error {
	now := s.mem.ChangeSeq()
	if now == before {
		return nil // nothing was written while the call ran
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.failure(); err != nil {
		return err
	}
	// Another call's write may have carried this call's changes with it.
	// Holding mu means that write, sync included, has finished.
	if s.durable >= now {
		return nil
	}
	changes, seq := s.mem.TakeChanges()
	if err := s.write(changes); err != nil {
		return s.fail(err)
	}
	s.durable = seq
	if s.journalSz >= s.compactAt {
		if err := s.compact(); err != nil {
			return s.fail(err)
		}
	}
	return nil
}

// write appends changes to the journal and the audit log and syncs both.
// The caller holds mu.
func (s *Store) write(changes []store.Change) error {
	var tables []store.Change
	var events bytes.Buffer
	for _, c := range changes {
		if c.Table == store.TableAudit {
			events.Write(c.Value)
			events.WriteByte('\n')
			continue
		}
		tables = append(tables, c)
	}
	if len(tables) > 0 {
		b, err := json.Marshal(journalLine{Changes: tables})
		if err != nil {
			return err
		}
		b = append(b, '\n')
		// One write per line: a line is on disk whole, or its end is missing.
		if _, err := s.journal.Write(b); err != nil {
			return err
		}
		s.journalSz += int64(len(b))
		if err := s.journal.Sync(); err != nil {
			return err
		}
	}
	if events.Len() > 0 {
		if _, err := s.audit.Write(events.Bytes()); err != nil {
			return err
		}
		if err := s.audit.Sync(); err != nil {
			return err
		}
	}
	return nil
}

// compact rewrites the snapshot from the current state and empties the
// journal. The caller holds mu, or is load.
//
// The changes not yet written go to the journal first, taken under the same
// lock as the state, so the journal's end and the snapshot describe the same
// moment. If the server stops after the snapshot is in place and before the
// journal is emptied, replaying that journal over that snapshot changes
// nothing.
func (s *Store) compact() error {
	pending, seq, state := s.mem.TakeChangesAndDump()
	if err := s.write(pending); err != nil {
		return err
	}
	s.durable = seq
	b, err := json.Marshal(snapshot{Version: snapshotVersion, Changes: state})
	if err != nil {
		return err
	}
	tmp := s.path(snapshotName + ".tmp")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path(snapshotName)); err != nil {
		return err
	}
	if err := syncDir(s.dir); err != nil {
		return err
	}
	if err := s.journal.Truncate(0); err != nil {
		return err
	}
	s.journalSz = 0
	return s.journal.Sync()
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (s *Store) failure() error {
	if p := s.failed.Load(); p != nil {
		return *p
	}
	return nil
}

func (s *Store) fail(err error) error {
	err = fmt.Errorf("the store could not write to %s and has stopped answering, so that nothing is reported that a restart would lose; fix the disk and restart the server: %w", s.dir, err)
	s.failed.CompareAndSwap(nil, &err)
	return s.failure()
}

func (s *Store) closeFiles() {
	if s.journal != nil {
		s.journal.Close()
	}
	if s.audit != nil {
		s.audit.Close()
	}
}

// Close writes what is left, closes the files and gives up the directory.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var err error
	if s.failure() == nil {
		changes, seq := s.mem.TakeChanges()
		if err = s.write(changes); err == nil {
			s.durable = seq
		}
	}
	s.closeFiles()
	s.lock.Close()
	closed := errors.New("the store is closed")
	s.failed.CompareAndSwap(nil, &closed)
	return err
}

// The three shapes a store call has. Each checks the store still answers,
// runs the memory store's call, then makes its changes durable. Every method
// below goes through one of them, reads included: a read costs one counter
// comparison, and no method can be left out by mistaking it for a read.
func run0(s *Store, fn func() error) error {
	if err := s.failure(); err != nil {
		return err
	}
	before := s.mem.ChangeSeq()
	err := fn()
	if werr := s.after(before); werr != nil {
		return werr
	}
	return err
}

func run1[A any](s *Store, fn func() (A, error)) (A, error) {
	var a A
	err := run0(s, func() (err error) { a, err = fn(); return })
	if err != nil {
		var zero A
		return zero, err
	}
	return a, nil
}

func run2[A, B any](s *Store, fn func() (A, B, error)) (A, B, error) {
	var a A
	var b B
	err := run0(s, func() (err error) { a, b, err = fn(); return })
	if err != nil {
		var za A
		var zb B
		return za, zb, err
	}
	return a, b, nil
}

func (s *Store) Installation(ctx context.Context) (string, error) {
	return run1(s, func() (string, error) { return s.mem.Installation(ctx) })
}

func (s *Store) CreateOrg(ctx context.Context, o api.Organization) error {
	return run0(s, func() error { return s.mem.CreateOrg(ctx, o) })
}
func (s *Store) GetOrg(ctx context.Context, id string) (api.Organization, error) {
	return run1(s, func() (api.Organization, error) { return s.mem.GetOrg(ctx, id) })
}
func (s *Store) ListOrgs(ctx context.Context) ([]api.Organization, error) {
	return run1(s, func() ([]api.Organization, error) { return s.mem.ListOrgs(ctx) })
}
func (s *Store) UpdateOrg(ctx context.Context, id string, fn func(*api.Organization) error) (api.Organization, error) {
	return run1(s, func() (api.Organization, error) { return s.mem.UpdateOrg(ctx, id, fn) })
}

func (s *Store) CreateProject(ctx context.Context, p api.Project) error {
	return run0(s, func() error { return s.mem.CreateProject(ctx, p) })
}
func (s *Store) GetProject(ctx context.Context, id string) (api.Project, error) {
	return run1(s, func() (api.Project, error) { return s.mem.GetProject(ctx, id) })
}
func (s *Store) ListProjects(ctx context.Context, orgID string) ([]api.Project, error) {
	return run1(s, func() ([]api.Project, error) { return s.mem.ListProjects(ctx, orgID) })
}
func (s *Store) UpdateProject(ctx context.Context, id string, fn func(*api.Project) error) (api.Project, error) {
	return run1(s, func() (api.Project, error) { return s.mem.UpdateProject(ctx, id, fn) })
}
func (s *Store) DeleteProject(ctx context.Context, id string) error {
	return run0(s, func() error { return s.mem.DeleteProject(ctx, id) })
}

func (s *Store) CreateKey(ctx context.Context, k store.KeyRecord) error {
	return run0(s, func() error { return s.mem.CreateKey(ctx, k) })
}
func (s *Store) GetKey(ctx context.Context, id string) (store.KeyRecord, error) {
	return run1(s, func() (store.KeyRecord, error) { return s.mem.GetKey(ctx, id) })
}
func (s *Store) ListKeys(ctx context.Context, orgID, projectID string) ([]store.KeyRecord, error) {
	return run1(s, func() ([]store.KeyRecord, error) { return s.mem.ListKeys(ctx, orgID, projectID) })
}
func (s *Store) UpdateKey(ctx context.Context, id string, fn func(*store.KeyRecord) error) (store.KeyRecord, error) {
	return run1(s, func() (store.KeyRecord, error) { return s.mem.UpdateKey(ctx, id, fn) })
}

func (s *Store) PutSecret(ctx context.Context, sec store.SecretRecord) error {
	return run0(s, func() error { return s.mem.PutSecret(ctx, sec) })
}
func (s *Store) GetSecret(ctx context.Context, name string) (store.SecretRecord, error) {
	return run1(s, func() (store.SecretRecord, error) { return s.mem.GetSecret(ctx, name) })
}
func (s *Store) ListSecrets(ctx context.Context) ([]store.SecretRecord, error) {
	return run1(s, func() ([]store.SecretRecord, error) { return s.mem.ListSecrets(ctx) })
}
func (s *Store) DeleteSecret(ctx context.Context, name string) error {
	return run0(s, func() error { return s.mem.DeleteSecret(ctx, name) })
}

func (s *Store) PutStack(ctx context.Context, st api.Stack) error {
	return run0(s, func() error { return s.mem.PutStack(ctx, st) })
}
func (s *Store) GetStack(ctx context.Context, name string) (api.Stack, error) {
	return run1(s, func() (api.Stack, error) { return s.mem.GetStack(ctx, name) })
}
func (s *Store) ListStacks(ctx context.Context) ([]api.Stack, error) {
	return run1(s, func() ([]api.Stack, error) { return s.mem.ListStacks(ctx) })
}
func (s *Store) DeleteStack(ctx context.Context, name string) error {
	return run0(s, func() error { return s.mem.DeleteStack(ctx, name) })
}

func (s *Store) UpsertPool(ctx context.Context, p api.Pool) error {
	return run0(s, func() error { return s.mem.UpsertPool(ctx, p) })
}
func (s *Store) GetPool(ctx context.Context, name string) (api.Pool, error) {
	return run1(s, func() (api.Pool, error) { return s.mem.GetPool(ctx, name) })
}
func (s *Store) ListPools(ctx context.Context) ([]api.Pool, error) {
	return run1(s, func() ([]api.Pool, error) { return s.mem.ListPools(ctx) })
}
func (s *Store) DeletePool(ctx context.Context, name string) error {
	return run0(s, func() error { return s.mem.DeletePool(ctx, name) })
}

func (s *Store) GetTemplate(ctx context.Context, name string) (api.Template, error) {
	return run1(s, func() (api.Template, error) { return s.mem.GetTemplate(ctx, name) })
}
func (s *Store) ListTemplates(ctx context.Context) ([]api.Template, error) {
	return run1(s, func() ([]api.Template, error) { return s.mem.ListTemplates(ctx) })
}
func (s *Store) UpdateTemplate(ctx context.Context, name string, fn func(t *api.Template, found bool) error) (api.Template, error) {
	return run1(s, func() (api.Template, error) { return s.mem.UpdateTemplate(ctx, name, fn) })
}
func (s *Store) DeleteTemplate(ctx context.Context, name string) error {
	return run0(s, func() error { return s.mem.DeleteTemplate(ctx, name) })
}

func (s *Store) PutPolicy(ctx context.Context, p api.PlacementPolicy) error {
	return run0(s, func() error { return s.mem.PutPolicy(ctx, p) })
}
func (s *Store) GetPolicy(ctx context.Context, scope string) (api.PlacementPolicy, bool, error) {
	return run2(s, func() (api.PlacementPolicy, bool, error) { return s.mem.GetPolicy(ctx, scope) })
}
func (s *Store) ListPolicies(ctx context.Context) ([]api.PlacementPolicy, error) {
	return run1(s, func() ([]api.PlacementPolicy, error) { return s.mem.ListPolicies(ctx) })
}
func (s *Store) DeletePolicy(ctx context.Context, scope string) error {
	return run0(s, func() error { return s.mem.DeletePolicy(ctx, scope) })
}

func (s *Store) AdmitSandbox(ctx context.Context, rec store.SandboxRecord, adm store.Admission) (store.SandboxRecord, bool, error) {
	return run2(s, func() (store.SandboxRecord, bool, error) { return s.mem.AdmitSandbox(ctx, rec, adm) })
}
func (s *Store) GetSandbox(ctx context.Context, id string) (store.SandboxRecord, error) {
	return run1(s, func() (store.SandboxRecord, error) { return s.mem.GetSandbox(ctx, id) })
}
func (s *Store) ListSandboxes(ctx context.Context, f store.SandboxFilter) ([]store.SandboxRecord, error) {
	return run1(s, func() ([]store.SandboxRecord, error) { return s.mem.ListSandboxes(ctx, f) })
}
func (s *Store) UpdateSandbox(ctx context.Context, id string, fn func(*store.SandboxRecord) error) (store.SandboxRecord, error) {
	return run1(s, func() (store.SandboxRecord, error) { return s.mem.UpdateSandbox(ctx, id, fn) })
}
func (s *Store) QueuedCandidates(ctx context.Context, now time.Time, limit int) ([]store.SandboxRecord, error) {
	return run1(s, func() ([]store.SandboxRecord, error) { return s.mem.QueuedCandidates(ctx, now, limit) })
}
func (s *Store) ActivateSandbox(ctx context.Context, id string, act store.Activation) (store.SandboxRecord, error) {
	return run1(s, func() (store.SandboxRecord, error) { return s.mem.ActivateSandbox(ctx, id, act) })
}
func (s *Store) ReserveCompute(ctx context.Context, id string, from, to api.SandboxStatus, orgQuota, projectQuota api.Quota, now time.Time) (store.SandboxRecord, error) {
	return run1(s, func() (store.SandboxRecord, error) {
		return s.mem.ReserveCompute(ctx, id, from, to, orgQuota, projectQuota, now)
	})
}
func (s *Store) Usage(ctx context.Context, orgID, projectID string) (api.QuotaUsage, error) {
	return run1(s, func() (api.QuotaUsage, error) { return s.mem.Usage(ctx, orgID, projectID) })
}
func (s *Store) PoolActive(ctx context.Context) (map[string]int, error) {
	return run1(s, func() (map[string]int, error) { return s.mem.PoolActive(ctx) })
}
func (s *Store) ReclaimSlot(ctx context.Context, id string, maxConcurrency int, now time.Time) (store.SandboxRecord, error) {
	return run1(s, func() (store.SandboxRecord, error) { return s.mem.ReclaimSlot(ctx, id, maxConcurrency, now) })
}

func (s *Store) CreateSnapshot(ctx context.Context, sn store.SnapshotRecord) error {
	return run0(s, func() error { return s.mem.CreateSnapshot(ctx, sn) })
}
func (s *Store) GetSnapshot(ctx context.Context, id string) (store.SnapshotRecord, error) {
	return run1(s, func() (store.SnapshotRecord, error) { return s.mem.GetSnapshot(ctx, id) })
}
func (s *Store) ListSnapshots(ctx context.Context, orgID, projectID string) ([]store.SnapshotRecord, error) {
	return run1(s, func() ([]store.SnapshotRecord, error) { return s.mem.ListSnapshots(ctx, orgID, projectID) })
}
func (s *Store) DeleteSnapshot(ctx context.Context, id string) error {
	return run0(s, func() error { return s.mem.DeleteSnapshot(ctx, id) })
}

func (s *Store) AppendAudit(ctx context.Context, ev api.AuditEvent) (api.AuditEvent, error) {
	return run1(s, func() (api.AuditEvent, error) { return s.mem.AppendAudit(ctx, ev) })
}
func (s *Store) ListAudit(ctx context.Context, f store.AuditFilter) ([]api.AuditEvent, error) {
	return run1(s, func() ([]api.AuditEvent, error) { return s.mem.ListAudit(ctx, f) })
}

// TryLeader is always true for a store that answers: one server has the
// directory, and it is the leader.
func (s *Store) TryLeader(ctx context.Context) (bool, error) {
	return run1(s, func() (bool, error) { return s.mem.TryLeader(ctx) })
}

var _ store.Store = (*Store)(nil)
