package main

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// fakeTools stands in for the tools on a machine: which are installed, and
// what each command line prints. A command line that is not listed fails.
func fakeTools(t *testing.T, installed []string, answers map[string]string) {
	t.Helper()
	run, look := toolRun, toolLook
	t.Cleanup(func() { toolRun, toolLook = run, look })
	toolLook = func(name string) (string, error) {
		for _, n := range installed {
			if n == name {
				return "/fake/" + name, nil
			}
		}
		return "", exec.ErrNotFound
	}
	toolRun = func(_ context.Context, name string, args ...string) (string, error) {
		line := name + " " + strings.Join(args, " ")
		out, ok := answers[line]
		if !ok {
			return "", errors.New("fake: no answer for " + line)
		}
		if strings.HasPrefix(out, "ERR:") {
			return "", errors.New(strings.TrimPrefix(out, "ERR:"))
		}
		return out, nil
	}
}

func whats(d discovery) string {
	var s []string
	for _, p := range d.Possible {
		s = append(s, p.What)
	}
	return strings.Join(s, " | ")
}

func TestDiscoverWithNoToolsPointsAtDevMode(t *testing.T) {
	fakeTools(t, nil, nil)
	d := discover(context.Background())
	if len(d.Tools) != 6 {
		t.Fatalf("want all six tools reported, got %d", len(d.Tools))
	}
	for _, tool := range d.Tools {
		if tool.Installed {
			t.Errorf("%s reported as installed", tool.Name)
		}
	}
	if len(d.Commands) != 0 {
		t.Errorf("ran commands for tools that are not installed: %v", d.Commands)
	}
	if got := whats(d); got != "Try Differently on this machine in dev mode" {
		t.Errorf("possible = %q", got)
	}
}

func TestDiscoverFindsGKEClusterAndWhatRunsOnIt(t *testing.T) {
	fakeTools(t, []string{"gcloud", "kubectl", "helm"}, map[string]string{
		"gcloud version --format=json":     `{"Google Cloud SDK": "567.0.0"}`,
		"gcloud config list --format=json": `{"core": {"account": "dev@example.com", "project": "acme"}}`,
		"gcloud container clusters list --project acme --format=json": `[
			{"name": "sandboxes", "location": "us-central1", "status": "RUNNING", "currentNodeCount": 3},
			{"name": "other", "location": "europe-west1", "status": "RUNNING", "autopilot": {"enabled": true}}]`,
		"kubectl version --client -o json":             `{"clientVersion": {"gitVersion": "v1.37.0"}}`,
		"kubectl config current-context":               "gke_acme_us-central1_sandboxes\n",
		"kubectl config get-contexts -o name":          "gke_acme_us-central1_sandboxes\nkind-dev\n",
		"kubectl --request-timeout=5s get crd -o name": "customresourcedefinition.apiextensions.k8s.io/sandboxes.agents.x-k8s.io\ncustomresourcedefinition.apiextensions.k8s.io/podsnapshots.podsnapshot.gke.io\n",
		"helm version --short":                         "v4.2.3+g43e8b7f\n",
	})
	d := discover(context.Background())

	tools := map[string]toolFinding{}
	for _, tool := range d.Tools {
		tools[tool.Name] = tool
	}
	if got := tools["gcloud"]; got.Version != "567.0.0" || got.PointedAt != "account dev@example.com, project acme" {
		t.Errorf("gcloud = %+v", got)
	}
	if got := tools["kubectl"]; got.Version != "1.37.0" || got.PointedAt != "context gke_acme_us-central1_sandboxes" {
		t.Errorf("kubectl = %+v", got)
	}
	if got := tools["helm"].Version; got != "4.2.3" {
		t.Errorf("helm version = %q", got)
	}
	if tools["docker"].Installed {
		t.Error("docker reported as installed")
	}

	var found []string
	for _, f := range d.Found {
		found = append(found, f.Kind+"="+f.Name+" ("+f.Detail+")")
	}
	for _, want := range []string{
		"GKE cluster=sandboxes (us-central1, 3 nodes, running)",
		"GKE cluster=other (europe-west1, Autopilot, running)",
		"Kubernetes context=gke_acme_us-central1_sandboxes (current)",
		"Kubernetes context=kind-dev ()",
		"Sandbox runtime=agent-sandbox (in context gke_acme_us-central1_sandboxes, with GKE Pod Snapshots)",
	} {
		if !strings.Contains(strings.Join(found, "\n"), want) {
			t.Errorf("found lacks %q in:\n%s", want, strings.Join(found, "\n"))
		}
	}

	// The cluster kubectl points at is described; the other one gets the
	// command that points kubectl at it. Nothing offers a Helm install on a
	// cluster that already has a runtime.
	got := whats(d)
	if want := "Look inside GKE cluster other | Register agent-sandbox on gke_acme_us-central1_sandboxes as a pool"; got != want {
		t.Errorf("possible = %q, want %q", got, want)
	}
	if next := d.Possible[0].Next; !strings.Contains(next, "gcloud container clusters get-credentials other --location europe-west1 --project acme") {
		t.Errorf("next = %q", next)
	}
	if p := d.Possible[1]; !strings.Contains(p.Why, "experimental") || !strings.Contains(p.Why, "not yet run on GKE") || !strings.HasSuffix(p.Next, "providers/gke-agent-sandbox/") {
		t.Errorf("agent-sandbox possibility = %+v", p)
	}
}

func TestDiscoverOpenSandboxClusterAndEmptyCluster(t *testing.T) {
	base := map[string]string{
		"kubectl version --client -o json":    `{"clientVersion": {"gitVersion": "v1.37.0"}}`,
		"kubectl config current-context":      "k3s\n",
		"kubectl config get-contexts -o name": "k3s\n",
	}
	base["kubectl --request-timeout=5s get crd -o name"] = "customresourcedefinition.apiextensions.k8s.io/batchsandboxes.sandbox.opensandbox.io\n"
	fakeTools(t, []string{"kubectl"}, base)
	if got := whats(discover(context.Background())); got != "Register the OpenSandbox on k3s as a private pool" {
		t.Errorf("with OpenSandbox: possible = %q", got)
	}

	base["kubectl --request-timeout=5s get crd -o name"] = "customresourcedefinition.apiextensions.k8s.io/certificates.cert-manager.io\n"
	fakeTools(t, []string{"kubectl"}, base)
	if got := whats(discover(context.Background())); got != "Install OpenSandbox and Differently on k3s with Helm" {
		t.Errorf("empty cluster: possible = %q", got)
	}
}

// A tool that is signed out, or a cluster that does not answer, is a note on
// its row. The rest of the report still comes out and nothing is offered on
// the strength of something that could not be read.
func TestDiscoverKeepsGoingWhenAToolFails(t *testing.T) {
	fakeTools(t, []string{"docker", "gcloud", "kubectl", "az"}, map[string]string{
		"docker --version":                                            "Docker version 27.3.1, build ce12230\n",
		"docker info --format {{.ServerVersion}}":                     "ERR:Cannot connect to the Docker daemon",
		"gcloud version --format=json":                                `{"Google Cloud SDK": "567.0.0"}`,
		"gcloud config list --format=json":                            `{"core": {"account": "dev@example.com", "project": "acme"}}`,
		"gcloud container clusters list --project acme --format=json": "ERR:ERROR: (gcloud.container.clusters.list) There was a problem refreshing your current auth tokens: Reauthentication failed.",
		"kubectl version --client -o json":                            `{"clientVersion": {"gitVersion": "v1.37.0"}}`,
		"kubectl config current-context":                              "old\n",
		"kubectl config get-contexts -o name":                         "old\n",
		"kubectl --request-timeout=5s get crd -o name":                "ERR:Unable to connect to the server: dial tcp 10.0.0.1:443: i/o timeout",
		"az version -o json":                                          `{"azure-cli": "2.91.0"}`,
		"az account show -o json":                                     "ERR:Please run 'az login' to setup account.",
	})
	d := discover(context.Background())
	notes := map[string]string{}
	for _, tool := range d.Tools {
		notes[tool.Name] = tool.Note
	}
	for name, want := range map[string]string{
		"docker":  "no Docker daemon answers",
		"gcloud":  "the sign-in has expired; run: gcloud auth login",
		"kubectl": "the cluster did not answer: Unable to connect",
		"az":      "not signed in; run: az login",
	} {
		if !strings.Contains(notes[name], want) {
			t.Errorf("%s note = %q, want it to contain %q", name, notes[name], want)
		}
	}
	if got := whats(d); got != "Try Differently on this machine in dev mode" {
		t.Errorf("possible = %q", got)
	}
}

func TestDiscoverCommandPrintsATableAndJSON(t *testing.T) {
	answers := map[string]string{
		"docker --version":                        "Docker version 27.3.1, build ce12230\n",
		"docker info --format {{.ServerVersion}}": "29.5.2\n",
	}
	fakeTools(t, []string{"docker"}, answers)
	out, _, err := runCLI(t, "", "discover", "--show-commands")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"docker   27.3.1   a running daemon, version 29.5.2",
		"gcloud   -        not installed",
		"1. Run the whole stack on this Docker host",
		"next: https://different-ai.github.io/differently/docs/deploy/docker/",
		"Commands run (all read-only):\n  docker --version\n  docker info --format {{.ServerVersion}}\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	fakeTools(t, []string{"docker"}, answers)
	out, _, err = runCLI(t, "", "discover", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var d discovery
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if len(d.Tools) != 6 || len(d.Found) != 1 || len(d.Possible) != 1 || len(d.Commands) != 2 {
		t.Errorf("json = %+v", d)
	}
}

// Every command discover can run only reads. This list is the whole set: a
// new command has to be added here, where a reviewer sees it.
func TestDiscoverRunsOnlyReadOnlyCommands(t *testing.T) {
	allowed := []string{
		"docker --version", "docker info ",
		"gcloud version ", "gcloud config list ", "gcloud container clusters list ",
		"kubectl version --client ", "kubectl config current-context", "kubectl config get-contexts ", "kubectl --request-timeout=5s get crd ",
		"helm version ", "az version ", "az account show ", "aws --version",
	}
	run, look := toolRun, toolLook
	t.Cleanup(func() { toolRun, toolLook = run, look })
	toolLook = func(name string) (string, error) { return "/fake/" + name, nil }
	toolRun = func(_ context.Context, name string, args ...string) (string, error) {
		line := name + " " + strings.Join(args, " ")
		switch {
		case strings.HasPrefix(line, "gcloud config list"):
			return `{"core": {"account": "dev@example.com", "project": "acme"}}`, nil
		case strings.HasPrefix(line, "gcloud container clusters list"):
			return `[{"name": "c", "location": "l", "status": "RUNNING"}]`, nil
		case line == "kubectl config current-context":
			return "ctx\n", nil
		case strings.HasPrefix(line, "docker info"):
			return "29.5.2\n", nil
		}
		return "{}", nil
	}
	for _, line := range discover(context.Background()).Commands {
		ok := false
		for _, a := range allowed {
			ok = ok || strings.HasPrefix(line+" ", a) || line == a
		}
		if !ok {
			t.Errorf("discover ran a command that is not on the read-only list: %s", line)
		}
	}
}
