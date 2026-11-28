package main

import (
	"context"
	"strings"
	"testing"

	"github.com/different-ai/differently/pkg/api"
	"github.com/different-ai/differently/pkg/client"
)

func orgNamed(t *testing.T, c *client.Client, name string) (api.Organization, bool) {
	t.Helper()
	var list api.List[api.Organization]
	if err := c.Do(context.Background(), "GET", "/v1/admin/organizations", nil, &list); err != nil {
		t.Fatal(err)
	}
	for _, o := range list.Items {
		if o.Name == name {
			return o, true
		}
	}
	return api.Organization{}, false
}

func projectsOf(t *testing.T, c *client.Client, orgID string) map[string]api.Project {
	t.Helper()
	var list api.List[api.Project]
	if err := c.Do(context.Background(), "GET", "/v1/projects?orgId="+orgID, nil, &list); err != nil {
		t.Fatal(err)
	}
	out := map[string]api.Project{}
	for _, p := range list.Items {
		out[p.Name] = p
	}
	return out
}

func policyAt(t *testing.T, c *client.Client, scope string) (api.PlacementPolicy, bool) {
	t.Helper()
	var list api.List[api.PlacementPolicy]
	if err := c.Do(context.Background(), "GET", "/v1/policies", nil, &list); err != nil {
		t.Fatal(err)
	}
	for _, p := range list.Items {
		if p.Scope == scope {
			return p, true
		}
	}
	return api.PlacementPolicy{}, false
}

const tenancySetup = `
pools:
  - name: private-a
    adapter: fake
    trust: private
organizations:
  - name: acme
    quota: {maxActiveSandboxes: 50, maxCPU: 100}
    policy:
      allowedPools: [private-a]
    projects:
      - name: default
        quota: {maxActiveSandboxes: 10}
      - name: ml
        policy:
          maxQueueSeconds: 600
`

// A new organization with its projects and policies is created in one
// apply, although the projects and policies need IDs the server only gives
// when the organization exists. A second apply finds nothing to do.
func TestApplyCreatesAnOrganizationWithItsProjectsAndPolicies(t *testing.T) {
	c := realServer(t)
	file := setup(t, tenancySetup)

	out := mustCLI(t, "diff", "-f", file)
	for _, want := range []string{
		"+ create           organization acme", "quota.maxActiveSandboxes: (unset) → 50", "quota.maxCPU: (unset) → 100",
		"+ create           policy organization acme", `allowedPools: (unset) → ["private-a"]`,
		"+ create           project acme/default", "quota.maxActiveSandboxes: (unset) → 10",
		"+ create           project acme/ml", "+ create           policy project acme/ml", "maxQueueSeconds: (unset) → 600",
		"6 to apply",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("diff lacks %q:\n%s", want, out)
		}
	}
	if _, ok := orgNamed(t, c, "acme"); ok {
		t.Fatal("diff created the organization")
	}

	out = mustCLI(t, "apply", "-f", file)
	if !strings.Contains(out, "Applied 6 change(s).") {
		t.Errorf("apply:\n%s", out)
	}
	org, ok := orgNamed(t, c, "acme")
	if !ok || org.Quota.MaxActiveSandboxes != 50 || org.Quota.MaxCPU != 100 {
		t.Fatalf("organization = %+v, found %v", org, ok)
	}
	projects := projectsOf(t, c, org.ID)
	if len(projects) != 2 || projects["default"].Quota.MaxActiveSandboxes != 10 || projects["ml"].OrgID != org.ID {
		t.Fatalf("projects = %+v", projects)
	}
	if p, ok := policyAt(t, c, "org:"+org.ID); !ok || len(p.AllowedPools) != 1 || p.AllowedPools[0] != "private-a" {
		t.Errorf("organization policy = %+v, found %v", p, ok)
	}
	if p, ok := policyAt(t, c, "project:"+projects["ml"].ID); !ok || p.MaxQueueSeconds != 600 {
		t.Errorf("project policy = %+v, found %v", p, ok)
	}
	if _, ok := policyAt(t, c, "project:"+projects["default"].ID); ok {
		t.Error("a policy was stored for a project the file gives none")
	}

	// Nothing left to do, and nothing done twice.
	out = mustCLI(t, "diff", "-f", file, "--exit-code")
	if !strings.Contains(out, "The server matches") {
		t.Errorf("diff after apply:\n%s", out)
	}
	mustCLI(t, "apply", "-f", file)
	if again, _ := orgNamed(t, c, "acme"); again.ID != org.ID || len(projectsOf(t, c, org.ID)) != 2 {
		t.Error("a second apply created something again")
	}
}

// A quota and a scoped policy are whole when the file gives them: a limit
// taken out of the file is taken off. One the file does not give at all is
// not compared.
func TestApplyChangesQuotasAndScopedPoliciesWhole(t *testing.T) {
	c := realServer(t)
	mustCLI(t, "apply", "-f", setup(t, tenancySetup))
	org, _ := orgNamed(t, c, "acme")
	ml := projectsOf(t, c, org.ID)["ml"]
	// A quota for a project whose quota the file never gives, set by hand.
	if err := c.Do(context.Background(), "PATCH", "/v1/admin/projects/"+ml.ID, map[string]any{"quota": map[string]any{"maxSnapshots": 3}}, nil); err != nil {
		t.Fatal(err)
	}

	changed := setup(t, `
organizations:
  - name: acme
    quota: {maxActiveSandboxes: 80}
    policy:
      deniedPools: [vendor-b]
    projects:
      - name: default
        quota: {maxActiveSandboxes: 10, maxTTLSeconds: 3600}
      - name: ml
      - name: batch
        quota: {maxQueued: 5}
`)
	out := mustCLI(t, "diff", "-f", changed)
	for _, want := range []string{
		"~ change           organization acme", "quota.maxActiveSandboxes: 50 → 80", "quota.maxCPU: 100 → (unset)",
		"~ change           policy organization acme", `allowedPools: ["private-a"] → (unset)`, `deniedPools: (unset) → ["vendor-b"]`,
		"~ change           project acme/default", "quota.maxTTLSeconds: (unset) → 3600",
		"+ create           project acme/batch", "4 to apply",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("diff lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "acme/ml") || strings.Contains(out, "maxSnapshots") {
		t.Errorf("diff touches the project whose quota and policy the file leaves out:\n%s", out)
	}
	mustCLI(t, "apply", "-f", changed)

	org, _ = orgNamed(t, c, "acme")
	if org.Quota.MaxActiveSandboxes != 80 || org.Quota.MaxCPU != 0 {
		t.Errorf("organization quota = %+v (the file's quota is the whole quota)", org.Quota)
	}
	projects := projectsOf(t, c, org.ID)
	if projects["default"].Quota.MaxTTLSeconds != 3600 || projects["batch"].Quota.MaxQueued != 5 || projects["ml"].Quota.MaxSnapshots != 3 {
		t.Errorf("projects = %+v", projects)
	}
	if p, _ := policyAt(t, c, "org:"+org.ID); len(p.AllowedPools) != 0 || len(p.DeniedPools) != 1 {
		t.Errorf("organization policy = %+v", p)
	}
	if p, ok := policyAt(t, c, "project:"+ml.ID); !ok || p.MaxQueueSeconds != 600 {
		t.Errorf("the project policy the file no longer gives was changed: %+v, found %v", p, ok)
	}
	if out := mustCLI(t, "diff", "-f", changed, "--exit-code"); !strings.Contains(out, "The server matches") {
		t.Errorf("diff after apply:\n%s", out)
	}
}

// Organizations and projects are never deleted: one the file does not name
// is listed, with or without --prune. A file with no organizations key says
// nothing about them at all.
func TestOrganizationsAreListedNeverDeleted(t *testing.T) {
	c := realServer(t)
	mustCLI(t, "apply", "-f", setup(t, tenancySetup))
	fewer := setup(t, `
pools:
  - name: private-a
    adapter: fake
    trust: private
organizations:
  - name: acme
    projects:
      - name: default
`)
	for _, args := range [][]string{{"apply", "-f", fewer}, {"apply", "-f", fewer, "--prune"}} {
		out := mustCLI(t, args...)
		if !strings.Contains(out, "not in the file  project acme/ml") || !strings.Contains(out, "The server matches") || strings.Contains(out, "delete") {
			t.Errorf("differently %s:\n%s", strings.Join(args, " "), out)
		}
	}
	org, _ := orgNamed(t, c, "acme")
	if _, ok := projectsOf(t, c, org.ID)["ml"]; !ok {
		t.Fatal("a project was deleted")
	}
	// With nothing --prune could delete, the summary does not offer it; with
	// a pool the file does not name, it offers it for pools and templates
	// only.
	if out := mustCLI(t, "diff", "-f", fewer); !strings.Contains(out, "(left alone)") || strings.Contains(out, "--prune") {
		t.Errorf("diff:\n%s", out)
	}
	noPool := setup(t, "organizations:\n  - name: acme\n")
	if out := mustCLI(t, "diff", "-f", noPool); !strings.Contains(out, "--prune deletes the pools and templates among them") {
		t.Errorf("diff with a pool the file does not name:\n%s", out)
	}
	out := mustCLI(t, "apply", "-f", noPool, "--prune")
	if !strings.Contains(out, "- delete           pool private-a") || !strings.Contains(out, "not in the file  organization Default") {
		t.Errorf("apply --prune:\n%s", out)
	}
	if _, ok := orgNamed(t, c, "Default"); !ok {
		t.Fatal("--prune deleted an organization")
	}

	noOrgs := mustCLI(t, "diff", "-f", setup(t, "policy: {}\n"))
	if strings.Contains(noOrgs, "organization") || strings.Contains(noOrgs, "project") {
		t.Errorf("a file with no organizations key lists them:\n%s", noOrgs)
	}
	// An empty list does manage them: everything on the server is listed.
	empty := mustCLI(t, "diff", "-f", setup(t, "organizations: []\n"))
	if !strings.Contains(empty, "not in the file  organization acme") {
		t.Errorf("an empty organizations list:\n%s", empty)
	}
}

// The server lets two organizations share a name. The file finds them by
// name, so it cannot say which one it means: nothing is applied.
func TestTwoOrganizationsWithOneNameCannotBeApplied(t *testing.T) {
	c := realServer(t)
	for i := 0; i < 2; i++ {
		if err := c.Do(context.Background(), "POST", "/v1/admin/organizations", map[string]any{"name": "twin"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	file := setup(t, `
pools:
  - name: private-a
    adapter: fake
    trust: private
organizations:
  - name: twin
    quota: {maxCPU: 4}
`)
	out, _, err := runCLI(t, "", "apply", "-f", file)
	if err == nil || !strings.Contains(err.Error(), "cannot be applied; nothing was changed") || !strings.Contains(out, "2 organizations with this name") {
		t.Fatalf("apply: err %v\n%s", err, out)
	}
	var p api.Pool
	if err := c.Do(context.Background(), "GET", "/v1/admin/pools/private-a", nil, &p); err == nil {
		t.Error("the pool beside it was created")
	}
}

func TestOrganizationMistakesInTheFileAreRefused(t *testing.T) {
	t.Setenv("DIFFERENTLY_URL", "http://127.0.0.1:1")
	for name, tc := range map[string]struct{ body, want string }{
		"an organization with no name": {"organizations:\n  - quota: {maxCPU: 1}\n", "has no name"},
		"an organization twice":        {"organizations:\n  - name: a\n  - name: a\n", "listed twice"},
		"a project twice":              {"organizations:\n  - name: a\n    projects: [{name: p}, {name: p}]\n", "lists project p twice"},
		"a project with no name":       {"organizations:\n  - name: a\n    projects: [{quota: {maxCPU: 1}}]\n", "has no name"},
		"a scope on a nested policy":   {"organizations:\n  - name: a\n    policy: {scope: global}\n", "belongs to what it is under"},
		"an unknown quota limit":       {"organizations:\n  - name: a\n    quota: {maxGPUs: 1}\n", "unknown field"},
		"an organization's ID":         {"organizations:\n  - name: a\n    id: org_1\n", "unknown field"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := runCLI(t, "", "diff", "-f", setup(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}
