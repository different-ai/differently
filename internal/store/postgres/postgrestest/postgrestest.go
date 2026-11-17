// Package postgrestest gives tests an empty Postgres store.
//
// Each store lives in its own schema of the database the DSN names, so tests
// do not see each other's rows and nothing has to be dropped between them.
// Scheduler leadership is an advisory lock on the whole database, though: run
// the packages that use this one after another (go test -p 1), not at once.
package postgrestest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/different-ai/differently/internal/store/postgres"
)

// EnvDSN names the environment variable with the test database's DSN.
const EnvDSN = "DIFFERENTLY_TEST_POSTGRES_URL"

// EnvRequire, when set, makes a missing DSN a failure instead of a skip, so a
// CI job that is meant to run against Postgres cannot pass without it.
const EnvRequire = "DIFFERENTLY_TEST_POSTGRES_REQUIRED"

// DSN returns the test database's DSN, or "" when there is none. It fails the
// test when EnvRequire is set and the DSN is missing.
func DSN(t testing.TB) string {
	t.Helper()
	dsn := os.Getenv(EnvDSN)
	if dsn == "" && os.Getenv(EnvRequire) != "" {
		t.Fatalf("%s is set but %s is empty: this run must use Postgres", EnvRequire, EnvDSN)
	}
	return dsn
}

// Open returns a migrated store in a new, empty schema of the database at
// dsn. The schema is dropped and the store closed when the test ends.
func Open(t testing.TB, dsn string) *postgres.Store {
	t.Helper()
	ctx := context.Background()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	schema := "t_" + hex.EncodeToString(b)

	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to %s: %v", EnvDSN, err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	_ = admin.Close(ctx)

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("%s must be a postgres:// URL: %v", EnvDSN, err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()

	s, err := postgres.Open(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = s.Close()
		if c, err := pgx.Connect(context.Background(), dsn); err == nil {
			_, _ = c.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
			_ = c.Close(context.Background())
		}
	})
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s
}
