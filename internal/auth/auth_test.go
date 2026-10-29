package auth

import (
	"testing"
	"time"

	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/pkg/api"
)

func TestTokenRoundTrip(t *testing.T) {
	keyID, token, secret := NewToken()
	gotID, gotSecret, ok := ParseToken(token)
	if !ok || gotID != keyID || gotSecret != secret {
		t.Fatalf("parse %q -> %q %q %v", token, gotID, gotSecret, ok)
	}
	rec := store.KeyRecord{APIKey: api.APIKey{ID: keyID}, Hash: Hash(secret)}
	if err := Verify(rec, secret, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := Verify(rec, secret+"x", time.Now()); !api.IsCode(err, api.CodeUnauthenticated) {
		t.Fatalf("wrong secret accepted: %v", err)
	}
	past := time.Now().Add(-time.Hour)
	rec.ExpiresAt = &past
	if err := Verify(rec, secret, time.Now()); err == nil {
		t.Fatal("expired key accepted")
	}
	for _, bad := range []string{"", "dly_", "dly_abc_short", "sk_abc_" + secret} {
		if _, _, ok := ParseToken(bad); ok {
			t.Fatalf("parsed bad token %q", bad)
		}
	}
}

// Scenario H: a project key cannot reach other tenants or escalate.
func TestProjectKeyIsContained(t *testing.T) {
	p := Principal{KeyID: "k1", OrgID: "org1", ProjectID: "proj1", Scopes: []string{ScopeSandboxesRead, ScopeSandboxesWrite, ScopeKeysWrite}}
	if !p.CanAccess("org1", "proj1") || p.CanAccess("org1", "proj2") || p.CanAccess("org2", "proj1") {
		t.Fatal("project containment broken")
	}
	if p.Has(ScopeAdmin) || p.Has(ScopeAuditRead) {
		t.Fatal("project key has scopes it was not granted")
	}
	if _, _, err := CheckGrant(p, api.CreateAPIKeyRequest{Scopes: []string{ScopeSandboxesExec}}); !api.IsCode(err, api.CodePermissionDenied) {
		t.Fatalf("escalation allowed: %v", err)
	}
	if _, _, err := CheckGrant(p, api.CreateAPIKeyRequest{Scopes: []string{ScopeSandboxesRead}, ProjectID: "proj2"}); !api.IsCode(err, api.CodePermissionDenied) {
		t.Fatalf("cross-project grant allowed: %v", err)
	}
	org, proj, err := CheckGrant(p, api.CreateAPIKeyRequest{Scopes: []string{ScopeSandboxesRead}})
	if err != nil || org != "org1" || proj != "proj1" {
		t.Fatalf("narrow grant: %s %s %v", org, proj, err)
	}
}

func TestOrgAdminAndOperator(t *testing.T) {
	orgAdmin := Principal{OrgID: "org1", Scopes: []string{ScopeOrgAdmin}}
	if !orgAdmin.Has(ScopeSandboxesExec) || orgAdmin.Has(ScopeAdmin) || orgAdmin.CanAccess("org2", "") {
		t.Fatal("org admin bounds wrong")
	}
	if _, _, err := CheckGrant(orgAdmin, api.CreateAPIKeyRequest{Scopes: []string{ScopeAdmin}}); err == nil {
		t.Fatal("org admin minted operator key")
	}
	op := Principal{Scopes: []string{ScopeAdmin}}
	if _, _, err := CheckGrant(op, api.CreateAPIKeyRequest{Scopes: []string{ScopeSandboxesRead}}); !api.IsCode(err, api.CodeInvalidRequest) {
		t.Fatalf("tenant key without org accepted: %v", err)
	}
}
