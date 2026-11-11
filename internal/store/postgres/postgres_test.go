package postgres_test

import (
	"testing"

	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/internal/store/postgres/postgrestest"
	"github.com/different-ai/differently/internal/store/storetest"
)

// TestConformance runs the shared store suite against a real database when
// DIFFERENTLY_TEST_POSTGRES_URL is set (each subtest gets an empty schema).
func TestConformance(t *testing.T) {
	dsn := postgrestest.DSN(t)
	if dsn == "" {
		t.Skip("set " + postgrestest.EnvDSN + " to run the postgres conformance suite")
	}
	storetest.Run(t, func(t *testing.T) store.Store { return postgrestest.Open(t, dsn) })
}
