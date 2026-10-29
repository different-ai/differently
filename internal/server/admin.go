package server

import (
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/different-ai/differently/internal/audit"
	"github.com/different-ai/differently/internal/auth"
	"github.com/different-ai/differently/internal/config"
	"github.com/different-ai/differently/internal/id"
	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/internal/version"
	"github.com/different-ai/differently/pkg/api"
)

func (s *Server) adminRoutes(authed func(string, handlerFunc)) {
	admin := func(pattern string, h handlerFunc) {
		authed(pattern, func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
			if !p.Operator() {
				return api.Errorf(api.CodePermissionDenied, "operator (admin) key required")
			}
			return h(w, r, p)
		})
	}
	admin("GET /v1/admin/organizations", s.listOrgs)
	admin("POST /v1/admin/organizations", s.createOrg)
	admin("GET /v1/admin/organizations/{id}", s.getOrg)
	admin("PATCH /v1/admin/organizations/{id}", s.updateOrg)
	admin("POST /v1/admin/organizations/{id}/suspend", s.suspendOrg(true))
	admin("POST /v1/admin/organizations/{id}/resume", s.suspendOrg(false))
	admin("PATCH /v1/admin/projects/{id}", s.updateProjectQuota)
	admin("GET /v1/admin/pools", s.listPools)
	admin("POST /v1/admin/pools", s.createPool)
	admin("GET /v1/admin/pools/{name}", s.getPool)
	admin("PATCH /v1/admin/pools/{name}", s.updatePool)
	admin("DELETE /v1/admin/pools/{name}", s.deletePool)
	admin("POST /v1/admin/pools/{name}/drain", s.drainPool(true))
	admin("POST /v1/admin/pools/{name}/undrain", s.drainPool(false))
	admin("POST /v1/admin/pools/{name}/use", s.usePool)
	admin("POST /v1/admin/sandboxes/{id}/terminate", s.forceTerminate)
	admin("GET /v1/admin/trust-report", s.trustReport)
	admin("GET /v1/admin/audit/verify", s.verifyAudit)
	s.registerSecrets(admin)
	admin("POST /v1/admin/reconcile", func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		s.Scheduler.Reconcile(r.Context())
		s.audit(r, p, "admin.reconcile", "installation", "", "", nil)
		writeJSON(w, 200, map[string]bool{"ok": true})
		return nil
	})
}

func (s *Server) listOrgs(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	list, err := s.Store.ListOrgs(r.Context())
	if err != nil {
		return err
	}
	writeJSON(w, 200, api.List[api.Organization]{Items: append([]api.Organization{}, list...)})
	return nil
}

func (s *Server) createOrg(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	var req struct {
		Name           string     `json:"name"`
		Quota          *api.Quota `json:"quota,omitempty"`
		DefaultProject string     `json:"defaultProject,omitempty"`
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	if strings.TrimSpace(req.Name) == "" {
		return api.Errorf(api.CodeInvalidRequest, "name is required")
	}
	org := api.Organization{ID: id.New("org"), Name: req.Name, Quota: s.Config.Defaults.OrgQuota, CreatedAt: s.now()}
	if req.Quota != nil {
		org.Quota = *req.Quota
	}
	if err := s.Store.CreateOrg(r.Context(), org); err != nil {
		return err
	}
	s.audit(r, p, "org.create", org.ID, org.ID, "", map[string]string{"name": org.Name})
	resp := map[string]any{"organization": org}
	if req.DefaultProject != "" {
		proj := api.Project{ID: id.New("prj"), OrgID: org.ID, Name: req.DefaultProject, Quota: s.Config.Defaults.ProjectQuota, CreatedAt: s.now()}
		if err := s.Store.CreateProject(r.Context(), proj); err != nil {
			return err
		}
		s.audit(r, p, "project.create", proj.ID, org.ID, proj.ID, map[string]string{"name": proj.Name})
		resp["project"] = proj
	}
	writeJSON(w, http.StatusCreated, resp)
	return nil
}

func (s *Server) getOrg(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	org, err := s.Store.GetOrg(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, 200, org)
	return nil
}

func (s *Server) updateOrg(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	var req struct {
		Name  string     `json:"name,omitempty"`
		Quota *api.Quota `json:"quota,omitempty"`
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	org, err := s.Store.UpdateOrg(r.Context(), r.PathValue("id"), func(o *api.Organization) error {
		if req.Name != "" {
			o.Name = req.Name
		}
		if req.Quota != nil {
			o.Quota = *req.Quota
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.audit(r, p, "org.update", org.ID, org.ID, "", nil)
	writeJSON(w, 200, org)
	return nil
}

// suspendOrg blocks new work for a tenant. With terminate=true in the body,
// its running sandboxes are terminated too.
func (s *Server) suspendOrg(suspend bool) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		var req struct {
			Terminate bool   `json:"terminate,omitempty"`
			Reason    string `json:"reason,omitempty"`
		}
		if err := decode(r, &req); err != nil {
			return err
		}
		org, err := s.Store.UpdateOrg(r.Context(), r.PathValue("id"), func(o *api.Organization) error { o.Suspended = suspend; return nil })
		if err != nil {
			return err
		}
		terminated := 0
		if suspend && req.Terminate {
			recs, _ := s.Store.ListSandboxes(r.Context(), store.SandboxFilter{OrgID: org.ID})
			for _, rec := range recs {
				if !rec.Status.Terminal() {
					if _, err := s.markTerminating(r.Context(), rec.ID, "organization suspended: "+req.Reason); err == nil {
						terminated++
					}
				}
			}
		}
		action := "org.resume"
		if suspend {
			action = "org.suspend"
		}
		s.audit(r, p, action, org.ID, org.ID, "", map[string]string{"reason": req.Reason, "terminated": itoa64(int64(terminated))})
		writeJSON(w, 200, map[string]any{"organization": org, "terminated": terminated})
		return nil
	}
}

func (s *Server) updateProjectQuota(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	var req struct {
		Quota api.Quota `json:"quota"`
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	proj, err := s.Store.UpdateProject(r.Context(), r.PathValue("id"), func(pr *api.Project) error { pr.Quota = req.Quota; return nil })
	if err != nil {
		return err
	}
	s.audit(r, p, "project.quota_update", proj.ID, proj.OrgID, proj.ID, nil)
	writeJSON(w, 200, proj)
	return nil
}

func (s *Server) decoratedPools(r *http.Request) ([]api.Pool, error) {
	list, err := s.Store.ListPools(r.Context())
	if err != nil {
		return nil, err
	}
	active, _ := s.Store.PoolActive(r.Context())
	for i := range list {
		s.Pools.Decorate(&list[i], active[list[i].Name])
	}
	return list, nil
}

func (s *Server) listPools(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	list, err := s.decoratedPools(r)
	if err != nil {
		return err
	}
	writeJSON(w, 200, api.List[api.Pool]{Items: append([]api.Pool{}, list...)})
	return nil
}

func (s *Server) getPool(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	pool, err := s.Store.GetPool(r.Context(), r.PathValue("name"))
	if err != nil {
		return err
	}
	active, _ := s.Store.PoolActive(r.Context())
	s.Pools.Decorate(&pool, active[pool.Name])
	writeJSON(w, 200, pool)
	return nil
}

// createPool registers a pool at runtime. Credentials are references to
// environment variables or mounted files on the server, never values, so
// secrets stay in the operator's secret store and out of the database.
func (s *Server) createPool(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	var pc config.PoolConfig
	if err := decode(r, &pc); err != nil {
		return err
	}
	if _, err := s.Store.GetPool(r.Context(), pc.Name); err == nil {
		return api.Errorf(api.CodeConflict, "pool %s exists", pc.Name)
	}
	cfg := s.Config
	cfg.Pools = []config.PoolConfig{pc}
	cfg.Policy.ExternalPools = nil
	parsed, err := validatePool(cfg)
	if err != nil {
		return err
	}
	pool := parsed.ToPool()
	pool.Source = "api"
	if err := s.checkPoolCredential(r.Context(), pool.CredentialRef); err != nil {
		return err
	}
	if err := s.Store.UpsertPool(r.Context(), pool); err != nil {
		return err
	}
	if err := s.SyncPools(r.Context()); err != nil {
		return err
	}
	s.audit(r, p, "pool.create", pool.Name, "", "", map[string]string{"adapter": pool.Adapter, "trust": pool.Trust, "credentialRef": pool.CredentialRef})
	writeJSON(w, http.StatusCreated, pool)
	return nil
}

func validatePool(cfg config.Config) (config.PoolConfig, error) {
	b, _ := jsonMarshal(cfg)
	parsed, err := config.Parse(b)
	if err != nil {
		return config.PoolConfig{}, api.Errorf(api.CodeInvalidRequest, "%v", err)
	}
	return parsed.Pools[0], nil
}

func (s *Server) updatePool(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	var req struct {
		Enabled        *bool          `json:"enabled,omitempty"`
		MaxConcurrency *int           `json:"maxConcurrency,omitempty"`
		CostModel      *api.CostModel `json:"costModel,omitempty"`
		Region         *string        `json:"region,omitempty"`
		// Preemption replaces the pool's preemption policy ({"mode":"off"}
		// turns it off).
		Preemption *api.Preemption `json:"preemption,omitempty"`
		// CredentialRef moves an API-registered pool to another credential,
		// for example from env:NAME to a stored secret:name.
		CredentialRef *string `json:"credentialRef,omitempty"`
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	pool, err := s.Store.GetPool(r.Context(), r.PathValue("name"))
	if err != nil {
		return err
	}
	if req.CredentialRef != nil {
		ref := *req.CredentialRef
		switch {
		case pool.Source == "config":
			return api.Errorf(api.CodeConflict, "pool %s is declared in the config file; change its credentials there", pool.Name)
		case config.ParseCredentialRef(ref).String() != ref:
			return api.Errorf(api.CodeInvalidRequest, "credentialRef must be env:NAME, file:/path or secret:name")
		}
		if err := s.checkPoolCredential(r.Context(), ref); err != nil {
			return err
		}
		pool.CredentialRef = ref
	}
	if req.Enabled != nil {
		pool.Enabled = *req.Enabled
	}
	if req.MaxConcurrency != nil {
		pool.MaxConcurrency = *req.MaxConcurrency
	}
	if req.CostModel != nil {
		pool.CostModel = *req.CostModel
	}
	if req.Region != nil {
		pool.Region = *req.Region
	}
	if req.Preemption != nil {
		if err := config.ValidatePreemption(req.Preemption); err != nil {
			return api.Errorf(api.CodeInvalidRequest, "%v", err)
		}
		pool.Preemption = req.Preemption
	}
	if err := s.Store.UpsertPool(r.Context(), pool); err != nil {
		return err
	}
	_ = s.SyncPools(r.Context())
	var details map[string]string
	if req.CredentialRef != nil {
		details = map[string]string{"credentialRef": pool.CredentialRef}
	}
	s.audit(r, p, "pool.update", pool.Name, "", "", details)
	writeJSON(w, 200, pool)
	return nil
}

func (s *Server) deletePool(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	name := r.PathValue("name")
	if slices.ContainsFunc(s.Config.Pools, func(pc config.PoolConfig) bool { return pc.Name == name }) {
		return api.Errorf(api.CodeConflict, "pool %s is declared in the config file; remove it there", name)
	}
	if err := s.Store.DeletePool(r.Context(), name); err != nil {
		return err
	}
	_ = s.SyncPools(r.Context())
	s.audit(r, p, "pool.delete", name, "", "", nil)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// drainPool stops new placements on a pool; running sandboxes continue.
func (s *Server) drainPool(drain bool) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		pool, err := s.Store.GetPool(r.Context(), r.PathValue("name"))
		if err != nil {
			return err
		}
		pool.Draining = drain
		if err := s.Store.UpsertPool(r.Context(), pool); err != nil {
			return err
		}
		_ = s.SyncPools(r.Context())
		action := "pool.undrain"
		if drain {
			action = "pool.drain"
		}
		s.audit(r, p, action, pool.Name, "", "", nil)
		writeJSON(w, 200, pool)
		return nil
	}
}

// usePool switches where new sandboxes go: it undrains the pool and drains
// every other one. Running sandboxes stay where they are. An external pool
// must already be approved by the global policy, or be approved here
// explicitly; private-only work still never leaves private pools.
func (s *Server) usePool(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	var req api.UsePoolRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	ctx := r.Context()
	target, err := s.Store.GetPool(ctx, r.PathValue("name"))
	if err != nil {
		return err
	}
	if !target.Enabled {
		return api.Errorf(api.CodeInvalidState, "pool %s is disabled; enable it first", target.Name)
	}
	out := api.UsePoolResponse{Drained: []string{}}
	if target.Trust != api.TrustPrivate {
		global := s.Config.Policy
		if stored, ok, err := s.Store.GetPolicy(ctx, "global"); err != nil {
			return err
		} else if ok {
			global = stored
		}
		approved := global.External != api.ExternalNever && global.External != "" && slices.Contains(global.ExternalPools, target.Name)
		switch {
		case !approved && !req.ApproveExternal:
			return api.Errorf(api.CodeInvalidState, "pool %s is external and the global policy does not approve it; pass approveExternal to approve it", target.Name).With("pool", target.Name)
		case !approved:
			if global.External == api.ExternalNever || global.External == "" {
				global.External = api.ExternalOverflow
			}
			if !slices.Contains(global.ExternalPools, target.Name) {
				global.ExternalPools = append(global.ExternalPools, target.Name)
			}
			global.Scope, global.UpdatedAt, global.UpdatedBy = "global", s.now(), p.Actor()
			if err := s.Store.PutPolicy(ctx, global); err != nil {
				return err
			}
			s.audit(r, p, "policy.update", "global", "", "", map[string]string{"approvedExternalPool": target.Name})
			out.Policy = &global
		}
	}
	pools, err := s.Store.ListPools(ctx)
	if err != nil {
		return err
	}
	for _, pool := range pools {
		want := pool.Name != target.Name
		if pool.Draining == want || (want && !pool.Enabled) {
			continue
		}
		pool.Draining = want
		if err := s.Store.UpsertPool(ctx, pool); err != nil {
			return err
		}
		if want {
			out.Drained = append(out.Drained, pool.Name)
		} else {
			target = pool
		}
	}
	_ = s.SyncPools(ctx)
	s.audit(r, p, "pool.use", target.Name, "", "", map[string]string{"drained": strings.Join(out.Drained, ",")})
	out.Pool = target
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) forceTerminate(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	var req struct {
		Reason string `json:"reason"`
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	rec, err := s.Store.GetSandbox(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	out, err := s.markTerminating(r.Context(), rec.ID, "terminated by operator: "+req.Reason)
	if err != nil {
		return err
	}
	s.audit(r, p, "admin.sandbox_terminate", rec.ID, rec.OrgID, rec.ProjectID, map[string]string{"reason": req.Reason})
	writeJSON(w, http.StatusAccepted, out.Sandbox)
	return nil
}

func (s *Server) verifyAudit(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	events, err := s.Store.ListAudit(r.Context(), store.AuditFilter{})
	if err != nil {
		return err
	}
	writeJSON(w, 200, audit.Verify(events))
	return nil
}

// TrustReport is what an operator inspects to see where workloads can go,
// where they went and what Differently cannot prove.
type TrustReport struct {
	GeneratedAt     time.Time             `json:"generatedAt"`
	Installation    string                `json:"installation"`
	Version         string                `json:"version"`
	Telemetry       string                `json:"telemetry"`
	ControlPlane    map[string]string     `json:"controlPlane"`
	Pools           []TrustPool           `json:"pools"`
	GlobalPolicy    api.PlacementPolicy   `json:"globalPolicy"`
	ScopedPolicies  []api.PlacementPolicy `json:"scopedPolicies"`
	Classifications []api.Classification  `json:"classifications"`
	Placements      []api.UsageLine       `json:"placementsByPool"`
	ExternalRecent  []TrustPlacement      `json:"recentExternalPlacements"`
	AdminActions    []api.AuditEvent      `json:"recentAdminActions"`
	AuditChain      audit.VerifyResult    `json:"auditChain"`
	Limitations     []string              `json:"limitations"`
}

// TrustPool summarizes a pool's trust boundary.
type TrustPool struct {
	Name             string `json:"name"`
	Adapter          string `json:"adapter"`
	Trust            string `json:"trust"`
	Enabled          bool   `json:"enabled"`
	Draining         bool   `json:"draining"`
	Region           string `json:"region,omitempty"`
	Isolation        string `json:"isolation,omitempty"`
	Endpoint         string `json:"endpoint,omitempty"`
	CredentialRef    string `json:"credentialRef,omitempty"`
	ApprovedExternal bool   `json:"approvedForExternalPlacement"`
	Health           string `json:"health"`
}

// TrustPlacement is one sandbox placed outside private pools.
type TrustPlacement struct {
	SandboxID      string    `json:"sandboxId"`
	OrgID          string    `json:"orgId"`
	Pool           string    `json:"pool"`
	Classification string    `json:"classification"`
	CreatedBy      string    `json:"createdBy"`
	PlacedAt       time.Time `json:"placedAt"`
	Reason         string    `json:"reason"`
}

// limitations are the residual risks Differently does not control, adjusted
// for how this installation is configured.
func limitations(cfg config.Config) []string {
	out := []string{
		"Differently does not isolate sandboxes itself: isolation and network egress come from each pool's backend runtime (for example OpenSandbox's Docker, Kubernetes or Fast Sandbox runtime). Each pool's declared isolation is listed in this report and should be checked against the runtime you actually deployed.",
		"Whoever operates a backend (an OpenSandbox server and its hosts, a cloud provider, an external provider) can read the workloads it runs, including code, data and environment variables.",
		"Actions taken directly on a backend (its own API or console, kubectl, SSH to hosts) bypass Differently and are not in its audit log.",
		"The audit chain is tamper-evident, not tamper-proof: anyone with database write access can rewrite it. Export events (audit.file) to write-once storage to anchor it.",
		"Backend credentials (for example an OpenSandbox or Daytona API key) are held by the control plane as environment variables, mounted files, or values stored in its database sealed with its secrets key; anyone who can read its environment, or its database together with that key, can use them.",
		"Sandbox environment variables are stored unencrypted in the control-plane database; protect it with storage encryption and access controls.",
		"Exec commands are audited by program name unless audit.recordCommands is enabled; command arguments and outputs are not retained.",
		"Usage costs are estimates from configured cost models, not reconciled provider invoices.",
	}
	if !cfg.Audit.FailClosed {
		out = append(out, "Audit logging is fail-open: if an audit append fails the action still proceeds and the failure is only logged and counted (set audit.failClosed to refuse changes instead).")
	}
	if cfg.Metrics.Listen == "" {
		out = append(out, "/metrics is served without authentication on the API listener; set metrics.listen to move it to a separate port.")
	}
	return out
}

func (s *Server) trustReport(w http.ResponseWriter, r *http.Request, _ auth.Principal) error {
	ctx := r.Context()
	rep := TrustReport{
		GeneratedAt: s.now(), Installation: s.Installation, Version: version.Version,
		Telemetry:       "none: Differently has no telemetry or phone-home code paths",
		ControlPlane:    map[string]string{"store": s.Config.Store.Driver, "publicURL": s.Config.PublicURL, "auditMirror": s.Config.Audit.File, "dev": itoa64(map[bool]int64{true: 1}[s.Config.Dev])},
		Classifications: s.Config.Classifications, Limitations: limitations(s.Config),
	}
	rep.GlobalPolicy = s.Config.Policy
	policies, _ := s.Store.ListPolicies(ctx)
	for _, pol := range policies {
		if pol.Scope == "global" {
			rep.GlobalPolicy = pol
		} else {
			rep.ScopedPolicies = append(rep.ScopedPolicies, pol)
		}
	}
	pools, err := s.decoratedPools(r)
	if err != nil {
		return err
	}
	endpoints := s.Pools.Endpoints()
	for _, pl := range pools {
		isolation := ""
		if a, err := s.Pools.Adapter(pl.Name); err == nil {
			isolation = a.Info().Isolation
		}
		rep.Pools = append(rep.Pools, TrustPool{
			Isolation: isolation,
			Name:      pl.Name, Adapter: pl.Adapter, Trust: pl.Trust, Enabled: pl.Enabled, Draining: pl.Draining, Region: pl.Region,
			Endpoint: endpoints[pl.Name], CredentialRef: pl.CredentialRef, Health: pl.Health.Status,
			ApprovedExternal: pl.Trust == api.TrustExternal && rep.GlobalPolicy.External != api.ExternalNever && slices.Contains(rep.GlobalPolicy.ExternalPools, pl.Name),
		})
	}
	recs, _ := s.Store.ListSandboxes(ctx, store.SandboxFilter{})
	byPool := map[string]*api.UsageLine{}
	for _, rec := range recs {
		if rec.Pool == "" {
			continue
		}
		l, ok := byPool[rec.Pool]
		if !ok {
			l = &api.UsageLine{Key: rec.Pool}
			byPool[rec.Pool] = l
		}
		l.Sandboxes++
		l.RunSeconds += rec.RunSeconds
		if rec.Trust == api.TrustExternal && len(rep.ExternalRecent) < 50 {
			placed := rec.CreatedAt
			reason := ""
			if rec.Decision != nil {
				placed, reason = rec.Decision.EvaluatedAt, rec.Decision.Reason
			}
			rep.ExternalRecent = append(rep.ExternalRecent, TrustPlacement{SandboxID: rec.ID, OrgID: rec.OrgID, Pool: rec.Pool, Classification: rec.Classification, CreatedBy: rec.CreatedBy, PlacedAt: placed, Reason: reason})
		}
	}
	for _, l := range byPool {
		rep.Placements = append(rep.Placements, *l)
	}
	slices.SortFunc(rep.Placements, func(a, b api.UsageLine) int { return strings.Compare(a.Key, b.Key) })
	events, _ := s.Store.ListAudit(ctx, store.AuditFilter{})
	rep.AuditChain = audit.Verify(events)
	for i := len(events) - 1; i >= 0 && len(rep.AdminActions) < 50; i-- {
		ev := events[i]
		if strings.HasPrefix(ev.Action, "admin.") || strings.HasPrefix(ev.Action, "pool.") || strings.HasPrefix(ev.Action, "policy.") ||
			strings.HasPrefix(ev.Action, "org.") || strings.HasPrefix(ev.Action, "secret.") || ev.Action == "key.create" || ev.Action == "key.revoke" || ev.Action == "key.rotate" {
			rep.AdminActions = append(rep.AdminActions, ev)
		}
	}
	writeJSON(w, 200, rep)
	return nil
}
