// Command differently is the Differently CLI.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/different-ai/differently/internal/version"
	"github.com/different-ai/differently/pkg/api"
	"github.com/different-ai/differently/pkg/client"
)

type settings struct {
	URL     string `json:"url"`
	APIKey  string `json:"apiKey"`
	Project string `json:"project,omitempty"`
}

var (
	flagURL, flagKey, flagProject, flagOutput string
)

func configPath() string {
	dir, _ := os.UserConfigDir()
	return filepath.Join(dir, "differently", "config.json")
}

func load() settings {
	var s settings
	if b, err := os.ReadFile(configPath()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	for _, o := range []struct {
		dst       *string
		env, flag string
	}{
		{&s.URL, "DIFFERENTLY_URL", flagURL}, {&s.APIKey, "DIFFERENTLY_API_KEY", flagKey}, {&s.Project, "DIFFERENTLY_PROJECT", flagProject},
	} {
		if v := os.Getenv(o.env); v != "" {
			*o.dst = v
		}
		if o.flag != "" {
			*o.dst = o.flag
		}
	}
	if s.URL == "" {
		s.URL = "http://localhost:8080"
	}
	// Local dev servers write an operator token; use it for localhost.
	if s.APIKey == "" && strings.Contains(s.URL, "localhost") {
		home, _ := os.UserHomeDir()
		if b, err := os.ReadFile(filepath.Join(home, ".differently", "dev", "token")); err == nil {
			s.APIKey = strings.TrimSpace(string(b))
			if s.Project == "" {
				s.Project = "prj_default"
			}
		}
	}
	return s
}

func cli() *client.Client {
	s := load()
	return client.New(s.URL, s.APIKey)
}

func ctx() context.Context {
	c, _ := signal.NotifyContext(context.Background(), os.Interrupt)
	return c
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func table(header string, rows [][]string) {
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, header)
	for _, r := range rows {
		fmt.Fprintln(w, strings.Join(r, "\t"))
	}
	w.Flush()
}

func sandboxRows(items []api.Sandbox) [][]string {
	var rows [][]string
	for _, s := range items {
		rows = append(rows, []string{s.ID, string(s.Status), s.Pool, s.Trust, fmt.Sprintf("%d/%dMiB", s.Resources.CPU, s.Resources.MemoryMiB), s.Classification, s.CreatedAt.Local().Format(time.DateTime)})
	}
	return rows
}

func showSandboxes(items ...api.Sandbox) error {
	if flagOutput == "json" {
		if len(items) == 1 {
			return printJSON(items[0])
		}
		return printJSON(items)
	}
	table("ID\tSTATUS\tPOOL\tTRUST\tSIZE\tCLASS\tCREATED", sandboxRows(items))
	return nil
}

// raw runs an arbitrary call and prints its JSON response.
func raw(method, path string, body any) error {
	var out any
	if err := cli().Do(ctx(), method, path, body, &out); err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return printJSON(out)
}

func main() {
	root := &cobra.Command{Use: "differently", Short: "Differently: your infrastructure, your rules, one API", SilenceUsage: true, SilenceErrors: true, Version: version.Version}
	root.PersistentFlags().StringVar(&flagURL, "url", "", "API URL (env DIFFERENTLY_URL)")
	root.PersistentFlags().StringVar(&flagKey, "api-key", "", "API key (env DIFFERENTLY_API_KEY)")
	root.PersistentFlags().StringVar(&flagProject, "project", "", "project ID for org/operator keys (env DIFFERENTLY_PROJECT)")
	root.PersistentFlags().StringVarP(&flagOutput, "output", "o", "table", "output format: table or json")
	root.AddCommand(loginCmd(), sandboxCmd(), projectCmd(), keyCmd(), usageCmd(), adminCmd(), doctorCmd(),
		&cobra.Command{Use: "whoami", Short: "Show the current key", RunE: func(*cobra.Command, []string) error { return raw("GET", "/v1/me", nil) }},
		&cobra.Command{Use: "capabilities", Short: "List pools and their capabilities", RunE: func(*cobra.Command, []string) error { return raw("GET", "/v1/capabilities", nil) }},
	)
	if err := root.Execute(); err != nil {
		var e *api.Error
		if errors.As(err, &e) {
			fmt.Fprintf(os.Stderr, "error [%s]: %s\n", e.Code, e.Message)
			for k, v := range e.Details {
				fmt.Fprintf(os.Stderr, "  %s: %s\n", k, v)
			}
		}
		os.Exit(1)
	}
}

func loginCmd() *cobra.Command {
	return &cobra.Command{
		Use: "login", Short: "Save the API URL, key and default project",
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := load()
			if flagKey == "" && os.Getenv("DIFFERENTLY_API_KEY") == "" {
				fmt.Print("API key: ")
				if _, err := fmt.Scanln(&s.APIKey); err != nil {
					return err
				}
			}
			var me map[string]any
			if err := client.New(s.URL, s.APIKey).Do(ctx(), "GET", "/v1/me", nil, &me); err != nil {
				return err
			}
			if p, _ := me["projectId"].(string); p != "" {
				s.Project = p
			}
			if err := os.MkdirAll(filepath.Dir(configPath()), 0o700); err != nil {
				return err
			}
			b, _ := json.MarshalIndent(s, "", "  ")
			if err := os.WriteFile(configPath(), b, 0o600); err != nil {
				return err
			}
			fmt.Printf("logged in to %s as key %v (saved to %s)\n", s.URL, me["keyId"], configPath())
			return nil
		},
	}
}

func sandboxCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "sandbox", Aliases: []string{"sb", "sandboxes"}, Short: "Create, use and delete sandboxes"}
	var (
		profile, template, class, strategy, external string
		cpu, mem, disk, ttl                          int
		wait                                         bool
		envs, labels, caps, pools                    []string
	)
	create := &cobra.Command{
		Use: "create", Short: "Create a sandbox",
		RunE: func(*cobra.Command, []string) error {
			req := api.CreateSandboxRequest{ProjectID: load().Project, Profile: profile, Template: template, Classification: class, TTLSeconds: ttl, Capabilities: caps}
			if cpu > 0 || mem > 0 || disk > 0 {
				req.Resources = &api.Resources{CPU: cpu, MemoryMiB: mem, DiskGiB: disk}
			}
			if strategy != "" || external != "" || len(pools) > 0 {
				req.Placement = &api.Placement{Strategy: strategy, External: external, AllowedPools: pools}
			}
			req.Env, req.Labels = kv(envs), kv(labels)
			sb, err := cli().CreateSandbox(ctx(), req, wait)
			if err != nil {
				if sb.ID != "" {
					_ = showSandboxes(sb)
				}
				return err
			}
			if flagOutput != "json" && !wait {
				fmt.Fprintf(os.Stderr, "queued; wait with: differently sandbox wait %s\n", sb.ID)
			}
			return showSandboxes(sb)
		},
	}
	f := create.Flags()
	f.StringVar(&profile, "profile", "", "resource profile (see GET /v1/profiles)")
	f.StringVar(&template, "template", "", "image template")
	f.StringVar(&class, "classification", "", "data classification, e.g. private-only")
	f.IntVar(&cpu, "cpu", 0, "vCPUs")
	f.IntVar(&mem, "memory", 0, "memory in MiB")
	f.IntVar(&disk, "disk", 0, "disk in GiB")
	f.IntVar(&ttl, "ttl", 0, "lifetime in seconds")
	f.StringVar(&strategy, "strategy", "", "placement strategy: private-first, cost or spread")
	f.StringVar(&external, "external", "", "narrow external placement: never or overflow")
	f.StringSliceVar(&pools, "pool", nil, "restrict to these pools")
	f.StringSliceVar(&caps, "capability", nil, "required capabilities, e.g. snapshots.disk")
	f.StringArrayVarP(&envs, "env", "e", nil, "environment KEY=VALUE")
	f.StringArrayVarP(&labels, "label", "l", nil, "label KEY=VALUE")
	f.BoolVar(&wait, "wait", false, "wait until running")

	list := &cobra.Command{Use: "list", Aliases: []string{"ls"}, Short: "List sandboxes", RunE: func(c *cobra.Command, _ []string) error {
		status, _ := c.Flags().GetString("status")
		q := url.Values{"limit": {"200"}}
		if status != "" {
			q.Set("status", status)
		}
		out, err := cli().ListSandboxes(ctx(), q)
		if err != nil {
			return err
		}
		return showSandboxes(out.Items...)
	}}
	list.Flags().String("status", "", "comma-separated statuses")

	get := &cobra.Command{Use: "get ID", Args: cobra.ExactArgs(1), Short: "Show a sandbox", RunE: func(_ *cobra.Command, a []string) error {
		sb, err := cli().GetSandbox(ctx(), a[0])
		if err != nil {
			return err
		}
		if flagOutput == "table" {
			flagOutput = "json"
		}
		return showSandboxes(sb)
	}}
	del := &cobra.Command{Use: "delete ID...", Aliases: []string{"rm"}, Args: cobra.MinimumNArgs(1), Short: "Delete sandboxes", RunE: func(_ *cobra.Command, a []string) error {
		for _, id := range a {
			if err := cli().DeleteSandbox(ctx(), id); err != nil {
				return err
			}
			fmt.Println("terminating", id)
		}
		return nil
	}}
	waitCmd := &cobra.Command{Use: "wait ID", Args: cobra.ExactArgs(1), Short: "Wait until a sandbox is running", RunE: func(c *cobra.Command, a []string) error {
		t, _ := c.Flags().GetDuration("timeout")
		sb, err := cli().WaitSandbox(ctx(), a[0], api.StatusRunning, t)
		_ = showSandboxes(sb)
		return err
	}}
	waitCmd.Flags().Duration("timeout", 5*time.Minute, "how long to wait")

	var cwd string
	var execEnv []string
	exec := &cobra.Command{
		Use: "exec ID -- COMMAND [ARGS...]", Short: "Run a command and stream its output", Args: cobra.MinimumNArgs(2),
		RunE: func(_ *cobra.Command, a []string) error {
			req := api.ExecRequest{Args: a[1:], Cwd: cwd, Env: kv(execEnv)}
			if len(a) == 2 && strings.ContainsAny(a[1], " |&;$<>") {
				req = api.ExecRequest{Command: a[1], Cwd: cwd, Env: kv(execEnv)}
			}
			code, err := cli().ExecStream(ctx(), a[0], req, func(ev api.ExecEvent) {
				if ev.Stream == "stderr" {
					fmt.Fprint(os.Stderr, ev.Data)
				} else {
					fmt.Print(ev.Data)
				}
			})
			if err != nil {
				return err
			}
			if code != 0 {
				os.Exit(code)
			}
			return nil
		},
	}
	exec.Flags().StringVar(&cwd, "cwd", "", "working directory")
	exec.Flags().StringArrayVarP(&execEnv, "env", "e", nil, "environment KEY=VALUE")

	files := &cobra.Command{Use: "files", Short: "Transfer files"}
	files.AddCommand(
		&cobra.Command{Use: "ls ID PATH", Args: cobra.ExactArgs(2), RunE: func(_ *cobra.Command, a []string) error {
			items, err := cli().ListFiles(ctx(), a[0], a[1])
			if err != nil {
				return err
			}
			var rows [][]string
			for _, it := range items {
				rows = append(rows, []string{it.Mode, fmt.Sprint(it.Size), it.Path})
			}
			table("MODE\tSIZE\tPATH", rows)
			return nil
		}},
		&cobra.Command{Use: "cat ID PATH", Args: cobra.ExactArgs(2), RunE: func(_ *cobra.Command, a []string) error {
			b, err := cli().ReadFile(ctx(), a[0], a[1])
			if err == nil {
				_, err = os.Stdout.Write(b)
			}
			return err
		}},
		&cobra.Command{Use: "put ID LOCAL REMOTE", Args: cobra.ExactArgs(3), RunE: func(_ *cobra.Command, a []string) error {
			b, err := os.ReadFile(a[1])
			if err != nil {
				return err
			}
			return cli().WriteFile(ctx(), a[0], a[2], b)
		}},
		&cobra.Command{Use: "get ID REMOTE LOCAL", Args: cobra.ExactArgs(3), RunE: func(_ *cobra.Command, a []string) error {
			b, err := cli().ReadFile(ctx(), a[0], a[1])
			if err != nil {
				return err
			}
			return os.WriteFile(a[2], b, 0o644)
		}},
	)
	cmd.AddCommand(create, list, get, del, waitCmd, exec, files)
	for _, action := range []string{"stop", "start", "pause", "resume"} {
		cmd.AddCommand(&cobra.Command{Use: action + " ID", Args: cobra.ExactArgs(1), Short: strings.ToUpper(action[:1]) + action[1:] + " a sandbox", RunE: func(_ *cobra.Command, a []string) error {
			sb, err := cli().Action(ctx(), a[0], action)
			if err != nil {
				return err
			}
			return showSandboxes(sb)
		}})
	}
	snap := &cobra.Command{Use: "snapshot ID", Args: cobra.ExactArgs(1), Short: "Snapshot a sandbox", RunE: func(c *cobra.Command, a []string) error {
		kind, _ := c.Flags().GetString("kind")
		s, err := cli().Snapshot(ctx(), a[0], kind)
		if err != nil {
			return err
		}
		return printJSON(s)
	}}
	snap.Flags().String("kind", "disk", "disk or memory")
	cmd.AddCommand(snap,
		&cobra.Command{Use: "fork ID", Args: cobra.ExactArgs(1), Short: "Fork a sandbox", RunE: func(_ *cobra.Command, a []string) error {
			sb, err := cli().Fork(ctx(), a[0])
			if err != nil {
				return err
			}
			return showSandboxes(sb)
		}},
		&cobra.Command{Use: "placement ID", Args: cobra.ExactArgs(1), Short: "Explain where a sandbox runs and why", RunE: func(_ *cobra.Command, a []string) error {
			return raw("GET", "/v1/sandboxes/"+a[0]+"/placement", nil)
		}},
		&cobra.Command{Use: "port ID PORT", Args: cobra.ExactArgs(2), Short: "Expose a service port", RunE: func(_ *cobra.Command, a []string) error {
			var port int
			fmt.Sscan(a[1], &port)
			out, err := cli().ExposePort(ctx(), a[0], port)
			if err != nil {
				return err
			}
			fmt.Println(out.URL)
			return nil
		}},
		&cobra.Command{Use: "snapshots", Short: "List snapshots", RunE: func(*cobra.Command, []string) error { return raw("GET", "/v1/snapshots", nil) }},
	)
	return cmd
}

func kv(pairs []string) map[string]string {
	if len(pairs) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, p := range pairs {
		k, v, _ := strings.Cut(p, "=")
		out[k] = v
	}
	return out
}

func projectCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "project", Short: "Manage projects"}
	create := &cobra.Command{Use: "create NAME", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, a []string) error {
		org, _ := c.Flags().GetString("org")
		return raw("POST", "/v1/projects", map[string]string{"name": a[0], "orgId": org})
	}}
	create.Flags().String("org", "", "organization (operator keys)")
	cmd.AddCommand(create,
		&cobra.Command{Use: "list", RunE: func(*cobra.Command, []string) error { return raw("GET", "/v1/projects", nil) }},
		&cobra.Command{Use: "delete ID", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error { return raw("DELETE", "/v1/projects/"+a[0], nil) }},
	)
	return cmd
}

func keyCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "key", Short: "Manage API keys"}
	var scopes []string
	var org, project string
	var ttl int
	create := &cobra.Command{Use: "create NAME", Args: cobra.ExactArgs(1), Short: "Create a scoped key (the token is shown once)", RunE: func(_ *cobra.Command, a []string) error {
		out, err := cli().CreateAPIKey(ctx(), api.CreateAPIKeyRequest{Name: a[0], OrgID: org, ProjectID: project, Scopes: scopes, TTLHours: ttl})
		if err != nil {
			return err
		}
		if flagOutput == "json" {
			return printJSON(out)
		}
		fmt.Printf("key %s created; store this token now, it is not shown again:\n%s\n", out.ID, out.Token)
		return nil
	}}
	create.Flags().StringSliceVar(&scopes, "scope", []string{"sandboxes:read", "sandboxes:write", "sandboxes:exec"}, "scopes")
	create.Flags().StringVar(&org, "org", "", "organization (operator keys)")
	create.Flags().StringVar(&project, "key-project", "", "limit the key to one project")
	create.Flags().IntVar(&ttl, "ttl-hours", 0, "expire after N hours")
	cmd.AddCommand(create,
		&cobra.Command{Use: "list", RunE: func(*cobra.Command, []string) error { return raw("GET", "/v1/api-keys", nil) }},
		&cobra.Command{Use: "revoke ID", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error { return raw("DELETE", "/v1/api-keys/"+a[0], nil) }},
	)
	return cmd
}

func usageCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "usage", Short: "Show estimated usage", RunE: func(c *cobra.Command, _ []string) error {
		by, _ := c.Flags().GetString("group-by")
		return raw("GET", "/v1/usage?groupBy="+url.QueryEscape(by), nil)
	}}
	cmd.Flags().String("group-by", "pool", "pool, project, org or trust")
	cmd.AddCommand(
		&cobra.Command{Use: "quota", Short: "Show quota and current consumption", RunE: func(*cobra.Command, []string) error { return raw("GET", "/v1/quotas", nil) }},
		&cobra.Command{Use: "audit", Short: "List audit events", RunE: func(*cobra.Command, []string) error { return raw("GET", "/v1/audit-events?limit=100", nil) }},
	)
	return cmd
}

func adminCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "admin", Short: "Operator commands (admin key)"}
	org := &cobra.Command{Use: "org", Short: "Organizations"}
	var defaultProject string
	createOrg := &cobra.Command{Use: "create NAME", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
		return raw("POST", "/v1/admin/organizations", map[string]string{"name": a[0], "defaultProject": defaultProject})
	}}
	createOrg.Flags().StringVar(&defaultProject, "default-project", "default", "also create a project with this name (empty to skip)")
	var reason string
	var terminate bool
	suspend := &cobra.Command{Use: "suspend ID", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
		return raw("POST", "/v1/admin/organizations/"+a[0]+"/suspend", map[string]any{"reason": reason, "terminate": terminate})
	}}
	suspend.Flags().StringVar(&reason, "reason", "", "recorded in the audit log")
	suspend.Flags().BoolVar(&terminate, "terminate", false, "also terminate its sandboxes")
	org.AddCommand(createOrg, suspend,
		&cobra.Command{Use: "list", RunE: func(*cobra.Command, []string) error { return raw("GET", "/v1/admin/organizations", nil) }},
		&cobra.Command{Use: "resume ID", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error {
			return raw("POST", "/v1/admin/organizations/"+a[0]+"/resume", map[string]any{})
		}},
	)
	pool := &cobra.Command{Use: "pool", Short: "Execution pools"}
	pool.AddCommand(
		&cobra.Command{Use: "list", RunE: func(*cobra.Command, []string) error { return raw("GET", "/v1/admin/pools", nil) }},
		&cobra.Command{Use: "get NAME", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error { return raw("GET", "/v1/admin/pools/"+a[0], nil) }},
		&cobra.Command{Use: "drain NAME", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error { return raw("POST", "/v1/admin/pools/"+a[0]+"/drain", nil) }},
		&cobra.Command{Use: "undrain NAME", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error { return raw("POST", "/v1/admin/pools/"+a[0]+"/undrain", nil) }},
	)
	cmd.AddCommand(org, pool,
		&cobra.Command{Use: "agents", Short: "List node agents", RunE: func(*cobra.Command, []string) error { return raw("GET", "/v1/admin/agents", nil) }},
		&cobra.Command{Use: "trust-report", Short: "Where work can go, where it went, and what cannot be proven", RunE: func(*cobra.Command, []string) error {
			return raw("GET", "/v1/admin/trust-report", nil)
		}},
		&cobra.Command{Use: "verify-audit", Short: "Verify the audit hash chain", RunE: func(*cobra.Command, []string) error { return raw("GET", "/v1/admin/audit/verify", nil) }},
		&cobra.Command{Use: "reconcile", Short: "Run the reconciler now", RunE: func(*cobra.Command, []string) error { return raw("POST", "/v1/admin/reconcile", nil) }},
		&cobra.Command{Use: "policy SCOPE FILE", Args: cobra.ExactArgs(2), Short: "Set a placement policy (global, org:<id>, project:<id>) from a JSON file", RunE: func(_ *cobra.Command, a []string) error {
			b, err := os.ReadFile(a[1])
			if err != nil {
				return err
			}
			var pol map[string]any
			if err := json.Unmarshal(b, &pol); err != nil {
				return err
			}
			return raw("PUT", "/v1/policies/"+a[0], pol)
		}},
	)
	return cmd
}

func doctorCmd() *cobra.Command {
	return &cobra.Command{Use: "doctor", Short: "Check connectivity and credentials", RunE: func(*cobra.Command, []string) error {
		s := load()
		c := client.New(s.URL, s.APIKey)
		var v api.VersionInfo
		if err := c.Do(ctx(), "GET", "/version", nil, &v); err != nil {
			return fmt.Errorf("cannot reach %s: %w", s.URL, err)
		}
		fmt.Printf("[ok  ] server %s reachable (version %s, installation %s)\n", s.URL, v.Version, v.Installation)
		var me map[string]any
		if err := c.Do(ctx(), "GET", "/v1/me", nil, &me); err != nil {
			fmt.Printf("[FAIL] API key rejected: %v\n", err)
			return err
		}
		fmt.Printf("[ok  ] authenticated as %v (scopes %v)\n", me["keyId"], me["scopes"])
		var caps struct {
			Items []struct {
				Pool, Trust, Health string
			} `json:"items"`
		}
		if err := c.Do(ctx(), "GET", "/v1/capabilities", nil, &caps); err == nil {
			for _, p := range caps.Items {
				fmt.Printf("[%s] pool %s (%s): %s\n", map[bool]string{true: "ok  ", false: "warn"}[p.Health == "healthy"], p.Pool, p.Trust, p.Health)
			}
		}
		return nil
	}}
}
