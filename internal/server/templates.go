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
	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/pkg/api"
)

// Templates set through the API sit beside the config file's. Rules that keep
// the two apart and keep a change from widening where a template may run:
//
//   - The config file wins: a name it declares cannot be set, changed or
//     removed here (409, saying so), and a stored template with that name is
//     ignored while the file declares it.
//   - PUT merges. An image is added or replaced only by naming its pool, so
//     no call drops a pool's image by leaving it out.
//   - A template's last image cannot be removed. Without one the name would
//     be passed to every pool's backend as given, so that takes removing the
//     template, which says what it does.
//
// A template only ever narrows placement: it is one more check a pool has to
// pass in policy.Decide, beside the placement policy, never instead of it.

var templateName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,62}$`)

const (
	maxTemplatePools = 256  // images or pools one template may name
	maxTemplateText  = 1024 // bytes in a description or an image reference
)

func (s *Server) registerTemplates(admin func(string, handlerFunc)) {
	admin("PUT /v1/admin/templates/{name}", s.putTemplate)
	admin("DELETE /v1/admin/templates/{name}", s.deleteTemplate)
	admin("DELETE /v1/admin/templates/{name}/images/{pool}", s.deleteTemplateImage)
}

func (s *Server) listTemplates(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	list, err := s.Scheduler.Templates(r.Context())
	if err != nil {
		s.Log.Warn("templates unavailable", "err", err)
		return api.Errorf(api.CodeProviderUnavailable, "templates could not be read; retry shortly")
	}
	writeJSON(w, http.StatusOK, api.List[api.Template]{Items: append([]api.Template{}, list...)})
	return nil
}

// declaredInConfig refuses a change to a template the config file declares.
// The file is read at every start, so a change made here could not last.
func (s *Server) declaredInConfig(name, what string) error {
	if _, ok := s.Config.Template(name); ok {
		return api.Errorf(api.CodeConflict, "template %s is declared in the config file; %s it there", name, what).With("template", name)
	}
	return nil
}

// knownPools refuses pool names nobody registered, naming the first one.
func (s *Server) knownPools(ctx context.Context, field string, names []string) error {
	pools, err := s.Store.ListPools(ctx)
	if err != nil {
		return err
	}
	have := make([]string, 0, len(pools))
	for _, p := range pools {
		have = append(have, p.Name)
	}
	for _, n := range names {
		if !slices.Contains(have, n) {
			return api.Errorf(api.CodeInvalidRequest, "%s names pool %s, which does not exist (pools: %s)", field, n, strings.Join(have, ", ")).With("pool", n)
		}
	}
	return nil
}

// putTemplate sets a template through the API, merging into what is stored:
// each image named is added or replaced and the others stay. Every replica
// reads templates from the store when it decides, so the change applies to
// the next placement everywhere, with no restart.
func (s *Server) putTemplate(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	name := r.PathValue("name")
	if !templateName.MatchString(name) {
		return api.Errorf(api.CodeInvalidRequest, "template names are letters, digits, '.', '_', '-' or ':', starting with a letter or digit, up to 63 characters")
	}
	if err := s.declaredInConfig(name, "change"); err != nil {
		return err
	}
	var req api.PutTemplateRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	if req.Description == nil && req.Pools == nil && len(req.Images) == 0 {
		return api.Errorf(api.CodeInvalidRequest, "nothing to set: give images (pool: image), a description or pools")
	}
	if req.Description != nil && len(*req.Description) > maxTemplateText {
		return api.Errorf(api.CodeInvalidRequest, "description is longer than %d bytes", maxTemplateText)
	}
	images := make(map[string]string, len(req.Images))
	named := make([]string, 0, len(req.Images))
	for pool, image := range req.Images {
		images[pool] = strings.TrimSpace(image)
		named = append(named, pool)
	}
	slices.Sort(named)
	for _, pool := range named {
		switch {
		case images[pool] == "":
			return api.Errorf(api.CodeInvalidRequest, "images.%s is empty; to stop offering the template on that pool, remove its image (DELETE /v1/admin/templates/%s/images/%s)", pool, name, pool).With("pool", pool)
		case len(images[pool]) > maxTemplateText:
			return api.Errorf(api.CodeInvalidRequest, "images.%s is longer than %d bytes", pool, maxTemplateText).With("pool", pool)
		}
	}
	if err := s.knownPools(r.Context(), "images", named); err != nil {
		return err
	}
	var pools []string
	if req.Pools != nil {
		for _, pool := range *req.Pools {
			if !slices.Contains(pools, pool) {
				pools = append(pools, pool)
			}
		}
		if err := s.knownPools(r.Context(), "pools", pools); err != nil {
			return err
		}
	}
	now, created := s.now(), false
	out, err := s.Store.UpdateTemplate(r.Context(), name, func(t *api.Template, found bool) error {
		created = !found
		if req.Description != nil {
			t.Description = *req.Description
		}
		if req.Pools != nil {
			t.Pools = pools
		}
		for pool, image := range images {
			if t.Images == nil {
				t.Images = map[string]string{}
			}
			t.Images[pool] = image
		}
		t.Name, t.Source, t.UpdatedAt, t.UpdatedBy = name, api.TemplateSourceAPI, &now, p.Actor()
		if len(t.Images) > maxTemplatePools || len(t.Pools) > maxTemplatePools {
			return api.Errorf(api.CodeInvalidRequest, "a template names at most %d pools", maxTemplatePools)
		}
		// The rules the config file's templates are held to.
		if errs := config.ValidateTemplate(*t); len(errs) > 0 {
			return api.Errorf(api.CodeInvalidRequest, "%v", errors.Join(errs...))
		}
		return nil
	})
	if err != nil {
		return err
	}
	details := map[string]string{}
	if len(named) > 0 {
		set := make([]string, 0, len(named))
		for _, pool := range named {
			set = append(set, pool+"="+images[pool])
		}
		details["images"] = strings.Join(set, ", ")
	}
	if req.Pools != nil {
		details["pools"] = strings.Join(pools, ",")
	}
	if created {
		details["created"] = "true"
	}
	s.audit(r, p, "template.put", name, "", "", details)
	s.Scheduler.Wake()
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, out)
	return nil
}

// deleteTemplateImage stops offering a template on one pool. The last image
// stays: see the rules at the top of this file.
func (s *Server) deleteTemplateImage(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	name, pool := r.PathValue("name"), r.PathValue("pool")
	if err := s.declaredInConfig(name, "change"); err != nil {
		return err
	}
	now, removed := s.now(), ""
	out, err := s.Store.UpdateTemplate(r.Context(), name, func(t *api.Template, found bool) error {
		switch {
		case !found:
			return store.NotFound("template", name)
		case t.Images[pool] == "":
			return api.Errorf(api.CodeNotFound, "template %s has no image for pool %s", name, pool).With("pool", pool)
		case len(t.Images) == 1:
			return api.Errorf(api.CodeInvalidState, "pool %s holds the last image of template %s: with none left, the name would be passed to every pool's backend as given. Remove the template (DELETE /v1/admin/templates/%s), or give it an image on another pool first", pool, name, name).With("pool", pool)
		}
		removed = t.Images[pool]
		delete(t.Images, pool)
		t.Source, t.UpdatedAt, t.UpdatedBy = api.TemplateSourceAPI, &now, p.Actor()
		return nil
	})
	if err != nil {
		return err
	}
	s.audit(r, p, "template.image_delete", name, "", "", map[string]string{"pool": pool, "image": removed})
	writeJSON(w, http.StatusOK, out)
	return nil
}

// deleteTemplate removes a template set through the API. From then on a
// request that names it has the name passed to the backend as given.
func (s *Server) deleteTemplate(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	name := r.PathValue("name")
	if err := s.declaredInConfig(name, "remove"); err != nil {
		return err
	}
	if err := s.Store.DeleteTemplate(r.Context(), name); err != nil {
		return err
	}
	s.audit(r, p, "template.delete", name, "", "", nil)
	w.WriteHeader(http.StatusNoContent)
	return nil
}
