package scheduler

import (
	"context"
	"fmt"

	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/pkg/api"
)

// Template finds a template by name: the config file's first, then one set
// through the admin API. The config file wins a name both define.
//
// Like Policies, every call reads the store instead of a cached copy, so a
// template changed through any replica applies to the next decision on all of
// them, with nothing to refresh and no restart. A template that cannot be
// read is an error, never an unknown name: an unknown name is passed to the
// backend as given and may run on any pool policy permits, which is wider
// than a template with images allows.
func (s *Scheduler) Template(ctx context.Context, name string) (api.Template, bool, error) {
	if name == "" {
		return api.Template{}, false, nil
	}
	if t, ok := s.Config.Template(name); ok {
		return t, true, nil
	}
	t, err := s.Store.GetTemplate(ctx, name)
	switch {
	case api.IsCode(err, api.CodeNotFound):
		return api.Template{}, false, nil
	case err != nil:
		return api.Template{}, false, fmt.Errorf("read template %s: %w", name, err)
	}
	t.Source = api.TemplateSourceAPI
	return t, true, nil
}

// Templates lists every template as one list: the config file's in file
// order, then those set through the API by name. A stored template whose
// name the config file also declares is left out; the config file's is the
// one in force.
func (s *Scheduler) Templates(ctx context.Context) ([]api.Template, error) {
	out := make([]api.Template, 0, len(s.Config.Templates))
	declared := map[string]bool{}
	for _, t := range s.Config.Templates {
		t.Source, t.UpdatedAt, t.UpdatedBy = api.TemplateSourceConfig, nil, ""
		declared[t.Name] = true
		out = append(out, t)
	}
	stored, err := s.Store.ListTemplates(ctx)
	if err != nil {
		return nil, fmt.Errorf("read templates: %w", err)
	}
	for _, t := range stored {
		if declared[t.Name] {
			continue
		}
		t.Source = api.TemplateSourceAPI
		out = append(out, t)
	}
	return out, nil
}

// placeAgain sends a sandbox that was activated on a pool back to the queue,
// without counting an attempt or blaming the pool. It is for a template that
// changed between placement and provisioning: the pool the sandbox was placed
// on is no longer one the template offers, so booting it there would run the
// template where its definition now says it must not.
func (s *Scheduler) placeAgain(ctx context.Context, rec store.SandboxRecord, why string) {
	moved := false
	_, _ = s.Store.UpdateSandbox(ctx, rec.ID, func(r *store.SandboxRecord) error {
		if moved = r.Status == api.StatusProvisioning; !moved {
			return nil
		}
		now := s.Now()
		r.Status, r.Pool, r.Adapter, r.Trust, r.Region = api.StatusQueued, "", "", "", ""
		r.StatusReason = "placing again: " + why
		if r.Attempts > 0 {
			r.Attempts-- // the pool never saw this attempt
		}
		r.NextAttemptAt, r.UpdatedAt = now, now
		return nil
	})
	if moved {
		s.Audit.Record(ctx, api.AuditEvent{Actor: "scheduler", OrgID: rec.OrgID, ProjectID: rec.ProjectID, Action: "sandbox.placed_again", Resource: rec.ID, Details: map[string]string{"pool": rec.Pool, "template": rec.Template, "reason": why}})
		s.Wake()
	}
}
