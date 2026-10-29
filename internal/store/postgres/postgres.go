// Package postgres is the production Store. Quota-sensitive operations run
// in transactions that lock the organization row (and the pool row for
// concurrency caps) in a fixed order, so concurrent replicas cannot
// double-allocate. Scheduler leadership is a session advisory lock.
package postgres

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/different-ai/differently/internal/audit"
	"github.com/different-ai/differently/internal/id"
	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/pkg/api"
)

//go:embed migrations/*.sql
var migrations embed.FS

const (
	lockMigrate int64 = 0x646c795f6d6967 // "dly_mig"
	lockLeader  int64 = 0x646c795f6c6472 // "dly_ldr"
	lockAudit   int64 = 0x646c795f617564 // "dly_aud"
)

// Store implements store.Store on PostgreSQL.
type Store struct {
	pool *pgxpool.Pool

	leaderMu   sync.Mutex
	leaderConn *pgxpool.Conn
}

// Open connects to dsn.
func Open(ctx context.Context, dsn string) (*Store, error) {
	p, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := p.Ping(ctx); err != nil {
		p.Close()
		return nil, err
	}
	return &Store{pool: p}, nil
}

// Migrate applies pending migrations under an advisory lock.
func (s *Store) Migrate(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", lockMigrate); err != nil {
		return err
	}
	defer conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", lockMigrate)
	if _, err := conn.Exec(ctx, "CREATE TABLE IF NOT EXISTS schema_migrations (version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())"); err != nil {
		return err
	}
	names, _ := fs.Glob(migrations, "migrations/*.sql")
	sort.Strings(names)
	for _, name := range names {
		var done bool
		if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", name).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sql, _ := migrations.ReadFile(name)
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("%s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", name); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Close releases leadership and the pool.
func (s *Store) Close() error {
	s.leaderMu.Lock()
	if s.leaderConn != nil {
		s.leaderConn.Release()
		s.leaderConn = nil
	}
	s.leaderMu.Unlock()
	s.pool.Close()
	return nil
}

type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *Store) tx(ctx context.Context, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, fn)
}

func getDoc[T any](ctx context.Context, q querier, kind, id, sql string, args ...any) (T, error) {
	var out T
	var raw []byte
	err := q.QueryRow(ctx, sql, args...).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, store.NotFound(kind, id)
	}
	if err != nil {
		return out, err
	}
	return out, json.Unmarshal(raw, &out)
}

func listDocs[T any](ctx context.Context, q querier, sql string, args ...any) ([]T, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var v T
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func conflictOnDup(err error, msg string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return api.Errorf(api.CodeConflict, "%s", msg)
	}
	return err
}

func (s *Store) Installation(ctx context.Context) (string, error) {
	if _, err := s.pool.Exec(ctx, "INSERT INTO meta (key, value) VALUES ('installation', $1) ON CONFLICT DO NOTHING", id.New("inst")); err != nil {
		return "", err
	}
	var v string
	return v, s.pool.QueryRow(ctx, "SELECT value FROM meta WHERE key = 'installation'").Scan(&v)
}

func (s *Store) CreateOrg(ctx context.Context, o api.Organization) error {
	_, err := s.pool.Exec(ctx, "INSERT INTO organizations (id, doc) VALUES ($1, $2)", o.ID, o)
	return conflictOnDup(err, "organization "+o.ID+" exists")
}

func (s *Store) GetOrg(ctx context.Context, oid string) (api.Organization, error) {
	return getDoc[api.Organization](ctx, s.pool, "organization", oid, "SELECT doc FROM organizations WHERE id = $1", oid)
}

func (s *Store) ListOrgs(ctx context.Context) ([]api.Organization, error) {
	return listDocs[api.Organization](ctx, s.pool, "SELECT doc FROM organizations ORDER BY id")
}

func (s *Store) UpdateOrg(ctx context.Context, oid string, fn func(*api.Organization) error) (api.Organization, error) {
	var out api.Organization
	err := s.tx(ctx, func(tx pgx.Tx) error {
		o, err := getDoc[api.Organization](ctx, tx, "organization", oid, "SELECT doc FROM organizations WHERE id = $1 FOR UPDATE", oid)
		if err != nil {
			return err
		}
		if err := fn(&o); err != nil {
			return err
		}
		out = o
		_, err = tx.Exec(ctx, "UPDATE organizations SET doc = $2 WHERE id = $1", oid, o)
		return err
	})
	return out, err
}

func (s *Store) CreateProject(ctx context.Context, p api.Project) error {
	_, err := s.pool.Exec(ctx, "INSERT INTO projects (id, org_id, doc) VALUES ($1, $2, $3)", p.ID, p.OrgID, p)
	return conflictOnDup(err, "project "+p.ID+" exists")
}

func (s *Store) GetProject(ctx context.Context, pid string) (api.Project, error) {
	return getDoc[api.Project](ctx, s.pool, "project", pid, "SELECT doc FROM projects WHERE id = $1", pid)
}

func (s *Store) ListProjects(ctx context.Context, orgID string) ([]api.Project, error) {
	return listDocs[api.Project](ctx, s.pool, "SELECT doc FROM projects WHERE $1 = '' OR org_id = $1 ORDER BY id", orgID)
}

func (s *Store) UpdateProject(ctx context.Context, pid string, fn func(*api.Project) error) (api.Project, error) {
	var out api.Project
	err := s.tx(ctx, func(tx pgx.Tx) error {
		p, err := getDoc[api.Project](ctx, tx, "project", pid, "SELECT doc FROM projects WHERE id = $1 FOR UPDATE", pid)
		if err != nil {
			return err
		}
		if err := fn(&p); err != nil {
			return err
		}
		out = p
		_, err = tx.Exec(ctx, "UPDATE projects SET doc = $2 WHERE id = $1", pid, p)
		return err
	})
	return out, err
}

func (s *Store) DeleteProject(ctx context.Context, pid string) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		p, err := getDoc[api.Project](ctx, tx, "project", pid, "SELECT doc FROM projects WHERE id = $1", pid)
		if err != nil {
			return err
		}
		// Lock the organization so no admission races the delete.
		if _, err := tx.Exec(ctx, "SELECT 1 FROM organizations WHERE id = $1 FOR UPDATE", p.OrgID); err != nil {
			return err
		}
		var busy string
		err = tx.QueryRow(ctx, "SELECT id FROM sandboxes WHERE project_id = $1 AND status NOT IN ('terminated','failed') LIMIT 1", pid).Scan(&busy)
		if err == nil {
			return api.Errorf(api.CodeConflict, "project %s still has sandbox %s", pid, busy)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		_, err = tx.Exec(ctx, "DELETE FROM projects WHERE id = $1", pid)
		return err
	})
}

func (s *Store) CreateKey(ctx context.Context, k store.KeyRecord) error {
	_, err := s.pool.Exec(ctx, "INSERT INTO api_keys (id, org_id, project_id, doc) VALUES ($1, $2, $3, $4)", k.ID, k.OrgID, k.ProjectID, k)
	return conflictOnDup(err, "key "+k.ID+" exists")
}

func (s *Store) GetKey(ctx context.Context, kid string) (store.KeyRecord, error) {
	return getDoc[store.KeyRecord](ctx, s.pool, "api key", kid, "SELECT doc FROM api_keys WHERE id = $1", kid)
}

func (s *Store) ListKeys(ctx context.Context, orgID, projectID string) ([]store.KeyRecord, error) {
	return listDocs[store.KeyRecord](ctx, s.pool, "SELECT doc FROM api_keys WHERE ($1 = '' OR org_id = $1) AND ($2 = '' OR project_id = $2) ORDER BY id", orgID, projectID)
}

func (s *Store) UpdateKey(ctx context.Context, kid string, fn func(*store.KeyRecord) error) (store.KeyRecord, error) {
	var out store.KeyRecord
	err := s.tx(ctx, func(tx pgx.Tx) error {
		k, err := getDoc[store.KeyRecord](ctx, tx, "api key", kid, "SELECT doc FROM api_keys WHERE id = $1 FOR UPDATE", kid)
		if err != nil {
			return err
		}
		if err := fn(&k); err != nil {
			return err
		}
		out = k
		_, err = tx.Exec(ctx, "UPDATE api_keys SET doc = $2 WHERE id = $1", kid, k)
		return err
	})
	return out, err
}

func (s *Store) PutSecret(ctx context.Context, rec store.SecretRecord) error {
	_, err := s.pool.Exec(ctx, "INSERT INTO secrets (name, doc) VALUES ($1, $2) ON CONFLICT (name) DO UPDATE SET doc = EXCLUDED.doc", rec.Name, rec)
	return err
}

func (s *Store) GetSecret(ctx context.Context, name string) (store.SecretRecord, error) {
	return getDoc[store.SecretRecord](ctx, s.pool, "secret", name, "SELECT doc FROM secrets WHERE name = $1", name)
}

func (s *Store) ListSecrets(ctx context.Context) ([]store.SecretRecord, error) {
	return listDocs[store.SecretRecord](ctx, s.pool, "SELECT doc FROM secrets ORDER BY name")
}

func (s *Store) DeleteSecret(ctx context.Context, name string) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		var user string
		err := tx.QueryRow(ctx, "SELECT name FROM pools WHERE doc->>'credentialRef' = $1 LIMIT 1", "secret:"+name).Scan(&user)
		if err == nil {
			return api.Errorf(api.CodeConflict, "secret %s is used by pool %s", name, user)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		tag, err := tx.Exec(ctx, "DELETE FROM secrets WHERE name = $1", name)
		if err == nil && tag.RowsAffected() == 0 {
			return store.NotFound("secret", name)
		}
		return err
	})
}

func (s *Store) UpsertPool(ctx context.Context, p api.Pool) error {
	p.Health, p.Capacity, p.Active = nil, nil, 0
	_, err := s.pool.Exec(ctx, "INSERT INTO pools (name, doc) VALUES ($1, $2) ON CONFLICT (name) DO UPDATE SET doc = EXCLUDED.doc", p.Name, p)
	return err
}

func (s *Store) GetPool(ctx context.Context, name string) (api.Pool, error) {
	return getDoc[api.Pool](ctx, s.pool, "pool", name, "SELECT doc FROM pools WHERE name = $1", name)
}

func (s *Store) ListPools(ctx context.Context) ([]api.Pool, error) {
	return listDocs[api.Pool](ctx, s.pool, "SELECT doc FROM pools ORDER BY name")
}

func (s *Store) DeletePool(ctx context.Context, name string) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT 1 FROM pools WHERE name = $1 FOR UPDATE", name); err != nil {
			return err
		}
		var busy string
		err := tx.QueryRow(ctx, "SELECT id FROM sandboxes WHERE pool = $1 AND status NOT IN ('terminated','failed','queued') LIMIT 1", name).Scan(&busy)
		if err == nil {
			return api.Errorf(api.CodeConflict, "pool %s still hosts sandbox %s; drain it first", name, busy)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		_, err = tx.Exec(ctx, "DELETE FROM pools WHERE name = $1", name)
		return err
	})
}

func (s *Store) PutPolicy(ctx context.Context, p api.PlacementPolicy) error {
	_, err := s.pool.Exec(ctx, "INSERT INTO policies (scope, doc) VALUES ($1, $2) ON CONFLICT (scope) DO UPDATE SET doc = EXCLUDED.doc", p.Scope, p)
	return err
}

func (s *Store) GetPolicy(ctx context.Context, scope string) (api.PlacementPolicy, bool, error) {
	p, err := getDoc[api.PlacementPolicy](ctx, s.pool, "policy", scope, "SELECT doc FROM policies WHERE scope = $1", scope)
	if api.IsCode(err, api.CodeNotFound) {
		return p, false, nil
	}
	return p, err == nil, err
}

func (s *Store) ListPolicies(ctx context.Context) ([]api.PlacementPolicy, error) {
	return listDocs[api.PlacementPolicy](ctx, s.pool, "SELECT doc FROM policies ORDER BY scope")
}

func (s *Store) DeletePolicy(ctx context.Context, scope string) error {
	_, err := s.pool.Exec(ctx, "DELETE FROM policies WHERE scope = $1", scope)
	return err
}

func tally(ctx context.Context, q querier, orgID, projectID string) (api.QuotaUsage, error) {
	recs, err := listDocs[store.SandboxRecord](ctx, q,
		"SELECT doc FROM sandboxes WHERE org_id = $1 AND ($2 = '' OR project_id = $2) AND status NOT IN ('terminated','failed')", orgID, projectID)
	if err != nil {
		return api.QuotaUsage{}, err
	}
	var snaps int
	if err := q.QueryRow(ctx, "SELECT count(*) FROM snapshots WHERE org_id = $1 AND ($2 = '' OR project_id = $2)", orgID, projectID).Scan(&snaps); err != nil {
		return api.QuotaUsage{}, err
	}
	return store.Tally(recs, snaps), nil
}

func lockOrg(ctx context.Context, tx pgx.Tx, orgID string) error {
	var x int
	err := tx.QueryRow(ctx, "SELECT 1 FROM organizations WHERE id = $1 FOR UPDATE", orgID).Scan(&x)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.NotFound("organization", orgID)
	}
	return err
}

func (s *Store) AdmitSandbox(ctx context.Context, rec store.SandboxRecord, adm store.Admission) (store.SandboxRecord, bool, error) {
	var out store.SandboxRecord
	existed := false
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if err := lockOrg(ctx, tx, rec.OrgID); err != nil {
			return err
		}
		if rec.IdempotencyKey != "" {
			prev, err := getDoc[store.SandboxRecord](ctx, tx, "sandbox", "", "SELECT doc FROM sandboxes WHERE project_id = $1 AND idempotency_key = $2 AND status NOT IN ('terminated', 'failed')", rec.ProjectID, rec.IdempotencyKey)
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
		return insertSandbox(ctx, tx, rec)
	})
	return out, existed, err
}

func insertSandbox(ctx context.Context, q querier, r store.SandboxRecord) error {
	_, err := q.Exec(ctx, `INSERT INTO sandboxes (id, org_id, project_id, pool, status, idempotency_key, next_attempt_at, doc)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, r.ID, r.OrgID, r.ProjectID, r.Pool, string(r.Status), r.IdempotencyKey, r.NextAttemptAt, r)
	return err
}

func saveSandbox(ctx context.Context, q querier, r store.SandboxRecord) error {
	_, err := q.Exec(ctx, "UPDATE sandboxes SET pool = $2, status = $3, next_attempt_at = $4, doc = $5 WHERE id = $1",
		r.ID, r.Pool, string(r.Status), r.NextAttemptAt, r)
	return err
}

func (s *Store) GetSandbox(ctx context.Context, sid string) (store.SandboxRecord, error) {
	return getDoc[store.SandboxRecord](ctx, s.pool, "sandbox", sid, "SELECT doc FROM sandboxes WHERE id = $1", sid)
}

func (s *Store) ListSandboxes(ctx context.Context, f store.SandboxFilter) ([]store.SandboxRecord, error) {
	where := []string{"TRUE"}
	var args []any
	add := func(clause string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if f.OrgID != "" {
		add("org_id = $%d", f.OrgID)
	}
	if f.ProjectID != "" {
		add("project_id = $%d", f.ProjectID)
	}
	if f.Pool != "" {
		add("pool = $%d", f.Pool)
	}
	if len(f.Statuses) > 0 {
		st := make([]string, len(f.Statuses))
		for i, v := range f.Statuses {
			st[i] = string(v)
		}
		add("status = ANY($%d)", st)
	}
	if len(f.Label) > 0 {
		b, _ := json.Marshal(f.Label)
		add("doc->'labels' @> $%d::jsonb", string(b))
	}
	if f.Cursor != "" {
		add("id < $%d", f.Cursor)
	}
	sql := "SELECT doc FROM sandboxes WHERE " + strings.Join(where, " AND ") + " ORDER BY id DESC"
	if f.Limit > 0 {
		sql += fmt.Sprintf(" LIMIT %d", f.Limit)
	}
	return listDocs[store.SandboxRecord](ctx, s.pool, sql, args...)
}

func (s *Store) UpdateSandbox(ctx context.Context, sid string, fn func(*store.SandboxRecord) error) (store.SandboxRecord, error) {
	var out store.SandboxRecord
	err := s.tx(ctx, func(tx pgx.Tx) error {
		r, err := getDoc[store.SandboxRecord](ctx, tx, "sandbox", sid, "SELECT doc FROM sandboxes WHERE id = $1 FOR UPDATE", sid)
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
	queued, err := listDocs[store.SandboxRecord](ctx, s.pool,
		"SELECT doc FROM sandboxes WHERE status = 'queued' AND next_attempt_at <= $1 ORDER BY id LIMIT 1000", now)
	if err != nil || len(queued) == 0 {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, "SELECT org_id, coalesce(sum((doc->'resources'->>'cpu')::int), 0) FROM sandboxes WHERE status IN ('provisioning','running','paused','terminating') GROUP BY org_id")
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
	rows.Close()
	out := store.FairOrder(queued, active)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *Store) ActivateSandbox(ctx context.Context, sid string, act store.Activation) (store.SandboxRecord, error) {
	var out store.SandboxRecord
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var orgID string
		if err := tx.QueryRow(ctx, "SELECT org_id FROM sandboxes WHERE id = $1", sid).Scan(&orgID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return store.NotFound("sandbox", sid)
			}
			return err
		}
		// Lock order everywhere: organization, then pool, then sandbox.
		if err := lockOrg(ctx, tx, orgID); err != nil {
			return err
		}
		if act.MaxConcurrency > 0 {
			if _, err := tx.Exec(ctx, "SELECT 1 FROM pools WHERE name = $1 FOR UPDATE", act.Pool); err != nil {
				return err
			}
		}
		r, err := getDoc[store.SandboxRecord](ctx, tx, "sandbox", sid, "SELECT doc FROM sandboxes WHERE id = $1 FOR UPDATE", sid)
		if err != nil {
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
			if err := tx.QueryRow(ctx, "SELECT count(*) FROM sandboxes WHERE pool = $1 AND status IN ('provisioning','running','paused','stopped','terminating')", act.Pool).Scan(&n); err != nil {
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
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var orgID string
		if err := tx.QueryRow(ctx, "SELECT org_id FROM sandboxes WHERE id = $1", sid).Scan(&orgID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return store.NotFound("sandbox", sid)
			}
			return err
		}
		if err := lockOrg(ctx, tx, orgID); err != nil {
			return err
		}
		r, err := getDoc[store.SandboxRecord](ctx, tx, "sandbox", sid, "SELECT doc FROM sandboxes WHERE id = $1 FOR UPDATE", sid)
		if err != nil {
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
	return tally(ctx, s.pool, orgID, projectID)
}

func (s *Store) PoolActive(ctx context.Context) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, "SELECT pool, count(*) FROM sandboxes WHERE pool <> '' AND status IN ('provisioning','running','paused','stopped','terminating') GROUP BY pool")
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
	_, err := s.pool.Exec(ctx, "INSERT INTO snapshots (id, org_id, project_id, doc) VALUES ($1, $2, $3, $4)", sn.ID, sn.OrgID, sn.ProjectID, sn)
	return err
}

func (s *Store) GetSnapshot(ctx context.Context, sid string) (store.SnapshotRecord, error) {
	return getDoc[store.SnapshotRecord](ctx, s.pool, "snapshot", sid, "SELECT doc FROM snapshots WHERE id = $1", sid)
}

func (s *Store) ListSnapshots(ctx context.Context, orgID, projectID string) ([]store.SnapshotRecord, error) {
	return listDocs[store.SnapshotRecord](ctx, s.pool, "SELECT doc FROM snapshots WHERE ($1 = '' OR org_id = $1) AND ($2 = '' OR project_id = $2) ORDER BY id DESC", orgID, projectID)
}

func (s *Store) DeleteSnapshot(ctx context.Context, sid string) error {
	_, err := s.pool.Exec(ctx, "DELETE FROM snapshots WHERE id = $1", sid)
	return err
}

func (s *Store) AppendAudit(ctx context.Context, ev api.AuditEvent) (api.AuditEvent, error) {
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", lockAudit); err != nil {
			return err
		}
		var seq int64
		var prev string
		err := tx.QueryRow(ctx, "SELECT seq, doc->>'hash' FROM audit_events ORDER BY seq DESC LIMIT 1").Scan(&seq, &prev)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		ev.Seq = seq + 1
		if ev.Time.IsZero() {
			ev.Time = time.Now().UTC()
		}
		ev.Time = ev.Time.UTC().Truncate(time.Microsecond) // match timestamp precision on reload
		ev = audit.Seal(prev, ev)
		_, err = tx.Exec(ctx, "INSERT INTO audit_events (seq, org_id, resource, action, doc) VALUES ($1, $2, $3, $4, $5)", ev.Seq, ev.OrgID, ev.Resource, ev.Action, ev)
		return err
	})
	return ev, err
}

func (s *Store) ListAudit(ctx context.Context, f store.AuditFilter) ([]api.AuditEvent, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 1 << 30
	}
	return listDocs[api.AuditEvent](ctx, s.pool, `SELECT doc FROM audit_events
		WHERE seq > $1 AND ($2 = '' OR org_id = $2) AND ($3 = '' OR resource = $3) AND ($4 = '' OR action = $4)
		ORDER BY seq LIMIT $5`, f.AfterSeq, f.OrgID, f.Resource, f.Action, limit)
}

// TryLeader holds a session advisory lock on a dedicated connection. If the
// connection drops, Postgres releases the lock and another replica takes over.
func (s *Store) TryLeader(ctx context.Context) (bool, error) {
	s.leaderMu.Lock()
	defer s.leaderMu.Unlock()
	if s.leaderConn != nil {
		if err := s.leaderConn.Ping(ctx); err == nil {
			return true, nil
		}
		s.leaderConn.Release()
		s.leaderConn = nil
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return false, err
	}
	var ok bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", lockLeader).Scan(&ok); err != nil || !ok {
		conn.Release()
		return false, err
	}
	s.leaderConn = conn
	return true, nil
}

var _ store.Store = (*Store)(nil)
