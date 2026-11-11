//go:build !experimental_adapters

package pools

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/different-ai/differently/pkg/api"
)

// A pool stored by a build that had an experimental adapter stays registered
// in a build without it: the server starts, the pool is unavailable, and the
// reason names the build tag.
func TestStoredPoolOfExperimentalKindNamesTheBuildTag(t *testing.T) {
	m := NewManager(Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	m.Sync([]api.Pool{{Name: "burst", Adapter: "e2b", Trust: api.TrustExternal, Enabled: true}})
	_, err := m.Adapter("burst")
	if !api.IsCode(err, api.CodeProviderUnavailable) || !strings.Contains(err.Error(), "-tags experimental_adapters") {
		t.Fatalf("want provider_unavailable naming the build tag, got %v", err)
	}
	pool := api.Pool{Name: "burst"}
	m.Decorate(&pool, 0)
	if pool.Health == nil || pool.Health.Status != "unavailable" || !strings.Contains(pool.Health.Message, "experimental_adapters") {
		t.Fatalf("pool health should say why it cannot run: %+v", pool.Health)
	}
}
