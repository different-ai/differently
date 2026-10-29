package postgres

import (
	"context"
	"os"
	"testing"

	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/internal/store/storetest"
)

// TestConformance runs the shared store suite against a real database when
// DIFFERENTLY_TEST_POSTGRES_URL is set (each subtest gets a fresh schema).
func TestConformance(t *testing.T) {
	dsn := os.Getenv("DIFFERENTLY_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set DIFFERENTLY_TEST_POSTGRES_URL to run the postgres conformance suite")
	}
	storetest.Run(t, func(t *testing.T) store.Store {
		ctx := context.Background()
		s, err := Open(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		for _, tbl := range []string{"sandboxes", "snapshots", "audit_events", "agents", "api_keys", "projects", "organizations", "pools", "policies", "meta", "schema_migrations"} {
			_, _ = s.pool.Exec(ctx, "DROP TABLE IF EXISTS "+tbl+" CASCADE")
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		return s
	})
}
