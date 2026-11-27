package server_test

import (
	"context"
	"os"
	"testing"

	"github.com/different-ai/differently/internal/store"
	_ "github.com/different-ai/differently/internal/store/file" // registers the file driver
	"github.com/different-ai/differently/internal/store/postgres/postgrestest"
	_ "github.com/different-ai/differently/internal/store/sqlite" // registers the sqlite driver
)

// newTestStore returns the store the acceptance scenarios run on: the memory
// store by default; an empty Postgres schema when
// DIFFERENTLY_TEST_POSTGRES_URL is set, so the same scenarios cover the store
// production uses; and, when DIFFERENTLY_TEST_STORE names another driver, a
// new store of that driver in a directory of its own (make test-stores).
func newTestStore(t *testing.T) store.Store {
	t.Helper()
	if dsn := postgrestest.DSN(t); dsn != "" {
		return postgrestest.Open(t, dsn)
	}
	if driver := os.Getenv("DIFFERENTLY_TEST_STORE"); driver != "" && driver != "memory" {
		st, err := store.Open(context.Background(), driver, store.Options{DataDir: t.TempDir()})
		if err != nil {
			t.Fatalf("open the %s store: %v", driver, err)
		}
		if m, ok := st.(store.Migrator); ok {
			if err := m.Migrate(context.Background()); err != nil {
				t.Fatalf("migrate the %s store: %v", driver, err)
			}
		}
		t.Cleanup(func() { st.Close() })
		return st
	}
	return store.NewMemory()
}
