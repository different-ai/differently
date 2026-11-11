package wiring

import (
	"slices"
	"testing"

	"github.com/different-ai/differently/internal/config"
	"github.com/different-ai/differently/internal/pools"
)

// The kinds config accepts and the kinds with a factory are the same set in
// every build, so a config never passes validation for a kind the binary
// cannot build, with or without the experimental_adapters tag.
func TestRegisteredKindsMatchConfig(t *testing.T) {
	Register()
	got, want := pools.Kinds(), slices.Clone(config.KnownAdapters)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("registered kinds %v, config.KnownAdapters %v (experimental built: %v)", got, want, config.ExperimentalBuilt())
	}
}
