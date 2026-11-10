package pools

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/different-ai/differently/internal/adapters/fake"
	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// A credential that could not be read just now (a database blip, an I/O
// error) is not a changed credential: the pool keeps the adapter that works.
// A changed value rebuilds it, and a deleted one takes the pool down.
func TestSyncKeepsAdapterWhenCredentialUnreadable(t *testing.T) {
	value, readErr := "key-1", error(nil)
	resolve := func(string) (string, error) { return value, readErr }
	builds := 0
	Register("pools-test", func(p api.Pool, deps Deps) (adapter.ExecutionAdapter, error) {
		if _, err := deps.Resolve(p.CredentialRef); err != nil {
			return nil, err
		}
		builds++
		return fake.New(fake.Options{Trust: api.TrustExternal}), nil
	})
	m := NewManager(Deps{Resolve: resolve, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	list := []api.Pool{{Name: "provider", Adapter: "pools-test", Trust: api.TrustExternal, Enabled: true, CredentialRef: "secret:provider-key"}}

	m.Sync(list)
	first, err := m.Adapter("provider")
	if err != nil || builds != 1 {
		t.Fatalf("first build: %v (builds %d)", err, builds)
	}

	readErr = errors.New("connection reset by peer")
	m.Sync(list)
	if got, err := m.Adapter("provider"); err != nil || got != first || builds != 1 {
		t.Fatalf("an unreadable credential replaced a working adapter: %v (builds %d)", err, builds)
	}

	value, readErr = "key-2", nil
	m.Sync(list)
	if got, err := m.Adapter("provider"); err != nil || got == first || builds != 2 {
		t.Fatalf("a rotated credential did not rebuild the adapter: %v (builds %d)", err, builds)
	}

	readErr = store.NotFound("secret", "provider-key")
	m.Sync(list)
	if _, err := m.Adapter("provider"); err == nil {
		t.Fatal("a deleted credential left the pool's adapter in place")
	}
}
