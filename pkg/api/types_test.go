package api

import (
	"testing"
	"time"
)

func TestValidAttestationName(t *testing.T) {
	for name, want := range map[string]bool{
		"soc2-type2": true, "iso27001": true, "hipaa-baa": true, "gdpr.dpa_2026": true, "7": true,
		"": false, "SOC2": false, "-soc2": false, ".x": false, "soc2 type2": false, "soc2/type2": false, "soc2,iso": false,
		"a123456789012345678901234567890123456789012345678901234567890123": false, // 64 characters
	} {
		if got := ValidAttestationName(name); got != want {
			t.Errorf("ValidAttestationName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestMissingAttestations(t *testing.T) {
	now := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	p := Pool{Attestations: map[string]Attestation{
		"soc2-type2": {ValidUntil: &future},
		"hipaa-baa":  {ValidUntil: &past},
		"iso27001":   {},
		"at-limit":   {ValidUntil: &now}, // validUntil is the first moment it no longer counts
	}}
	got := p.MissingAttestations([]string{"soc2-type2", "hipaa-baa", "iso27001", "gdpr-dpa", "at-limit"}, now)
	want := []string{"hipaa-baa (expired 2026-11-15)", "gdpr-dpa", "at-limit (expired 2026-11-16)"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if got := (Pool{}).MissingAttestations(nil, now); got != nil {
		t.Fatalf("nothing required must miss nothing, got %v", got)
	}
}
