package demo

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/different-ai/differently/pkg/api"
)

// The demo installation must be a valid configuration that only uses the
// fake adapter, so it can never reach a real provider.
func TestConfigIsValidAndOnlyFake(t *testing.T) {
	c := Config(t.TempDir())
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if !c.Dev {
		t.Fatal("the demo must run in dev mode: the fake adapter exists only there")
	}
	private := 0
	for _, p := range c.Pools {
		if p.Adapter != "fake" {
			t.Fatalf("pool %s uses the %s adapter", p.Name, p.Adapter)
		}
		if p.Trust == api.TrustPrivate {
			private++
		}
	}
	if private == 0 || private == len(c.Pools) {
		t.Fatalf("the demo needs private and external pools, got %d of %d private", private, len(c.Pools))
	}
	// Every request kind names a classification the config defines.
	for _, k := range kinds() {
		if _, ok := c.Classification(k.Req.Classification); !ok {
			t.Errorf("%s uses undefined classification %q", k.Name, k.Req.Classification)
		}
	}
}

// Every scenario only names request kinds and pools that exist, and has
// something for the presenter to say.
func TestScenariosAreConsistent(t *testing.T) {
	known := map[string]bool{}
	for _, k := range kinds() {
		known[k.Name] = true
	}
	pools := map[string]bool{}
	for _, p := range Config(t.TempDir()).Pools {
		pools[p.Name] = true
	}
	if len(pools) != len(Pools) {
		t.Fatalf("Pools lists %d names, the config has %d", len(Pools), len(pools))
	}
	for _, s := range Scenarios {
		if s.Say == "" || s.Label == "" {
			t.Errorf("%s: no label or talk track", s.ID)
		}
		for name := range s.Mix {
			if !known[name] {
				t.Errorf("%s: mix names unknown workload %q", s.ID, name)
			}
		}
		for _, name := range s.Down {
			if !pools[name] {
				t.Errorf("%s: takes down unknown pool %q", s.ID, name)
			}
		}
	}
}

func TestScenarioMixAndSwitching(t *testing.T) {
	g := NewGenerator("http://example.invalid", "tok", nil)
	if !g.Set("regulated") || g.Set("nope") || g.current().ID != "regulated" {
		t.Fatalf("scenario switching: %+v", g.current())
	}
	for i := 0; i < 200; i++ {
		if k := pick(g.current()); g.current().Mix[k.Name] == 0 {
			t.Fatalf("picked %s, which the scenario's mix leaves out", k.Name)
		}
	}
}

// Anyone may read the demo state; switching the scenario needs the token.
func TestStateHandlerNeedsTheTokenToSwitch(t *testing.T) {
	g := NewGenerator("http://example.invalid", "tok", nil)
	call := func(method, token, body string) int {
		req := httptest.NewRequest(method, "/demo/state", strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		g.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	for name, got := range map[string][2]int{
		"read":            {call(http.MethodGet, "", ""), 200},
		"switch no token": {call(http.MethodPost, "", `{"scenario":"burst"}`), 401},
		"switch wrong":    {call(http.MethodPost, "nope", `{"scenario":"burst"}`), 401},
		"unknown":         {call(http.MethodPost, "tok", `{"scenario":"x"}`), 400},
		"switch":          {call(http.MethodPost, "tok", `{"scenario":"burst"}`), 200},
		"delete":          {call(http.MethodDelete, "tok", ""), 405},
	} {
		if got[0] != got[1] {
			t.Errorf("%s: status %d, want %d", name, got[0], got[1])
		}
	}
	if g.current().ID != "burst" {
		t.Fatalf("scenario not switched: %+v", g.current())
	}
}
