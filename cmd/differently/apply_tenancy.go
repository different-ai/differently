package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strings"

	"github.com/different-ai/differently/pkg/api"
)

// Organizations in the setup file: who may run work, and how much.
//
//	organizations:
//	  - name: acme
//	    quota: {maxActiveSandboxes: 50, maxCPU: 100}
//	    policy: {allowedPools: [opensandbox]}
//	    projects:
//	      - name: default
//	        quota: {maxActiveSandboxes: 10}
//
// The server gives organizations and projects IDs and lets names repeat, so
// the file finds them by name: an organization among all, a project within
// its organization. A name the server has twice cannot be told apart and is
// refused. One that is missing is created; none is ever deleted, renamed or
// suspended from here, because each of those ends or interrupts someone's
// work.

type fileOrg struct {
	Name string `json:"name"`
	// Quota is the whole quota when given: the API replaces it whole.
	Quota *api.Quota `json:"quota,omitempty"`
	// Policy is the organization's placement policy, whole, when given.
	Policy   *api.PlacementPolicy `json:"policy,omitempty"`
	Projects []fileProject        `json:"projects,omitempty"`
}

type fileProject struct {
	Name   string               `json:"name"`
	Quota  *api.Quota           `json:"quota,omitempty"`
	Policy *api.PlacementPolicy `json:"policy,omitempty"`
}

func checkOrgs(path string, orgs []fileOrg) error {
	seen := map[string]bool{}
	for i, o := range orgs {
		switch {
		case strings.TrimSpace(o.Name) == "":
			return fmt.Errorf("%s: organizations[%d] has no name", path, i)
		case seen[o.Name]:
			return fmt.Errorf("%s: organization %s is listed twice", path, o.Name)
		}
		seen[o.Name] = true
		if err := checkScopedPolicy(path, "organization "+o.Name, o.Policy); err != nil {
			return err
		}
		inOrg := map[string]bool{}
		for j, p := range o.Projects {
			switch {
			case strings.TrimSpace(p.Name) == "":
				return fmt.Errorf("%s: organization %s: projects[%d] has no name", path, o.Name, j)
			case inOrg[p.Name]:
				return fmt.Errorf("%s: organization %s lists project %s twice", path, o.Name, p.Name)
			}
			inOrg[p.Name] = true
			if err := checkScopedPolicy(path, "project "+o.Name+"/"+p.Name, p.Policy); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkScopedPolicy(path, owner string, p *api.PlacementPolicy) error {
	if p == nil {
		return nil
	}
	if p.Scope != "" || !p.UpdatedAt.IsZero() || p.UpdatedBy != "" {
		return fmt.Errorf("%s: %s: policy.scope, policy.updatedAt and policy.updatedBy are not written in the file; the policy belongs to what it is under", path, owner)
	}
	return nil
}

// quotaDiffs names the limits that differ.
func quotaDiffs(have, want api.Quota) []string {
	var out []string
	hv, wv, t := reflect.ValueOf(have), reflect.ValueOf(want), reflect.TypeOf(have)
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		h, w := hv.Field(i).Interface(), wv.Field(i).Interface()
		if !sameJSON(h, w) {
			out = append(out, differs("quota."+name, zeroAsUnset(h), zeroAsUnset(w)))
		}
	}
	return out
}

// scopedPolicyDiffs names the fields of a policy that differ. Unlike the
// global policy, nothing unset stands for a default here: an organization's
// or a project's policy only narrows, and unset means it adds nothing.
func scopedPolicyDiffs(have, want api.PlacementPolicy) []string {
	var out []string
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
			out = append(out, differs(f.name, zeroAsUnset(f.was), zeroAsUnset(f.will)))
		}
	}
	return out
}

// planScopedPolicy plans one organization's or project's policy. idKey names
// the owner in the ids of a run; id is its ID when it exists already.
func planScopedPolicy(kind, label, idKey, id string, want api.PlacementPolicy, s serverState) (planned, bool) {
	want.Scope = ""
	scope := func(ownerID string) string { return kind + ":" + ownerID }
	have, stored := api.PlacementPolicy{}, false
	if id != "" {
		have, stored = s.policies[scope(id)]
	}
	diffs := scopedPolicyDiffs(have, want)
	if stored && len(diffs) == 0 {
		return planned{}, false
	}
	// A policy that says nothing, for an owner with none stored, is the same
	// as no policy.
	if !stored && len(diffs) == 0 {
		return planned{}, false
	}
	item := planned{Kind: "policy", Name: label, Action: actChange, Details: diffs}
	if !stored {
		item.Action = actCreate
	}
	item.steps = []step{{method: "PUT", resolve: func(ids map[string]string) (string, any) {
		owner := id
		if owner == "" {
			owner = ids[idKey]
		}
		return "/v1/policies/" + url.PathEscape(scope(owner)), want
	}}}
	return item, true
}

// planOrgs plans the organizations the file names, each followed by its
// policy, its projects and their policies, then lists what the file does
// not name.
func planOrgs(want []fileOrg, s serverState) []planned {
	var out []planned
	byName := map[string][]api.Organization{}
	for _, o := range s.orgs {
		byName[o.Name] = append(byName[o.Name], o)
	}
	named := map[string]bool{}
	for _, w := range want {
		named[w.Name] = true
		orgKey := "org:" + w.Name
		found := byName[w.Name]
		if len(found) > 1 {
			out = append(out, planned{Kind: "organization", Name: w.Name, Action: actBlocked, Details: []string{
				fmt.Sprintf("the server has %d organizations with this name, and the file finds them by name: rename all but one (differently admin org)", len(found))}})
			continue
		}
		orgID := ""
		if len(found) == 0 {
			body := map[string]any{"name": w.Name}
			details := []string{"new"}
			if w.Quota != nil {
				body["quota"] = *w.Quota
				details = append(details, quotaDiffs(api.Quota{}, *w.Quota)...)
			}
			out = append(out, planned{Kind: "organization", Name: w.Name, Action: actCreate, Details: details,
				steps: []step{{method: "POST", path: "/v1/admin/organizations", body: body, capture: func(resp []byte, ids map[string]string) {
					var created struct {
						Organization api.Organization `json:"organization"`
					}
					if json.Unmarshal(resp, &created) == nil {
						ids[orgKey] = created.Organization.ID
					}
				}}}})
		} else {
			orgID = found[0].ID
			if w.Quota != nil {
				if diffs := quotaDiffs(found[0].Quota, *w.Quota); len(diffs) > 0 {
					out = append(out, planned{Kind: "organization", Name: w.Name, Action: actChange, Details: diffs,
						steps: []step{call("PATCH", "/v1/admin/organizations/"+url.PathEscape(orgID), map[string]any{"quota": *w.Quota})}})
				}
			}
		}
		if w.Policy != nil {
			if item, ok := planScopedPolicy("org", "organization "+w.Name, orgKey, orgID, *w.Policy, s); ok {
				out = append(out, item)
			}
		}

		projects := map[string][]api.Project{}
		for _, p := range s.projects {
			if orgID != "" && p.OrgID == orgID {
				projects[p.Name] = append(projects[p.Name], p)
			}
		}
		namedP := map[string]bool{}
		for _, wp := range w.Projects {
			namedP[wp.Name] = true
			label := w.Name + "/" + wp.Name
			projKey := "project:" + label
			foundP := projects[wp.Name]
			if len(foundP) > 1 {
				out = append(out, planned{Kind: "project", Name: label, Action: actBlocked, Details: []string{
					fmt.Sprintf("organization %s has %d projects with this name, and the file finds them by name", w.Name, len(foundP))}})
				continue
			}
			projID := ""
			if len(foundP) == 0 {
				details := []string{"new"}
				if wp.Quota != nil {
					details = append(details, quotaDiffs(api.Quota{}, *wp.Quota)...)
				}
				name, quota, owner := wp.Name, wp.Quota, orgID
				out = append(out, planned{Kind: "project", Name: label, Action: actCreate, Details: details,
					steps: []step{{method: "POST", resolve: func(ids map[string]string) (string, any) {
						body := map[string]any{"name": name, "orgId": owner}
						if owner == "" {
							body["orgId"] = ids[orgKey]
						}
						if quota != nil {
							body["quota"] = *quota
						}
						return "/v1/projects", body
					}, capture: func(resp []byte, ids map[string]string) {
						var created api.Project
						if json.Unmarshal(resp, &created) == nil {
							ids[projKey] = created.ID
						}
					}}}})
			} else {
				projID = foundP[0].ID
				if wp.Quota != nil {
					if diffs := quotaDiffs(foundP[0].Quota, *wp.Quota); len(diffs) > 0 {
						out = append(out, planned{Kind: "project", Name: label, Action: actChange, Details: diffs,
							steps: []step{call("PATCH", "/v1/admin/projects/"+url.PathEscape(projID), map[string]any{"quota": *wp.Quota})}})
					}
				}
			}
			if wp.Policy != nil {
				if item, ok := planScopedPolicy("project", "project "+label, projKey, projID, *wp.Policy, s); ok {
					out = append(out, item)
				}
			}
		}
		// Projects of a named organization that the file does not name, when
		// it names any: listed, never deleted.
		if len(w.Projects) > 0 {
			for _, name := range sortedKeys(projects) {
				if !namedP[name] {
					out = append(out, planned{Kind: "project", Name: w.Name + "/" + name, Action: actExtra, Details: []string{"on the server, not in the file"}})
				}
			}
		}
	}
	for _, name := range sortedKeys(byName) {
		if !named[name] {
			out = append(out, planned{Kind: "organization", Name: name, Action: actExtra, Details: []string{"on the server, not in the file"}})
		}
	}
	return out
}
