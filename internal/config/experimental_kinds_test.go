//go:build experimental_adapters

package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/different-ai/differently/pkg/api"
)

// With the tag, every experimental kind is one the server can build.
func TestExperimentalKindsAreKnown(t *testing.T) {
	if !ExperimentalBuilt() {
		t.Fatal("built with the tag, but ExperimentalBuilt is false")
	}
	for _, kind := range ExperimentalAdapters {
		if !slices.Contains(KnownAdapters, kind) || UnavailableAdapter(kind) != "" {
			t.Errorf("%s should be available in a build with -tags %s", kind, ExperimentalTag)
		}
	}
}

// Managed providers are external whatever a pool says; E2B, which can also
// be self-hosted, must declare who operates it and is private only when it
// says so.
func TestProviderKindsTrust(t *testing.T) {
	pool := func(kind, trust string) []byte {
		return []byte("pools:\n  - name: p\n    adapter: " + kind + "\n" + trust + "    credentials: {env: KEY}\n")
	}
	for _, kind := range []string{"boat", "morph"} {
		for _, trust := range []string{"", "    trust: private\n"} {
			c, err := Parse(pool(kind, trust))
			if err != nil || c.Pools[0].Trust != api.TrustExternal {
				t.Errorf("%s with %q: %+v %v", kind, trust, c.Pools, err)
			}
		}
	}
	if _, err := Parse(pool("e2b", "")); err == nil || !strings.Contains(err.Error(), "set trust explicitly") {
		t.Fatalf("e2b without trust must be rejected, got %v", err)
	}
	for _, trust := range []string{api.TrustExternal, api.TrustPrivate} {
		c, err := Parse(pool("e2b", "    trust: "+trust+"\n"))
		if err != nil || c.Pools[0].Trust != trust {
			t.Errorf("e2b declared %s: %+v %v", trust, c.Pools, err)
		}
	}
}
