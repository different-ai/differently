//go:build !experimental_adapters

package server_test

import (
	"strings"
	"testing"

	"github.com/different-ai/differently/internal/config"
	"github.com/different-ai/differently/internal/wiring"
	"github.com/different-ai/differently/pkg/api"
)

// A server built without the experimental adapters refuses a pool of one of
// those kinds through the API, and says which build tag adds it.
func TestDefaultBuildRefusesExperimentalPoolThroughAPI(t *testing.T) {
	wiring.Register()
	h := newHarness(t, api.PlacementPolicy{})
	for _, kind := range config.ExperimentalAdapters {
		var raw string
		pool := map[string]any{"name": "p-" + kind, "adapter": kind, "trust": "external", "config": map[string]string{"apiURL": "http://backend.invalid"}, "credentials": map[string]string{"env": "PROVIDER_KEY"}}
		if got := h.call("POST", "/v1/admin/pools", operatorToken, pool, &raw); got != 400 {
			t.Fatalf("%s: status %d, want 400: %s", kind, got, raw)
		}
		if !strings.Contains(raw, api.CodeInvalidRequest) || !strings.Contains(raw, "-tags experimental_adapters") {
			t.Errorf("%s: the refusal does not name the build tag: %s", kind, raw)
		}
		if got := h.call("GET", "/v1/admin/pools/p-"+kind, operatorToken, nil, &raw); got != 404 {
			t.Errorf("%s: the refused pool was stored (status %d): %s", kind, got, raw)
		}
	}
}
