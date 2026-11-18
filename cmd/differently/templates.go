package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/different-ai/differently/pkg/api"
	"github.com/different-ai/differently/pkg/client"
)

// differently admin template set|remove changes templates on a running
// server: no config edit, no restart. `differently templates` shows the
// result.

// parseImages reads --image POOL=IMAGE flags. The image may itself contain
// "=" (a digest, a query), so only the first one separates.
func parseImages(flags []string) (map[string]string, error) {
	images := map[string]string{}
	for _, f := range flags {
		pool, image, ok := strings.Cut(f, "=")
		pool, image = strings.TrimSpace(pool), strings.TrimSpace(image)
		switch {
		case !ok || pool == "" || image == "":
			return nil, fmt.Errorf("--image takes POOL=IMAGE, for example --image daytona=python-312; got %q", f)
		case images[pool] != "" && images[pool] != image:
			return nil, fmt.Errorf("--image names pool %s twice, as %s and as %s", pool, images[pool], image)
		}
		images[pool] = image
	}
	return images, nil
}

// printTemplate shows what each pool boots for one template.
func printTemplate(out io.Writer, t api.Template) {
	pools := make([]string, 0, len(t.Images))
	for p := range t.Images {
		pools = append(pools, p)
	}
	sort.Strings(pools)
	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "POOL\tBOOTS")
	for _, p := range pools {
		fmt.Fprintf(w, "%s\t%s\n", p, t.Images[p])
	}
	w.Flush()
	switch {
	case len(pools) > 0:
		fmt.Fprintf(out, "A sandbox that asks for %s is placed only on a pool listed here, and only where the placement policy allows.\n", t.Name)
	case len(t.Pools) > 0:
		fmt.Fprintf(out, "No pool has its own image for %s: the name is passed as given to %s.\n", t.Name, strings.Join(t.Pools, ", "))
	default:
		fmt.Fprintf(out, "No pool has its own image for %s: the name is passed to the backend as given.\n", t.Name)
	}
}

func writeJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// templateSet sends one merge: the images named are added or replaced, and
// whatever else the template holds stays.
func templateSet(c context.Context, cl *client.Client, out io.Writer, name string, req api.PutTemplateRequest, asJSON bool) error {
	t, err := cl.SetTemplate(c, name, req)
	if err != nil {
		return err
	}
	if asJSON {
		return writeJSON(out, t)
	}
	fmt.Fprintf(out, "template %s set; it applies from the next request, with no restart\n", t.Name)
	printTemplate(out, t)
	return nil
}

// templateRemove takes one pool's image away, or the whole template when no
// pool is named.
func templateRemove(c context.Context, cl *client.Client, out io.Writer, name, pool string, asJSON bool) error {
	if pool == "" {
		if err := cl.DeleteTemplate(c, name); err != nil {
			return err
		}
		if !asJSON {
			fmt.Fprintf(out, "template %s removed; a request that names %s now has the name passed to the backend as given\n", name, name)
		}
		return nil
	}
	t, err := cl.RemoveTemplateImage(c, name, pool)
	if err != nil {
		return err
	}
	if asJSON {
		return writeJSON(out, t)
	}
	fmt.Fprintf(out, "template %s is no longer offered on %s\n", t.Name, pool)
	printTemplate(out, t)
	return nil
}

func adminTemplateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "template", Aliases: []string{"templates"},
		Short: "Give a baseline one name on every pool, on a running server",
		Long: `Each backend boots its own images, so a baseline has to exist on every pool
that should run it. A template gives those images one name:

  differently admin template set python --image onprem=registry.example.com/python:3.12 --image daytona=python-312
  differently admin template set python --image freestyle=python-312-fs   # add a pool later
  differently admin template remove python --pool daytona                 # stop offering it there
  differently admin template remove python                                # remove the template
  differently templates                                                   # what every pool boots

Changes apply from the next request, with no config edit and no restart. A
template declared in the server's config file is changed there instead. A
template only narrows where a sandbox runs: the placement policy still decides
which pools may take the work.`,
	}
	var images []string
	var description string
	var pools []string
	set := &cobra.Command{
		Use: "set NAME --image POOL=IMAGE [--image POOL=IMAGE ...]", Args: cobra.ExactArgs(1),
		Short: "Set what a pool boots for a template (adds to what is already set)",
		Long: `Set gives a template an image on each pool named with --image, creating the
template if it does not exist. It adds to what is stored: pools you do not
name keep their image, so adding a provider later takes one flag.

IMAGE is whatever that pool's backend calls the baseline: an image reference
for OpenSandbox, a snapshot name or ID for Daytona or Freestyle.`,
		RunE: func(cmd *cobra.Command, a []string) error {
			parsed, err := parseImages(images)
			if err != nil {
				return err
			}
			req := api.PutTemplateRequest{Images: parsed}
			if cmd.Flags().Changed("description") {
				req.Description = &description
			}
			if cmd.Flags().Changed("pools") {
				req.Pools = &pools
			}
			if len(parsed) == 0 && req.Description == nil && req.Pools == nil {
				return fmt.Errorf("nothing to set: name a pool's image, for example --image daytona=python-312")
			}
			return templateSet(ctx(), cli(), cmd.OutOrStdout(), a[0], req, flagOutput == "json")
		},
	}
	set.Flags().StringArrayVar(&images, "image", nil, "POOL=IMAGE: what that pool's backend boots for the template (repeatable)")
	set.Flags().StringVar(&description, "description", "", "what the template is, shown by `differently templates`")
	set.Flags().StringSliceVar(&pools, "pools", nil, "restrict the template to these pools (replaces the list; \"\" removes the restriction)")

	var pool string
	remove := &cobra.Command{
		Use: "remove NAME [--pool POOL]", Aliases: []string{"rm", "delete"}, Args: cobra.ExactArgs(1),
		Short: "Remove a template, or with --pool only that pool's image",
		Long: `Remove deletes a template set with "admin template set". From then on a
request that names it has the name passed to the backend as given.

With --pool it removes only that pool's image, so the template is no longer
placed there. A template's last image cannot be removed that way: without one
the name would be passed to every pool as given, so remove the template.`,
		RunE: func(cmd *cobra.Command, a []string) error {
			if cmd.Flags().Changed("pool") && pool == "" {
				return fmt.Errorf("--pool needs a pool name")
			}
			err := templateRemove(ctx(), cli(), cmd.OutOrStdout(), a[0], pool, flagOutput == "json")
			if pool != "" && api.IsCode(err, api.CodeInvalidState) {
				// The server names its own route; say the command too.
				fmt.Fprintf(cmd.ErrOrStderr(), "To remove the template itself: differently admin template remove %s\n", a[0])
			}
			return err
		},
	}
	remove.Flags().StringVar(&pool, "pool", "", "remove only this pool's image")
	cmd.AddCommand(set, remove)
	return cmd
}
