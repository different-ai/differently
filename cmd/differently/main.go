// Command differently is the Differently CLI.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/different-ai/differently/internal/version"
	"github.com/different-ai/differently/pkg/api"
	"github.com/different-ai/differently/pkg/client"
)

type settings struct {
	URL     string `json:"url,omitempty"`
	APIKey  string `json:"apiKey,omitempty"`
	Project string `json:"project,omitempty"`
}

var (
	flagURL, flagKey, flagProject, flagOutput string
)

func configPath() string {
	dir, _ := os.UserConfigDir()
	return filepath.Join(dir, "differently", "config.json")
}

// saved reads the config file alone, with nothing from flags, the
// environment or a local dev server: what login and org create wrote.
func saved() settings {
	var s settings
	if b, err := os.ReadFile(configPath()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

// save writes the config file, readable by its owner only: it holds a key.
func save(s settings) error {
	if err := os.MkdirAll(filepath.Dir(configPath()), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	return os.WriteFile(configPath(), b, 0o600)
}

// rememberProject makes id the default project when there is none: no
// --project, no DIFFERENTLY_PROJECT, nothing saved and no local dev server's
// project. It never replaces a default, and it leaves alone a file saved for
// another server, where the ID would mean nothing. When it does not save, why
// says so.
func rememberProject(id string) (ok bool, why string, err error) {
	cur := load()
	if cur.Project != "" {
		return false, "the default stays " + cur.Project, nil
	}
	f := saved()
	if f.URL != "" && strings.TrimRight(f.URL, "/") != strings.TrimRight(cur.URL, "/") {
		return false, "the saved login is for " + f.URL, nil
	}
	f.Project = id
	return true, "", save(f)
}

func load() settings {
	s := saved()
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
	// Local dev servers write an operator token; use it for this machine only.
	if s.APIKey == "" && isLoopbackURL(s.URL) {
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

// isLoopbackURL reports whether raw points at this machine: localhost, a
// *.localhost name or a loopback address. The dev operator token is sent
// nowhere else; a host that merely contains "localhost" (for example
// localhost.example.com) is someone else's server.
func isLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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

func table(header string, rows [][]string) { tableTo(os.Stdout, header, rows) }

func tableTo(out io.Writer, header string, rows [][]string) {
	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
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
	if flagOutput == "json" && len(items) == 1 {
		return printJSON(items[0])
	}
	return showSandboxList(items)
}

// showSandboxList prints a list; JSON output is always an array.
func showSandboxList(items []api.Sandbox) error {
	if flagOutput == "json" {
		if items == nil {
			items = []api.Sandbox{}
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

// newRoot builds the command tree. Building it also resets the global flags.
func newRoot() *cobra.Command {
	root := &cobra.Command{Use: "differently", Short: "Differently: your infrastructure, your rules, one API", SilenceUsage: true, SilenceErrors: true, Version: version.Version}
	root.PersistentFlags().StringVar(&flagURL, "url", "", "API URL (env DIFFERENTLY_URL)")
	root.PersistentFlags().StringVar(&flagKey, "api-key", "", "API key (env DIFFERENTLY_API_KEY)")
	root.PersistentFlags().StringVar(&flagProject, "project", "", "project ID for org/operator keys (env DIFFERENTLY_PROJECT; default: the one login or admin org create saved)")
	root.PersistentFlags().StringVarP(&flagOutput, "output", "o", "table", "output format: table or json")
	root.AddCommand(runCmd(), loginCmd(), sandboxCmd(), projectCmd(), keyCmd(), usageCmd(), adminCmd(), doctorCmd(),
		&cobra.Command{Use: "whoami", Short: "Show the current key", RunE: func(*cobra.Command, []string) error { return raw("GET", "/v1/me", nil) }},
		&cobra.Command{Use: "capabilities", Short: "List pools and their capabilities", RunE: func(*cobra.Command, []string) error { return raw("GET", "/v1/capabilities", nil) }},
	)
	return root
}

func main() {
	if err := newRoot().Execute(); err != nil {
		reportError(os.Stderr, err)
		for _, hint := range []string{startHint(err, load().URL), projectHint(err)} {
			if hint != "" {
				fmt.Fprintln(os.Stderr, hint)
			}
		}
		os.Exit(1)
	}
}

// projectHint says how to get a default project when a key that is not
// limited to one named none, which is what a first sandbox on a new
// installation looks like.
func projectHint(err error) string {
	var e *api.Error
	if !errors.As(err, &e) || e.Code != api.CodeInvalidRequest || !strings.Contains(e.Message, "projectId is required") {
		return ""
	}
	return "no default project. On a new installation, make one with: differently admin org create NAME\n" +
		"otherwise pick one from 'differently project list' with --project ID, or save it with: differently login --project ID"
}

// startHint says how to get a server when nothing answers on this machine,
// which is what a first run without one looks like.
func startHint(err error, url string) string {
	if !errors.Is(err, syscall.ECONNREFUSED) || !isLoopbackURL(url) {
		return ""
	}
	return "nothing is listening at " + url + ". Start a local server with: differently-server --dev"
}

// reportError prints a failed command's error. API errors show their code and
// details; anything else (an unknown flag, a missing argument, an unreachable
// server) is printed as is, so the CLI never exits without saying why.
func reportError(w io.Writer, err error) {
	var e *api.Error
	if !errors.As(err, &e) {
		fmt.Fprintf(w, "error: %v\n", err)
		return
	}
	fmt.Fprintf(w, "error [%s]: %s\n", e.Code, e.Message)
	for k, v := range e.Details {
		fmt.Fprintf(w, "  %s: %s\n", k, v)
	}
}

func loginCmd() *cobra.Command {
	return &cobra.Command{
		Use: "login", Short: "Save the API URL, key and default project",
		Long: `Login checks the key against the server and saves the URL, the key and a
default project, so later commands need no flags.

The URL and key come from --url and --api-key or DIFFERENTLY_URL and
DIFFERENTLY_API_KEY; without a key, login asks for one.

A key limited to one project uses that project. For an organization or
operator key, login saves the project given with --project or
DIFFERENTLY_PROJECT, else keeps the one saved before if the key still sees
it, else takes the only project the key can see. With no project it says how
to create one; with several it lists them and saves none.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := load()
			if flagKey == "" && os.Getenv("DIFFERENTLY_API_KEY") == "" {
				fmt.Fprint(cmd.OutOrStdout(), "API key: ")
				if _, err := fmt.Fscanln(cmd.InOrStdin(), &s.APIKey); err != nil {
					return err
				}
			}
			named := flagProject
			if named == "" {
				named = os.Getenv("DIFFERENTLY_PROJECT")
			}
			return login(ctx(), s, named, saved().Project, cmd.OutOrStdout())
		},
	}
}

// whoami is what GET /v1/me says about a key.
type whoami struct {
	KeyID     string `json:"keyId"`
	ProjectID string `json:"projectId"`
	Operator  bool   `json:"operator"`
}

// login checks the key, works out its default project and saves the URL, the
// key and that project. named is a project the caller asked for; remembered is
// the one saved before, which may belong to another key or server.
func login(c context.Context, s settings, named, remembered string, out io.Writer) error {
	cl := client.New(s.URL, s.APIKey)
	var me whoami
	if err := cl.Do(c, "GET", "/v1/me", nil, &me); err != nil {
		return err
	}
	project, visible, err := loginProject(c, cl, me, named, remembered)
	if err != nil {
		return err
	}
	s.Project = project
	if err := save(s); err != nil {
		return err
	}
	fmt.Fprintf(out, "logged in to %s as key %s (saved to %s)\n", s.URL, me.KeyID, configPath())
	switch {
	case project != "":
		fmt.Fprintf(out, "default project: %s\n", project)
	case len(visible) == 0 && me.Operator:
		fmt.Fprintln(out, "no project exists yet. Make an organization and its first project with: differently admin org create NAME")
	case len(visible) == 0:
		fmt.Fprintln(out, "no project exists yet. Make one with 'differently project create NAME', then run 'differently login' again")
	default:
		fmt.Fprintf(out, "this key can use %d projects, so none was saved as the default:\n", len(visible))
		var rows [][]string
		for _, p := range visible {
			rows = append(rows, []string{p.ID, p.OrgID, p.Name})
		}
		tableTo(out, "ID\tORGANIZATION\tNAME", rows)
		fmt.Fprintln(out, "name one per command with --project ID or DIFFERENTLY_PROJECT=ID, or save it with: differently login --project ID")
	}
	return nil
}

// loginProject picks the default project for a key. A key limited to one
// project has no choice. Any other key keeps the project the caller named,
// or the remembered one while the key can still see it, and otherwise takes
// the only project it can see. With none or several to choose from it
// returns no project and the ones it saw: guessing would send work to a
// project nobody chose.
func loginProject(c context.Context, cl *client.Client, me whoami, named, remembered string) (string, []api.Project, error) {
	if me.ProjectID != "" {
		return me.ProjectID, nil, nil
	}
	visible := func(id string) error {
		return cl.Do(c, "GET", "/v1/projects/"+url.PathEscape(id), nil, &api.Project{})
	}
	if named != "" {
		return named, nil, visible(named)
	}
	if remembered != "" {
		switch err := visible(remembered); {
		case err == nil:
			return remembered, nil, nil
		case !api.IsCode(err, api.CodeNotFound):
			return "", nil, err
		}
	}
	var list api.List[api.Project]
	if err := cl.Do(c, "GET", "/v1/projects", nil, &list); err != nil {
		return "", nil, err
	}
	if len(list.Items) == 1 {
		return list.Items[0].ID, nil, nil
	}
	return "", list.Items, nil
}

func sandboxCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "sandbox", Aliases: []string{"sb", "sandboxes"}, Short: "Create, use and delete sandboxes"}
	var (
		profile, template, class, strategy, external string
		cpu, mem, disk, ttl, idle                    int
		wait                                         bool
		envs, labels, caps, pools, attests           []string
	)
	create := &cobra.Command{
		Use: "create", Short: "Create a sandbox",
		RunE: func(*cobra.Command, []string) error {
			req := api.CreateSandboxRequest{ProjectID: load().Project, Profile: profile, Template: template, Classification: class, TTLSeconds: ttl, Capabilities: caps, IdleTimeoutSeconds: idle}
			if cpu > 0 || mem > 0 || disk > 0 {
				req.Resources = &api.Resources{CPU: cpu, MemoryMiB: mem, DiskGiB: disk}
			}
			if strategy != "" || external != "" || len(pools) > 0 || len(attests) > 0 {
				req.Placement = &api.Placement{Strategy: strategy, External: external, AllowedPools: pools, RequiredAttestations: attests}
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
	f.IntVar(&idle, "idle-timeout", 0, "stop or pause after this many seconds unused (at least 60; -1 off); the next call wakes it")
	f.StringVar(&strategy, "strategy", "", "placement strategy: private-first, cost or spread")
	f.StringVar(&external, "external", "", "narrow external placement: never or overflow")
	f.StringSliceVar(&pools, "pool", nil, "restrict to these pools")
	f.StringSliceVar(&attests, "require-attestation", nil, "only pools that hold these attestations, e.g. soc2-type2,hipaa-baa")
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
		return showSandboxList(out.Items)
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
			code, err := cli().ExecStream(ctx(), a[0], execRequest(a[1:], cwd, execEnv), printExecEvent(os.Stdout, os.Stderr))
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
	cmd.AddCommand(create, list, get, del, waitCmd, exec, files, runCmd())
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

// execRequest builds the request for a command line. A single argument
// holding shell syntax runs through the shell; anything else runs directly.
func execRequest(argv []string, cwd string, env []string) api.ExecRequest {
	if len(argv) == 1 && strings.ContainsAny(argv[0], " |&;$<>") {
		return api.ExecRequest{Command: argv[0], Cwd: cwd, Env: kv(env)}
	}
	return api.ExecRequest{Args: argv, Cwd: cwd, Env: kv(env)}
}

// printExecEvent copies a command's output to the matching local stream.
func printExecEvent(stdout, stderr io.Writer) func(api.ExecEvent) {
	return func(ev api.ExecEvent) {
		if ev.Stream == "stderr" {
			fmt.Fprint(stderr, ev.Data)
		} else {
			fmt.Fprint(stdout, ev.Data)
		}
	}
}

// runCmd is the shortest way to a result: one sandbox for one command.
func runCmd() *cobra.Command {
	var profile, template, cwd string
	var envs []string
	var keep bool
	cmd := &cobra.Command{
		Use:   "run [flags] -- COMMAND [ARGS...]",
		Short: "Run one command in a new sandbox, then delete the sandbox",
		Long: `Run creates a sandbox, waits until it is running, runs the command with its
output streamed here, and deletes the sandbox. The exit code is the command's.
With --keep the sandbox stays, and its ID is printed for later commands.`,
		Example: `  differently run -- echo hello
  differently run 'uname -sm && id -un'
  differently run --keep -- npm test`,
		Args: func(_ *cobra.Command, a []string) error {
			if len(a) == 0 {
				return errors.New("run needs a command, for example: differently run -- echo hello")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, a []string) error {
			req := api.CreateSandboxRequest{ProjectID: load().Project, Profile: profile, Template: template}
			code, err := runOnce(ctx(), cli(), req, execRequest(a, cwd, envs), keep, cmd.OutOrStdout(), cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if code != 0 {
				os.Exit(code)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&profile, "profile", "", "resource profile (default: the server's)")
	f.StringVar(&template, "template", "", "image template (default: the server's)")
	f.StringVar(&cwd, "cwd", "", "working directory")
	f.StringArrayVarP(&envs, "env", "e", nil, "environment KEY=VALUE")
	f.BoolVar(&keep, "keep", false, "leave the sandbox running instead of deleting it")
	return cmd
}

// runOnce creates a sandbox, runs one command in it and returns the
// command's exit code. Unless keep is set the sandbox is deleted whatever
// happened, including an interrupt, so a run never leaves one behind.
func runOnce(c context.Context, cl *client.Client, req api.CreateSandboxRequest, cmd api.ExecRequest, keep bool, stdout, stderr io.Writer) (int, error) {
	sb, err := cl.CreateSandbox(c, req, true)
	remove := func() {
		if sb.ID == "" {
			return
		}
		if keep {
			fmt.Fprintf(stderr, "kept %s; delete it with: differently sandbox delete %s\n", sb.ID, sb.ID)
			return
		}
		// The run's own context may already be cancelled by an interrupt.
		done, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := cl.DeleteSandbox(done, sb.ID); err != nil {
			fmt.Fprintf(stderr, "could not delete %s (%v); delete it with: differently sandbox delete %s\n", sb.ID, err, sb.ID)
		}
	}
	defer remove()
	if err != nil {
		return -1, err
	}
	fmt.Fprintf(stderr, "sandbox %s running on pool %s (%s)\n", sb.ID, sb.Pool, sb.Trust)
	return cl.ExecStream(c, sb.ID, cmd, printExecEvent(stdout, stderr))
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
	create := &cobra.Command{
		Use: "create NAME", Args: cobra.ExactArgs(1), Short: "Create a scoped key (the token is shown once)",
		Long: `Create makes a key and shows its token once.

With neither --org nor --key-project the key is limited to the default
project (--project, DIFFERENTLY_PROJECT, or the one login or admin org create
saved). --key-project names another project; its organization is looked up.
--org alone makes a key for every project of that organization. A key with
the admin or org:admin scope is never limited to the default project.`,
		Example: `  differently key create dev
  differently key create ci --key-project prj_... --ttl-hours 720
  differently key create team-admins --org org_... --scope org:admin`,
		RunE: func(cmd *cobra.Command, a []string) error {
			cl := cli()
			req, err := keyRequest(ctx(), cl, api.CreateAPIKeyRequest{Name: a[0], OrgID: org, ProjectID: project, Scopes: scopes, TTLHours: ttl}, load().Project)
			if err != nil {
				return err
			}
			out, err := cl.CreateAPIKey(ctx(), req)
			if err != nil {
				return err
			}
			if flagOutput == "json" {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "key %s created for %s; store this token now, it is not shown again:\n%s\n", out.ID, keyReach(out.APIKey), out.Token)
			return nil
		},
	}
	create.Flags().StringSliceVar(&scopes, "scope", []string{"sandboxes:read", "sandboxes:write", "sandboxes:exec"}, "scopes")
	create.Flags().StringVar(&org, "org", "", "organization; alone, the key works in every project of it")
	create.Flags().StringVar(&project, "key-project", "", "limit the key to this project (default: the default project, unless --org is given)")
	create.Flags().IntVar(&ttl, "ttl-hours", 0, "expire after N hours")
	var grace time.Duration
	rotate := &cobra.Command{Use: "rotate ID", Args: cobra.ExactArgs(1), Short: "Replace a key with a new one (same name, scopes and project); the token is shown once", RunE: func(_ *cobra.Command, a []string) error {
		out, err := cli().RotateAPIKey(ctx(), a[0], grace)
		if err != nil {
			return err
		}
		if flagOutput == "json" {
			return printJSON(out)
		}
		old := "revoked now"
		if out.Replaced != nil && !out.Replaced.Revoked && out.Replaced.ExpiresAt != nil {
			old = "valid until " + out.Replaced.ExpiresAt.Local().Format(time.DateTime)
		}
		fmt.Printf("key %s replaces %s (%s); store this token now, it is not shown again:\n%s\n", out.ID, a[0], old, out.Token)
		return nil
	}}
	rotate.Flags().DurationVar(&grace, "grace", 0, "keep the old key valid this long (e.g. 1h; at most 168h); 0 revokes it now")
	list := &cobra.Command{Use: "list", Short: "List keys", RunE: func(*cobra.Command, []string) error {
		keys, err := cli().ListAPIKeys(ctx())
		if err != nil || flagOutput == "json" {
			if err == nil {
				err = printJSON(keys)
			}
			return err
		}
		var rows [][]string
		for _, k := range keys {
			state := "active"
			switch {
			case k.Revoked:
				state = "revoked"
			case k.ExpiresAt != nil && k.ExpiresAt.Before(time.Now()):
				state = "expired"
			}
			rows = append(rows, []string{k.Name, k.ID, orDash(k.ProjectID), strings.Join(k.Scopes, ","), when(k.ExpiresAt), when(k.LastUsedAt), state})
		}
		table("NAME\tID\tPROJECT\tSCOPES\tEXPIRES\tLAST USED\tSTATE", rows)
		return nil
	}}
	cmd.AddCommand(create, rotate, list,
		&cobra.Command{Use: "get ID", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error { return raw("GET", "/v1/api-keys/"+a[0], nil) }},
		&cobra.Command{Use: "revoke ID", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error { return raw("DELETE", "/v1/api-keys/"+a[0], nil) }},
	)
	return cmd
}

// keyRequest fills in what a key create left out. With neither an
// organization nor a project, a tenant key is limited to the default project.
// A project without an organization takes the project's own, which an
// operator key must name. Keys with the admin or org:admin scope are left
// alone: they are meant to reach more than one project.
//
// A default project the key cannot see is an error, not a reason to fall
// back to a wider key.
func keyRequest(c context.Context, cl *client.Client, req api.CreateAPIKeyRequest, defaultProject string) (api.CreateAPIKeyRequest, error) {
	wide := slices.Contains(req.Scopes, "admin") || slices.Contains(req.Scopes, "org:admin")
	byDefault := req.OrgID == "" && req.ProjectID == "" && !wide && defaultProject != ""
	if byDefault {
		req.ProjectID = defaultProject
	}
	if req.ProjectID != "" && req.OrgID == "" {
		var p api.Project
		err := cl.Do(c, "GET", "/v1/projects/"+url.PathEscape(req.ProjectID), nil, &p)
		if byDefault && api.IsCode(err, api.CodeNotFound) {
			return req, api.Errorf(api.CodeNotFound, "this key cannot use the default project %s; name a project with --key-project or an organization with --org", req.ProjectID)
		}
		if err != nil {
			return req, err
		}
		req.OrgID = p.OrgID
	}
	return req, nil
}

// keyReach says where a key works, so nobody has to guess what a default did.
func keyReach(k api.APIKey) string {
	switch {
	case k.ProjectID != "":
		return "project " + k.ProjectID
	case k.OrgID != "":
		return "every project of organization " + k.OrgID
	}
	return "every organization (operator)"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func when(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Local().Format(time.DateTime)
}

// secretValue reads a credential from a file or stdin, never from a flag, so
// it stays out of shell history and process listings.
func secretValue(file string) (string, error) {
	var b []byte
	var err error
	if file != "" {
		b, err = os.ReadFile(file)
	} else {
		if fi, statErr := os.Stdin.Stat(); statErr == nil && fi.Mode()&os.ModeCharDevice != 0 {
			fmt.Fprint(os.Stderr, "value (then Ctrl-D): ")
		}
		b, err = io.ReadAll(io.LimitReader(os.Stdin, 64<<10))
	}
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", errors.New("the value is empty")
	}
	return v, nil
}

func secretCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "secret", Short: "Provider credentials stored in Differently (values are never shown)"}
	var fromFile string
	put := &cobra.Command{Use: "put NAME", Args: cobra.ExactArgs(1), Short: "Store or replace a credential, read from stdin or --from-file", RunE: func(_ *cobra.Command, a []string) error {
		v, err := secretValue(fromFile)
		if err != nil {
			return err
		}
		sec, err := cli().PutSecret(ctx(), a[0], v)
		if err != nil {
			return err
		}
		if flagOutput == "json" {
			return printJSON(sec)
		}
		fmt.Printf("secret %s stored (fingerprint %s); pools using it switch to it within seconds\n", sec.Name, sec.Fingerprint)
		return nil
	}}
	put.Flags().StringVar(&fromFile, "from-file", "", "read the value from this file")
	list := &cobra.Command{Use: "list", RunE: func(*cobra.Command, []string) error {
		items, err := cli().ListSecrets(ctx())
		if err != nil || flagOutput == "json" {
			if err == nil {
				err = printJSON(items)
			}
			return err
		}
		var rows [][]string
		for _, s := range items {
			rows = append(rows, []string{s.Name, s.Fingerprint, orDash(strings.Join(s.UsedBy, ",")), s.UpdatedAt.Local().Format(time.DateTime), s.UpdatedBy})
		}
		table("NAME\tFINGERPRINT\tUSED BY\tUPDATED\tBY", rows)
		return nil
	}}
	cmd.AddCommand(put, list,
		&cobra.Command{Use: "delete NAME", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error { return raw("DELETE", "/v1/admin/secrets/"+a[0], nil) }},
	)
	return cmd
}

// poolAddCmd registers a provider at runtime. With --api-key-stdin the key is
// stored in Differently as <name>-key first, so no Kubernetes Secret is needed.
func poolAddCmd() *cobra.Command {
	var adapterKind, trust, region, secret, credential string
	var settings, attests []string
	var maxConcurrency int
	var keyStdin bool
	var budget float64
	var costs []string
	cmd := &cobra.Command{Use: "add NAME", Args: cobra.ExactArgs(1), Short: "Add a provider pool (e.g. Daytona or Freestyle) at runtime", RunE: func(_ *cobra.Command, a []string) error {
		cfg := map[string]string{}
		for _, kv := range settings {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return fmt.Errorf("--config takes key=value, got %q", kv)
			}
			cfg[k] = v
		}
		creds := map[string]string{}
		switch {
		case keyStdin:
			v, err := secretValue("")
			if err != nil {
				return err
			}
			if secret == "" {
				secret = a[0] + "-key"
			}
			if _, err := cli().PutSecret(ctx(), secret, v); err != nil {
				return fmt.Errorf("storing the key as secret %s: %w", secret, err)
			}
			creds["secret"] = secret
		case secret != "":
			creds["secret"] = secret
		case credential != "":
			kind, ref, _ := strings.Cut(credential, ":")
			if (kind != "env" && kind != "file") || ref == "" {
				return fmt.Errorf("--credential takes env:NAME or file:/path")
			}
			creds[kind] = ref
		}
		body := map[string]any{"name": a[0], "adapter": adapterKind, "trust": trust, "config": cfg, "credentials": creds}
		if region != "" {
			body["region"] = region
		}
		if maxConcurrency > 0 {
			body["maxConcurrency"] = maxConcurrency
		}
		if len(costs) > 0 {
			model := map[string]float64{}
			for _, kv := range costs {
				k, v, _ := strings.Cut(kv, "=")
				price, err := strconv.ParseFloat(v, 64)
				if err != nil || price < 0 {
					return fmt.Errorf("--cost takes NAME=PRICE such as cpuHour=0.0504, got %q", kv)
				}
				model[k] = price
			}
			body["costModel"] = model
		}
		if budget > 0 {
			body["budget"] = map[string]float64{"monthly": budget}
		}
		if len(attests) > 0 {
			held := map[string]any{}
			for _, a := range attests {
				name, evidence, _ := strings.Cut(a, "=")
				held[name] = map[string]string{"evidence": evidence}
			}
			body["attestations"] = held
		}
		return raw("POST", "/v1/admin/pools", body)
	}}
	f := cmd.Flags()
	f.StringVar(&adapterKind, "adapter", "", "opensandbox, daytona, freestyle or remote; a server built with -tags experimental_adapters has more (docs/providers)")
	f.StringVar(&trust, "trust", "", "external (someone else runs it) or private (you do); managed providers are always external, self-hostable kinds must set one")
	f.StringArrayVar(&settings, "config", nil, "adapter setting key=value (repeatable), e.g. apiURL=https://app.daytona.io/api")
	f.StringVar(&region, "region", "", "region label")
	f.IntVar(&maxConcurrency, "max-concurrency", 0, "most sandboxes at once (0: quotas only)")
	f.StringArrayVar(&attests, "attestation", nil, "what the provider is certified for or has signed, NAME[=EVIDENCE] (repeatable), e.g. soc2-type2=https://trust.example.com")
	f.StringArrayVar(&costs, "cost", nil, "list price NAME=PRICE (repeatable): cpuHour, memoryGiBHour, diskGiBHour, sandboxHour")
	f.Float64Var(&budget, "monthly-budget", 0, "stop sending new sandboxes once the month's estimated spend reaches this (needs --cost)")
	f.StringVar(&secret, "secret", "", "use a credential stored with 'admin secret put'")
	f.BoolVar(&keyStdin, "api-key-stdin", false, "read the provider's API key from stdin and store it as --secret (default <name>-key)")
	f.StringVar(&credential, "credential", "", "reference instead of storing: env:NAME or file:/path on the server")
	_ = cmd.MarkFlagRequired("adapter")
	return cmd
}

func poolUseCmd() *cobra.Command {
	var approve bool
	cmd := &cobra.Command{Use: "use NAME", Args: cobra.ExactArgs(1), Short: "Send new sandboxes to this pool (drains the others; running sandboxes stay)", RunE: func(_ *cobra.Command, a []string) error {
		out, err := cli().UsePool(ctx(), a[0], approve)
		if err != nil {
			return err
		}
		if flagOutput == "json" {
			return printJSON(out)
		}
		fmt.Printf("new sandboxes now go to %s; drained: %s\n", out.Pool.Name, orDash(strings.Join(out.Drained, ", ")))
		if out.Policy != nil {
			fmt.Printf("approved %s for external placement (global policy: external %s)\n", out.Pool.Name, out.Policy.External)
		}
		return nil
	}}
	cmd.Flags().BoolVar(&approve, "approve-external", false, "approve an external pool in the global policy if it is not yet")
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
		&cobra.Command{Use: "audit", Short: "List the latest audit events, newest first", RunE: func(*cobra.Command, []string) error { return raw("GET", "/v1/audit-events?limit=100&order=desc", nil) }},
	)
	return cmd
}

func adminCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "admin", Short: "Operator commands (admin key)"}
	org := &cobra.Command{Use: "org", Short: "Organizations"}
	var defaultProject string
	createOrg := &cobra.Command{
		Use: "create NAME", Args: cobra.ExactArgs(1), Short: "Create an organization and its first project",
		Long: `Create makes an organization and, unless --default-project is empty, a first
project in it, and prints both IDs.

When this CLI has no default project yet, the new one is saved as the
default, so 'differently run' and 'differently key create' work right after
with no ID to copy. A default that already exists is never replaced.`,
		RunE: func(cmd *cobra.Command, a []string) error {
			return orgCreate(ctx(), cli(), a[0], defaultProject, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
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
	pool.AddCommand(poolAddCmd(), poolUseCmd(),
		&cobra.Command{Use: "budget NAME AMOUNT", Args: cobra.ExactArgs(2), Short: "Set a pool's monthly budget (0 removes it); new sandboxes stop once estimated spend reaches it", RunE: func(_ *cobra.Command, a []string) error {
			amount, err := strconv.ParseFloat(a[1], 64)
			if err != nil || amount < 0 {
				return fmt.Errorf("AMOUNT must be a number, 0 or more, got %q", a[1])
			}
			return raw("PATCH", "/v1/admin/pools/"+a[0], map[string]any{"budget": map[string]float64{"monthly": amount}})
		}},
		&cobra.Command{Use: "list", RunE: func(*cobra.Command, []string) error { return raw("GET", "/v1/admin/pools", nil) }},
		&cobra.Command{Use: "get NAME", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error { return raw("GET", "/v1/admin/pools/"+a[0], nil) }},
		&cobra.Command{Use: "drain NAME", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error { return raw("POST", "/v1/admin/pools/"+a[0]+"/drain", nil) }},
		&cobra.Command{Use: "undrain NAME", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, a []string) error { return raw("POST", "/v1/admin/pools/"+a[0]+"/undrain", nil) }},
	)
	cmd.AddCommand(org, pool, secretCmd(),
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

// orgCreate makes an organization with its first project, prints what it
// made and remembers the project as the default when there is none. The
// summary (or the JSON response) goes to stdout and the note about the
// default to stderr, so -o json stays parseable.
func orgCreate(c context.Context, cl *client.Client, name, firstProject string, stdout, stderr io.Writer) error {
	var body json.RawMessage
	if err := cl.Do(c, "POST", "/v1/admin/organizations", map[string]string{"name": name, "defaultProject": firstProject}, &body); err != nil {
		return err
	}
	var made struct {
		Organization api.Organization `json:"organization"`
		Project      *api.Project     `json:"project"`
	}
	if err := json.Unmarshal(body, &made); err != nil {
		return err
	}
	if flagOutput == "json" {
		var v any
		_ = json.Unmarshal(body, &v)
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(v); err != nil {
			return err
		}
	} else {
		rows := [][]string{{"organization", made.Organization.ID, made.Organization.Name}}
		if made.Project != nil {
			rows = append(rows, []string{"project", made.Project.ID, made.Project.Name})
		}
		tableTo(stdout, "KIND\tID\tNAME", rows)
	}
	if made.Project == nil {
		fmt.Fprintf(stderr, "no project was made; add one with: differently project create NAME --org %s\n", made.Organization.ID)
		return nil
	}
	id := made.Project.ID
	ok, why, err := rememberProject(id)
	switch {
	case err != nil:
		fmt.Fprintf(stderr, "could not save %s as the default project (%v); use it with --project %s\n", id, err, id)
	case ok:
		fmt.Fprintf(stderr, "%s is now the default project (saved to %s); try: differently run -- echo hello\n", id, configPath())
	default:
		fmt.Fprintf(stderr, "%s was not saved as the default project (%s); use it with --project %s\n", id, why, id)
	}
	return nil
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
