package server

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/different-ai/differently/internal/auth"
	"github.com/different-ai/differently/internal/config"
	"github.com/different-ai/differently/internal/secrets"
	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/pkg/api"
)

var secretName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// maxSecretBytes bounds a stored credential; provider API keys are far smaller.
const maxSecretBytes = 16 << 10

// CredentialResolver resolves a pool's credential reference: env: and file:
// on this host, secret: from the store, opened with box.
func CredentialResolver(st store.Store, box *secrets.Box) func(ref string) (string, error) {
	return func(ref string) (string, error) {
		c := config.ParseCredentialRef(ref)
		if c.Secret == "" {
			return c.Resolve()
		}
		rec, err := st.GetSecret(context.Background(), c.Secret)
		if err != nil {
			return "", err
		}
		return box.Open(rec.Name, rec.Sealed)
	}
}

func (s *Server) registerSecrets(admin func(string, handlerFunc)) {
	admin("GET /v1/admin/secrets", s.listSecrets)
	admin("PUT /v1/admin/secrets/{name}", s.putSecret)
	admin("DELETE /v1/admin/secrets/{name}", s.deleteSecret)
}

func (s *Server) listSecrets(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	list, err := s.Store.ListSecrets(r.Context())
	if err != nil {
		return err
	}
	pools, err := s.Store.ListPools(r.Context())
	if err != nil {
		return err
	}
	out := api.List[api.Secret]{Items: []api.Secret{}}
	for _, rec := range list {
		sec := rec.Secret
		sec.UsedBy = []string{}
		for _, p := range pools {
			if p.CredentialRef == "secret:"+sec.Name {
				sec.UsedBy = append(sec.UsedBy, p.Name)
			}
		}
		out.Items = append(out.Items, sec)
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// putSecret stores or replaces a provider credential. Pools that use it pick
// up the new value at once on this replica, and within seconds on the others.
func (s *Server) putSecret(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	name := r.PathValue("name")
	if !secretName.MatchString(name) {
		return api.Errorf(api.CodeInvalidRequest, "secret names are lowercase letters, digits, '.', '_' or '-', up to 63 characters")
	}
	var req api.PutSecretRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	value := strings.TrimSpace(req.Value)
	switch {
	case value == "":
		return api.Errorf(api.CodeInvalidRequest, "value is required")
	case len(value) > maxSecretBytes:
		return api.Errorf(api.CodeInvalidRequest, "value is larger than %d bytes", maxSecretBytes)
	}
	sealed, err := s.Secrets.Seal(name, value)
	if errors.Is(err, secrets.ErrNoKey) {
		return api.Errorf(api.CodeInvalidState, "this server has no secrets key (%s), so it cannot store credentials; reference an env: or file: credential instead", s.secretsKeySource())
	}
	if err != nil {
		return err
	}
	now := s.now()
	rec := store.SecretRecord{Secret: api.Secret{Name: name, Fingerprint: secrets.Fingerprint(value), CreatedAt: now, CreatedBy: p.Actor(), UpdatedAt: now, UpdatedBy: p.Actor()}, Sealed: sealed}
	status := http.StatusCreated
	if old, err := s.Store.GetSecret(r.Context(), name); err == nil {
		rec.CreatedAt, rec.CreatedBy, status = old.CreatedAt, old.CreatedBy, http.StatusOK
	}
	if err := s.Store.PutSecret(r.Context(), rec); err != nil {
		return err
	}
	s.audit(r, p, "secret.put", name, "", "", map[string]string{"fingerprint": rec.Fingerprint})
	s.Pools.Recheck()
	rec.UsedBy = []string{}
	if pools, err := s.Store.ListPools(r.Context()); err == nil {
		for _, pool := range pools {
			if pool.CredentialRef == "secret:"+name {
				rec.UsedBy = append(rec.UsedBy, pool.Name)
			}
		}
	}
	writeJSON(w, status, rec.Secret)
	return nil
}

func (s *Server) deleteSecret(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	name := r.PathValue("name")
	if err := s.Store.DeleteSecret(r.Context(), name); err != nil {
		return err
	}
	s.audit(r, p, "secret.delete", name, "", "", nil)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// checkPoolCredential refuses a pool whose credential names a stored secret
// that does not exist yet, so a typo fails at registration, not first use.
func (s *Server) checkPoolCredential(ctx context.Context, ref string) error {
	c := config.ParseCredentialRef(ref)
	if c.Secret == "" {
		return nil
	}
	if s.Secrets == nil {
		return api.Errorf(api.CodeInvalidState, "this server has no secrets key (%s), so secret: credentials cannot be opened", s.secretsKeySource())
	}
	if _, err := s.Store.GetSecret(ctx, c.Secret); err != nil {
		return api.Errorf(api.CodeInvalidRequest, "secret %s does not exist; store it first with PUT /v1/admin/secrets/%s", c.Secret, c.Secret)
	}
	return nil
}

func (s *Server) secretsKeySource() string {
	return strings.Join(slices.DeleteFunc([]string{s.Config.Secrets.KeyEnv, s.Config.Secrets.KeyFile}, func(v string) bool { return v == "" }), " or ")
}
