package server

import (
	"context"
	"net/http"

	"github.com/different-ai/differently/internal/auth"
	"github.com/different-ai/differently/internal/stacks"
	"github.com/different-ai/differently/pkg/api"
)

func (s *Server) registerStacks(admin func(string, handlerFunc)) {
	admin("GET /v1/admin/stacks", s.listStacks)
	admin("GET /v1/admin/stacks/{name}", s.getStack)
	admin("PUT /v1/admin/stacks/{name}", s.putStack)
	admin("DELETE /v1/admin/stacks/{name}", s.deleteStack)
	admin("POST /v1/admin/stacks/{name}/check", s.checkStack)
}

func (s *Server) listStacks(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	list, err := s.Store.ListStacks(r.Context())
	if err != nil {
		return err
	}
	out := api.List[api.Stack]{Items: []api.Stack{}}
	for _, st := range list {
		s.Stacks.Decorate(&st)
		out.Items = append(out.Items, st)
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) getStack(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	st, err := s.Store.GetStack(r.Context(), r.PathValue("name"))
	if err != nil {
		return err
	}
	s.Stacks.Decorate(&st)
	writeJSON(w, http.StatusOK, st)
	return nil
}

// putStack registers or replaces a stack to watch and probes it at once, so
// the answer already says whether each component is reachable from here.
func (s *Server) putStack(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	name := r.PathValue("name")
	if !stacks.ValidName(name) {
		return api.Errorf(api.CodeInvalidRequest, "stack names are lowercase letters, digits, '.', '_' or '-', up to 63 characters")
	}
	var req api.PutStackRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	kind, components, err := stacks.Normalize(req.Kind, req.Components)
	if err != nil {
		return err
	}
	for _, c := range components {
		if err := s.checkPoolCredential(r.Context(), c.CredentialRef); err != nil {
			return err
		}
	}
	now := s.now()
	st := api.Stack{Name: name, Kind: kind, Components: components, CreatedAt: now, CreatedBy: p.Actor(), UpdatedAt: now, UpdatedBy: p.Actor()}
	status := http.StatusCreated
	if old, err := s.Store.GetStack(r.Context(), name); err == nil {
		st.CreatedAt, st.CreatedBy, status = old.CreatedAt, old.CreatedBy, http.StatusOK
	} else if !api.IsCode(err, api.CodeNotFound) {
		return err
	}
	if err := s.Store.PutStack(r.Context(), st); err != nil {
		return err
	}
	s.audit(r, p, "stack.put", name, "", "", map[string]string{"kind": kind, "components": itoa64(int64(len(components)))})
	s.Stacks.Check(r.Context(), st)
	s.Stacks.Decorate(&st)
	writeJSON(w, status, st)
	return nil
}

func (s *Server) deleteStack(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	name := r.PathValue("name")
	if err := s.Store.DeleteStack(r.Context(), name); err != nil {
		return err
	}
	s.audit(r, p, "stack.delete", name, "", "", nil)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// checkStack probes a stack now instead of waiting for the next round.
func (s *Server) checkStack(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	st, err := s.Store.GetStack(r.Context(), r.PathValue("name"))
	if err != nil {
		return err
	}
	s.Stacks.Check(r.Context(), st)
	s.Stacks.Decorate(&st)
	writeJSON(w, http.StatusOK, st)
	return nil
}

// stackUsingSecret names a stack whose probes send the stored secret.
func (s *Server) stackUsingSecret(ctx context.Context, secret string) (string, error) {
	list, err := s.Store.ListStacks(ctx)
	if err != nil {
		return "", err
	}
	for _, st := range list {
		for _, c := range st.Components {
			if c.CredentialRef == "secret:"+secret {
				return st.Name, nil
			}
		}
	}
	return "", nil
}
