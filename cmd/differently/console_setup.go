package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/different-ai/differently/pkg/client"
)

// The setup file in the local admin: the page shows what "differently diff"
// would print for the file beside the command, and its Apply button does
// what "differently apply" does, without --prune. Both run in this process,
// with the key the CLI already uses.

// setupStatus is what the page shows about the setup file.
type setupStatus struct {
	File   string `json:"file"`
	Exists bool   `json:"exists"`
	// Problem says why there is no plan: the file cannot be read, or the
	// orchestrator does not answer or refuses the key.
	Problem string    `json:"problem,omitempty"`
	Changes []planned `json:"changes"`
	ToApply int       `json:"toApply"`
	Blocked int       `json:"blocked"`
	Extra   int       `json:"extra"`
	Summary string    `json:"summary,omitempty"`
	// Plan names exactly this plan. Apply sends it back, and is refused if
	// the file or the server has changed since: nothing is applied that was
	// not on the page.
	Plan string `json:"plan,omitempty"`
}

func planID(items []planned) string {
	b, _ := json.Marshal(items)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:12])
}

func (a *localAdmin) client() *client.Client {
	c := client.New(a.orch.URL, a.orch.APIKey)
	c.Retries = 0
	return c
}

// setup reads the file and the orchestrator and compares them. It returns
// the plan too, for apply.
func (a *localAdmin) setup(ctx context.Context) (setupStatus, []planned) {
	st := setupStatus{File: a.setupFile, Changes: []planned{}}
	if _, err := os.Stat(a.setupFile); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			st.Problem = err.Error()
		}
		return st, nil
	}
	st.Exists = true
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	items, err := loadPlan(ctx, a.client(), a.setupFile, false)
	if err != nil {
		st.Problem = err.Error()
		return st, nil
	}
	if items != nil {
		st.Changes = items
	}
	st.ToApply, st.Blocked, st.Extra = pending(items)
	// The page shows the path once, above; the sentence names the file alone.
	st.Summary = summary(items, filepath.Base(a.setupFile), false)
	st.Plan = planID(items)
	return st, items
}

func (a *localAdmin) getSetup(w http.ResponseWriter, r *http.Request) {
	st, _ := a.setup(r.Context())
	writeLocalJSON(w, st)
}

// applySetup applies the plan the page showed. It is the one thing the
// local admin's own endpoints change, so it takes more than the session:
// a JSON body, which a form on another site cannot send, and the plan's
// name, so a click cannot apply what the page did not show.
func (a *localAdmin) applySetup(w http.ResponseWriter, r *http.Request) {
	refuse := func(code int, problem string) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"applied": 0, "problem": problem})
	}
	if r.Header.Get("Content-Type") != "application/json" {
		refuse(http.StatusUnsupportedMediaType, "send JSON")
		return
	}
	var req struct {
		Plan string `json:"plan"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.Plan == "" {
		refuse(http.StatusBadRequest, "name the plan to apply")
		return
	}
	st, items := a.setup(r.Context())
	switch {
	case st.Problem != "":
		refuse(http.StatusConflict, st.Problem)
		return
	case st.Plan != req.Plan:
		refuse(http.StatusConflict, "the file or the server changed since this page read them; look again before applying")
		return
	case st.Blocked > 0:
		refuse(http.StatusConflict, "the file asks for something that cannot be applied; nothing was changed")
		return
	}
	// The apply outlives a closed tab: stopping part-way would leave the
	// server between two setups for no reason.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	defer cancel()
	done, err := runPlan(ctx, a.client(), items, nil)
	out := map[string]any{"applied": done}
	if err != nil {
		out["problem"] = err.Error()
	}
	after, _ := a.setup(r.Context())
	out["setup"] = after
	writeLocalJSON(w, out)
}
