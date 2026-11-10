//go:build !experimental_adapters

package config

import (
	"slices"
	"strings"
	"testing"
)

// The default build holds only the proven kinds. A pool of an experimental
// kind is refused with the build tag that adds it, never accepted and left to
// fail later.
func TestDefaultBuildRefusesExperimentalKinds(t *testing.T) {
	if ExperimentalBuilt() {
		t.Fatal("built without the tag, but ExperimentalBuilt is true")
	}
	if want := []string{"opensandbox", "daytona", "freestyle", "remote", "devhost", "fake"}; !slices.Equal(KnownAdapters, want) {
		t.Fatalf("default build knows %v, want %v", KnownAdapters, want)
	}
	for _, kind := range ExperimentalAdapters {
		_, err := Parse([]byte("pools:\n  - {name: p, adapter: " + kind + ", trust: external, config: {apiURL: \"http://x\"}, credentials: {env: KEY}}\n"))
		if err == nil {
			t.Errorf("%s: accepted by a build that has no such adapter", kind)
			continue
		}
		for _, want := range []string{"the " + kind + " adapter is experimental", "-tags experimental_adapters"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error %q does not say %q", kind, err, want)
			}
		}
		if strings.Contains(err.Error(), "unknown adapter") {
			t.Errorf("%s: reported as unknown, not as left out of the build: %v", kind, err)
		}
	}
}
