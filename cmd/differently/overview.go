package main

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/different-ai/differently/pkg/api"
)

// Backends differ: one takes memory snapshots, another cannot fork, and each
// boots its own images. These two commands put the differences side by side,
// so nobody has to find them out from a failed request.

const baselinesGuide = "https://different-ai.github.io/differently/docs/guides/baselines/"

// poolView is one pool as GET /v1/capabilities reports it to this key.
type poolView struct {
	Pool         string   `json:"pool"`
	Adapter      string   `json:"adapter"`
	Trust        string   `json:"trust"`
	Region       string   `json:"region,omitempty"`
	Enabled      bool     `json:"enabled"`
	Health       string   `json:"health"`
	Capabilities []string `json:"capabilities"`
	Attestations []string `json:"attestations,omitempty"`
}

func visiblePools() ([]poolView, error) {
	var out api.List[poolView]
	err := cli().Do(ctx(), "GET", "/v1/capabilities", nil, &out)
	return out.Items, err
}

// capabilityRows builds a table with one column per pool and one row per
// feature any of them has, so a gap shows as a dash next to a yes.
func capabilityRows(pools []poolView) (header []string, rows [][]string) {
	header = []string{""}
	features := map[string]bool{}
	for _, p := range pools {
		header = append(header, p.Pool)
		for _, f := range p.Capabilities {
			features[f] = true
		}
	}
	about := func(label string, value func(poolView) string) {
		row := []string{label}
		for _, p := range pools {
			row = append(row, value(p))
		}
		rows = append(rows, row)
	}
	about("adapter", func(p poolView) string { return p.Adapter })
	about("trust", func(p poolView) string { return p.Trust })
	about("status", func(p poolView) string {
		if !p.Enabled {
			return "not taking new work"
		}
		return p.Health
	})
	names := make([]string, 0, len(features))
	for f := range features {
		names = append(names, f)
	}
	sort.Strings(names)
	for _, f := range names {
		about(f, func(p poolView) string {
			if slices.Contains(p.Capabilities, f) {
				return "yes"
			}
			return "-"
		})
	}
	return header, rows
}

func capabilitiesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "capabilities",
		Short: "Show what each pool can do, side by side",
		Long: `Capabilities lists the pools this key may use and which features each one
has: snapshots, fork, stop and start, isolation and so on. A request can
require a feature (sandbox create --capability snapshots.memory) and is then
placed only on a pool that has it. With -o json it prints the API response.`,
		RunE: func(*cobra.Command, []string) error {
			if flagOutput == "json" {
				return raw("GET", "/v1/capabilities", nil)
			}
			pools, err := visiblePools()
			if err != nil {
				return err
			}
			if len(pools) == 0 {
				fmt.Println("no pool is available to this key")
				return nil
			}
			header, rows := capabilityRows(pools)
			printRows(header, rows)
			return nil
		},
	}
}

// templateRows builds a table with one row per template and one column per
// pool, holding what that pool's backend boots for the template: its own
// image or snapshot, or a dash when the pool cannot run the template.
func templateRows(templates []api.Template, pools []string) (header []string, rows [][]string) {
	seen := map[string]bool{}
	for _, p := range pools {
		seen[p] = true
	}
	// A template may name a pool this key cannot use; show it all the same,
	// since the API does.
	var extra []string
	for _, t := range templates {
		for p := range t.Images {
			if !seen[p] {
				seen[p] = true
				extra = append(extra, p)
			}
		}
		for _, p := range t.Pools {
			if !seen[p] {
				seen[p] = true
				extra = append(extra, p)
			}
		}
	}
	sort.Strings(extra)
	pools = append(slices.Clone(pools), extra...)
	header = append(append([]string{"TEMPLATE"}, pools...), "DESCRIPTION")
	for _, t := range templates {
		row := []string{t.Name}
		for _, p := range pools {
			if ok, _ := t.Offers(p); ok {
				row = append(row, t.ImageFor(p))
			} else {
				row = append(row, "-")
			}
		}
		rows = append(rows, append(row, t.Description))
	}
	return header, rows
}

func templatesCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "templates",
		Aliases: []string{"template"},
		Short:   "Show each template and what every pool boots for it",
		Long: `Templates lists the templates the server knows and, for every pool, the
image or snapshot that pool's backend boots for it. A dash means the pool has
none, so a sandbox that asks for the template is never placed there.

Each backend boots its own images: a template gives them one name. With
-o json it prints the API response.`,
		RunE: func(*cobra.Command, []string) error {
			if flagOutput == "json" {
				return raw("GET", "/v1/templates", nil)
			}
			var list api.List[api.Template]
			if err := cli().Do(ctx(), "GET", "/v1/templates", nil, &list); err != nil {
				return err
			}
			if len(list.Items) == 0 {
				fmt.Println("No templates are configured: a template named in a request is passed to the backend as given.")
				fmt.Println("To give one name an image on every pool, see " + baselinesGuide)
				return nil
			}
			pools, err := visiblePools()
			if err != nil {
				return err
			}
			names := make([]string, 0, len(pools))
			for _, p := range pools {
				names = append(names, p.Pool)
			}
			header, rows := templateRows(list.Items, names)
			printRows(header, rows)
			return nil
		},
	}
}

// printRows prints a table whose header is a row like any other.
func printRows(header []string, rows [][]string) {
	table(strings.Join(header, "\t"), rows)
}
