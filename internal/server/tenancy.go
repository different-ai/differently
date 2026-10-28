package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/different-ai/differently/internal/auth"
	"github.com/different-ai/differently/internal/id"
	"github.com/different-ai/differently/internal/policy"
	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/pkg/api"
)

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func (s *Server) tenancyRoutes(authed func(string, handlerFunc)) {
	authed("GET /v1/projects", s.listProjects)
	authed("POST /v1/projects", s.createProject)
	authed("GET /v1/projects/{id}", s.getProject)
	authed("DELETE /v1/projects/{id}", s.deleteProject)
	authed("GET /v1/api-keys", s.listKeys)
	authed("POST /v1/api-keys", s.createKey)
	authed("DELETE /v1/api-keys/{id}", s.revokeKey)
	authed("GET /v1/quotas", s.quotas)
	authed("GET /v1/usage", s.usage)
	authed("GET /v1/audit-events", s.listAudit)
	authed("GET /v1/policies", s.listPolicies)
	authed("PUT /v1/policies/{scope}", s.putPolicy)
	authed("DELETE /v1/policies/{scope}", s.deletePolicy)
	authed("GET /v1/capabilities", s.capabilities)
	authed("GET /v1/profiles", func(w http.ResponseWriter, _ *http.Request, _ auth.Principal) error {
		writeJSON(w, 200, api.List[api.Profile]{Items: s.Config.Profiles})
		return nil
	})
	authed("GET /v1/templates", func(w http.ResponseWriter, _ *http.Request, _ auth.Principal) error {
		writeJSON(w, 200, api.List[api.Template]{Items: append([]api.Template{}, s.Config.Templates...)})
		return nil
	})
	authed("GET /v1/classifications", func(w http.ResponseWriter, _ *http.Request, _ auth.Principal) error {
		writeJSON(w, 200, api.List[api.Classification]{Items: s.Config.Classifications})
		return nil
	})
}

// orgFor resolves the organization a tenant call targets.
func orgFor(r *http.Request, p auth.Principal) (string, error) {
	if !p.Operator() {
		return p.OrgID, nil
	}
	if v := r.URL.Query().Get("orgId"); v != "" {
		return v, nil
	}
	return "", api.Errorf(api.CodeInvalidRequest, "operator keys must pass orgId")
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	orgID := p.OrgID
	if p.Operator() {
		orgID = r.URL.Query().Get("orgId")
	}
	list, err := s.Store.ListProjects(r.Context(), orgID)
	if err != nil {
		return err
	}
	out := api.List[api.Project]{Items: []api.Project{}}
	for _, pr := range list {
		if p.CanAccess(pr.OrgID, pr.ID) {
			out.Items = append(out.Items, pr)
		}
	}
	writeJSON(w, 200, out)
	return nil
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if err := p.Require(auth.ScopeProjectsWrite); err != nil {
		return err
	}
	if p.ProjectID != "" {
		return api.Errorf(api.CodePermissionDenied, "project keys cannot create projects")
	}
	var req struct {
		Name  string     `json:"name"`
		OrgID string     `json:"orgId,omitempty"`
		Quota *api.Quota `json:"quota,omitempty"`
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	if strings.TrimSpace(req.Name) == "" {
		return api.Errorf(api.CodeInvalidRequest, "name is required")
	}
	orgID := p.OrgID
	if p.Operator() {
		orgID = req.OrgID
	}
	if _, err := s.Store.GetOrg(r.Context(), orgID); err != nil {
		return api.Errorf(api.CodeNotFound, "organization %q not found", orgID)
	}
	proj := api.Project{ID: id.New("prj"), OrgID: orgID, Name: req.Name, Quota: s.Config.Defaults.ProjectQuota, CreatedAt: s.now()}
	if req.Quota != nil {
		if !p.Operator() && !slices.Contains(p.Scopes, auth.ScopeOrgAdmin) {
			return api.Errorf(api.CodePermissionDenied, "setting a project quota requires org:admin")
		}
		proj.Quota = *req.Quota
	}
	if err := s.Store.CreateProject(r.Context(), proj); err != nil {
		return err
	}
	s.audit(r, p, "project.create", proj.ID, proj.OrgID, proj.ID, map[string]string{"name": proj.Name})
	writeJSON(w, http.StatusCreated, proj)
	return nil
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	proj, err := s.Store.GetProject(r.Context(), r.PathValue("id"))
	if err != nil || !p.CanAccess(proj.OrgID, proj.ID) {
		return api.Errorf(api.CodeNotFound, "project %s not found", r.PathValue("id"))
	}
	writeJSON(w, 200, proj)
	return nil
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if err := p.Require(auth.ScopeProjectsWrite); err != nil {
		return err
	}
	proj, err := s.Store.GetProject(r.Context(), r.PathValue("id"))
	if err != nil || !p.CanAccess(proj.OrgID, proj.ID) || p.ProjectID != "" {
		return api.Errorf(api.CodeNotFound, "project %s not found", r.PathValue("id"))
	}
	if err := s.Store.DeleteProject(r.Context(), proj.ID); err != nil {
		return err
	}
	s.audit(r, p, "project.delete", proj.ID, proj.OrgID, proj.ID, nil)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if err := p.Require(auth.ScopeKeysWrite); err != nil {
		return err
	}
	orgID := p.OrgID
	if p.Operator() {
		orgID = r.URL.Query().Get("orgId")
	}
	list, err := s.Store.ListKeys(r.Context(), orgID, p.ProjectID)
	if err != nil {
		return err
	}
	out := api.List[api.APIKey]{Items: []api.APIKey{}}
	for _, k := range list {
		if p.Operator() || k.OrgID != "" {
			out.Items = append(out.Items, k.APIKey)
		}
	}
	writeJSON(w, 200, out)
	return nil
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	var req api.CreateAPIKeyRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	orgID, projectID, err := auth.CheckGrant(p, req)
	if err != nil {
		return err
	}
	if orgID != "" {
		if _, err := s.Store.GetOrg(r.Context(), orgID); err != nil {
			return api.Errorf(api.CodeNotFound, "organization %s not found", orgID)
		}
	}
	if projectID != "" {
		proj, err := s.Store.GetProject(r.Context(), projectID)
		if err != nil || proj.OrgID != orgID {
			return api.Errorf(api.CodeNotFound, "project %s not found", projectID)
		}
	}
	keyID, token, secret := auth.NewToken()
	k := store.KeyRecord{
		APIKey: api.APIKey{ID: keyID, Name: req.Name, OrgID: orgID, ProjectID: projectID, Scopes: req.Scopes, CreatedBy: p.Actor(), CreatedAt: s.now()},
		Hash:   auth.Hash(secret),
	}
	if req.TTLHours > 0 {
		exp := s.now().Add(time.Duration(req.TTLHours) * time.Hour)
		k.ExpiresAt = &exp
	}
	if err := s.Store.CreateKey(r.Context(), k); err != nil {
		return err
	}
	s.audit(r, p, "key.create", keyID, orgID, projectID, map[string]string{"scopes": strings.Join(req.Scopes, ","), "name": req.Name})
	writeJSON(w, http.StatusCreated, api.CreateAPIKeyResponse{APIKey: k.APIKey, Token: token})
	return nil
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if err := p.Require(auth.ScopeKeysWrite); err != nil {
		return err
	}
	k, err := s.Store.GetKey(r.Context(), r.PathValue("id"))
	if err != nil || (!p.Operator() && (k.OrgID != p.OrgID || (p.ProjectID != "" && k.ProjectID != p.ProjectID))) {
		return api.Errorf(api.CodeNotFound, "api key %s not found", r.PathValue("id"))
	}
	if _, err := s.Store.UpdateKey(r.Context(), k.ID, func(x *store.KeyRecord) error { x.Revoked = true; return nil }); err != nil {
		return err
	}
	s.audit(r, p, "key.revoke", k.ID, k.OrgID, k.ProjectID, nil)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) quotas(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if err := p.Require(auth.ScopeUsageRead); err != nil {
		return err
	}
	orgID, err := orgFor(r, p)
	if err != nil {
		return err
	}
	org, err := s.Store.GetOrg(r.Context(), orgID)
	if err != nil {
		return err
	}
	ou, err := s.Store.Usage(r.Context(), orgID, "")
	if err != nil {
		return err
	}
	rep := api.QuotaReport{OrgID: orgID, OrgQuota: org.Quota, OrgUsage: ou}
	projectID := r.URL.Query().Get("projectId")
	if p.ProjectID != "" {
		projectID = p.ProjectID
	}
	if projectID != "" {
		proj, err := s.Store.GetProject(r.Context(), projectID)
		if err != nil || proj.OrgID != orgID {
			return api.Errorf(api.CodeNotFound, "project %s not found", projectID)
		}
		pu, _ := s.Store.Usage(r.Context(), orgID, projectID)
		rep.ProjectID, rep.ProjectQuota, rep.ProjectUsage = projectID, &proj.Quota, &pu
	}
	writeJSON(w, 200, rep)
	return nil
}

// usage aggregates estimated usage from sandbox records. Run time is the
// accumulated running time of sandboxes that overlap the window; costs use
// each pool's configured cost model. Reconciled provider invoices are not
// imported yet, so every figure is an estimate.
func (s *Server) usage(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if err := p.Require(auth.ScopeUsageRead); err != nil {
		return err
	}
	now := s.now()
	from, to := now.Add(-30*24*time.Hour), now
	if v, err := time.Parse(time.RFC3339, r.URL.Query().Get("from")); err == nil {
		from = v
	}
	if v, err := time.Parse(time.RFC3339, r.URL.Query().Get("to")); err == nil {
		to = v
	}
	groupBy := r.URL.Query().Get("groupBy")
	if groupBy == "" {
		groupBy = "pool"
	}
	if !slices.Contains([]string{"pool", "project", "org", "trust"}, groupBy) {
		return api.Errorf(api.CodeInvalidRequest, "groupBy must be pool, project, org or trust")
	}
	f := store.SandboxFilter{OrgID: p.OrgID, ProjectID: p.ProjectID}
	if p.Operator() {
		f.OrgID = r.URL.Query().Get("orgId")
	}
	recs, err := s.Store.ListSandboxes(r.Context(), f)
	if err != nil {
		return err
	}
	poolList, _ := s.Store.ListPools(r.Context())
	costs := map[string]api.CostModel{}
	for _, pl := range poolList {
		costs[pl.Name] = pl.CostModel
	}
	lines := map[string]*api.UsageLine{}
	total := api.UsageLine{Key: "total"}
	for _, rec := range recs {
		if rec.CreatedAt.After(to) || (rec.EndedAt != nil && rec.EndedAt.Before(from)) {
			continue
		}
		key := map[string]string{"pool": rec.Pool, "project": rec.ProjectID, "org": rec.OrgID, "trust": rec.Trust}[groupBy]
		if key == "" {
			key = "(unplaced)"
		}
		l, ok := lines[key]
		if !ok {
			l = &api.UsageLine{Key: key}
			lines[key] = l
		}
		run := rec.RunSeconds
		if rec.RunningSince != nil {
			run += now.Sub(*rec.RunningSince).Seconds()
		}
		add := api.UsageLine{
			Sandboxes: 1, RunSeconds: run, QueueSeconds: rec.QueueSeconds,
			CPUSeconds: run * float64(rec.Resources.CPU), MemoryGiBSeconds: run * float64(rec.Resources.MemoryMiB) / 1024,
			EstimatedCost: run / 3600 * costs[rec.Pool].HourlyCost(rec.Resources),
		}
		for _, t := range []*api.UsageLine{l, &total} {
			t.Sandboxes += add.Sandboxes
			t.RunSeconds += add.RunSeconds
			t.QueueSeconds += add.QueueSeconds
			t.CPUSeconds += add.CPUSeconds
			t.MemoryGiBSeconds += add.MemoryGiBSeconds
			t.EstimatedCost += add.EstimatedCost
		}
	}
	rep := api.UsageReport{From: from, To: to, GroupBy: groupBy, Total: total, Note: "estimated from configured pool cost models; provider invoices are not reconciled"}
	for _, l := range lines {
		rep.Lines = append(rep.Lines, *l)
	}
	sort.Slice(rep.Lines, func(i, j int) bool { return rep.Lines[i].Key < rep.Lines[j].Key })
	writeJSON(w, 200, rep)
	return nil
}

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if err := p.Require(auth.ScopeAuditRead); err != nil {
		return err
	}
	f := store.AuditFilter{OrgID: p.OrgID, Resource: r.URL.Query().Get("resource"), Action: r.URL.Query().Get("action"), AfterSeq: int64(queryInt(r, "afterSeq", 0)), Limit: min(max(queryInt(r, "limit", 100), 1), 1000)}
	if p.Operator() {
		f.OrgID = r.URL.Query().Get("orgId")
	}
	events, err := s.Store.ListAudit(r.Context(), f)
	if err != nil {
		return err
	}
	if events == nil {
		events = []api.AuditEvent{}
	}
	out := api.List[api.AuditEvent]{Items: events}
	if len(events) == f.Limit {
		out.NextCursor = itoa64(events[len(events)-1].Seq)
	}
	writeJSON(w, 200, out)
	return nil
}

func itoa64(n int64) string { b, _ := json.Marshal(n); return string(b) }

// policyScopeAllowed reports whether p may change scope.
func policyScopeAllowed(p auth.Principal, scope string) bool {
	if p.Operator() {
		return scope == "global" || strings.HasPrefix(scope, "org:") || strings.HasPrefix(scope, "project:")
	}
	if !p.Has(auth.ScopePoliciesWrite) {
		return false
	}
	if scope == "org:"+p.OrgID {
		return p.ProjectID == ""
	}
	return strings.HasPrefix(scope, "project:") && (p.ProjectID == "" || scope == "project:"+p.ProjectID)
}

func (s *Server) listPolicies(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	list, err := s.Store.ListPolicies(r.Context())
	if err != nil {
		return err
	}
	global := s.Config.Policy
	out := api.List[api.PlacementPolicy]{Items: []api.PlacementPolicy{}}
	hasGlobal := false
	for _, pol := range list {
		hasGlobal = hasGlobal || pol.Scope == "global"
		visible := p.Operator() || pol.Scope == "global" || pol.Scope == "org:"+p.OrgID
		if strings.HasPrefix(pol.Scope, "project:") && !p.Operator() {
			proj, err := s.Store.GetProject(r.Context(), strings.TrimPrefix(pol.Scope, "project:"))
			visible = err == nil && p.CanAccess(proj.OrgID, proj.ID)
		}
		if visible {
			out.Items = append(out.Items, pol)
		}
	}
	if !hasGlobal {
		out.Items = append([]api.PlacementPolicy{global}, out.Items...)
	}
	writeJSON(w, 200, out)
	return nil
}

func (s *Server) putPolicy(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	scope := r.PathValue("scope")
	if !policyScopeAllowed(p, scope) {
		return api.Errorf(api.CodePermissionDenied, "this key cannot change policy %s", scope)
	}
	if strings.HasPrefix(scope, "project:") && !p.Operator() {
		proj, err := s.Store.GetProject(r.Context(), strings.TrimPrefix(scope, "project:"))
		if err != nil || !p.CanAccess(proj.OrgID, proj.ID) {
			return api.Errorf(api.CodeNotFound, "project not found")
		}
	}
	var pol api.PlacementPolicy
	if err := decode(r, &pol); err != nil {
		return err
	}
	pol.Scope, pol.UpdatedAt, pol.UpdatedBy = scope, s.now(), p.Actor()
	if pol.External != "" && !policy.ValidExternal(pol.External) {
		return api.Errorf(api.CodeInvalidRequest, "external must be never, overflow or allowed")
	}
	if pol.Strategy != "" && !policy.ValidStrategy(pol.Strategy) {
		return api.Errorf(api.CodeInvalidRequest, "unknown strategy %q", pol.Strategy)
	}
	if err := s.Store.PutPolicy(r.Context(), pol); err != nil {
		return err
	}
	b, _ := json.Marshal(pol)
	s.audit(r, p, "policy.update", scope, p.OrgID, p.ProjectID, map[string]string{"policy": truncate(string(b), 1024)})
	s.Scheduler.Wake()
	writeJSON(w, 200, pol)
	return nil
}

func (s *Server) deletePolicy(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	scope := r.PathValue("scope")
	if !policyScopeAllowed(p, scope) {
		return api.Errorf(api.CodePermissionDenied, "this key cannot change policy %s", scope)
	}
	if strings.HasPrefix(scope, "project:") && !p.Operator() {
		proj, err := s.Store.GetProject(r.Context(), strings.TrimPrefix(scope, "project:"))
		if err != nil || !p.CanAccess(proj.OrgID, proj.ID) {
			return api.Errorf(api.CodeNotFound, "project not found")
		}
	}
	if err := s.Store.DeletePolicy(r.Context(), scope); err != nil {
		return err
	}
	s.audit(r, p, "policy.delete", scope, p.OrgID, p.ProjectID, nil)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// capabilities lists pools as tenants see them: no config or credentials.
func (s *Server) capabilities(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	list, err := s.Store.ListPools(r.Context())
	if err != nil {
		return err
	}
	active, _ := s.Store.PoolActive(r.Context())
	type view struct {
		Pool         string   `json:"pool"`
		Adapter      string   `json:"adapter"`
		Trust        string   `json:"trust"`
		Region       string   `json:"region,omitempty"`
		Enabled      bool     `json:"enabled"`
		Health       string   `json:"health"`
		Capabilities []string `json:"capabilities"`
	}
	out := api.List[view]{Items: []view{}}
	for _, pl := range list {
		s.Pools.Decorate(&pl, active[pl.Name])
		out.Items = append(out.Items, view{Pool: pl.Name, Adapter: pl.Adapter, Trust: pl.Trust, Region: pl.Region, Enabled: pl.Enabled && !pl.Draining, Health: pl.Health.Status, Capabilities: pl.Capabilities})
	}
	writeJSON(w, 200, out)
	return nil
}
