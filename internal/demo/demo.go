// Package demo backs `differently-server --demo`: a self-contained
// installation with mock connectors and generated traffic, for showing
// placement without any real provider.
//
// Everything runs through the real control plane. The pools use the fake
// adapter (dev mode only), so the scheduler, the policy engine, the audit log
// and the API behave exactly as they do in production; only the sandboxes
// are simulated. Nothing here is reachable outside --demo.
package demo

import (
	"bytes"
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/different-ai/differently/internal/config"
	"github.com/different-ai/differently/pkg/api"
)

// Project is the dev-mode project the generated sandboxes belong to.
const Project = "prj_default"

func attested(names ...string) map[string]api.Attestation {
	out := map[string]api.Attestation{}
	for _, n := range names {
		out[n] = api.Attestation{Evidence: "demo data"}
	}
	return out
}

// TimeScale compresses time for the demo: prices are multiplied by it, so one
// second a mock sandbox runs is billed like an hour. Budgets then fill within
// a few minutes of traffic instead of a month.
const TimeScale = 3600

// Config is the demo installation: two private pools standing in for your own
// cloud accounts and four external ones standing in for vendors, with regions,
// list prices and attestations, and a policy that lets eligible work overflow.
func Config(dataDir string) config.Config {
	c := config.DevDefault(dataDir)
	pool := func(name, trust, region string, slots int, cpuHour, memHour, budget float64, held ...string) config.PoolConfig {
		p := config.PoolConfig{
			Name: name, Adapter: "fake", Trust: trust, Region: region, MaxConcurrency: slots,
			CostModel:    api.CostModel{Currency: "USD", CPUHour: cpuHour * TimeScale, MemoryGiBHour: memHour * TimeScale},
			Attestations: attested(held...), Labels: map[string]string{"demo": "true"},
		}
		if budget > 0 {
			p.Budget = &api.Budget{Monthly: budget}
		}
		return p
	}
	c.Pools = []config.PoolConfig{
		// List prices per vCPU-hour and GiB-hour as read on 2026-11-16; the
		// two private pools carry a nominal cost for your own nodes. The
		// attestations are illustrative, not a statement about any vendor.
		pool("aws-eks", api.TrustPrivate, "us", 6, 0.012, 0.004, 0, "soc2-type2", "hipaa-baa"),
		pool("azure-aks", api.TrustPrivate, "eu", 4, 0.014, 0.005, 0, "soc2-type2", "hipaa-baa", "iso27001"),
		pool("northflank", api.TrustExternal, "eu", 8, 0.01667, 0.00833, 150, "soc2-type2"),
		pool("freestyle", api.TrustExternal, "us", 8, 0.04032, 0.0129, 150),
		pool("daytona", api.TrustExternal, "eu", 8, 0.0504, 0.0162, 60, "soc2-type2", "hipaa-baa"),
		pool("e2b", api.TrustExternal, "us", 8, 0.0504, 0.0162, 200, "soc2-type2", "hipaa-baa"),
	}
	c.Classifications = []api.Classification{
		{Name: "private-only", Description: "Must never leave private pools.", External: api.ExternalNever},
		{Name: "internal", Description: "May overflow to approved external pools.", External: api.ExternalOverflow},
		{Name: "regulated", Description: "Only pools with SOC 2 Type II and a signed BAA.", External: api.ExternalOverflow, RequiredAttestations: []string{"soc2-type2", "hipaa-baa"}},
		{Name: "public", Description: "No residency restriction beyond policy.", External: api.ExternalAllowed},
	}
	c.Defaults.Classification = "internal"
	c.Policy = api.PlacementPolicy{Scope: "global", External: api.ExternalAllowed, ExternalPools: []string{"northflank", "freestyle", "daytona", "e2b"}}
	c.RateLimit.RequestsPerSecond = 0
	// The admin console's dev server, so its Live routing page can read this server.
	c.CORS.AllowedOrigins = []string{"http://localhost:4321", "http://127.0.0.1:4321"}
	return c
}

// kind is one sort of generated request.
type kind struct {
	Name   string
	Weight int
	Req    api.CreateSandboxRequest
}

func kinds() []kind {
	small := &api.Resources{CPU: 1, MemoryMiB: 2048}
	eu := &api.Placement{Regions: []string{"eu"}}
	return []kind{
		// Everyday engineering work: may overflow to an approved vendor.
		{"code-review-agent", 40, api.CreateSandboxRequest{Classification: "internal", Resources: small}},
		// Patient data: only pools with SOC 2 Type II and a signed BAA.
		{"claims-processing", 15, api.CreateSandboxRequest{Classification: "regulated", Resources: small}},
		// EU customers' data must stay in EU regions.
		{"eu-customer-analytics", 15, api.CreateSandboxRequest{Classification: "internal", Resources: small, Placement: eu}},
		// Deal documents never leave your own infrastructure.
		{"deal-room-analysis", 15, api.CreateSandboxRequest{Classification: "private-only", Resources: small}},
		// Public marketing previews: cheapest connector wins.
		{"marketing-preview", 15, api.CreateSandboxRequest{Classification: "public", Resources: small, Placement: &api.Placement{Strategy: api.StrategyCost}}},
	}
}

// Scenario is a situation the presenter can switch to: a traffic shape, the
// vendors that are down, and what to say about it.
type Scenario struct {
	ID    string         `json:"id"`
	Label string         `json:"label"`
	Rate  float64        `json:"rate"` // requests per second
	Mix   map[string]int `json:"mix,omitempty"`
	// Down lists pools switched off for the scenario; every other pool is on.
	Down []string `json:"down,omitempty"`
	// Say is the talk track: what the audience should notice.
	Say string `json:"say"`
}

// MinLife and MaxLife bound how long a generated sandbox runs before the
// generator deletes it.
const (
	MinLife = 6 * time.Second
	MaxLife = 17 * time.Second
)

// Scenarios are the situations on offer, in the order a demo usually runs.
// A normal day's rate times MaxLife fits in the private pools' slots, so its
// talk track holds: nothing leaves your own accounts.
var Scenarios = []Scenario{
	{ID: "steady", Label: "A normal day", Rate: 0.5,
		Say: "Everything runs on your own AWS and Azure accounts. No vendor sees any work, and nothing is billed outside your cloud."},
	{ID: "burst", Label: "Quarter-end peak", Rate: 4,
		Say: "Your own capacity fills, so eligible work overflows to approved vendors, cheapest first. Deal-room work is private-only: it waits for your capacity and never leaves. Watch a vendor stop taking work when its monthly budget is reached."},
	{ID: "regulated", Label: "Regulated workload", Rate: 2.5, Mix: map[string]int{"claims-processing": 85, "deal-room-analysis": 15},
		Say: "Claims processing handles patient data. It only lands on connectors with a SOC 2 Type II report and a signed BAA. Freestyle and Northflank stay empty, and every decision says why."},
	{ID: "residency", Label: "EU data residency", Rate: 2.5, Mix: map[string]int{"eu-customer-analytics": 80, "code-review-agent": 20},
		Say: "EU customer analytics may only run in EU regions: Azure West Europe first, then EU vendors. US connectors are ruled out by region, whatever their price."},
	{ID: "incident", Label: "Vendor incident", Rate: 3.5, Down: []string{"daytona", "e2b"},
		Say: "Two vendors are switched off, as you would do during an incident or a contract dispute. Work re-routes to the remaining connectors within seconds, with no change to any application. Claims processing has no vendor with a BAA left, so it waits for your own capacity rather than go anywhere else."},
	{ID: "paused", Label: "Pause traffic", Rate: 0,
		Say: "Traffic is paused. Open any line in the decision log to walk through how each connector was judged, or the audit log and trust report for the evidence an auditor would ask for."},
}

// Generator creates and deletes sandboxes through the public API.
type Generator struct {
	BaseURL string
	Token   string
	Log     *slog.Logger

	mu       sync.Mutex
	scenario Scenario
}

// NewGenerator returns a generator on the steady scenario.
func NewGenerator(baseURL, token string, log *slog.Logger) *Generator {
	if log == nil {
		log = slog.Default()
	}
	return &Generator{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, Log: log, scenario: Scenarios[0]}
}

func (g *Generator) current() Scenario {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.scenario
}

// Set switches the scenario by ID and reports whether it exists. Pools the
// scenario takes down are switched off through the admin API, and the others
// back on, the way an operator would.
func (g *Generator) Set(id string) bool {
	for _, s := range Scenarios {
		if s.ID == id {
			g.mu.Lock()
			g.scenario = s
			g.mu.Unlock()
			go g.applyPools(s)
			return true
		}
	}
	return false
}

// Pools are the demo pools a scenario may switch off.
var Pools = []string{"aws-eks", "azure-aks", "northflank", "freestyle", "daytona", "e2b"}

func (g *Generator) applyPools(s Scenario) {
	client := &http.Client{Timeout: 5 * time.Second}
	for _, name := range Pools {
		g.do(context.Background(), client, http.MethodPatch, "/v1/admin/pools/"+name, map[string]bool{"enabled": !slices.Contains(s.Down, name)}, nil)
	}
}

func pick(s Scenario) kind {
	all := kinds()
	total := 0
	weight := func(k kind) int {
		if s.Mix != nil {
			return s.Mix[k.Name]
		}
		return k.Weight
	}
	for _, k := range all {
		total += weight(k)
	}
	r := rand.IntN(max(total, 1))
	for _, k := range all {
		if r -= weight(k); r < 0 {
			return k
		}
	}
	return all[0]
}

// Run generates traffic until ctx ends.
func (g *Generator) Run(ctx context.Context) {
	client := &http.Client{Timeout: 10 * time.Second}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	due := 0.0
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		s := g.current()
		for due += s.Rate / 10; due >= 1; due-- {
			go g.one(ctx, client, pick(s))
		}
	}
}

// one creates a sandbox and deletes it after a short simulated lifetime.
func (g *Generator) one(ctx context.Context, client *http.Client, k kind) {
	req := k.Req
	req.ProjectID = Project
	req.Labels = map[string]string{"workload": k.Name}
	var sb api.Sandbox
	if code := g.do(ctx, client, http.MethodPost, "/v1/sandboxes", req, &sb); code != http.StatusAccepted || sb.ID == "" {
		return // refused by policy: that is part of the demo, and it is in the audit log
	}
	life := MinLife + rand.N(MaxLife-MinLife)
	select {
	case <-ctx.Done():
	case <-time.After(life):
		g.do(context.WithoutCancel(ctx), client, http.MethodDelete, "/v1/sandboxes/"+sb.ID, nil, nil)
	}
}

func (g *Generator) do(ctx context.Context, client *http.Client, method, path string, body, out any) int {
	var r *bytes.Reader = bytes.NewReader(nil)
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.BaseURL+path, r)
	if err != nil {
		return 0
	}
	req.Header.Set("Authorization", "Bearer "+g.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			g.Log.Debug("demo traffic request failed", "err", err)
		}
		return 0
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

// Page is the presenter's remote: the scenarios with their talk track. It
// holds no secret; switching a scenario asks for the operator token once and
// keeps it in the tab.
//
//go:embed presenter.html
var page []byte

// PageHandler serves the presenter page.
func PageHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
		_, _ = w.Write(page)
	})
}

// Handler serves /demo/state: GET reports the scenario and the ones on offer,
// POST switches it and needs the operator token.
func (g *Generator) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
		case http.MethodPost:
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(g.Token)) != 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			var in struct {
				Scenario string `json:"scenario"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in); err != nil || !g.Set(in.Scenario) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"demo": true, "scenario": g.current(), "scenarios": Scenarios})
	})
}
