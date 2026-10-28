// Package auth implements API keys, scopes and tenant authorization.
//
// Tokens look like dly_<keyid>_<secret>. Only a SHA-256 hash of the
// high-entropy secret is stored; the token is shown once at creation. A key
// belongs to the operator (no organization), an organization, or a single
// project, and carries scopes. Keys can only mint keys with a subset of their
// own scopes inside their own tenancy, so no key can escalate.
package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"slices"
	"strings"
	"time"

	"github.com/different-ai/differently/internal/id"
	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/pkg/api"
)

// TokenPrefix starts every Differently API token.
const TokenPrefix = "dly_"

// Scopes.
const (
	ScopeAdmin          = "admin"           // operator: every organization and admin API
	ScopeOrgAdmin       = "org:admin"       // everything inside one organization
	ScopeSandboxesRead  = "sandboxes:read"  // list and inspect sandboxes
	ScopeSandboxesWrite = "sandboxes:write" // create, delete, lifecycle, snapshots
	ScopeSandboxesExec  = "sandboxes:exec"  // run commands and transfer files
	ScopeProjectsWrite  = "projects:write"  // create and delete projects
	ScopeKeysWrite      = "keys:write"      // create and revoke keys
	ScopeUsageRead      = "usage:read"      // usage, quotas
	ScopeAuditRead      = "audit:read"      // audit events
	ScopePoliciesWrite  = "policies:write"  // org/project placement policy
)

// AllScopes lists every grantable scope.
var AllScopes = []string{ScopeAdmin, ScopeOrgAdmin, ScopeSandboxesRead, ScopeSandboxesWrite, ScopeSandboxesExec, ScopeProjectsWrite, ScopeKeysWrite, ScopeUsageRead, ScopeAuditRead, ScopePoliciesWrite}

// Principal is an authenticated caller.
type Principal struct {
	KeyID     string
	OrgID     string // empty for operator keys
	ProjectID string // set for project-scoped keys
	Scopes    []string
}

// Operator reports whether p is an installation operator.
func (p Principal) Operator() bool { return slices.Contains(p.Scopes, ScopeAdmin) }

// Has reports whether p holds scope (admin and org:admin imply tenant scopes).
func (p Principal) Has(scope string) bool {
	if slices.Contains(p.Scopes, ScopeAdmin) || slices.Contains(p.Scopes, scope) {
		return true
	}
	return scope != ScopeAdmin && slices.Contains(p.Scopes, ScopeOrgAdmin)
}

// Actor is the audit identity.
func (p Principal) Actor() string { return "key:" + p.KeyID }

// CanAccess reports whether p may act on a resource owned by org/project.
func (p Principal) CanAccess(orgID, projectID string) bool {
	switch {
	case p.Operator():
		return true
	case p.OrgID != orgID:
		return false
	case p.ProjectID != "" && p.ProjectID != projectID:
		return false
	}
	return true
}

// Require returns a permission error unless p holds scope.
func (p Principal) Require(scope string) error {
	if p.Has(scope) {
		return nil
	}
	return api.Errorf(api.CodePermissionDenied, "this key lacks the %s scope", scope).With("scope", scope)
}

// Hash returns the stored hash of a secret.
func Hash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// ParseToken splits a token into key ID and secret.
func ParseToken(token string) (keyID, secret string, ok bool) {
	if !strings.HasPrefix(token, TokenPrefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(token, TokenPrefix)
	i := strings.LastIndex(rest, "_")
	if i <= 0 || i == len(rest)-1 {
		return "", "", false
	}
	keyID, secret = rest[:i], rest[i+1:]
	if len(secret) < 24 {
		return "", "", false
	}
	return keyID, secret, true
}

// NewToken creates a key ID and token for a new key.
func NewToken() (keyID, token, secret string) {
	keyID = strings.ReplaceAll(id.New("key"), "_", "")
	secret = id.Secret(40)
	return keyID, TokenPrefix + keyID + "_" + secret, secret
}

// Verify checks token against a stored key.
func Verify(k store.KeyRecord, secret string, now time.Time) error {
	if subtle.ConstantTimeCompare([]byte(Hash(secret)), []byte(k.Hash)) != 1 {
		return api.Errorf(api.CodeUnauthenticated, "invalid API key")
	}
	if k.Revoked {
		return api.Errorf(api.CodeUnauthenticated, "API key revoked")
	}
	if k.ExpiresAt != nil && now.After(*k.ExpiresAt) {
		return api.Errorf(api.CodeUnauthenticated, "API key expired")
	}
	return nil
}

// CheckGrant validates that creator may mint a key for req. It returns the
// normalized org/project for the new key.
func CheckGrant(creator Principal, req api.CreateAPIKeyRequest) (orgID, projectID string, err error) {
	if err := creator.Require(ScopeKeysWrite); err != nil {
		return "", "", err
	}
	if len(req.Scopes) == 0 {
		return "", "", api.Errorf(api.CodeInvalidRequest, "scopes are required")
	}
	for _, s := range req.Scopes {
		if !slices.Contains(AllScopes, s) {
			return "", "", api.Errorf(api.CodeInvalidRequest, "unknown scope %q", s)
		}
		if !creator.Has(s) || (s == ScopeOrgAdmin && !creator.Operator() && !slices.Contains(creator.Scopes, ScopeOrgAdmin)) {
			return "", "", api.Errorf(api.CodePermissionDenied, "cannot grant scope %s you do not hold", s)
		}
	}
	orgID, projectID = req.OrgID, req.ProjectID
	if !creator.Operator() {
		if orgID != "" && orgID != creator.OrgID {
			return "", "", api.Errorf(api.CodePermissionDenied, "cannot create keys for another organization")
		}
		orgID = creator.OrgID
		if creator.ProjectID != "" {
			if projectID != "" && projectID != creator.ProjectID {
				return "", "", api.Errorf(api.CodePermissionDenied, "cannot create keys for another project")
			}
			projectID = creator.ProjectID
		}
	}
	if slices.Contains(req.Scopes, ScopeAdmin) && (orgID != "" || projectID != "") {
		return "", "", api.Errorf(api.CodeInvalidRequest, "the admin scope is only valid on operator keys")
	}
	if orgID == "" && !slices.Contains(req.Scopes, ScopeAdmin) {
		return "", "", api.Errorf(api.CodeInvalidRequest, "orgId is required for tenant keys")
	}
	return orgID, projectID, nil
}
