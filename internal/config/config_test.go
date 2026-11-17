package config

import (
	"slices"
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

func TestManagedProvidersAreAlwaysExternal(t *testing.T) {
	c, err := Parse([]byte(`
pools:
  - {name: fs, adapter: freestyle, trust: private, credentials: {env: FREESTYLE_API_KEY}}
  - {name: fs2, adapter: freestyle}
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range c.Pools {
		if p.Trust != api.TrustExternal {
			t.Fatalf("pool %s on adapter %s came out %q", p.Name, p.Adapter, p.Trust)
		}
	}
}

func TestValidateReportsProblems(t *testing.T) {
	_, err := Parse([]byte(`
store: {driver: sqlite}
pools:
  - {name: a, adapter: devhost}
  - {name: a, adapter: kata}
policy: {externalPools: [ghost]}
templates:
  - {name: workbot, pools: [a], images: {b: snap-1}}
  - {name: workbot}
`))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"store.driver", "only allowed with dev", "duplicate pool", "unknown adapter", "unknown pool ghost", "duplicate template", "images names pool b"} {
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

// A new adapter kind never becomes private by omission: unless it is listed
// as external or private by default, its pools must declare their trust.
func TestNewAdapterKindsMustDeclareTrust(t *testing.T) {
	saved := KnownAdapters
	KnownAdapters = append(slices.Clone(KnownAdapters), "newcloud")
	t.Cleanup(func() { KnownAdapters = saved })
	cfg := func(trust string) []byte {
		return []byte("pools:\n  - name: p\n    adapter: newcloud\n" + trust + "    credentials: {env: NEWCLOUD_KEY}\n")
	}
	if _, err := Parse(cfg("")); err == nil || !strings.Contains(err.Error(), "set trust explicitly") {
		t.Fatalf("an undeclared new kind must be rejected, got %v", err)
	}
	c, err := Parse(cfg("    trust: external\n"))
	if err != nil || c.Pools[0].Trust != api.TrustExternal {
		t.Fatalf("declared external: %+v %v", c.Pools, err)
	}
}

// Every adapter kind, in this build or behind the experimental tag, sits in
// exactly one trust list, so adding a kind means deciding who operates it.
func TestEveryAdapterKindHasATrustRule(t *testing.T) {
	for _, kind := range slices.Concat(DefaultAdapters, ExperimentalAdapters) {
		n := 0
		for _, list := range [][]string{externalKinds, privateByDefaultKinds, declaredTrustKinds} {
			if slices.Contains(list, kind) {
				n++
			}
		}
		if n != 1 {
			t.Errorf("adapter %s is in %d trust lists; put it in exactly one of externalKinds, privateByDefaultKinds or declaredTrustKinds", kind, n)
		}
	}
}

// CORS origins must be exact origins, never wildcards or URLs with paths.
func TestCORSOriginsMustBeExact(t *testing.T) {
	for _, o := range []string{"*", "https://*.example.com", "console.example.com", "https://console.example.com/app", "ftp://x.example"} {
		c := Default()
		c.CORS.AllowedOrigins = []string{o}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "cors.allowedOrigins") {
			t.Errorf("%q: want a cors.allowedOrigins error, got %v", o, err)
		}
	}
	c := Default()
	c.CORS.AllowedOrigins = []string{"https://console.example.com", "http://localhost:4321"}
	if err := c.Validate(); err != nil && strings.Contains(err.Error(), "cors") {
		t.Fatalf("exact origins are valid: %v", err)
	}
}

// Attestations are declared on pools and required by classifications and the
// global policy; a malformed name is refused wherever it appears.
func TestAttestationsInConfig(t *testing.T) {
	c, err := Parse([]byte(`
classifications:
  - {name: internal, external: overflow}
  - {name: regulated, external: overflow, requiredAttestations: [soc2-type2, hipaa-baa]}
pools:
  - name: osb
    adapter: opensandbox
    trust: private
    config: {apiURL: "http://opensandbox:8090/v1"}
  - name: burst
    adapter: daytona
    credentials: {env: DAYTONA_API_KEY}
    attestations:
      soc2-type2: {evidence: "https://trust.example.com", validUntil: "2027-06-30T00:00:00Z"}
      hipaa-baa: {evidence: "BAA signed 2026-09-01"}
policy:
  requiredAttestations: [gdpr-dpa]
`))
	if err != nil {
		t.Fatal(err)
	}
	burst := c.Pools[1].ToPool()
	if a := burst.Attestations["soc2-type2"]; a.Evidence != "https://trust.example.com" || a.ValidUntil == nil || a.ValidUntil.Year() != 2027 {
		t.Fatalf("attestation not carried to the pool: %+v", burst.Attestations)
	}
	if class, _ := c.Classification("regulated"); !slices.Equal(class.RequiredAttestations, []string{"soc2-type2", "hipaa-baa"}) {
		t.Fatalf("classification requirements: %+v", class)
	}
	for _, bad := range []string{
		"classifications:\n  - {name: internal, external: never, requiredAttestations: [SOC2]}\n",
		"pools:\n  - name: osb\n    adapter: opensandbox\n    trust: private\n    config: {apiURL: \"http://o/v1\"}\n    attestations: {\"soc2 type2\": {}}\n",
		"policy:\n  requiredAttestations: [\"\"]\n",
	} {
		if _, err := Parse([]byte(bad)); err == nil || !strings.Contains(err.Error(), "attestation name") {
			t.Errorf("malformed attestation name accepted: %v\n%s", err, bad)
		}
	}
}
