package server_test

import (
	"testing"

	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/internal/store/postgres/postgrestest"
)

// newTestStore returns the store the acceptance scenarios run on: the memory
// store by default, and an empty Postgres schema when
// DIFFERENTLY_TEST_POSTGRES_URL is set, so the same scenarios cover the store
// production uses.
func newTestStore(t *testing.T) store.Store {
	t.Helper()
	if dsn := postgrestest.DSN(t); dsn != "" {
		return postgrestest.Open(t, dsn)
	}
	return store.NewMemory()
}
