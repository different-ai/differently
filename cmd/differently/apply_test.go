package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/different-ai/differently/internal/auditlog"
	"github.com/different-ai/differently/internal/config"
	"github.com/different-ai/differently/internal/pools"
	"github.com/different-ai/differently/internal/scheduler"
	"github.com/different-ai/differently/internal/server"
	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/internal/wiring"
	"github.com/different-ai/differently/pkg/api"
	"github.com/different-ai/differently/pkg/client"
)

var registerAdapters sync.Once

// realServer is the real control plane in this process, in dev mode, with
// the CLI pointed at it: the memory store, the scheduler, the server's own
// handlers and one pool its config file declares (local, devhost). The file
// commands are tested against it and not against a stand-in, because what
// they must agree with is what the server validates, stores and reports.
func realServer(t *testing.T) *client.Client {
	t.Helper()
	registerAdapters.Do(wiring.Register)
	dir := t.TempDir()
	cfg := config.DevDefault(filepath.Join(dir, "data"))
	cfg.RateLimit.RequestsPerSecond = 0
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	st := store.NewMemory()
	rec, err := auditlog.New(st, log, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rec.Close() })
	pm := pools.NewManager(pools.Deps{Store: st, Dev: true, DataDir: cfg.DataDir, Logger: log, Resolve: server.CredentialResolver(st, nil)})
	inst, err := st.Installation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sch := scheduler.New(st, pm, cfg, inst, rec, log)
	srv := server.New(cfg, st, pm, sch, rec, log, inst)
	if err := srv.Bootstrap(ctx); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	token, err := os.ReadFile(filepath.Join(cfg.DataDir, "token"))
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "home")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("DIFFERENTLY_URL", ts.URL)
	t.Setenv("DIFFERENTLY_API_KEY", strings.TrimSpace(string(token)))
	t.Setenv("DIFFERENTLY_PROJECT", "")
	return client.New(ts.URL, strings.TrimSpace(string(token)))
}

// setup writes a setup file and returns its path.
func setup(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "differently.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustCLI(t *testing.T, args ...string) string {
	t.Helper()
	out, _, err := runCLI(t, "", args...)
	if err != nil {
		t.Fatalf("differently %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func pool(t *testing.T, c *client.Client, name string) api.Pool {
	t.Helper()
	var p api.Pool
	if err := c.Do(context.Background(), "GET", "/v1/admin/pools/"+name, nil, &p); err != nil {
		t.Fatalf("pool %s: %v", name, err)
	}
	return p
}

func templates(t *testing.T, c *client.Client) map[string]api.Template {
	t.Helper()
	var list api.List[api.Template]
	if err := c.Do(context.Background(), "GET", "/v1/templates", nil, &list); err != nil {
		t.Fatal(err)
	}
	out := map[string]api.Template{}
	for _, tpl := range list.Items {
		out[tpl.Name] = tpl
	}
	return out
}

func globalPolicy(t *testing.T, c *client.Client) api.PlacementPolicy {
	t.Helper()
	var list api.List[api.PlacementPolicy]
	if err := c.Do(context.Background(), "GET", "/v1/policies", nil, &list); err != nil {
		t.Fatal(err)
	}
	for _, p := range list.Items {
		if p.Scope == "global" {
			return p
		}
	}
	return api.PlacementPolicy{}
}

const firstSetup = `
pools:
  - name: private-a
    adapter: fake
    trust: private
    region: eu
    maxConcurrency: 20
    labels: {tier: gold}
    config: {latencyMs: "5"}
  - name: vendor-b
    adapter: fake
    trust: external
    maxConcurrency: 5
    costModel: {currency: USD, sandboxHour: 0.05}
    budget: {monthly: 100}
    attestations:
      soc2-type2: {evidence: "https://trust.example/soc2"}
policy:
  external: overflow
  externalPools: [vendor-b]
  strategy: private-first
templates:
  - name: python
    description: Python 3.12
    images:
      private-a: python:3.12-slim
      vendor-b: py312
`

// The whole point: apply makes the server match, a second apply and a diff
// then find nothing, and the server holds what the file says.
func TestApplyMakesTheServerMatchAndIsIdempotent(t *testing.T) {
	c := realServer(t)
	file := setup(t, firstSetup)

	out := mustCLI(t, "diff", "-f", file)
	for _, want := range []string{"+ create", "policy global", "pool private-a", "pool vendor-b", "template python", "4 to apply."} {
		if !strings.Contains(out, want) {
			t.Errorf("first diff lacks %q:\n%s", want, out)
		}
	}
	// A diff changes nothing.
	if _, ok := templates(t, c)["python"]; ok {
		t.Fatal("diff created the template")
	}

	out = mustCLI(t, "apply", "-f", file)
	if !strings.Contains(out, "Applied 4 change(s). The server matches "+file+".") {
		t.Errorf("apply:\n%s", out)
	}
	a, b := pool(t, c, "private-a"), pool(t, c, "vendor-b")
	if a.Adapter != "fake" || a.Trust != "private" || a.Region != "eu" || a.MaxConcurrency != 20 || a.Labels["tier"] != "gold" || a.Source != "api" || !a.Enabled {
		t.Errorf("private-a = %+v", a)
	}
	if b.Trust != "external" || b.CostModel.SandboxHour != 0.05 || b.Budget == nil || b.Budget.Monthly != 100 || b.Attestations["soc2-type2"].Evidence != "https://trust.example/soc2" {
		t.Errorf("vendor-b = %+v", b)
	}
	if p := globalPolicy(t, c); p.External != "overflow" || len(p.ExternalPools) != 1 || p.ExternalPools[0] != "vendor-b" || p.Strategy != "private-first" {
		t.Errorf("policy = %+v", p)
	}
	if tpl := templates(t, c)["python"]; tpl.Images["private-a"] != "python:3.12-slim" || tpl.Images["vendor-b"] != "py312" || tpl.Description != "Python 3.12" {
		t.Errorf("template = %+v", tpl)
	}

	if out := mustCLI(t, "diff", "-f", file, "--exit-code"); !strings.Contains(out, "The server matches "+file+".") {
		t.Errorf("diff after apply:\n%s", out)
	}
	if out := mustCLI(t, "apply", "-f", file); !strings.Contains(out, "The server matches") || strings.Contains(out, "Applied") {
		t.Errorf("second apply:\n%s", out)
	}
}

// Changes of every kind the API can make to what exists, and the sets that
// are exact: an attestation or a template image the file no longer gives is
// removed, because each widens where work may run.
func TestApplyChangesWhatDiffersAndKeepsExactSets(t *testing.T) {
	c := realServer(t)
	mustCLI(t, "apply", "-f", setup(t, firstSetup))
	// Something the file never mentions, set by hand: it must be left alone.
	if err := c.Do(context.Background(), "PATCH", "/v1/admin/pools/private-a", map[string]any{"costModel": map[string]any{"currency": "EUR", "cpuHour": 0.01}}, nil); err != nil {
		t.Fatal(err)
	}

	changed := setup(t, `
pools:
  - name: private-a
    adapter: fake
    trust: private
    maxConcurrency: 8
    enabled: false
  - name: vendor-b
    adapter: fake
    trust: external
    budget: {monthly: 0}
    attestations:
      hipaa-baa: {evidence: "contract 42"}
policy:
  external: never
templates:
  - name: python
    images:
      private-a: python:3.13-slim
`)
	out := mustCLI(t, "diff", "-f", changed)
	for _, want := range []string{
		"~ change", "maxConcurrency: 20 → 8", "enabled: true → false",
		"budget.monthly: 100 → (unset)", "attestations.hipaa-baa: added", "attestations.soc2-type2: removed",
		"external: overflow → never", `externalPools: ["vendor-b"] → (unset)`,
		"images.private-a: python:3.12-slim → python:3.13-slim", "images.vendor-b: py312 → (unset)",
		"4 to apply.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("diff lacks %q:\n%s", want, out)
		}
	}
	for _, not := range []string{"costModel", "region", "description"} {
		if strings.Contains(out, not) {
			t.Errorf("diff mentions %s, which the file leaves out:\n%s", not, out)
		}
	}
	mustCLI(t, "apply", "-f", changed)

	a, b := pool(t, c, "private-a"), pool(t, c, "vendor-b")
	if a.MaxConcurrency != 8 || a.Enabled || a.Region != "eu" || a.CostModel.Currency != "EUR" {
		t.Errorf("private-a = %+v (region and the cost model were not in the file and must stay)", a)
	}
	if b.Budget != nil || len(b.Attestations) != 1 || b.Attestations["hipaa-baa"].Evidence != "contract 42" || b.MaxConcurrency != 5 {
		t.Errorf("vendor-b = %+v", b)
	}
	if p := globalPolicy(t, c); p.External != "never" || len(p.ExternalPools) != 0 || p.Strategy != "" {
		t.Errorf("policy = %+v (the file is the whole policy)", p)
	}
	tpl := templates(t, c)["python"]
	if len(tpl.Images) != 1 || tpl.Images["private-a"] != "python:3.13-slim" || tpl.Description != "Python 3.12" {
		t.Errorf("template = %+v", tpl)
	}
	if out := mustCLI(t, "diff", "-f", changed); !strings.Contains(out, "The server matches") {
		t.Errorf("diff after apply:\n%s", out)
	}
}

// What cannot be applied stops everything, before anything changes: an
// existing pool's trust or config, and a pool the server's own config file
// declares.
func TestApplyRefusesWhatCannotBeAppliedAndChangesNothing(t *testing.T) {
	c := realServer(t)
	mustCLI(t, "apply", "-f", setup(t, firstSetup))

	for name, body := range map[string]string{
		"an existing pool's trust":  strings.Replace(firstSetup, "trust: private", "trust: external", 1),
		"an existing pool's config": strings.Replace(firstSetup, `latencyMs: "5"`, `latencyMs: "50"`, 1),
		"a pool the server's config file declares": firstSetup + `
  - name: local
    adapter: devhost
    maxConcurrency: 3
`,
	} {
		t.Run(name, func(t *testing.T) {
			// A change that could be applied rides along and must not be.
			body := strings.Replace(body, "maxConcurrency: 5", "maxConcurrency: 6", 1)
			if strings.Contains(name, "config file") {
				body = strings.Replace(firstSetup, "pools:\n", "pools:\n  - name: local\n    adapter: devhost\n    maxConcurrency: 3\n", 1)
				body = strings.Replace(body, "maxConcurrency: 5", "maxConcurrency: 6", 1)
			}
			file := setup(t, body)
			out, _, err := runCLI(t, "", "apply", "-f", file)
			if err == nil || !strings.Contains(err.Error(), "cannot be applied; nothing was changed") {
				t.Fatalf("apply: err %v\n%s", err, out)
			}
			if !strings.Contains(out, "! cannot apply") {
				t.Errorf("output does not say what cannot be applied:\n%s", out)
			}
			if got := pool(t, c, "vendor-b").MaxConcurrency; got != 5 {
				t.Errorf("vendor-b maxConcurrency = %d: something was applied", got)
			}
			if got := pool(t, c, "private-a"); got.Trust != "private" || got.Config["latencyMs"] != "5" {
				t.Errorf("private-a = %+v", got)
			}
			if _, _, err := runCLI(t, "", "diff", "-f", file, "--exit-code"); err == nil {
				t.Error("diff --exit-code passed with something that cannot be applied")
			}
		})
	}

	// The same pool named as the server's config file has it is no difference.
	same := setup(t, "pools:\n  - name: local\n    adapter: devhost\n    trust: private\n    maxConcurrency: 20\n")
	if out := mustCLI(t, "diff", "-f", same); strings.Contains(out, "local") || strings.Contains(out, "cannot apply") {
		t.Errorf("a config-declared pool named as it is shows as a difference:\n%s", out)
	}
}

// What the file does not name is left alone, and listed. --prune deletes
// what was set through the API, templates before the pools they name, and
// never what the server's config file declares.
func TestPruneDeletesOnlyWhatTheFileDoesNotNameAndTheAPISet(t *testing.T) {
	c := realServer(t)
	mustCLI(t, "apply", "-f", setup(t, firstSetup))
	less := setup(t, `
pools:
  - name: private-a
    adapter: fake
    trust: private
policy:
  external: never
`)
	out := mustCLI(t, "apply", "-f", less)
	if !strings.Contains(out, "not in the file  pool vendor-b") || !strings.Contains(out, "not in the file  template python") || !strings.Contains(out, "apart from 2 item(s) the file does not name") {
		t.Errorf("apply without --prune:\n%s", out)
	}
	if strings.Contains(out, "local") {
		t.Errorf("the config-declared pool is listed as if the file could manage it:\n%s", out)
	}
	if pool(t, c, "vendor-b").Name != "vendor-b" || templates(t, c)["python"].Name != "python" {
		t.Fatal("something was deleted without --prune")
	}

	out = mustCLI(t, "diff", "-f", less, "--prune")
	if !strings.Contains(out, "- delete") || !strings.Contains(out, "2 to apply.") {
		t.Errorf("diff --prune:\n%s", out)
	}
	out = mustCLI(t, "apply", "-f", less, "--prune")
	if strings.Index(out, "template python") > strings.Index(out, "pool vendor-b") {
		t.Errorf("the pool was deleted before the template that names it:\n%s", out)
	}
	if _, ok := templates(t, c)["python"]; ok {
		t.Error("the template is still there")
	}
	var p api.Pool
	if err := c.Do(context.Background(), "GET", "/v1/admin/pools/vendor-b", nil, &p); err == nil {
		t.Error("vendor-b is still there")
	}
	if pool(t, c, "local").Source != "config" || pool(t, c, "private-a").Name != "private-a" {
		t.Error("a pool that must stay is gone")
	}
	if out := mustCLI(t, "diff", "-f", less, "--prune", "--exit-code"); !strings.Contains(out, "The server matches") {
		t.Errorf("diff after prune:\n%s", out)
	}
}

// A call the server refuses stops the apply, says how far it got, and leaves
// what was done. Applying the corrected file goes on from there.
func TestApplyStopsAtTheFirstRefusalAndCanBeRunAgain(t *testing.T) {
	c := realServer(t)
	bad := setup(t, `
pools:
  - name: private-a
    adapter: fake
    trust: private
templates:
  - name: python
    images: {no-such-pool: x}
`)
	out, _, err := runCLI(t, "", "apply", "-f", bad)
	if err == nil || !strings.Contains(err.Error(), "template python") || !strings.Contains(err.Error(), "applied 1 of 2") {
		t.Fatalf("apply: err %v\n%s", err, out)
	}
	if pool(t, c, "private-a").Name != "private-a" {
		t.Fatal("the pool created before the refusal is gone")
	}
	good := setup(t, `
pools:
  - name: private-a
    adapter: fake
    trust: private
templates:
  - name: python
    images: {private-a: x}
`)
	if out := mustCLI(t, "apply", "-f", good); !strings.Contains(out, "Applied 1 change(s)") {
		t.Errorf("apply of the corrected file:\n%s", out)
	}
}

func TestSetupFileMistakesAreRefusedBeforeTheServerIsAsked(t *testing.T) {
	// No server: every one of these must fail on the file alone.
	t.Setenv("DIFFERENTLY_URL", "http://127.0.0.1:1")
	for name, tc := range map[string]struct{ body, want string }{
		"an unknown key":       {"pool:\n  - name: a\n", "unknown field"},
		"a key in a pool":      {"pools:\n  - name: a\n    adapter: fake\n    trsut: private\n", "unknown field"},
		"a pool with no name":  {"pools:\n  - adapter: fake\n", "has no name"},
		"no adapter":           {"pools:\n  - name: a\n", "has no adapter"},
		"a pool twice":         {"pools:\n  - {name: a, adapter: fake}\n  - {name: a, adapter: fake}\n", "listed twice"},
		"an empty template":    {"templates:\n  - name: t\n", "says nothing"},
		"a template twice":     {"templates:\n  - {name: t, description: x}\n  - {name: t, description: y}\n", "listed twice"},
		"a policy for a scope": {"policy:\n  scope: org:org_1\n", "global placement policy"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := runCLI(t, "", "diff", "-f", setup(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
	if _, _, err := runCLI(t, "", "apply", "-f", filepath.Join(t.TempDir(), "missing.yaml")); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("a missing file: %v", err)
	}
}

func TestDiffAsJSON(t *testing.T) {
	realServer(t)
	out := mustCLI(t, "diff", "-f", setup(t, firstSetup), "-o", "json")
	var got struct {
		Changes []planned `json:"changes"`
		ToApply int       `json:"toApply"`
		Blocked int       `json:"blocked"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if got.ToApply != 4 || got.Blocked != 0 || len(got.Changes) != 4 || got.Changes[0].Kind != "policy" || got.Changes[1].Action != actCreate {
		t.Errorf("json = %+v", got)
	}
}

// In the global policy, leaving the strategy or the external mode out means
// their defaults. A file that writes the default and one that leaves it out
// describe the same policy, so neither shows a difference from the other.
func TestPolicyDefaultsAreNotDifferences(t *testing.T) {
	realServer(t)
	// A new server's global policy is external: never, strategy: private-first.
	for name, body := range map[string]string{
		"nothing written":  "policy: {}\n",
		"defaults written": "policy:\n  external: never\n  strategy: private-first\n",
	} {
		if out := mustCLI(t, "diff", "-f", setup(t, body)); !strings.Contains(out, "The server matches") {
			t.Errorf("%s:\n%s", name, out)
		}
	}
	out := mustCLI(t, "diff", "-f", setup(t, "policy:\n  external: overflow\n"))
	if !strings.Contains(out, "external: never → overflow") || strings.Contains(out, "strategy") {
		t.Errorf("a real change beside an unwritten default:\n%s", out)
	}
}

// The example in the guide is a file the commands read.
func TestTheGuidesExampleIsAValidSetupFile(t *testing.T) {
	guide, err := os.ReadFile(filepath.Join("..", "..", "docs", "guides", "setup-file.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(guide), "```yaml\n")
	example, _, ok2 := strings.Cut(rest, "```")
	if !ok || !ok2 {
		t.Fatal("the guide has no yaml example")
	}
	d, err := readApplyFile(setup(t, example))
	if err != nil {
		t.Fatalf("the guide's example is refused: %v", err)
	}
	if len(d.Pools) != 2 || d.Policy == nil || len(d.Templates) != 1 || d.Pools[1].Credentials.String() != "secret:daytona" ||
		len(d.Organizations) != 1 || len(d.Organizations[0].Projects) != 2 || d.Organizations[0].Quota.MaxCPU != 100 || !d.managesOrgs {
		t.Errorf("the guide's example reads as %+v", d.applyFile)
	}
}

// Keys are read without regard to case, by the file's parser and by the
// server. A key written in another case is still a key the file wrote.
func TestKeysInAnotherCaseAreStillManaged(t *testing.T) {
	c := realServer(t)
	mustCLI(t, "apply", "-f", setup(t, "pools:\n  - name: private-a\n    adapter: fake\n    trust: private\n    maxConcurrency: 20\n"))
	lower := setup(t, "pools:\n  - name: private-a\n    adapter: fake\n    trust: private\n    maxconcurrency: 7\n")
	if out := mustCLI(t, "diff", "-f", lower); !strings.Contains(out, "maxConcurrency: 20 → 7") {
		t.Fatalf("a key in lower case is not compared:\n%s", out)
	}
	mustCLI(t, "apply", "-f", lower)
	if got := pool(t, c, "private-a").MaxConcurrency; got != 7 {
		t.Errorf("maxConcurrency = %d", got)
	}
}
