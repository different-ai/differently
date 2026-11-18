package main

import (
	"io"
	"reflect"
	"testing"

	"github.com/different-ai/differently/pkg/api"
)

// The capabilities table has one column per pool, so a feature one backend
// lacks shows as a dash beside the others' yes.
func TestCapabilityRowsPutPoolsSideBySide(t *testing.T) {
	header, rows := capabilityRows([]poolView{
		{Pool: "onprem", Adapter: "opensandbox", Trust: "private", Enabled: true, Health: "healthy", Capabilities: []string{"exec", "snapshots.disk"}},
		{Pool: "freestyle", Adapter: "freestyle", Trust: "external", Enabled: false, Health: "healthy", Capabilities: []string{"exec", "snapshots.memory"}},
	})
	if !reflect.DeepEqual(header, []string{"", "onprem", "freestyle"}) {
		t.Fatalf("header %v", header)
	}
	want := [][]string{
		{"adapter", "opensandbox", "freestyle"},
		{"trust", "private", "external"},
		{"status", "healthy", "not taking new work"},
		{"exec", "yes", "yes"},
		{"snapshots.disk", "yes", "-"},
		{"snapshots.memory", "-", "yes"},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows\n%v\nwant\n%v", rows, want)
	}
}

// The templates table shows what each pool boots for a template: its own
// image where one is mapped, a dash where the pool cannot run it, and the
// name itself for a template that maps nothing.
func TestTemplateRowsShowEachPoolsImage(t *testing.T) {
	header, rows := templateRows([]api.Template{
		{Name: "python", Description: "Python 3.12", Images: map[string]string{"onprem": "registry.example.com/python:3.12", "freestyle": "python-312"}},
		{Name: "node", Images: map[string]string{"onprem": "registry.example.com/node:22"}},
		{Name: "ubuntu:24.04"},
		{Name: "gpu", Pools: []string{"lab"}},
	}, []string{"onprem", "daytona"})
	// freestyle and lab are named by templates but not usable by this key:
	// they still get a column, after the usable pools.
	if !reflect.DeepEqual(header, []string{"TEMPLATE", "onprem", "daytona", "freestyle", "lab", "DESCRIPTION"}) {
		t.Fatalf("header %v", header)
	}
	want := [][]string{
		{"python", "registry.example.com/python:3.12", "-", "python-312", "-", "Python 3.12"},
		{"node", "registry.example.com/node:22", "-", "-", "-", ""},
		{"ubuntu:24.04", "ubuntu:24.04", "ubuntu:24.04", "ubuntu:24.04", "ubuntu:24.04", ""},
		{"gpu", "-", "-", "-", "gpu", ""},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("rows\n%v\nwant\n%v", rows, want)
	}
}

// Starting from a snapshot is a create like any other: the flag reaches the
// request, for a kept sandbox and for a one-command run.
func TestFromSnapshotReachesTheRequest(t *testing.T) {
	f := &fakeAPI{status: api.StatusRunning}
	if _, err := runOnce(t.Context(), f.serve(t), api.CreateSandboxRequest{FromSnapshot: "snp_1"}, execRequest([]string{"true"}, "", nil), false, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if f.created.FromSnapshot != "snp_1" {
		t.Fatalf("create request %+v lost the snapshot", f.created)
	}
	for _, cmd := range []string{"create", "run"} {
		c, _, err := sandboxCmd().Find([]string{cmd})
		if err != nil || c.Flags().Lookup("from-snapshot") == nil {
			t.Errorf("sandbox %s has no --from-snapshot flag (%v)", cmd, err)
		}
	}
}
