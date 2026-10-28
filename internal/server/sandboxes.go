package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/different-ai/differently/internal/auth"
	"github.com/different-ai/differently/internal/id"
	"github.com/different-ai/differently/internal/policy"
	"github.com/different-ai/differently/internal/scheduler"
	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// KnownFeatures are the capability names a request may ask for.
var KnownFeatures = []string{
	adapter.FeatureExec, adapter.FeatureExecStream, adapter.FeatureFiles, adapter.FeatureStopStart, adapter.FeaturePauseResume,
	adapter.FeatureSnapshotDisk, adapter.FeatureSnapshotMem, adapter.FeatureFork, adapter.FeaturePersistDisk, adapter.FeaturePorts,
	adapter.FeatureSSH, adapter.FeaturePTY, adapter.FeatureEgressControl, adapter.FeatureGPU, adapter.FeatureArchAMD64,
	adapter.FeatureArchARM64, adapter.FeatureMicroVM, adapter.FeatureContainer,
}

func (s *Server) sandboxRoutes(authed func(string, handlerFunc)) {
	authed("POST /v1/sandboxes", s.createSandbox)
	authed("GET /v1/sandboxes", s.listSandboxes)
	authed("GET /v1/sandboxes/{id}", s.getSandbox)
	authed("PATCH /v1/sandboxes/{id}", s.updateSandbox)
	authed("DELETE /v1/sandboxes/{id}", s.deleteSandbox)
	authed("GET /v1/sandboxes/{id}/wait", s.waitSandbox)
	authed("GET /v1/sandboxes/{id}/placement", s.explainPlacement)
	authed("POST /v1/sandboxes/{id}/exec", s.exec)
	authed("GET /v1/sandboxes/{id}/files", s.listFiles)
	authed("GET /v1/sandboxes/{id}/files/content", s.readFile)
	authed("PUT /v1/sandboxes/{id}/files/content", s.writeFile)
	authed("POST /v1/sandboxes/{id}/stop", s.lifecycle("stop"))
	authed("POST /v1/sandboxes/{id}/start", s.lifecycle("start"))
	authed("POST /v1/sandboxes/{id}/pause", s.lifecycle("pause"))
	authed("POST /v1/sandboxes/{id}/resume", s.lifecycle("resume"))
	authed("POST /v1/sandboxes/{id}/snapshots", s.createSnapshot)
	authed("POST /v1/sandboxes/{id}/fork", s.forkSandbox)
	authed("POST /v1/sandboxes/{id}/ports", s.exposePort)
	authed("GET /v1/snapshots", s.listSnapshots)
	authed("GET /v1/snapshots/{id}", s.getSnapshot)
	authed("DELETE /v1/snapshots/{id}", s.deleteSnapshot)
}

// resolveProject finds the project a request targets and checks access.
func (s *Server) resolveProject(ctx context.Context, p auth.Principal, projectID string) (api.Organization, api.Project, error) {
	if p.ProjectID != "" {
		if projectID != "" && projectID != p.ProjectID {
			return api.Organization{}, api.Project{}, api.Errorf(api.CodePermissionDenied, "this key is limited to project %s", p.ProjectID)
		}
		projectID = p.ProjectID
	}
	if projectID == "" {
		return api.Organization{}, api.Project{}, api.Errorf(api.CodeInvalidRequest, "projectId is required for organization and operator keys")
	}
	proj, err := s.Store.GetProject(ctx, projectID)
	if err != nil || !p.CanAccess(proj.OrgID, proj.ID) {
		return api.Organization{}, api.Project{}, api.Errorf(api.CodeNotFound, "project %s not found", projectID)
	}
	org, err := s.Store.GetOrg(ctx, proj.OrgID)
	if err != nil {
		return api.Organization{}, api.Project{}, err
	}
	if org.Suspended {
		return org, proj, api.Errorf(api.CodeSuspended, "organization %s is suspended", org.ID)
	}
	return org, proj, nil
}

// loadSandbox fetches a sandbox the principal may see. Sandboxes in other
// tenants are reported as not found, never as forbidden.
func (s *Server) loadSandbox(r *http.Request, p auth.Principal) (store.SandboxRecord, error) {
	rec, err := s.Store.GetSandbox(r.Context(), r.PathValue("id"))
	if err != nil || !p.CanAccess(rec.OrgID, rec.ProjectID) {
		return rec, api.Errorf(api.CodeNotFound, "sandbox %s not found", r.PathValue("id"))
	}
	return rec, nil
}

func (s *Server) createSandbox(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if err := p.Require(auth.ScopeSandboxesWrite); err != nil {
		return err
	}
	var req api.CreateSandboxRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	rec, err := s.buildSandbox(r.Context(), p, req)
	if err != nil {
		return err
	}
	rec.IdempotencyKey = r.Header.Get("Idempotency-Key")
	return s.admit(w, r, p, rec)
}

func (s *Server) admit(w http.ResponseWriter, r *http.Request, p auth.Principal, rec store.SandboxRecord) error {
	ctx := r.Context()
	org, err := s.Store.GetOrg(ctx, rec.OrgID)
	if err != nil {
		return err
	}
	proj, err := s.Store.GetProject(ctx, rec.ProjectID)
	if err != nil {
		return err
	}
	// Fail fast with a precise error when no pool could ever take this.
	active, _ := s.Store.PoolActive(ctx)
	if res := policy.Decide(s.Scheduler.PlacementInput(ctx, rec, active)); res.Err != nil {
		return res.Err
	}
	out, existed, err := s.Store.AdmitSandbox(ctx, rec, store.Admission{OrgQuota: org.Quota, ProjectQuota: proj.Quota})
	if err != nil {
		return err
	}
	if existed {
		writeJSON(w, http.StatusOK, out.Sandbox)
		return nil
	}
	s.audit(r, p, "sandbox.create", out.ID, out.OrgID, out.ProjectID, map[string]string{
		"classification": out.Classification, "cpu": fmt.Sprint(out.Resources.CPU), "memoryMiB": fmt.Sprint(out.Resources.MemoryMiB), "template": out.Template,
	})
	s.Scheduler.Wake()
	writeJSON(w, http.StatusAccepted, out.Sandbox)
	return nil
}

func (s *Server) buildSandbox(ctx context.Context, p auth.Principal, req api.CreateSandboxRequest) (store.SandboxRecord, error) {
	org, proj, err := s.resolveProject(ctx, p, req.ProjectID)
	if err != nil {
		return store.SandboxRecord{}, err
	}
	profileName := req.Profile
	if profileName == "" && req.Resources == nil {
		profileName = s.Config.Defaults.Profile
	}
	resources := s.Config.Defaults.Resources
	if def, ok := s.Config.Profile(s.Config.Defaults.Profile); ok && resources == (api.Resources{}) {
		resources = def.Resources // explicit partial resources start from the default profile
	}
	template := s.Config.Defaults.Template
	caps := slices.Clone(req.Capabilities)
	if profileName != "" {
		prof, ok := s.Config.Profile(profileName)
		if !ok {
			return store.SandboxRecord{}, api.Errorf(api.CodeInvalidRequest, "unknown profile %q", profileName)
		}
		resources = prof.Resources
		if prof.Template != "" {
			template = prof.Template
		}
		for _, c := range prof.Capabilities {
			if !slices.Contains(caps, c) {
				caps = append(caps, c)
			}
		}
	}
	if req.Resources != nil {
		if req.Resources.CPU > 0 {
			resources.CPU = req.Resources.CPU
		}
		if req.Resources.MemoryMiB > 0 {
			resources.MemoryMiB = req.Resources.MemoryMiB
		}
		if req.Resources.DiskGiB > 0 {
			resources.DiskGiB = req.Resources.DiskGiB
		}
	}
	if req.Template != "" {
		template = req.Template
	}
	switch {
	case resources.CPU <= 0 || resources.CPU > 256:
		return store.SandboxRecord{}, api.Errorf(api.CodeInvalidRequest, "resources.cpu must be between 1 and 256")
	case resources.MemoryMiB < 128 || resources.MemoryMiB > 1<<20:
		return store.SandboxRecord{}, api.Errorf(api.CodeInvalidRequest, "resources.memoryMiB must be between 128 and 1048576")
	case resources.DiskGiB < 0 || resources.DiskGiB > 16384:
		return store.SandboxRecord{}, api.Errorf(api.CodeInvalidRequest, "resources.diskGiB must be between 0 and 16384")
	}
	for _, c := range caps {
		if !slices.Contains(KnownFeatures, c) {
			return store.SandboxRecord{}, api.Errorf(api.CodeInvalidRequest, "unknown capability %q", c)
		}
	}
	class := req.Classification
	if class == "" {
		class = s.Config.Defaults.Classification
	}
	if _, ok := s.Config.Classification(class); !ok {
		return store.SandboxRecord{}, api.Errorf(api.CodeInvalidRequest, "unknown classification %q", class)
	}
	if pl := req.Placement; pl != nil {
		if pl.Strategy != "" && !policy.ValidStrategy(pl.Strategy) {
			return store.SandboxRecord{}, api.Errorf(api.CodeInvalidRequest, "unknown placement strategy %q", pl.Strategy)
		}
		if pl.External != "" && !policy.ValidExternal(pl.External) {
			return store.SandboxRecord{}, api.Errorf(api.CodeInvalidRequest, "placement.external must be never, overflow or allowed")
		}
	}
	ttl := req.TTLSeconds
	if ttl == 0 {
		ttl = s.Config.Defaults.TTLSeconds
	}
	for k := range req.Labels {
		if strings.HasPrefix(k, "differently.dev/") {
			return store.SandboxRecord{}, api.Errorf(api.CodeInvalidRequest, "label prefix differently.dev/ is reserved")
		}
	}
	now := s.now()
	rec := store.SandboxRecord{
		Sandbox: api.Sandbox{
			ID: id.New("sbx"), OrgID: org.ID, ProjectID: proj.ID, Status: api.StatusQueued, Template: template, Profile: profileName,
			Resources: resources, Capabilities: caps, Classification: class, Placement: req.Placement, Labels: req.Labels,
			Priority: req.Priority, TTLSeconds: ttl, CreatedBy: p.Actor(), CreatedAt: now, UpdatedAt: now, FromSnapshot: req.FromSnapshot,
		},
		Env: req.Env, NextAttemptAt: now,
	}
	for k := range req.Env {
		rec.EnvKeys = append(rec.EnvKeys, k)
	}
	slices.Sort(rec.EnvKeys)
	if req.FromSnapshot != "" {
		snap, err := s.Store.GetSnapshot(ctx, req.FromSnapshot)
		if err != nil || snap.OrgID != org.ID || !p.CanAccess(snap.OrgID, snap.ProjectID) {
			return rec, api.Errorf(api.CodeNotFound, "snapshot %s not found", req.FromSnapshot)
		}
		rec.SnapshotBackendRef = snap.BackendRef
	}
	return rec, nil
}

func (s *Server) listSandboxes(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if err := p.Require(auth.ScopeSandboxesRead); err != nil {
		return err
	}
	q := r.URL.Query()
	f := store.SandboxFilter{OrgID: p.OrgID, ProjectID: p.ProjectID, Cursor: q.Get("cursor"), Limit: min(max(queryInt(r, "limit", 50), 1), 500), Pool: q.Get("pool")}
	if p.Operator() && q.Get("orgId") != "" {
		f.OrgID = q.Get("orgId")
	}
	if v := q.Get("projectId"); v != "" && (p.ProjectID == "" || v == p.ProjectID) {
		f.ProjectID = v
	}
	if v := q.Get("status"); v != "" {
		for _, st := range strings.Split(v, ",") {
			f.Statuses = append(f.Statuses, api.SandboxStatus(st))
		}
	}
	for _, l := range q["label"] {
		if k, v, ok := strings.Cut(l, "="); ok {
			if f.Label == nil {
				f.Label = map[string]string{}
			}
			f.Label[k] = v
		}
	}
	recs, err := s.Store.ListSandboxes(r.Context(), f)
	if err != nil {
		return err
	}
	out := api.List[api.Sandbox]{Items: make([]api.Sandbox, 0, len(recs))}
	for _, rec := range recs {
		out.Items = append(out.Items, rec.Sandbox)
	}
	if len(recs) == f.Limit {
		out.NextCursor = recs[len(recs)-1].ID
	}
	writeJSON(w, 200, out)
	return nil
}

func (s *Server) getSandbox(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if err := p.Require(auth.ScopeSandboxesRead); err != nil {
		return err
	}
	rec, err := s.loadSandbox(r, p)
	if err != nil {
		return err
	}
	writeJSON(w, 200, rec.Sandbox)
	return nil
}

func (s *Server) updateSandbox(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if err := p.Require(auth.ScopeSandboxesWrite); err != nil {
		return err
	}
	rec, err := s.loadSandbox(r, p)
	if err != nil {
		return err
	}
	var req api.UpdateSandboxRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	org, _ := s.Store.GetOrg(r.Context(), rec.OrgID)
	proj, _ := s.Store.GetProject(r.Context(), rec.ProjectID)
	if req.TTLSeconds != nil {
		for _, q := range []api.Quota{org.Quota, proj.Quota} {
			if q.MaxTTLSeconds > 0 && *req.TTLSeconds > q.MaxTTLSeconds {
				return api.Errorf(api.CodeQuotaExceeded, "ttl exceeds limit %ds", q.MaxTTLSeconds).With("dimension", "ttlSeconds")
			}
		}
	}
	out, err := s.Store.UpdateSandbox(r.Context(), rec.ID, func(x *store.SandboxRecord) error {
		if x.Status.Terminal() {
			return api.Errorf(api.CodeInvalidState, "sandbox is %s", x.Status)
		}
		for k, v := range req.Labels {
			if strings.HasPrefix(k, "differently.dev/") {
				return api.Errorf(api.CodeInvalidRequest, "label prefix differently.dev/ is reserved")
			}
			if x.Labels == nil {
				x.Labels = map[string]string{}
			}
			if v == "" {
				delete(x.Labels, k)
			} else {
				x.Labels[k] = v
			}
		}
		if req.TTLSeconds != nil {
			x.TTLSeconds = *req.TTLSeconds
			if x.StartedAt != nil {
				exp := s.now().Add(time.Duration(*req.TTLSeconds) * time.Second)
				x.ExpiresAt = &exp
			}
		}
		x.UpdatedAt = s.now()
		return nil
	})
	if err != nil {
		return err
	}
	s.audit(r, p, "sandbox.update", out.ID, out.OrgID, out.ProjectID, nil)
	writeJSON(w, 200, out.Sandbox)
	return nil
}

func (s *Server) deleteSandbox(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if err := p.Require(auth.ScopeSandboxesWrite); err != nil {
		return err
	}
	rec, err := s.loadSandbox(r, p)
	if err != nil {
		return err
	}
	out, err := s.markTerminating(r.Context(), rec.ID, "deleted by "+p.Actor())
	if err != nil {
		return err
	}
	s.audit(r, p, "sandbox.delete", out.ID, out.OrgID, out.ProjectID, nil)
	writeJSON(w, http.StatusAccepted, out.Sandbox)
	return nil
}

// markTerminating requests deletion. Queued sandboxes have no backend
// instance and terminate immediately; others are torn down by the scheduler.
func (s *Server) markTerminating(ctx context.Context, sid, reason string) (store.SandboxRecord, error) {
	out, err := s.Store.UpdateSandbox(ctx, sid, func(x *store.SandboxRecord) error {
		now := s.now()
		switch {
		case x.Status.Terminal():
			return nil
		case x.Status == api.StatusQueued:
			x.Status, x.EndedAt = api.StatusTerminated, &now
		default:
			x.Status = api.StatusTerminating
		}
		x.StatusReason, x.UpdatedAt = reason, now
		return nil
	})
	s.Scheduler.Wake()
	return out, err
}

func (s *Server) waitSandbox(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if err := p.Require(auth.ScopeSandboxesRead); err != nil {
		return err
	}
	want := api.SandboxStatus(r.URL.Query().Get("status"))
	if want == "" {
		want = api.StatusRunning
	}
	timeout := time.Duration(min(max(queryInt(r, "timeoutSeconds", 60), 1), 300)) * time.Second
	deadline := time.Now().Add(timeout)
	for {
		rec, err := s.loadSandbox(r, p)
		if err != nil {
			return err
		}
		if rec.Status == want || rec.Status.Terminal() || time.Now().After(deadline) {
			writeJSON(w, 200, rec.Sandbox)
			return nil
		}
		select {
		case <-r.Context().Done():
			return r.Context().Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (s *Server) explainPlacement(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if err := p.Require(auth.ScopeSandboxesRead); err != nil {
		return err
	}
	rec, err := s.loadSandbox(r, p)
	if err != nil {
		return err
	}
	out := map[string]any{"sandboxId": rec.ID, "status": rec.Status, "pool": rec.Pool, "trust": rec.Trust, "decision": rec.Decision}
	if rec.Status == api.StatusQueued {
		active, _ := s.Store.PoolActive(r.Context())
		out["current"] = policy.Decide(s.Scheduler.PlacementInput(r.Context(), rec, active)).Decision
	}
	writeJSON(w, 200, out)
	return nil
}

// running returns the sandbox's adapter after checking it is running.
func (s *Server) running(r *http.Request, p auth.Principal, scope string) (store.SandboxRecord, adapter.ExecutionAdapter, error) {
	if err := p.Require(scope); err != nil {
		return store.SandboxRecord{}, nil, err
	}
	rec, err := s.loadSandbox(r, p)
	if err != nil {
		return rec, nil, err
	}
	if rec.Status != api.StatusRunning {
		return rec, nil, api.Errorf(api.CodeInvalidState, "sandbox %s is %s, not running", rec.ID, rec.Status).With("status", string(rec.Status))
	}
	a, err := s.Pools.Adapter(rec.Pool)
	return rec, a, err
}

func unsupported(rec store.SandboxRecord, op string) error {
	return api.Errorf(api.CodeUnsupportedOperation, "the %s backend of pool %s does not support %s", rec.Adapter, rec.Pool, op).
		With("operation", op).With("pool", rec.Pool).With("adapter", rec.Adapter)
}

func (s *Server) exec(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	rec, a, err := s.running(r, p, auth.ScopeSandboxesExec)
	if err != nil {
		return err
	}
	var req api.ExecRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	if req.Command == "" && len(req.Args) == 0 {
		return api.Errorf(api.CodeInvalidRequest, "command or args is required")
	}
	program := req.Command
	if len(req.Args) > 0 {
		program = req.Args[0]
	}
	detail := map[string]string{"program": strings.Fields(program + " ")[0]}
	if s.Config.Audit.RecordCommands {
		detail["command"] = truncate(req.Command+strings.Join(req.Args, " "), 512)
	}
	s.audit(r, p, "sandbox.exec", rec.ID, rec.OrgID, rec.ProjectID, detail)
	if r.URL.Query().Get("stream") != "" || strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		return s.streamExec(w, r, rec, a, req)
	}
	res, err := a.Exec(r.Context(), rec.BackendRef, req)
	if err != nil {
		return err
	}
	writeJSON(w, 200, res)
	return nil
}

// streamExec sends server-sent events: stdout/stderr chunks, then exit.
func (s *Server) streamExec(w http.ResponseWriter, r *http.Request, rec store.SandboxRecord, a adapter.ExecutionAdapter, req api.ExecRequest) error {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)
	send := func(event string, ev api.ExecEvent) error {
		b, _ := jsonMarshal(ev)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	var code int
	var err error
	if st, ok := a.(adapter.Streamer); ok {
		code, err = st.ExecStream(r.Context(), rec.BackendRef, req, func(ev api.ExecEvent) error { return send(ev.Stream, ev) })
	} else {
		var res api.ExecResult
		res, err = a.Exec(r.Context(), rec.BackendRef, req)
		if err == nil {
			if res.Stdout != "" {
				_ = send("stdout", api.ExecEvent{Stream: "stdout", Data: res.Stdout})
			}
			if res.Stderr != "" {
				_ = send("stderr", api.ExecEvent{Stream: "stderr", Data: res.Stderr})
			}
			code = res.ExitCode
		}
	}
	if err != nil {
		return send("error", api.ExecEvent{Error: err.Error()})
	}
	return send("exit", api.ExecEvent{ExitCode: &code})
}

func (s *Server) listFiles(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	rec, a, err := s.running(r, p, auth.ScopeSandboxesExec)
	if err != nil {
		return err
	}
	fs, ok := a.(adapter.FileSystem)
	if !ok || !s.Pools.Supports(r.Context(), rec.Pool, adapter.FeatureFiles) {
		return unsupported(rec, adapter.FeatureFiles)
	}
	items, err := fs.ListFiles(r.Context(), rec.BackendRef, r.URL.Query().Get("path"))
	if err != nil {
		return err
	}
	if items == nil {
		items = []api.FileInfo{}
	}
	writeJSON(w, 200, api.List[api.FileInfo]{Items: items})
	return nil
}

func (s *Server) readFile(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	rec, a, err := s.running(r, p, auth.ScopeSandboxesExec)
	if err != nil {
		return err
	}
	fs, ok := a.(adapter.FileSystem)
	if !ok || !s.Pools.Supports(r.Context(), rec.Pool, adapter.FeatureFiles) {
		return unsupported(rec, adapter.FeatureFiles)
	}
	path := r.URL.Query().Get("path")
	data, err := fs.ReadFile(r.Context(), rec.BackendRef, path)
	if err != nil {
		return err
	}
	s.audit(r, p, "sandbox.file_read", rec.ID, rec.OrgID, rec.ProjectID, map[string]string{"path": truncate(path, 256)})
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(data)
	return nil
}

func (s *Server) writeFile(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	rec, a, err := s.running(r, p, auth.ScopeSandboxesExec)
	if err != nil {
		return err
	}
	fs, ok := a.(adapter.FileSystem)
	if !ok || !s.Pools.Supports(r.Context(), rec.Pool, adapter.FeatureFiles) {
		return unsupported(rec, adapter.FeatureFiles)
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		return api.Errorf(api.CodeInvalidRequest, "path is required")
	}
	const maxUpload = 100 << 20
	data, err := io.ReadAll(io.LimitReader(r.Body, maxUpload+1))
	if err != nil {
		return err
	}
	if len(data) > maxUpload {
		return api.Errorf(api.CodeInvalidRequest, "upload larger than %d bytes", maxUpload)
	}
	if err := fs.WriteFile(r.Context(), rec.BackendRef, path, data); err != nil {
		return err
	}
	s.audit(r, p, "sandbox.file_write", rec.ID, rec.OrgID, rec.ProjectID, map[string]string{"path": truncate(path, 256), "bytes": fmt.Sprint(len(data))})
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// lifecycle handles stop/start/pause/resume.
func (s *Server) lifecycle(action string) handlerFunc {
	type step struct {
		from, to api.SandboxStatus
		feature  string
	}
	steps := map[string]step{
		"stop":   {api.StatusRunning, api.StatusStopped, adapter.FeatureStopStart},
		"start":  {api.StatusStopped, api.StatusRunning, adapter.FeatureStopStart},
		"pause":  {api.StatusRunning, api.StatusPaused, adapter.FeaturePauseResume},
		"resume": {api.StatusPaused, api.StatusRunning, adapter.FeaturePauseResume},
	}
	st := steps[action]
	return func(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
		if err := p.Require(auth.ScopeSandboxesWrite); err != nil {
			return err
		}
		rec, err := s.loadSandbox(r, p)
		if err != nil {
			return err
		}
		if rec.Status == st.to {
			writeJSON(w, 200, rec.Sandbox) // idempotent
			return nil
		}
		if rec.Status != st.from {
			return api.Errorf(api.CodeInvalidState, "cannot %s a %s sandbox", action, rec.Status).With("status", string(rec.Status))
		}
		a, err := s.Pools.Adapter(rec.Pool)
		if err != nil {
			return err
		}
		if !s.Pools.Supports(r.Context(), rec.Pool, st.feature) {
			return unsupported(rec, st.feature)
		}
		if action == "start" {
			// Starting re-reserves CPU and memory: check quota atomically
			// first, and release the reservation if the backend fails.
			org, _ := s.Store.GetOrg(r.Context(), rec.OrgID)
			proj, _ := s.Store.GetProject(r.Context(), rec.ProjectID)
			// The reservation marks it running (not provisioning, which the
			// scheduler would pick up) until the backend confirms.
			if _, err := s.Store.ReserveCompute(r.Context(), rec.ID, api.StatusStopped, api.StatusRunning, org.Quota, proj.Quota, s.now()); err != nil {
				return err
			}
			st.from = api.StatusRunning
		}
		switch action {
		case "stop", "start":
			l, ok := a.(adapter.Lifecycle)
			if !ok {
				return unsupported(rec, st.feature)
			}
			if action == "stop" {
				err = l.Stop(r.Context(), rec.BackendRef)
			} else {
				err = l.Start(r.Context(), rec.BackendRef)
			}
		default:
			pz, ok := a.(adapter.Pauser)
			if !ok {
				return unsupported(rec, st.feature)
			}
			if action == "pause" {
				err = pz.Pause(r.Context(), rec.BackendRef)
			} else {
				err = pz.Resume(r.Context(), rec.BackendRef)
			}
		}
		if err != nil {
			if action == "start" {
				_, _ = s.Store.UpdateSandbox(r.Context(), rec.ID, func(x *store.SandboxRecord) error {
					if x.Status == api.StatusRunning {
						x.Status = api.StatusStopped
					}
					return nil
				})
			}
			return err
		}
		out, err := s.Store.UpdateSandbox(r.Context(), rec.ID, func(x *store.SandboxRecord) error {
			if x.Status != st.from {
				return api.Errorf(api.CodeConflict, "sandbox changed to %s concurrently", x.Status)
			}
			now := s.now()
			if st.to == api.StatusRunning {
				x.RunningSince = &now
			} else {
				scheduler.StopClock(x, now)
			}
			x.Status, x.UpdatedAt = st.to, now
			return nil
		})
		if err != nil {
			return err
		}
		s.audit(r, p, "sandbox."+action, out.ID, out.OrgID, out.ProjectID, nil)
		writeJSON(w, 200, out.Sandbox)
		return nil
	}
}

func (s *Server) snapshot(r *http.Request, p auth.Principal, rec store.SandboxRecord, kind string) (store.SnapshotRecord, error) {
	if kind == "" {
		kind = "disk"
	}
	if kind != "disk" && kind != "memory" {
		return store.SnapshotRecord{}, api.Errorf(api.CodeInvalidRequest, "kind must be disk or memory")
	}
	if rec.Status != api.StatusRunning && rec.Status != api.StatusStopped && rec.Status != api.StatusPaused {
		return store.SnapshotRecord{}, api.Errorf(api.CodeInvalidState, "cannot snapshot a %s sandbox", rec.Status)
	}
	a, err := s.Pools.Adapter(rec.Pool)
	if err != nil {
		return store.SnapshotRecord{}, err
	}
	sn, ok := a.(adapter.Snapshotter)
	feature := adapter.FeatureSnapshotDisk
	if kind == "memory" {
		feature = adapter.FeatureSnapshotMem
	}
	if !ok || !s.Pools.Supports(r.Context(), rec.Pool, feature) {
		return store.SnapshotRecord{}, unsupported(rec, feature)
	}
	org, _ := s.Store.GetOrg(r.Context(), rec.OrgID)
	if usage, _ := s.Store.Usage(r.Context(), rec.OrgID, ""); org.Quota.MaxSnapshots > 0 && usage.Snapshots >= org.Quota.MaxSnapshots {
		return store.SnapshotRecord{}, api.Errorf(api.CodeQuotaExceeded, "organization snapshot limit %d reached", org.Quota.MaxSnapshots).With("dimension", "snapshots")
	}
	snap, err := sn.Snapshot(r.Context(), rec.BackendRef, kind)
	if err != nil {
		return store.SnapshotRecord{}, err
	}
	out := store.SnapshotRecord{
		Snapshot: api.Snapshot{
			ID: id.New("snp"), SandboxID: rec.ID, OrgID: rec.OrgID, ProjectID: rec.ProjectID, Pool: rec.Pool,
			Kind: snap.Kind, Status: "ready", SizeBytes: snap.SizeBytes, CreatedAt: s.now(), CreatedBy: p.Actor(),
		},
		BackendRef: snap.Ref,
	}
	if err := s.Store.CreateSnapshot(r.Context(), out); err != nil {
		return out, err
	}
	s.audit(r, p, "snapshot.create", out.ID, out.OrgID, out.ProjectID, map[string]string{"sandbox": rec.ID, "kind": out.Kind, "pool": rec.Pool})
	return out, nil
}

func (s *Server) createSnapshot(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if err := p.Require(auth.ScopeSandboxesWrite); err != nil {
		return err
	}
	rec, err := s.loadSandbox(r, p)
	if err != nil {
		return err
	}
	var req api.CreateSnapshotRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	snap, err := s.snapshot(r, p, rec, req.Kind)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, snap.Snapshot)
	return nil
}

// forkSandbox snapshots a sandbox and starts a new one from it on the same
// pool. Snapshots are not portable across backends.
func (s *Server) forkSandbox(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if err := p.Require(auth.ScopeSandboxesWrite); err != nil {
		return err
	}
	rec, err := s.loadSandbox(r, p)
	if err != nil {
		return err
	}
	kind := "disk"
	if s.Pools.Supports(r.Context(), rec.Pool, adapter.FeatureSnapshotMem) && rec.Status == api.StatusRunning {
		kind = "memory"
	}
	snap, err := s.snapshot(r, p, rec, kind)
	if err != nil {
		return err
	}
	labels := map[string]string{"forked-from": rec.ID}
	for k, v := range rec.Labels {
		labels[k] = v
	}
	child, err := s.buildSandbox(r.Context(), p, api.CreateSandboxRequest{
		ProjectID: rec.ProjectID, Template: rec.Template, Resources: &rec.Resources, Capabilities: rec.Capabilities,
		Classification: rec.Classification, Placement: rec.Placement, TTLSeconds: rec.TTLSeconds, Env: rec.Env, Labels: labels, FromSnapshot: snap.ID,
	})
	if err != nil {
		return err
	}
	child.Profile = rec.Profile
	return s.admit(w, r, p, child)
}

func (s *Server) exposePort(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	rec, a, err := s.running(r, p, auth.ScopeSandboxesWrite)
	if err != nil {
		return err
	}
	var req struct {
		Port int `json:"port"`
	}
	if err := decode(r, &req); err != nil {
		return err
	}
	if req.Port < 1 || req.Port > 65535 {
		return api.Errorf(api.CodeInvalidRequest, "port must be 1-65535")
	}
	pe, ok := a.(adapter.PortExposer)
	if !ok || !s.Pools.Supports(r.Context(), rec.Pool, adapter.FeaturePorts) {
		return unsupported(rec, adapter.FeaturePorts)
	}
	u, err := pe.ExposePort(r.Context(), rec.BackendRef, req.Port)
	if err != nil {
		return err
	}
	s.audit(r, p, "sandbox.port_expose", rec.ID, rec.OrgID, rec.ProjectID, map[string]string{"port": fmt.Sprint(req.Port), "pool": rec.Pool})
	writeJSON(w, 200, api.PortURL{Port: req.Port, URL: u})
	return nil
}

func (s *Server) listSnapshots(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if err := p.Require(auth.ScopeSandboxesRead); err != nil {
		return err
	}
	recs, err := s.Store.ListSnapshots(r.Context(), p.OrgID, p.ProjectID)
	if err != nil {
		return err
	}
	out := api.List[api.Snapshot]{Items: []api.Snapshot{}}
	for _, sn := range recs {
		out.Items = append(out.Items, sn.Snapshot)
	}
	writeJSON(w, 200, out)
	return nil
}

func (s *Server) loadSnapshot(r *http.Request, p auth.Principal) (store.SnapshotRecord, error) {
	sn, err := s.Store.GetSnapshot(r.Context(), r.PathValue("id"))
	if err != nil || !p.CanAccess(sn.OrgID, sn.ProjectID) {
		return sn, api.Errorf(api.CodeNotFound, "snapshot %s not found", r.PathValue("id"))
	}
	return sn, nil
}

func (s *Server) getSnapshot(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if err := p.Require(auth.ScopeSandboxesRead); err != nil {
		return err
	}
	sn, err := s.loadSnapshot(r, p)
	if err != nil {
		return err
	}
	writeJSON(w, 200, sn.Snapshot)
	return nil
}

func (s *Server) deleteSnapshot(w http.ResponseWriter, r *http.Request, p auth.Principal) error {
	if err := p.Require(auth.ScopeSandboxesWrite); err != nil {
		return err
	}
	sn, err := s.loadSnapshot(r, p)
	if err != nil {
		return err
	}
	if a, err := s.Pools.Adapter(sn.Pool); err == nil {
		if snr, ok := a.(adapter.Snapshotter); ok {
			if err := snr.DeleteSnapshot(r.Context(), sn.BackendRef); err != nil {
				return err
			}
		}
	}
	if err := s.Store.DeleteSnapshot(r.Context(), sn.ID); err != nil {
		return err
	}
	s.audit(r, p, "snapshot.delete", sn.ID, sn.OrgID, sn.ProjectID, nil)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
