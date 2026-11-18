package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/different-ai/differently/pkg/api"
)

// fakeTemplates is the admin template API as the commands use it: it merges
// like the server, records every call and its body, and refuses the two
// things the server refuses that an operator can run into.
type fakeTemplates struct {
	mu        sync.Mutex
	calls     []string
	bodies    []string
	templates map[string]api.Template
	inConfig  []api.Template // what the server's config file declares
}

func (f *fakeTemplates) start(t *testing.T) string {
	t.Helper()
	if f.templates == nil {
		f.templates = map[string]api.Template{}
	}
	fail := func(w http.ResponseWriter, e *api.Error) {
		w.WriteHeader(e.HTTPStatus())
		_ = json.NewEncoder(w).Encode(api.ErrorEnvelope{Error: e})
	}
	record := func(r *http.Request) []byte {
		b, _ := io.ReadAll(r.Body)
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		f.bodies = append(f.bodies, string(b))
		return b
	}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/admin/templates/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var req api.PutTemplateRequest
		_ = json.Unmarshal(record(r), &req)
		name := r.PathValue("name")
		if name == "workbot" {
			fail(w, api.Errorf(api.CodeConflict, "template workbot is declared in the config file; change it there"))
			return
		}
		tpl, ok := f.templates[name]
		if !ok {
			tpl = api.Template{Name: name, Source: api.TemplateSourceAPI, Images: map[string]string{}}
			w.WriteHeader(http.StatusCreated)
		}
		if req.Description != nil {
			tpl.Description = *req.Description
		}
		if req.Pools != nil {
			tpl.Pools = *req.Pools
		}
		for pool, image := range req.Images {
			tpl.Images[pool] = image
		}
		f.templates[name] = tpl
		_ = json.NewEncoder(w).Encode(tpl)
	})
	mux.HandleFunc("DELETE /v1/admin/templates/{name}/images/{pool}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		record(r)
		tpl := f.templates[r.PathValue("name")]
		if len(tpl.Images) == 1 {
			fail(w, api.Errorf(api.CodeInvalidState, "pool %s holds the last image of template %s", r.PathValue("pool"), tpl.Name))
			return
		}
		delete(tpl.Images, r.PathValue("pool"))
		_ = json.NewEncoder(w).Encode(tpl)
	})
	mux.HandleFunc("DELETE /v1/admin/templates/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		record(r)
		delete(f.templates, r.PathValue("name"))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /v1/templates", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		list := api.List[api.Template]{Items: append([]api.Template{}, f.inConfig...)}
		for _, tpl := range f.templates {
			if !slices.ContainsFunc(f.inConfig, func(c api.Template) bool { return c.Name == tpl.Name }) {
				list.Items = append(list.Items, tpl)
			}
		}
		_ = json.NewEncoder(w).Encode(list)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// admin runs "differently admin ARGS" against the fake and returns what it
// printed.
func (f *fakeTemplates) admin(t *testing.T, args ...string) (string, error) {
	t.Helper()
	t.Setenv("DIFFERENTLY_URL", f.start(t))
	t.Setenv("DIFFERENTLY_API_KEY", "dly_test_key")
	t.Setenv("HOME", t.TempDir()) // no saved login, no dev token
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cmd := adminCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.Execute()
	return out.String(), err
}

// The commands an operator types: one name on two pools, a third pool later,
// one pool taken away, then the template itself.
func TestAdminTemplateSetAndRemove(t *testing.T) {
	f := &fakeTemplates{}
	out, err := f.admin(t, "template", "set", "python",
		"--image", "opensandbox=registry.example.com/python:3.12", "--image", "daytona=python-312", "--description", "Python 3.12 with our tools")
	if err != nil {
		t.Fatal(err)
	}
	var sent map[string]any
	_ = json.Unmarshal([]byte(f.bodies[0]), &sent)
	want := map[string]any{"description": "Python 3.12 with our tools", "images": map[string]any{"opensandbox": "registry.example.com/python:3.12", "daytona": "python-312"}}
	if f.calls[0] != "PUT /v1/admin/templates/python" || !reflect.DeepEqual(sent, want) {
		t.Fatalf("call %s sent %s", f.calls[0], f.bodies[0])
	}
	for _, line := range []string{"template python set; it applies from the next request, with no restart", "daytona      python-312", "opensandbox  registry.example.com/python:3.12", "placed only on a pool listed here"} {
		if !strings.Contains(out, line) {
			t.Errorf("output lacks %q:\n%s", line, out)
		}
	}

	// A pool later: only that pool is sent, so nothing else can change, and
	// the description is not sent as empty.
	out, err = f.admin(t, "template", "set", "python", "--image", "freestyle=python-312-fs")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.bodies[len(f.bodies)-1]; got != `{"images":{"freestyle":"python-312-fs"}}` {
		t.Fatalf("adding a pool sent %s", got)
	}
	if !strings.Contains(out, "freestyle    python-312-fs") || !strings.Contains(out, "daytona      python-312") {
		t.Errorf("output does not show all three pools:\n%s", out)
	}

	out, err = f.admin(t, "template", "remove", "python", "--pool", "daytona")
	if err != nil {
		t.Fatal(err)
	}
	if last := f.calls[len(f.calls)-1]; last != "DELETE /v1/admin/templates/python/images/daytona" {
		t.Fatalf("removing a pool's image called %s", last)
	}
	if !strings.Contains(out, "template python is no longer offered on daytona") || strings.Contains(out, "daytona      ") {
		t.Errorf("output after removing daytona:\n%s", out)
	}

	out, err = f.admin(t, "template", "remove", "python")
	if err != nil {
		t.Fatal(err)
	}
	if last := f.calls[len(f.calls)-1]; last != "DELETE /v1/admin/templates/python" {
		t.Fatalf("removing the template called %s", last)
	}
	if !strings.Contains(out, "template python removed") || !strings.Contains(out, "passed to the backend as given") {
		t.Errorf("output after removing the template:\n%s", out)
	}
}

// What the server refuses reaches the operator in the server's words, and a
// flag the CLI can check itself never reaches the server.
func TestAdminTemplateRefusals(t *testing.T) {
	f := &fakeTemplates{templates: map[string]api.Template{"node": {Name: "node", Images: map[string]string{"onprem": "node:22"}}}}
	if _, err := f.admin(t, "template", "set", "workbot", "--image", "daytona=x"); !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), "declared in the config file") {
		t.Errorf("a config-declared template: %v", err)
	}
	if _, err := f.admin(t, "template", "remove", "node", "--pool", "onprem"); !api.IsCode(err, api.CodeInvalidState) || !strings.Contains(err.Error(), "last image") {
		t.Errorf("the last image: %v", err)
	}
	calls := len(f.calls)
	for _, args := range [][]string{
		{"template", "set", "python"},
		{"template", "set", "python", "--image", "daytona"},
		{"template", "set", "python", "--image", "=python-312"},
		{"template", "set", "python", "--image", "daytona="},
		{"template", "set", "python", "--image", "daytona=a", "--image", "daytona=b"},
		{"template", "set"},
		{"template", "remove", "python", "--pool", ""},
	} {
		if _, err := f.admin(t, args...); err == nil {
			t.Errorf("differently admin %s was accepted", strings.Join(args, " "))
		}
	}
	if len(f.calls) != calls {
		t.Errorf("an invalid command reached the server: %v", f.calls[calls:])
	}
}

func TestParseImages(t *testing.T) {
	got, err := parseImages([]string{"onprem=registry.example.com/python@sha256:abc=", " daytona = python-312 "})
	want := map[string]string{"onprem": "registry.example.com/python@sha256:abc=", "daytona": "python-312"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v %v", got, err)
	}
}

// --pools replaces the list, and an empty value clears it; -o json prints
// the template as the API returned it.
func TestAdminTemplatePoolsAndJSON(t *testing.T) {
	f := &fakeTemplates{}
	if _, err := f.admin(t, "template", "set", "gpu", "--pools", "lab,onprem"); err != nil {
		t.Fatal(err)
	}
	if got := f.bodies[0]; got != `{"pools":["lab","onprem"]}` {
		t.Fatalf("--pools sent %s", got)
	}
	if _, err := f.admin(t, "template", "set", "gpu", "--pools", ""); err != nil {
		t.Fatal(err)
	}
	if got := f.bodies[1]; got != `{"pools":[]}` {
		t.Fatalf(`--pools "" sent %s`, got)
	}
	flagOutput = "json"
	t.Cleanup(func() { flagOutput = "" })
	out, err := f.admin(t, "template", "set", "gpu", "--image", "lab=gpu-image")
	var tpl api.Template
	if err != nil || json.Unmarshal([]byte(out), &tpl) != nil || tpl.Images["lab"] != "gpu-image" {
		t.Fatalf("-o json printed %q (%v)", out, err)
	}
}

// Removing a name the config file also declares clears only the stored copy,
// and the command says so instead of claiming the template is gone.
func TestAdminTemplateRemoveSaysWhenTheConfigFileKeepsIt(t *testing.T) {
	f := &fakeTemplates{
		templates: map[string]api.Template{"workbot": {Name: "workbot", Source: api.TemplateSourceAPI, Images: map[string]string{"burst": "old"}}},
		inConfig:  []api.Template{{Name: "workbot", Source: api.TemplateSourceConfig, Images: map[string]string{"onprem": "workbot-v3"}}},
	}
	out, err := f.admin(t, "template", "remove", "workbot")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "the stored copy of template workbot was removed") || !strings.Contains(out, "stays in force") || strings.Contains(out, "passed to the backend as given") {
		t.Fatalf("output:\n%s", out)
	}
}
