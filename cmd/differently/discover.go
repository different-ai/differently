package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// discover looks at the tools already installed and signed in on this
// machine (gcloud, kubectl, helm, docker, az, aws) and says what they can
// see and what Differently could run on it. It needs no Differently server,
// it only reads, and it uses each tool's own session: Differently never sees
// a cloud credential.

const docsBase = "https://different-ai.github.io/differently/docs/"

// toolTimeout bounds one external command. A cluster that does not answer
// must not hold the whole report.
const toolTimeout = 10 * time.Second

// toolRun runs one external command and returns its standard output.
// Tests replace it and toolLook with fakes.
var (
	toolRun  = execTool
	toolLook = exec.LookPath
)

func execTool(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("no answer in %s", toolTimeout)
		}
		if msg := firstLine(errOut.String()); msg != "" {
			return "", errors.New(msg)
		}
		return "", err
	}
	return out.String(), nil
}

// toolProblem is a tool's complaint, short enough for one table cell.
func toolProblem(err error) string {
	msg := err.Error()
	if r := []rune(msg); len(r) > 100 {
		msg = string(r[:100]) + "..."
	}
	return msg
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

type toolFinding struct {
	Name      string `json:"name"`
	Installed bool   `json:"installed"`
	Version   string `json:"version,omitempty"`
	// PointedAt is the account, project, context or daemon the tool uses now.
	PointedAt string `json:"pointedAt,omitempty"`
	Note      string `json:"note,omitempty"`
}

type foundResource struct {
	Tool   string `json:"tool"`
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Detail string `json:"detail,omitempty"`
}

type possibility struct {
	What string `json:"what"`
	Why  string `json:"why"`
	Next string `json:"next"`
}

type discovery struct {
	Tools    []toolFinding   `json:"tools"`
	Found    []foundResource `json:"found"`
	Possible []possibility   `json:"possible"`
	// Commands is every external command that was run, as typed.
	Commands []string `json:"commands"`
}

// discoverer collects one report. Every external command goes through run,
// so the report can list exactly what was executed.
type discoverer struct {
	ctx context.Context
	d   discovery
}

func (x *discoverer) run(name string, args ...string) (string, error) {
	x.d.Commands = append(x.d.Commands, name+" "+strings.Join(args, " "))
	return toolRun(x.ctx, name, args...)
}

func (x *discoverer) found(tool, kind, name, detail string) {
	x.d.Found = append(x.d.Found, foundResource{Tool: tool, Kind: kind, Name: name, Detail: detail})
}

func (x *discoverer) possible(what, why, next string) {
	x.d.Possible = append(x.d.Possible, possibility{What: what, Why: why, Next: next})
}

// tool records a tool as installed or not; it reports whether to go on.
func (x *discoverer) tool(name string) (*toolFinding, bool) {
	x.d.Tools = append(x.d.Tools, toolFinding{Name: name})
	t := &x.d.Tools[len(x.d.Tools)-1]
	if _, err := toolLook(name); err != nil {
		return t, false
	}
	t.Installed = true
	return t, true
}

func discover(ctx context.Context) discovery {
	x := &discoverer{ctx: ctx, d: discovery{Found: []foundResource{}, Possible: []possibility{}}}
	dockerUp := x.docker()
	clusters := x.gcloud()
	x.kubectl(clusters)
	x.helm()
	x.az()
	x.aws()
	if !dockerUp && len(x.d.Possible) == 0 {
		x.possible("Try Differently on this machine in dev mode",
			"nothing here can host sandboxes yet; dev mode needs nothing, and gives no isolation",
			"differently-server --dev   ("+docsBase+"guides/quickstart/)")
	}
	return x.d
}

func (x *discoverer) docker() (up bool) {
	t, ok := x.tool("docker")
	if !ok {
		return false
	}
	if v, err := x.run("docker", "--version"); err == nil {
		t.Version = strings.TrimSuffix(strings.TrimPrefix(firstLine(v), "Docker version "), ",")
		if i := strings.IndexByte(t.Version, ','); i >= 0 {
			t.Version = t.Version[:i]
		}
	}
	v, err := x.run("docker", "info", "--format", "{{.ServerVersion}}")
	if err != nil || firstLine(v) == "" {
		t.Note = "no Docker daemon answers; start one to run the stack here"
		return false
	}
	t.PointedAt = "a running daemon, version " + firstLine(v)
	x.found("docker", "Docker host", "this machine", "daemon "+firstLine(v))
	x.possible("Run the whole stack on this Docker host",
		"a Docker daemon is running: one Compose file starts OpenSandbox and Differently, with no database to run",
		docsBase+"deploy/docker/")
	return true
}

type gkeCluster struct {
	Name, Location, Project string
}

func (x *discoverer) gcloud() []gkeCluster {
	t, ok := x.tool("gcloud")
	if !ok {
		return nil
	}
	if v, err := x.run("gcloud", "version", "--format=json"); err == nil {
		var ver map[string]any
		if json.Unmarshal([]byte(v), &ver) == nil {
			t.Version, _ = ver["Google Cloud SDK"].(string)
		}
	}
	out, err := x.run("gcloud", "config", "list", "--format=json")
	if err != nil {
		t.Note = "could not read its configuration: " + toolProblem(err)
		return nil
	}
	var cfg struct {
		Core struct{ Account, Project string } `json:"core"`
	}
	_ = json.Unmarshal([]byte(out), &cfg)
	if cfg.Core.Account == "" {
		t.Note = "not signed in; run: gcloud auth login"
		return nil
	}
	t.PointedAt = "account " + cfg.Core.Account
	if cfg.Core.Project == "" {
		t.Note = "no project set; run: gcloud config set project <id>"
		return nil
	}
	project := cfg.Core.Project
	t.PointedAt += ", project " + project

	out, err = x.run("gcloud", "container", "clusters", "list", "--project", project, "--format=json")
	if err != nil {
		t.Note = "could not list GKE clusters: " + toolProblem(err)
		if strings.Contains(strings.ToLower(err.Error()), "auth") {
			t.Note = "the sign-in has expired; run: gcloud auth login"
		}
		return nil
	}
	var list []struct {
		Name, Location, Status string
		CurrentNodeCount       int
		Autopilot              struct{ Enabled bool }
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Note = "could not read the GKE cluster list"
		return nil
	}
	var clusters []gkeCluster
	for _, c := range list {
		mode := fmt.Sprintf("%d nodes", c.CurrentNodeCount)
		if c.Autopilot.Enabled {
			mode = "Autopilot"
		}
		x.found("gcloud", "GKE cluster", c.Name, strings.Join([]string{c.Location, mode, strings.ToLower(c.Status)}, ", "))
		clusters = append(clusters, gkeCluster{Name: c.Name, Location: c.Location, Project: project})
	}
	if len(clusters) == 0 {
		x.possible("Run Differently on two Compute Engine VMs in project "+project,
			"gcloud is signed in and the project has no GKE cluster; this layout is documented but has not been run",
			docsBase+"deploy/gcp-vms/")
	}
	return clusters
}

func (x *discoverer) kubectl(gke []gkeCluster) {
	t, ok := x.tool("kubectl")
	if !ok {
		if len(gke) > 0 {
			x.possible("Install kubectl to look inside your GKE clusters",
				"gcloud lists clusters, but what is installed on them is read with kubectl", "https://kubernetes.io/docs/tasks/tools/")
		}
		return
	}
	if v, err := x.run("kubectl", "version", "--client", "-o", "json"); err == nil {
		var ver struct {
			ClientVersion struct{ GitVersion string } `json:"clientVersion"`
		}
		if json.Unmarshal([]byte(v), &ver) == nil {
			t.Version = strings.TrimPrefix(ver.ClientVersion.GitVersion, "v")
		}
	}
	current := ""
	if v, err := x.run("kubectl", "config", "current-context"); err == nil {
		current = firstLine(v)
	}
	if v, err := x.run("kubectl", "config", "get-contexts", "-o", "name"); err == nil {
		for _, name := range strings.Fields(v) {
			detail := ""
			if name == current {
				detail = "current"
			}
			x.found("kubectl", "Kubernetes context", name, detail)
		}
	}
	// A GKE cluster kubectl has no context for is named
	// gke_<project>_<location>_<cluster> once its credentials are fetched.
	for _, c := range gke {
		if current == "gke_"+c.Project+"_"+c.Location+"_"+c.Name {
			continue
		}
		x.possible("Look inside GKE cluster "+c.Name,
			"kubectl is not pointed at it, so what it already runs is unknown",
			fmt.Sprintf("gcloud container clusters get-credentials %s --location %s --project %s   then: differently discover", c.Name, c.Location, c.Project))
	}
	if current == "" {
		t.Note = "no current context"
		return
	}
	t.PointedAt = "context " + current

	out, err := x.run("kubectl", "--request-timeout=5s", "get", "crd", "-o", "name")
	if err != nil {
		t.Note = "the cluster did not answer: " + toolProblem(err)
		return
	}
	crds := strings.ToLower(out)
	hasOpenSandbox := strings.Contains(crds, "batchsandbox")
	hasAgentSandbox := strings.Contains(crds, ".agents.x-k8s.io")
	hasPodSnapshots := strings.Contains(crds, ".podsnapshot.gke.io")
	if hasOpenSandbox {
		x.found("kubectl", "Sandbox runtime", "OpenSandbox", "in context "+current)
		x.possible("Register the OpenSandbox on "+current+" as a private pool",
			"its resource definitions are installed there; OpenSandbox is the default private runtime",
			docsBase+"guides/opensandbox-pools/")
	}
	if hasAgentSandbox {
		detail, why := "in context "+current, "its resource definitions are installed there; the adapter is experimental and needs a server built with -tags experimental_adapters"
		next := docsBase + "providers/agent-sandbox/"
		if hasPodSnapshots {
			detail += ", with GKE Pod Snapshots"
			why += "; Pod Snapshots add pause, resume and snapshots, tested with fakes and not yet run on GKE"
			next = docsBase + "providers/gke-agent-sandbox/"
		}
		x.found("kubectl", "Sandbox runtime", "agent-sandbox", detail)
		x.possible("Register agent-sandbox on "+current+" as a pool", why, next)
	}
	if !hasOpenSandbox && !hasAgentSandbox {
		x.possible("Install OpenSandbox and Differently on "+current+" with Helm",
			"the cluster answers and has no sandbox runtime Differently knows",
			docsBase+"deploy/kubernetes/")
	}
}

func (x *discoverer) helm() {
	t, ok := x.tool("helm")
	if !ok {
		return
	}
	if v, err := x.run("helm", "version", "--short"); err == nil {
		t.Version = strings.TrimPrefix(firstLine(v), "v")
		if i := strings.IndexByte(t.Version, '+'); i >= 0 {
			t.Version = t.Version[:i]
		}
	}
}

func (x *discoverer) az() {
	t, ok := x.tool("az")
	if !ok {
		return
	}
	if v, err := x.run("az", "version", "-o", "json"); err == nil {
		var ver map[string]any
		if json.Unmarshal([]byte(v), &ver) == nil {
			t.Version, _ = ver["azure-cli"].(string)
		}
	}
	out, err := x.run("az", "account", "show", "-o", "json")
	if err != nil {
		t.Note = "not signed in; run: az login"
		return
	}
	var acct struct {
		Name string
		User struct{ Name string }
	}
	_ = json.Unmarshal([]byte(out), &acct)
	t.PointedAt = "account " + acct.User.Name + ", subscription " + acct.Name
	t.Note = "resources are not listed yet"
	x.possible("Run Differently on Azure VMs in subscription "+acct.Name,
		"az is signed in; this layout (k3s, OpenSandbox with microVMs, managed Postgres) ran on 2026-10-29",
		docsBase+"deploy/azure-vms/")
}

func (x *discoverer) aws() {
	t, ok := x.tool("aws")
	if !ok {
		return
	}
	if v, err := x.run("aws", "--version"); err == nil {
		t.Version = strings.TrimPrefix(strings.Fields(firstLine(v) + " ")[0], "aws-cli/")
	}
	t.Note = "resources are not listed yet"
}

func discoverCmd() *cobra.Command {
	var showCommands bool
	cmd := &cobra.Command{
		Use:   "discover",
		Short: "See what your cloud tools can reach and what Differently could run there",
		Long: `Looks at the tools installed and signed in on this machine (gcloud, kubectl,
helm, docker, az, aws), lists what they can see, and says which Differently
setups are possible from here.

It needs no Differently server and changes nothing: every command it runs
only reads, through each tool's own session, and --show-commands lists them.
Today it lists GKE clusters in gcloud's current project, kubectl's contexts,
the sandbox runtimes on the current context and the local Docker daemon.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d := discover(cmd.Context())
			out := cmd.OutOrStdout()
			if flagOutput == "json" {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(d)
			}
			printDiscovery(out, d, showCommands)
			return nil
		},
	}
	cmd.Flags().BoolVar(&showCommands, "show-commands", false, "list every command that was run")
	return cmd
}

func printDiscovery(out io.Writer, d discovery, showCommands bool) {
	var rows [][]string
	for _, t := range d.Tools {
		if !t.Installed {
			rows = append(rows, []string{t.Name, "-", "not installed"})
			continue
		}
		at := t.PointedAt
		if t.Note != "" {
			at = strings.TrimPrefix(at+" ("+t.Note+")", " ")
		}
		rows = append(rows, []string{t.Name, orDash(t.Version), orDash(at)})
	}
	tableTo(out, "TOOL\tVERSION\tPOINTED AT", rows)

	fmt.Fprintln(out)
	if len(d.Found) == 0 {
		fmt.Fprintln(out, "Found nothing that could host sandboxes.")
	} else {
		rows = nil
		for _, f := range d.Found {
			rows = append(rows, []string{f.Kind, f.Name, orDash(f.Detail), f.Tool})
		}
		tableTo(out, "FOUND\tNAME\tDETAIL\tSEEN BY", rows)
	}

	fmt.Fprintln(out, "\nPossible from here:")
	for i, p := range d.Possible {
		fmt.Fprintf(out, "%d. %s\n   why:  %s\n   next: %s\n", i+1, p.What, p.Why, p.Next)
	}
	if showCommands {
		fmt.Fprintln(out, "\nCommands run (all read-only):")
		for _, c := range d.Commands {
			fmt.Fprintln(out, "  "+c)
		}
	} else {
		fmt.Fprintf(out, "\nRan %d read-only commands; --show-commands lists them.\n", len(d.Commands))
	}
}
