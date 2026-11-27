// Package sqlite is a store driver on SQLite: one database file, opened by
// one server, with no database service to run.
//
// It is the postgres driver's queries in SQLite's dialect, with the same
// tables. Where Postgres locks rows so that replicas cannot allocate the
// same capacity twice, this driver runs every write in one transaction at a
// time: one server has the file, so a single writer is the whole lock.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	sqlite3 "modernc.org/sqlite"

	"github.com/different-ai/differently/internal/audit"
	"github.com/different-ai/differently/internal/id"
	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/internal/store/lockfile"
	"github.com/different-ai/differently/pkg/api"
)

//go:embed migrations/*.sql
var migrations embed.FS

func init() {
	store.Register("sqlite", store.Driver{
		Summary: "one SQLite file; durable, one replica, no database service",
		Durable: true,
		Open: func(ctx context.Context, o store.Options) (store.Store, error) {
			path := o.DSN
			if path == "" {
				if o.DataDir == "" {
					return nil, errors.New("store.driver is sqlite but there is no data directory and store.dsnEnv names no file")
				}
				dir := filepath.Join(o.DataDir, "sqlite")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					return nil, err
				}
				// The file holds key hashes and sealed credentials.
				if err := os.Chmod(dir, 0o700); err != nil {
					return nil, err
				}
				path = filepath.Join(dir, "differently.db")
			}
			return Open(ctx, path)
		},
	})
}

// slotHolders is store.SlotHolders in SQLite's dialect: the condition
// matching store.HoldsPoolSlot.
const slotHolders = `(status IN ('provisioning','running','pausing','stopped','terminating') OR (status = 'paused' AND NOT COALESCE(json_extract(doc, '$.slotReleased'), 0)))`

// Store implements store.Store on one SQLite file.
type Store struct {
	db   *sql.DB
	lock *os.File
	// wmu lets one write transaction run at a time. SQLite allows one writer
	// anyway; taking turns here means none waits on, or gives up on, the
	// database's own lock.
	wmu sync.Mutex
}

// Open opens the database file at path, creating it when it is not there.
// It refuses a file another server has open.
func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	lock, err := lockfile.Acquire(path + ".lock")
	if errors.Is(err, lockfile.ErrLocked) {
		return nil, fmt.Errorf("the store in %s is in use by another differently-server; the sqlite driver supports one at a time", path)
	}
	if err != nil {
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	// Create the file ourselves so it is the owner's alone; SQLite gives its
	// journal files the same mode.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		lock.Close()
		return nil, err
	}
	f.Close()

	// WAL lets reads go on beside the one writer. synchronous=FULL syncs
	// every commit: a call that returned is on disk.
	q := url.Values{}
	for _, p := range []string{"busy_timeout(10000)", "journal_mode(WAL)", "synchronous(FULL)", "foreign_keys(1)"} {
		q.Add("_pragma", p)
	}
	q.Set("_txlock", "immediate")
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: q.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		lock.Close()
		return nil, err
	}
	db.SetMaxOpenConns(8)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		lock.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return &Store{db: db, lock: lock}, nil
}

// Migrate applies pending migrations.
func (s *Store) Migrate(ctx context.Context) error {
	names, _ := fs.Glob(migrations, "migrations/*.sql")
	sort.Strings(names)
	if _, err := s.db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL)"); err != nil {
		return err
	}
	for _, name := range names {
		err := s.write(ctx, func(tx *sql.Tx) error {
			var done int
			if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations WHERE version = ?1", name).Scan(&done); err != nil || done > 0 {
				return err
			}
			script, _ := migrations.ReadFile(name)
			if _, err := tx.ExecContext(ctx, string(script)); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			_, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations (version, applied_at) VALUES (?1, ?2)", name, time.Now().UTC().Format(time.RFC3339))
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// Close closes the database and gives up the file.
func (s *Store) Close() error {
	err := s.db.Close()
	s.lock.Close()
	return err
}

type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// write runs fn in the one write transaction. Everything fn reads it reads
// through tx, so a check and the write that depends on it see the same
// state and no other write comes between them.
func (s *Store) write(ctx context.Context, fn func(tx *sql.Tx) error) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// exec runs one statement as a write.
func (s *Store) exec(ctx context.Context, query string, args ...any) (int64, error) {
	var n int64
	err := s.write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return n, err
}

// doc is a record as the JSON kept in a doc column.
func doc(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // records are plain data
	}
	return string(b)
}

// micros is a time as kept in next_attempt_at.
func micros(t time.Time) int64 { return t.UnixMicro() }

func getDoc[T any](ctx context.Context, q querier, kind, id, query string, args ...any) (T, error) {
	var out T
	var raw string
	err := q.QueryRowContext(ctx, query, args...).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return out, store.NotFound(kind, id)
	}
	if err != nil {
		return out, err
	}
	return out, json.Unmarshal([]byte(raw), &out)
}

func listDocs[T any](ctx context.Context, q querier, query string, args ...any) ([]T, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var v T
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// conflictOnDup turns a broken uniqueness rule into a conflict.
func conflictOnDup(err error, msg string) error {
	const constraint = 19 // SQLITE_CONSTRAINT; the low byte of the extended codes
	var se *sqlite3.Error
	if errors.As(err, &se) && se.Code()&0xff == constraint && strings.Contains(se.Error(), "UNIQUE") {
		return api.Errorf(api.CodeConflict, "%s", msg)
	}
	return err
}

func (s *Store) Installation(ctx context.Context) (string, error) {
	var v string
	err := s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT INTO meta (key, value) VALUES ('installation', ?1) ON CONFLICT DO NOTHING", id.New("inst")); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'installation'").Scan(&v)
	})
	return v, err
}

func (s *Store) CreateOrg(ctx context.Context, o api.Organization) error {
	_, err := s.exec(ctx, "INSERT INTO organizations (id, doc) VALUES (?1, ?2)", o.ID, doc(o))
	return conflictOnDup(err, "organization "+o.ID+" exists")
}

func (s *Store) GetOrg(ctx context.Context, oid string) (api.Organization, error) {
	return getDoc[api.Organization](ctx, s.db, "organization", oid, "SELECT doc FROM organizations WHERE id = ?1", oid)
}

func (s *Store) ListOrgs(ctx context.Context) ([]api.Organization, error) {
	return listDocs[api.Organization](ctx, s.db, "SELECT doc FROM organizations ORDER BY id")
}

func (s *Store) UpdateOrg(ctx context.Context, oid string, fn func(*api.Organization) error) (api.Organization, error) {
	var out api.Organization
	err := s.write(ctx, func(tx *sql.Tx) error {
		o, err := getDoc[api.Organization](ctx, tx, "organization", oid, "SELECT doc FROM organizations WHERE id = ?1", oid)
		if err != nil {
			return err
		}
		if err := fn(&o); err != nil {
			return err
		}
		out = o
		_, err = tx.ExecContext(ctx, "UPDATE organizations SET doc = ?2 WHERE id = ?1", oid, doc(o))
		return err
	})
	return out, err
}

func (s *Store) CreateProject(ctx context.Context, p api.Project) error {
	_, err := s.exec(ctx, "INSERT INTO projects (id, org_id, doc) VALUES (?1, ?2, ?3)", p.ID, p.OrgID, doc(p))
	return conflictOnDup(err, "project "+p.ID+" exists")
}

func (s *Store) GetProject(ctx context.Context, pid string) (api.Project, error) {
	return getDoc[api.Project](ctx, s.db, "project", pid, "SELECT doc FROM projects WHERE id = ?1", pid)
}

func (s *Store) ListProjects(ctx context.Context, orgID string) ([]api.Project, error) {
	return listDocs[api.Project](ctx, s.db, "SELECT doc FROM projects WHERE ?1 = '' OR org_id = ?1 ORDER BY id", orgID)
}

func (s *Store) UpdateProject(ctx context.Context, pid string, fn func(*api.Project) error) (api.Project, error) {
	var out api.Project
	err := s.write(ctx, func(tx *sql.Tx) error {
		p, err := getDoc[api.Project](ctx, tx, "project", pid, "SELECT doc FROM projects WHERE id = ?1", pid)
		if err != nil {
			return err
		}
		if err := fn(&p); err != nil {
			return err
		}
		out = p
		_, err = tx.ExecContext(ctx, "UPDATE projects SET doc = ?2 WHERE id = ?1", pid, doc(p))
		return err
	})
	return out, err
}

func (s *Store) DeleteProject(ctx context.Context, pid string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if _, err := getDoc[api.Project](ctx, tx, "project", pid, "SELECT doc FROM projects WHERE id = ?1", pid); err != nil {
			return err
		}
		var busy string
		err := tx.QueryRowContext(ctx, "SELECT id FROM sandboxes WHERE project_id = ?1 AND status NOT IN ('terminated','failed') LIMIT 1", pid).Scan(&busy)
		if err == nil {
			return api.Errorf(api.CodeConflict, "project %s still has sandbox %s", pid, busy)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM projects WHERE id = ?1", pid)
		return err
	})
}

func (s *Store) CreateKey(ctx context.Context, k store.KeyRecord) error {
	_, err := s.exec(ctx, "INSERT INTO api_keys (id, org_id, project_id, doc) VALUES (?1, ?2, ?3, ?4)", k.ID, k.OrgID, k.ProjectID, doc(k))
	return conflictOnDup(err, "key "+k.ID+" exists")
}

func (s *Store) GetKey(ctx context.Context, kid string) (store.KeyRecord, error) {
	return getDoc[store.KeyRecord](ctx, s.db, "api key", kid, "SELECT doc FROM api_keys WHERE id = ?1", kid)
}

func (s *Store) ListKeys(ctx context.Context, orgID, projectID string) ([]store.KeyRecord, error) {
	return listDocs[store.KeyRecord](ctx, s.db, "SELECT doc FROM api_keys WHERE (?1 = '' OR org_id = ?1) AND (?2 = '' OR project_id = ?2) ORDER BY id", orgID, projectID)
}

func (s *Store) UpdateKey(ctx context.Context, kid string, fn func(*store.KeyRecord) error) (store.KeyRecord, error) {
	var out store.KeyRecord
	err := s.write(ctx, func(tx *sql.Tx) error {
		k, err := getDoc[store.KeyRecord](ctx, tx, "api key", kid, "SELECT doc FROM api_keys WHERE id = ?1", kid)
		if err != nil {
			return err
		}
		if err := fn(&k); err != nil {
			return err
		}
		out = k
		_, err = tx.ExecContext(ctx, "UPDATE api_keys SET doc = ?2 WHERE id = ?1", kid, doc(k))
		return err
	})
	return out, err
}

func (s *Store) PutStack(ctx context.Context, st api.Stack) error {
	_, err := s.exec(ctx, "INSERT INTO stacks (name, doc) VALUES (?1, ?2) ON CONFLICT (name) DO UPDATE SET doc = excluded.doc", st.Name, doc(st))
	return err
}

func (s *Store) GetStack(ctx context.Context, name string) (api.Stack, error) {
	return getDoc[api.Stack](ctx, s.db, "stack", name, "SELECT doc FROM stacks WHERE name = ?1", name)
}

func (s *Store) ListStacks(ctx context.Context) ([]api.Stack, error) {
	return listDocs[api.Stack](ctx, s.db, "SELECT doc FROM stacks ORDER BY name")
}

func (s *Store) DeleteStack(ctx context.Context, name string) error {
	n, err := s.exec(ctx, "DELETE FROM stacks WHERE name = ?1", name)
	if err == nil && n == 0 {
		return store.NotFound("stack", name)
	}
	return err
}

func (s *Store) PutSecret(ctx context.Context, rec store.SecretRecord) error {
	_, err := s.exec(ctx, "INSERT INTO secrets (name, doc) VALUES (?1, ?2) ON CONFLICT (name) DO UPDATE SET doc = excluded.doc", rec.Name, doc(rec))
	return err
}

func (s *Store) GetSecret(ctx context.Context, name string) (store.SecretRecord, error) {
	return getDoc[store.SecretRecord](ctx, s.db, "secret", name, "SELECT doc FROM secrets WHERE name = ?1", name)
}

func (s *Store) ListSecrets(ctx context.Context) ([]store.SecretRecord, error) {
	return listDocs[store.SecretRecord](ctx, s.db, "SELECT doc FROM secrets ORDER BY name")
}

func (s *Store) DeleteSecret(ctx context.Context, name string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		var user string
		err := tx.QueryRowContext(ctx, "SELECT name FROM pools WHERE json_extract(doc, '$.credentialRef') = ?1 LIMIT 1", "secret:"+name).Scan(&user)
		if err == nil {
			return api.Errorf(api.CodeConflict, "secret %s is used by pool %s", name, user)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		res, err := tx.ExecContext(ctx, "DELETE FROM secrets WHERE name = ?1", name)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return store.NotFound("secret", name)
		}
		return nil
	})
}

func (s *Store) UpsertPool(ctx context.Context, p api.Pool) error {
	p.Health, p.Capacity, p.Active = nil, nil, 0
	_, err := s.exec(ctx, "INSERT INTO pools (name, doc) VALUES (?1, ?2) ON CONFLICT (name) DO UPDATE SET doc = excluded.doc", p.Name, doc(p))
	return err
}

func (s *Store) GetPool(ctx context.Context, name string) (api.Pool, error) {
	return getDoc[api.Pool](ctx, s.db, "pool", name, "SELECT doc FROM pools WHERE name = ?1", name)
}

func (s *Store) ListPools(ctx context.Context) ([]api.Pool, error) {
	return listDocs[api.Pool](ctx, s.db, "SELECT doc FROM pools ORDER BY name")
}

func (s *Store) DeletePool(ctx context.Context, name string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		var busy string
		err := tx.QueryRowContext(ctx, "SELECT id FROM sandboxes WHERE pool = ?1 AND status NOT IN ('terminated','failed','queued') LIMIT 1", name).Scan(&busy)
		if err == nil {
			return api.Errorf(api.CodeConflict, "pool %s still hosts sandbox %s; drain it first", name, busy)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM pools WHERE name = ?1", name)
		return err
	})
}

func (s *Store) GetTemplate(ctx context.Context, name string) (api.Template, error) {
	return getDoc[api.Template](ctx, s.db, "template", name, "SELECT doc FROM templates WHERE name = ?1", name)
}

func (s *Store) ListTemplates(ctx context.Context) ([]api.Template, error) {
	// SQLite compares text byte by byte unless told otherwise, as the memory
	// store sorts.
	return listDocs[api.Template](ctx, s.db, "SELECT doc FROM templates ORDER BY name")
}

func (s *Store) UpdateTemplate(ctx context.Context, name string, fn func(*api.Template, bool) error) (api.Template, error) {
	var out api.Template
	err := s.write(ctx, func(tx *sql.Tx) error {
		t, err := getDoc[api.Template](ctx, tx, "template", name, "SELECT doc FROM templates WHERE name = ?1", name)
		found := err == nil
		if err != nil && !api.IsCode(err, api.CodeNotFound) {
			return err
		}
		if err := fn(&t, found); err != nil {
			return err
		}
		t.Name = name
		out = t
		_, err = tx.ExecContext(ctx, "INSERT INTO templates (name, doc) VALUES (?1, ?2) ON CONFLICT (name) DO UPDATE SET doc = excluded.doc", name, doc(t))
		return err
	})
	if err != nil {
		return api.Template{}, err
	}
	return out, nil
}

func (s *Store) DeleteTemplate(ctx context.Context, name string) error {
	n, err := s.exec(ctx, "DELETE FROM templates WHERE name = ?1", name)
	if err == nil && n == 0 {
		return store.NotFound("template", name)
	}
	return err
}

func (s *Store) PutPolicy(ctx context.Context, p api.PlacementPolicy) error {
	_, err := s.exec(ctx, "INSERT INTO policies (scope, doc) VALUES (?1, ?2) ON CONFLICT (scope) DO UPDATE SET doc = excluded.doc", p.Scope, doc(p))
	return err
}

func (s *Store) GetPolicy(ctx context.Context, scope string) (api.PlacementPolicy, bool, error) {
	p, err := getDoc[api.PlacementPolicy](ctx, s.db, "policy", scope, "SELECT doc FROM policies WHERE scope = ?1", scope)
	if api.IsCode(err, api.CodeNotFound) {
		return p, false, nil
	}
	return p, err == nil, err
}

func (s *Store) ListPolicies(ctx context.Context) ([]api.PlacementPolicy, error) {
	return listDocs[api.PlacementPolicy](ctx, s.db, "SELECT doc FROM policies ORDER BY scope")
}

func (s *Store) DeletePolicy(ctx context.Context, scope string) error {
	_, err := s.exec(ctx, "DELETE FROM policies WHERE scope = ?1", scope)
	return err
}

func tally(ctx context.Context, q querier, orgID, projectID string) (api.QuotaUsage, error) {
	recs, err := listDocs[store.SandboxRecord](ctx, q,
		"SELECT doc FROM sandboxes WHERE org_id = ?1 AND (?2 = '' OR project_id = ?2) AND status NOT IN ('terminated','failed')", orgID, projectID)
	if err != nil {
		return api.QuotaUsage{}, err
	}
	var snaps int
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM snapshots WHERE org_id = ?1 AND (?2 = '' OR project_id = ?2)", orgID, projectID).Scan(&snaps); err != nil {
		return api.QuotaUsage{}, err
	}
	return store.Tally(recs, snaps), nil
}

// needOrg answers not_found for an organization that is not there. The
// postgres driver locks the organization's row at this point; here the write
// transaction already keeps every other write out.
func needOrg(ctx context.Context, tx *sql.Tx, orgID string) error {
	var x int
	err := tx.QueryRowContext(ctx, "SELECT 1 FROM organizations WHERE id = ?1", orgID).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return store.NotFound("organization", orgID)
	}
	return err
}

func (s *Store) AdmitSandbox(ctx context.Context, rec store.SandboxRecord, adm store.Admission) (store.SandboxRecord, bool, error) {
	var out store.SandboxRecord
	existed := false
	err := s.write(ctx, func(tx *sql.Tx) error {
		if err := needOrg(ctx, tx, rec.OrgID); err != nil {
			return err
		}
		if rec.IdempotencyKey != "" {
			prev, err := getDoc[store.SandboxRecord](ctx, tx, "sandbox", "", "SELECT doc FROM sandboxes WHERE project_id = ?1 AND idempotency_key = ?2 AND status NOT IN ('terminated', 'failed')", rec.ProjectID, rec.IdempotencyKey)
			if err == nil {
				out, existed = prev, true
				return nil
			}
			if !api.IsCode(err, api.CodeNotFound) {
				return err
			}
		}
		ou, err := tally(ctx, tx, rec.OrgID, "")
		if err != nil {
			return err
		}
		if err := store.CheckAdmission("organization", ou, adm.OrgQuota, rec.Resources, rec.TTLSeconds); err != nil {
			return err
		}
		pu, err := tally(ctx, tx, rec.OrgID, rec.ProjectID)
		if err != nil {
			return err
		}
		if err := store.CheckAdmission("project", pu, adm.ProjectQuota, rec.Resources, rec.TTLSeconds); err != nil {
			return err
		}
		rec.Status = api.StatusQueued
		out = rec
		_, err = tx.ExecContext(ctx, `INSERT INTO sandboxes (id, org_id, project_id, pool, status, idempotency_key, next_attempt_at, doc)
			VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8)`, rec.ID, rec.OrgID, rec.ProjectID, rec.Pool, string(rec.Status), rec.IdempotencyKey, micros(rec.NextAttemptAt), doc(rec))
		return err
	})
	return out, existed, err
}

func saveSandbox(ctx context.Context, q querier, r store.SandboxRecord) error {
	_, err := q.ExecContext(ctx, "UPDATE sandboxes SET pool = ?2, status = ?3, next_attempt_at = ?4, doc = ?5 WHERE id = ?1",
		r.ID, r.Pool, string(r.Status), micros(r.NextAttemptAt), doc(r))
	return err
}

func (s *Store) GetSandbox(ctx context.Context, sid string) (store.SandboxRecord, error) {
	return getDoc[store.SandboxRecord](ctx, s.db, "sandbox", sid, "SELECT doc FROM sandboxes WHERE id = ?1", sid)
}

func (s *Store) ListSandboxes(ctx context.Context, f store.SandboxFilter) ([]store.SandboxRecord, error) {
	where := []string{"1"}
	var args []any
	add := func(clause string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if f.OrgID != "" {
		add("org_id = ?%d", f.OrgID)
	}
	if f.ProjectID != "" {
		add("project_id = ?%d", f.ProjectID)
	}
	if f.Pool != "" {
		add("pool = ?%d", f.Pool)
	}
	if len(f.Statuses) > 0 {
		marks := make([]string, len(f.Statuses))
		for i, v := range f.Statuses {
			args = append(args, string(v))
			marks[i] = fmt.Sprintf("?%d", len(args))
		}
		where = append(where, "status IN ("+strings.Join(marks, ",")+")")
	}
	if f.Cursor != "" {
		add("id < ?%d", f.Cursor)
	}
	query := "SELECT doc FROM sandboxes WHERE " + strings.Join(where, " AND ") + " ORDER BY id DESC"
	// Labels are matched here, not in SQL, so the limit is applied after
	// them: a limit in the query would count rows the labels then drop.
	if f.Limit > 0 && len(f.Label) == 0 {
		query += fmt.Sprintf(" LIMIT %d", f.Limit)
	}
	recs, err := listDocs[store.SandboxRecord](ctx, s.db, query, args...)
	if err != nil || len(f.Label) == 0 {
		return recs, err
	}
	var out []store.SandboxRecord
	for _, r := range recs {
		if !f.Matches(r) {
			continue
		}
		out = append(out, r)
		if f.Limit > 0 && len(out) >= f.Limit {
			break
		}
	}
	return out, nil
}

func (s *Store) UpdateSandbox(ctx context.Context, sid string, fn func(*store.SandboxRecord) error) (store.SandboxRecord, error) {
	var out store.SandboxRecord
	err := s.write(ctx, func(tx *sql.Tx) error {
		r, err := getDoc[store.SandboxRecord](ctx, tx, "sandbox", sid, "SELECT doc FROM sandboxes WHERE id = ?1", sid)
		if err != nil {
			return err
		}
		if err := fn(&r); err != nil {
			return err
		}
		out = r
		return saveSandbox(ctx, tx, r)
	})
	return out, err
}

func (s *Store) QueuedCandidates(ctx context.Context, now time.Time, limit int) ([]store.SandboxRecord, error) {
	queued, err := listDocs[store.SandboxRecord](ctx, s.db,
		"SELECT doc FROM sandboxes WHERE status = 'queued' AND next_attempt_at <= ?1 ORDER BY id LIMIT 1000", micros(now))
	if err != nil || len(queued) == 0 {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT org_id, coalesce(sum(json_extract(doc, '$.resources.cpu')), 0) FROM sandboxes WHERE status IN ('provisioning','running','paused','terminating') GROUP BY org_id")
	if err != nil {
		return nil, err
	}
	active := map[string]int{}
	for rows.Next() {
		var org string
		var cpu int
		if err := rows.Scan(&org, &cpu); err != nil {
			rows.Close()
			return nil, err
		}
		active[org] = cpu
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	out := store.FairOrder(queued, active)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *Store) ActivateSandbox(ctx context.Context, sid string, act store.Activation) (store.SandboxRecord, error) {
	var out store.SandboxRecord
	err := s.write(ctx, func(tx *sql.Tx) error {
		r, err := getDoc[store.SandboxRecord](ctx, tx, "sandbox", sid, "SELECT doc FROM sandboxes WHERE id = ?1", sid)
		if err != nil {
			return err
		}
		if err := needOrg(ctx, tx, r.OrgID); err != nil {
			return err
		}
		if r.Status != api.StatusQueued {
			return api.Errorf(api.CodeInvalidState, "sandbox %s is %s, not queued", sid, r.Status)
		}
		ou, err := tally(ctx, tx, r.OrgID, "")
		if err != nil {
			return err
		}
		if err := store.CheckActivation("organization", ou, act.OrgQuota, r.Resources); err != nil {
			return err
		}
		pu, err := tally(ctx, tx, r.OrgID, r.ProjectID)
		if err != nil {
			return err
		}
		if err := store.CheckActivation("project", pu, act.ProjectQuota, r.Resources); err != nil {
			return err
		}
		if act.MaxConcurrency > 0 {
			var n int
			if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sandboxes WHERE pool = ?1 AND "+slotHolders, act.Pool).Scan(&n); err != nil {
				return err
			}
			if n >= act.MaxConcurrency {
				return &api.Error{Code: "insufficient_capacity", Message: "pool " + act.Pool + " is at its concurrency limit"}
			}
		}
		now := act.Now
		r.Status, r.StatusReason = api.StatusProvisioning, ""
		r.Pool, r.Adapter, r.Trust, r.Region = act.Pool, act.Adapter, act.Trust, act.Region
		r.Decision = act.Decision
		r.Attempts++
		r.QueueSeconds = now.Sub(r.CreatedAt).Seconds() - r.RunSeconds
		r.ProvisionStartedAt, r.UpdatedAt = &now, now
		out = r
		return saveSandbox(ctx, tx, r)
	})
	return out, err
}

func (s *Store) ReserveCompute(ctx context.Context, sid string, from, to api.SandboxStatus, oq, pq api.Quota, now time.Time) (store.SandboxRecord, error) {
	var out store.SandboxRecord
	err := s.write(ctx, func(tx *sql.Tx) error {
		r, err := getDoc[store.SandboxRecord](ctx, tx, "sandbox", sid, "SELECT doc FROM sandboxes WHERE id = ?1", sid)
		if err != nil {
			return err
		}
		if err := needOrg(ctx, tx, r.OrgID); err != nil {
			return err
		}
		if r.Status != from {
			return api.Errorf(api.CodeInvalidState, "sandbox %s is %s, not %s", sid, r.Status, from)
		}
		ou, err := tally(ctx, tx, r.OrgID, "")
		if err != nil {
			return err
		}
		if err := store.CheckCompute("organization", ou, oq, r.Resources); err != nil {
			return err
		}
		pu, err := tally(ctx, tx, r.OrgID, r.ProjectID)
		if err != nil {
			return err
		}
		if err := store.CheckCompute("project", pu, pq, r.Resources); err != nil {
			return err
		}
		r.Status, r.UpdatedAt = to, now
		out = r
		return saveSandbox(ctx, tx, r)
	})
	return out, err
}

func (s *Store) Usage(ctx context.Context, orgID, projectID string) (api.QuotaUsage, error) {
	return tally(ctx, s.db, orgID, projectID)
}

func (s *Store) ReclaimSlot(ctx context.Context, sid string, maxConcurrency int, now time.Time) (store.SandboxRecord, error) {
	var out store.SandboxRecord
	err := s.write(ctx, func(tx *sql.Tx) error {
		r, err := getDoc[store.SandboxRecord](ctx, tx, "sandbox", sid, "SELECT doc FROM sandboxes WHERE id = ?1", sid)
		if err != nil {
			return err
		}
		out = r
		if !r.SlotReleased {
			return nil
		}
		if maxConcurrency > 0 {
			var n int
			if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sandboxes WHERE pool = ?1 AND "+slotHolders, r.Pool).Scan(&n); err != nil {
				return err
			}
			if n >= maxConcurrency {
				return store.InsufficientCapacity(r.Pool)
			}
		}
		r.SlotReleased, r.UpdatedAt = false, now
		out = r
		return saveSandbox(ctx, tx, r)
	})
	return out, err
}

func (s *Store) PoolActive(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT pool, count(*) FROM sandboxes WHERE pool <> '' AND "+slotHolders+" GROUP BY pool")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		out[name] = n
	}
	return out, rows.Err()
}

func (s *Store) CreateSnapshot(ctx context.Context, sn store.SnapshotRecord) error {
	_, err := s.exec(ctx, "INSERT INTO snapshots (id, org_id, project_id, doc) VALUES (?1, ?2, ?3, ?4)", sn.ID, sn.OrgID, sn.ProjectID, doc(sn))
	return err
}

func (s *Store) GetSnapshot(ctx context.Context, sid string) (store.SnapshotRecord, error) {
	return getDoc[store.SnapshotRecord](ctx, s.db, "snapshot", sid, "SELECT doc FROM snapshots WHERE id = ?1", sid)
}

func (s *Store) ListSnapshots(ctx context.Context, orgID, projectID string) ([]store.SnapshotRecord, error) {
	return listDocs[store.SnapshotRecord](ctx, s.db, "SELECT doc FROM snapshots WHERE (?1 = '' OR org_id = ?1) AND (?2 = '' OR project_id = ?2) ORDER BY id DESC", orgID, projectID)
}

func (s *Store) DeleteSnapshot(ctx context.Context, sid string) error {
	_, err := s.exec(ctx, "DELETE FROM snapshots WHERE id = ?1", sid)
	return err
}

// AppendAudit seals ev to the event before it. The write transaction is the
// lock: two appends cannot read the same predecessor.
func (s *Store) AppendAudit(ctx context.Context, ev api.AuditEvent) (api.AuditEvent, error) {
	err := s.write(ctx, func(tx *sql.Tx) error {
		var seq int64
		var prev sql.NullString
		err := tx.QueryRowContext(ctx, "SELECT seq, json_extract(doc, '$.hash') FROM audit_events ORDER BY seq DESC LIMIT 1").Scan(&seq, &prev)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		ev.Seq = seq + 1
		if ev.Time.IsZero() {
			ev.Time = time.Now().UTC()
		}
		ev.Time = ev.Time.UTC()
		ev = audit.Seal(prev.String, ev)
		_, err = tx.ExecContext(ctx, "INSERT INTO audit_events (seq, org_id, resource, action, doc) VALUES (?1, ?2, ?3, ?4, ?5)", ev.Seq, ev.OrgID, ev.Resource, ev.Action, doc(ev))
		return err
	})
	return ev, err
}

func (s *Store) ListAudit(ctx context.Context, f store.AuditFilter) ([]api.AuditEvent, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 1 << 30
	}
	order := "ORDER BY seq"
	if f.Desc {
		order = "ORDER BY seq DESC"
	}
	return listDocs[api.AuditEvent](ctx, s.db, `SELECT doc FROM audit_events
		WHERE seq > ?1 AND (?6 <= 0 OR seq < ?6) AND (?2 = '' OR org_id = ?2) AND (?3 = '' OR resource = ?3) AND (?4 = '' OR action = ?4)
		`+order+` LIMIT ?5`, f.AfterSeq, f.OrgID, f.Resource, f.Action, limit, f.BeforeSeq)
}

// TryLeader is always true: one server has the file, and it is the leader.
func (s *Store) TryLeader(context.Context) (bool, error) { return true, nil }

var _ store.Store = (*Store)(nil)
