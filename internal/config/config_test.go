package config

import (
	"strings"
	"testing"

	"github.com/different-ai/differently/pkg/api"
)

func TestParseAppliesSafeDefaults(t *testing.T) {
	c, err := Parse([]byte(`
pools:
  - name: osb
    adapter: opensandbox
    trust: private
    config: {apiURL: "http://opensandbox:8090/v1"}
  - name: burst
    adapter: daytona
    trust: private   # ignored: providers are always external
    credentials: {env: DAYTONA_API_KEY}
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Policy.External != api.ExternalNever {
		t.Fatalf("external must default to never, got %q", c.Policy.External)
	}
	if c.Pools[1].Trust != api.TrustExternal {
		t.Fatal("daytona pool was allowed to claim private trust")
	}
	if got := c.Pools[1].ToPool().CredentialRef; got != "env:DAYTONA_API_KEY" {
		t.Fatalf("credential ref %q", got)
	}
}

func TestValidateReportsProblems(t *testing.T) {
	_, err := Parse([]byte(`
store: {driver: sqlite}
pools:
  - {name: a, adapter: devhost}
  - {name: a, adapter: kata}
policy: {externalPools: [ghost]}
`))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"store.driver", "only allowed with dev", "duplicate pool", "unknown adapter", "unknown pool ghost"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
	if _, err := Parse([]byte("unknownField: 1")); err == nil {
		t.Fatal("unknown fields must be rejected")
	}
}

func TestCredentialRef(t *testing.T) {
	t.Setenv("X_TOKEN", "secret")
	v, err := ParseCredentialRef("env:X_TOKEN").Resolve()
	if err != nil || v != "secret" {
		t.Fatalf("%q %v", v, err)
	}
	if _, err := ParseCredentialRef("env:MISSING_TOKEN_XYZ").Resolve(); err == nil {
		t.Fatal("empty env should error")
	}
}
