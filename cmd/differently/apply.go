package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/different-ai/differently/internal/config"
	"github.com/different-ai/differently/pkg/api"
)

// diff and apply keep an installation's setup in a file in the repository:
// the pools work may run on, the global placement policy, the templates,
// and the organizations with their projects, quotas and policies
// (apply_tenancy.go).
// diff says what differs between the file and the server; apply makes the
// server match, through the same admin API the other commands use, and a
// second apply finds nothing to do.
//
// The file states what it names. A field the file leaves out is left as the
// server has it, and a pool or template the file does not name is left
// alone unless --prune is given. Three sets are exact when the file gives
// them, because each one decides where work may run: a pool's attestations,
// a template's images and the whole policy.

const defaultApplyFile = "differently.yaml"

// applyFile is the file's shape. pools has the shape of the server config
// file's pools, so an entry can move from one to the other unchanged.
type applyFile struct {
	Pools     []config.PoolConfig  `json:"pools,omitempty"`
	Policy    *api.PlacementPolicy `json:"policy,omitempty"`
	Templates []fileTemplate       `json:"templates,omitempty"`
	// Organizations is nil when the file has no such key: tenancy is then
	// not this file's business, and nothing about it is compared or listed.
	Organizations []fileOrg `json:"organizations,omitempty"`
}

type fileTemplate struct {
	Name        string            `json:"name"`
	Description *string           `json:"description,omitempty"`
	Pools       *[]string         `json:"pools,omitempty"`
	Images      map[string]string `json:"images,omitempty"`
}

// desired is the parsed file plus, for each pool, which keys it wrote: a
// key that is absent is not managed, which a zero value cannot say.
type desired struct {
	applyFile
	poolKeys []map[string]json.RawMessage
	// managesOrgs says the file has an organizations key, even an empty one.
	managesOrgs bool
}

func readApplyFile(path string) (desired, error) {
	var d desired
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return d, fmt.Errorf("%s does not exist; name the file with -f, or create it (pools, policy, templates)", path)
		}
		return d, err
	}
	if err := yaml.UnmarshalStrict(b, &d.applyFile); err != nil {
		return d, fmt.Errorf("%s: %w", path, err)
	}
	var raw struct {
		Pools []map[string]json.RawMessage `json:"pools"`
	}
	if err := yaml.Unmarshal(b, &raw); err != nil {
		return d, fmt.Errorf("%s: %w", path, err)
	}
	d.poolKeys = raw.Pools

	seen := map[string]bool{}
	for i, p := range d.Pools {
		switch {
		case p.Name == "":
			return d, fmt.Errorf("%s: pools[%d] has no name", path, i)
		case p.Adapter == "":
			return d, fmt.Errorf("%s: pool %s has no adapter", path, p.Name)
		case seen[p.Name]:
			return d, fmt.Errorf("%s: pool %s is listed twice", path, p.Name)
		}
		seen[p.Name] = true
	}
	seen = map[string]bool{}
	for i, t := range d.Templates {
		switch {
		case t.Name == "":
			return d, fmt.Errorf("%s: templates[%d] has no name", path, i)
		case seen[t.Name]:
			return d, fmt.Errorf("%s: template %s is listed twice", path, t.Name)
		case t.Description == nil && t.Pools == nil && len(t.Images) == 0:
			return d, fmt.Errorf("%s: template %s says nothing: give images (pool: image), pools or a description", path, t.Name)
		}
		seen[t.Name] = true
	}
	if p := d.Policy; p != nil {
		if p.Scope != "" && p.Scope != "global" {
			return d, fmt.Errorf("%s: policy is the global placement policy; scope %q cannot be set here (an organization's or a project's policy goes under it, in organizations)", path, p.Scope)
		}
		if !p.UpdatedAt.IsZero() || p.UpdatedBy != "" {
			return d, fmt.Errorf("%s: policy.updatedAt and policy.updatedBy are the server's to write", path)
		}
	}
	var keys struct {
		Organizations json.RawMessage `json:"organizations"`
	}
	_ = yaml.Unmarshal(b, &keys)
	d.managesOrgs = keys.Organizations != nil
	if err := checkOrgs(path, d.Organizations); err != nil {
		return d, err
	}
	return d, nil
}

// What a planned item does.
const (
	actCreate  = "create"
	actChange  = "change"
	actDelete  = "delete"
	actBlocked = "blocked" // differs, and cannot be made to match through the API
	actExtra   = "extra"   // on the server, not in the file; left alone
)

// planned is one pool, template or the policy, and what applying would do.
type planned struct {
	Kind    string   `json:"kind"`
	Name    string   `json:"name"`
	Action  string   `json:"action"`
	Details []string `json:"details,omitempty"`
	steps   []step
	// prunable marks something not in the file that --prune would delete.
	// Organizations and projects are never deleted by this command.
	prunable bool
}

// step is one API call. Most are known when the plan is made. One that
// needs the ID of something an earlier step creates (a project in a new
// organization) works its path and body out when it runs, from ids, and a
// step that creates something records its ID there.
type step struct {
	method, path string
	body         any
	resolve      func(ids map[string]string) (path string, body any)
	capture      func(response []byte, ids map[string]string)
}

func call(method, path string, body any) step { return step{method: method, path: path, body: body} }

// show renders a value for a diff line.
func show(v any) string {
	if s, ok := v.(string); ok {
		if s == "" {
			return "(unset)"
		}
		return s
	}
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" || string(b) == "{}" || string(b) == "[]" || string(b) == `""` {
		return "(unset)"
	}
	return string(b)
}

func differs(field string, was, want any) string {
	return fmt.Sprintf("%s: %s → %s", field, show(was), show(want))
}

// sameJSON compares two values as the API would carry them.
func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func sameSet(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	sort.Strings(x)
	sort.Strings(y)
	return slices.Equal(slices.Compact(x), slices.Compact(y))
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// server is what the server has now.
type serverState struct {
	pools     map[string]api.Pool
	policy    api.PlacementPolicy
	hasPolicy bool
	templates map[string]api.Template
	// policies holds every stored policy by scope (global, org:ID,
	// project:ID). orgs and projects are read only for a file that has
	// organizations.
	policies map[string]api.PlacementPolicy
	orgs     []api.Organization
	projects []api.Project
}

func readServer(withOrgs bool) (serverState, error) {
	s := serverState{pools: map[string]api.Pool{}, templates: map[string]api.Template{}, policies: map[string]api.PlacementPolicy{}}
	c := cli()
	var pools api.List[api.Pool]
	if err := c.Do(ctx(), "GET", "/v1/admin/pools", nil, &pools); err != nil {
		return s, fmt.Errorf("read the pools: %w", err)
	}
	for _, p := range pools.Items {
		s.pools[p.Name] = p
	}
	var policies api.List[api.PlacementPolicy]
	if err := c.Do(ctx(), "GET", "/v1/policies", nil, &policies); err != nil {
		return s, fmt.Errorf("read the policies: %w", err)
	}
	for _, p := range policies.Items {
		s.policies[p.Scope] = p
		if p.Scope == "global" {
			s.policy, s.hasPolicy = p, true
		}
	}
	var templates api.List[api.Template]
	if err := c.Do(ctx(), "GET", "/v1/templates", nil, &templates); err != nil {
		return s, fmt.Errorf("read the templates: %w", err)
	}
	for _, t := range templates.Items {
		s.templates[t.Name] = t
	}
	if withOrgs {
		var orgs api.List[api.Organization]
		if err := c.Do(ctx(), "GET", "/v1/admin/organizations", nil, &orgs); err != nil {
			return s, fmt.Errorf("read the organizations: %w", err)
		}
		var projects api.List[api.Project]
		if err := c.Do(ctx(), "GET", "/v1/projects", nil, &projects); err != nil {
			return s, fmt.Errorf("read the projects: %w", err)
		}
		s.orgs, s.projects = orgs.Items, projects.Items
	}
	return s, nil
}

// plan compares the file with the server. The order of the result is the
// order apply works in: the policy, then pools, then templates (which name
// pools), then organizations with their projects and policies (which name
// pools too), then removals, templates before the pools they name.
func plan(d desired, s serverState, prune bool) []planned {
	var out []planned
	if d.Policy != nil {
		if p, ok := planPolicy(*d.Policy, s); ok {
			out = append(out, p)
		}
	}
	named := map[string]bool{}
	for i, p := range d.Pools {
		named[p.Name] = true
		if item, ok := planPool(p, d.poolKeys[i], s); ok {
			out = append(out, item)
		}
	}
	namedT := map[string]bool{}
	for _, t := range d.Templates {
		namedT[t.Name] = true
		if item, ok := planTemplate(t, s); ok {
			out = append(out, item)
		}
	}
	if d.managesOrgs {
		out = append(out, planOrgs(d.Organizations, s)...)
	}
	for _, name := range sortedKeys(s.templates) {
		t := s.templates[name]
		if namedT[name] || t.Source != api.TemplateSourceAPI {
			continue // the server's config file declares it; that file manages it
		}
		item := planned{Kind: "template", Name: name, Action: actExtra, Details: []string{"on the server, not in the file"}, prunable: true}
		if prune {
			item.Action = actDelete
			item.steps = []step{call("DELETE", "/v1/admin/templates/"+url.PathEscape(name), nil)}
		}
		out = append(out, item)
	}
	for _, name := range sortedKeys(s.pools) {
		p := s.pools[name]
		if named[name] || p.Source != "api" {
			continue
		}
		item := planned{Kind: "pool", Name: name, Action: actExtra, Details: []string{"on the server, not in the file"}, prunable: true}
		if prune {
			item.Action = actDelete
			item.steps = []step{call("DELETE", "/v1/admin/pools/"+url.PathEscape(name), nil)}
		}
		out = append(out, item)
	}
	return out
}

// planPolicy compares the whole policy: the API replaces it whole, so the
// file is the policy, not a part of it.
func planPolicy(want api.PlacementPolicy, s serverState) (planned, bool) {
	want.Scope = ""
	body := want // what is sent: the policy as the file has it
	have := s.policy
	have.Scope, have.UpdatedBy = "", ""
	have.UpdatedAt = want.UpdatedAt
	// In the global policy an unset strategy means private-first and an
	// unset external mode means never: the scheduler reads them so
	// (policy.Merge). Leaving one out and writing its default are the same
	// policy, and must not show as a difference.
	for _, p := range []*api.PlacementPolicy{&have, &want} {
		if p.Strategy == "" {
			p.Strategy = api.StrategyPrivateFirst
		}
		if p.External == "" {
			p.External = api.ExternalNever
		}
	}
	if s.hasPolicy && sameJSON(have, want) {
		return planned{}, false
	}
	item := planned{Kind: "policy", Name: "global", Action: actChange,
		steps: []step{call("PUT", "/v1/policies/global", body)}}
	if !s.hasPolicy {
		item.Action = actCreate
	}
	for _, f := range []struct {
		name      string
		was, will any
	}{
		{"allowedPools", have.AllowedPools, want.AllowedPools},
		{"deniedPools", have.DeniedPools, want.DeniedPools},
		{"allowedRegions", have.AllowedRegions, want.AllowedRegions},
		{"external", have.External, want.External},
		{"externalPools", have.ExternalPools, want.ExternalPools},
		{"strategy", have.Strategy, want.Strategy},
		{"maxCostPerHour", have.MaxCostPerHour, want.MaxCostPerHour},
		{"maxQueueSeconds", have.MaxQueueSeconds, want.MaxQueueSeconds},
		{"requiredAttestations", have.RequiredAttestations, want.RequiredAttestations},
	} {
		if !sameJSON(f.was, f.will) {
			item.Details = append(item.Details, differs(f.name, zeroAsUnset(f.was), zeroAsUnset(f.will)))
		}
	}
	return item, true
}

// zeroAsUnset shows a number that is zero as unset, as the API omits it.
func zeroAsUnset(v any) any {
	switch n := v.(type) {
	case int:
		if n == 0 {
			return ""
		}
	case float64:
		if n == 0 {
			return ""
		}
	}
	return v
}

func planPool(want config.PoolConfig, keys map[string]json.RawMessage, s serverState) (planned, bool) {
	// Keys are matched without regard to case, as the file's parser and the
	// server's both do: "maxconcurrency" sets maxConcurrency, so it must
	// count as written here too.
	written := map[string]bool{}
	for k := range keys {
		written[strings.ToLower(k)] = true
	}
	has := func(k string) bool { return written[strings.ToLower(k)] }
	item := planned{Kind: "pool", Name: want.Name}
	have, exists := s.pools[want.Name]
	if !exists {
		item.Action = actCreate
		item.Details = []string{"adapter " + want.Adapter}
		if want.Trust != "" {
			item.Details = append(item.Details, "trust "+want.Trust)
		}
		// Send the entry as the file has it: the server validates it and
		// fills in what the file leaves out.
		item.steps = []step{call("POST", "/v1/admin/pools", keys)}
		return item, true
	}

	// What an existing pool cannot change: the API has no way to, because a
	// pool with another adapter, trust or backend is another pool.
	var fixed []string
	if want.Adapter != have.Adapter {
		fixed = append(fixed, differs("adapter", have.Adapter, want.Adapter))
	}
	if has("trust") && want.Trust != have.Trust {
		fixed = append(fixed, differs("trust", have.Trust, want.Trust))
	}
	if has("labels") && !sameJSON(emptyAsNil(want.Labels), emptyAsNil(have.Labels)) {
		fixed = append(fixed, differs("labels", have.Labels, want.Labels))
	}
	for _, k := range sortedKeys(want.Config) {
		if have.Config[k] != want.Config[k] {
			fixed = append(fixed, differs("config."+k, have.Config[k], want.Config[k]))
		}
	}

	patch := map[string]any{}
	var changes []string
	if want.Enabled != nil && *want.Enabled != have.Enabled {
		patch["enabled"] = *want.Enabled
		changes = append(changes, differs("enabled", have.Enabled, *want.Enabled))
	}
	if has("maxConcurrency") && want.MaxConcurrency != have.MaxConcurrency {
		patch["maxConcurrency"] = want.MaxConcurrency
		changes = append(changes, differs("maxConcurrency", have.MaxConcurrency, want.MaxConcurrency))
	}
	if has("region") && want.Region != have.Region {
		patch["region"] = want.Region
		changes = append(changes, differs("region", have.Region, want.Region))
	}
	if has("costModel") && want.CostModel != have.CostModel {
		patch["costModel"] = want.CostModel
		changes = append(changes, differs("costModel", have.CostModel, want.CostModel))
	}
	if has("credentials") && want.Credentials.String() != have.CredentialRef {
		patch["credentialRef"] = want.Credentials.String()
		changes = append(changes, differs("credentials", have.CredentialRef, want.Credentials.String()))
	}
	if has("preemption") && !samePreemption(want.Preemption, have.Preemption) {
		next := want.Preemption
		if next == nil {
			next = &api.Preemption{Mode: api.PreemptOff}
		}
		patch["preemption"] = next
		changes = append(changes, differs("preemption", have.Preemption, want.Preemption))
	}
	if has("budget") && budgetOf(want.Budget) != budgetOf(have.Budget) {
		patch["budget"] = api.Budget{Monthly: budgetOf(want.Budget)}
		changes = append(changes, differs("budget.monthly", zeroAsUnset(budgetOf(have.Budget)), zeroAsUnset(budgetOf(want.Budget))))
	}
	// Attestations are an exact set: one the server holds and the file does
	// not would let work that requires it run here without the file saying so.
	if has("attestations") {
		if ds := attestationDiffs(want.Attestations, have.Attestations); len(ds) > 0 {
			set := map[string]api.Attestation{}
			for name, a := range want.Attestations {
				set[name] = api.Attestation{Evidence: a.Evidence, ValidUntil: a.ValidUntil}
			}
			patch["attestations"] = set
			changes = append(changes, ds...)
		}
	}

	if len(fixed) == 0 && len(changes) == 0 {
		return planned{}, false
	}
	switch {
	case have.Source != "api":
		item.Action = actBlocked
		item.Details = append(append(fixed, changes...), "the server's config file declares this pool and is applied again at every start: change it there, or take it out of one of the two files")
	case len(fixed) > 0:
		item.Action = actBlocked
		item.Details = append(append(fixed, changes...), "an existing pool cannot change these: drain and delete it (differently admin pool), then apply again")
	default:
		item.Action = actChange
		item.Details = changes
		item.steps = []step{call("PATCH", "/v1/admin/pools/"+url.PathEscape(want.Name), patch)}
	}
	return item, true
}

func emptyAsNil(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return m
}

func budgetOf(b *api.Budget) float64 {
	if b == nil {
		return 0
	}
	return b.Monthly
}

// samePreemption treats no policy and mode "off" as the same.
func samePreemption(a, b *api.Preemption) bool {
	off := func(p *api.Preemption) bool { return p == nil || p.Mode == "" || p.Mode == api.PreemptOff }
	if off(a) || off(b) {
		return off(a) && off(b)
	}
	return sameJSON(a, b)
}

func attestationDiffs(want, have map[string]api.Attestation) []string {
	var out []string
	for _, name := range sortedKeys(want) {
		w, h := want[name], have[name]
		if _, ok := have[name]; !ok {
			out = append(out, "attestations."+name+": added")
			continue
		}
		sameUntil := (w.ValidUntil == nil && h.ValidUntil == nil) || (w.ValidUntil != nil && h.ValidUntil != nil && w.ValidUntil.Equal(*h.ValidUntil))
		if w.Evidence != h.Evidence || !sameUntil {
			out = append(out, "attestations."+name+": evidence or expiry changed")
		}
	}
	for _, name := range sortedKeys(have) {
		if _, ok := want[name]; !ok {
			out = append(out, "attestations."+name+": removed")
		}
	}
	return out
}

func planTemplate(want fileTemplate, s serverState) (planned, bool) {
	item := planned{Kind: "template", Name: want.Name}
	path := "/v1/admin/templates/" + url.PathEscape(want.Name)
	have, exists := s.templates[want.Name]
	put := map[string]any{}
	var changes []string
	if want.Description != nil && (!exists || *want.Description != have.Description) {
		put["description"] = *want.Description
		changes = append(changes, differs("description", have.Description, *want.Description))
	}
	if want.Pools != nil && (!exists || !sameSet(*want.Pools, have.Pools)) {
		put["pools"] = *want.Pools
		changes = append(changes, differs("pools", have.Pools, *want.Pools))
	}
	images := map[string]string{}
	for _, pool := range sortedKeys(want.Images) {
		if have.Images[pool] != want.Images[pool] {
			images[pool] = want.Images[pool]
			changes = append(changes, differs("images."+pool, have.Images[pool], want.Images[pool]))
		}
	}
	if len(images) > 0 {
		put["images"] = images
	}
	var steps []step
	if len(put) > 0 {
		steps = append(steps, call("PUT", path, put))
	}
	// Images are an exact set when the file gives any: one the server holds
	// and the file does not would run the template on a pool the file does
	// not name. They go after the PUT, so the template is never left with
	// no image, which would let it run anywhere.
	if exists && len(want.Images) > 0 {
		for _, pool := range sortedKeys(have.Images) {
			if _, ok := want.Images[pool]; !ok {
				steps = append(steps, call("DELETE", path+"/images/"+url.PathEscape(pool), nil))
				changes = append(changes, differs("images."+pool, have.Images[pool], ""))
			}
		}
	}
	if len(changes) == 0 {
		return planned{}, false
	}
	switch {
	case !exists:
		item.Action = actCreate
		item.Details = changes
		item.steps = steps
	case have.Source != api.TemplateSourceAPI:
		item.Action = actBlocked
		item.Details = append(changes, "the server's config file declares this template and wins: change it there, or take it out of one of the two files")
	default:
		item.Action = actChange
		item.Details = changes
		item.steps = steps
	}
	return item, true
}

// pending counts what apply would do, and what it cannot.
func pending(items []planned) (todo, blocked, extra int) {
	for _, it := range items {
		switch it.Action {
		case actBlocked:
			blocked++
		case actExtra:
			extra++
		default:
			todo++
		}
	}
	return
}

func printPlan(out io.Writer, items []planned) {
	for _, it := range items {
		label := map[string]string{actCreate: "+ create", actChange: "~ change", actDelete: "- delete", actBlocked: "! cannot apply", actExtra: "  not in the file"}[it.Action]
		fmt.Fprintf(out, "%-18s %s %s\n", label, it.Kind, it.Name)
		for _, d := range it.Details {
			if it.Action == actExtra {
				continue
			}
			fmt.Fprintf(out, "%-18s   %s\n", "", d)
		}
	}
}

func summary(items []planned, file string, prune bool) string {
	todo, blocked, extra := pending(items)
	var parts []string
	if todo > 0 {
		parts = append(parts, fmt.Sprintf("%d to apply", todo))
	}
	if blocked > 0 {
		parts = append(parts, fmt.Sprintf("%d that cannot be applied", blocked))
	}
	if extra > 0 {
		note := "left alone"
		if !prune && slices.ContainsFunc(items, func(it planned) bool { return it.Action == actExtra && it.prunable }) {
			note += "; --prune deletes the pools and templates among them"
		}
		parts = append(parts, fmt.Sprintf("%d on the server that the file does not name (%s)", extra, note))
	}
	if todo == 0 && blocked == 0 {
		if extra == 0 {
			return "The server matches " + file + "."
		}
		return "The server matches " + file + ", apart from " + parts[0] + "."
	}
	return strings.Join(parts, ", ") + "."
}

func loadPlan(file string, prune bool) ([]planned, error) {
	d, err := readApplyFile(file)
	if err != nil {
		return nil, err
	}
	s, err := readServer(d.managesOrgs)
	if err != nil {
		return nil, err
	}
	return plan(d, s, prune), nil
}

func diffCmd() *cobra.Command {
	var file string
	var prune, exitCode bool
	cmd := &cobra.Command{
		Use:   "diff",
		Short: "Show what differs between a setup file and the server",
		Long: `Compares a setup file (differently.yaml: pools, the global placement
policy, templates and organizations) with the server and prints what
"differently apply" would do. It changes nothing.

A field the file leaves out is not compared. A pool or template on the
server that the file does not name is listed and left alone; with --prune
it is shown as a deletion.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			items, err := loadPlan(file, prune)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			todo, blocked, _ := pending(items)
			if flagOutput == "json" {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if items == nil {
					items = []planned{}
				}
				if err := enc.Encode(map[string]any{"changes": items, "toApply": todo, "blocked": blocked}); err != nil {
					return err
				}
			} else {
				printPlan(out, items)
				fmt.Fprintln(out, summary(items, file, prune))
			}
			if exitCode && todo+blocked > 0 {
				return fmt.Errorf("the server does not match %s", file)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", defaultApplyFile, "the setup file")
	cmd.Flags().BoolVar(&prune, "prune", false, "show pools and templates the file does not name as deletions")
	cmd.Flags().BoolVar(&exitCode, "exit-code", false, "fail when the server does not match the file, for a check in CI")
	return cmd
}

func applyCmd() *cobra.Command {
	var file string
	var prune bool
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Make the server match a setup file",
		Long: `Reads a setup file (differently.yaml: pools, the global placement policy,
templates and organizations) and changes the server to match, with the same
admin API the other commands use. Applying the same file again changes
nothing.

  pools:
    - name: daytona
      adapter: daytona
      trust: external
      credentials: {secret: daytona}
      maxConcurrency: 10
  policy:
    external: overflow
    externalPools: [daytona]
  templates:
    - name: python
      images: {daytona: python-3.12}
  organizations:
    - name: acme
      quota: {maxActiveSandboxes: 50, maxCPU: 100}
      projects:
        - name: default
          quota: {maxActiveSandboxes: 10}

The file states what it names. A field it leaves out stays as the server has
it; a pool or template it does not name is left alone unless --prune is
given. A pool's attestations, a template's images, a policy and a quota are
exact when the file gives them, because each decides where work may run or
how much of it. Organizations and projects are found by name, created when
missing and never deleted.

If something in the file cannot be applied (an existing pool's adapter,
trust, labels or config, or anything the server's own config file
declares), nothing is changed. Otherwise the changes are made in order: the
policy, pools, templates, organizations and their projects, then deletions. They are separate API calls, not
one transaction: if one fails, the ones before it stay, and applying again
goes on from there. Credentials are never in the file: a pool names an
environment variable, a file or a stored secret
(differently admin secret).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			items, err := loadPlan(file, prune)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			todo, blocked, _ := pending(items)
			if blocked > 0 {
				printPlan(out, items)
				return fmt.Errorf("%d item(s) in %s cannot be applied; nothing was changed", blocked, file)
			}
			if todo == 0 {
				printPlan(out, items)
				fmt.Fprintln(out, summary(items, file, prune))
				return nil
			}
			done := 0
			c := cli()
			// ids holds what this run creates, for the steps that need it.
			ids := map[string]string{}
			for _, it := range items {
				if len(it.steps) == 0 {
					continue
				}
				printPlan(out, []planned{it})
				for _, st := range it.steps {
					path, body := st.path, st.body
					if st.resolve != nil {
						path, body = st.resolve(ids)
					}
					var response []byte
					if err := c.Do(ctx(), st.method, path, body, &response); err != nil {
						return fmt.Errorf("%s %s: %w (applied %d of %d; the rest is unchanged, and applying again goes on from here)", it.Kind, it.Name, err, done, todo)
					}
					if st.capture != nil {
						st.capture(response, ids)
					}
				}
				done++
			}
			// What the file does not name, last, so it is not lost among
			// the changes.
			for _, it := range items {
				if it.Action == actExtra {
					printPlan(out, []planned{it})
				}
			}
			fmt.Fprintf(out, "Applied %d change(s). The server matches %s", done, file)
			if _, _, extra := pending(items); extra > 0 {
				fmt.Fprintf(out, ", apart from %d item(s) the file does not name", extra)
			}
			fmt.Fprintln(out, ".")
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", defaultApplyFile, "the setup file")
	cmd.Flags().BoolVar(&prune, "prune", false, "also delete pools and templates set through the API that the file does not name")
	return cmd
}
