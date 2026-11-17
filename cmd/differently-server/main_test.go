package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// The command dev mode prints must work as printed: the CLI finds a dev
// server only at the default address and data directory.
func TestDevWelcomeNamesACommandThatWorks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	def := filepath.Join(home, ".differently", "dev")

	plain := devWelcome(":8080", def)
	if !strings.Contains(plain, "  Try        differently run -- echo hello\n") {
		t.Errorf("default address and directory need no settings:\n%s", plain)
	}
	if !strings.Contains(plain, "http://localhost:8080/dashboard/") || !strings.Contains(plain, filepath.Join(def, "token")) {
		t.Errorf("the dashboard or the token path is missing:\n%s", plain)
	}

	moved := devWelcome("127.0.0.1:18080", def)
	if !strings.Contains(moved, "DIFFERENTLY_URL=http://127.0.0.1:18080 differently run") {
		t.Errorf("another address must be passed to the CLI:\n%s", moved)
	}

	elsewhere := devWelcome(":8080", filepath.Join(home, "data"))
	for _, want := range []string{"DIFFERENTLY_API_KEY=$(cat " + filepath.Join(home, "data", "token") + ")", "DIFFERENTLY_PROJECT=prj_default"} {
		if !strings.Contains(elsewhere, want) {
			t.Errorf("another data directory: want %q in\n%s", want, elsewhere)
		}
	}
}

// The demo keeps its own data directory, so the CLI always needs its token,
// and the console's dev server needs the address unless it is the default.
func TestDemoWelcomeNamesCommandsThatWork(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	token := filepath.Join(home, ".differently", "demo", "token")

	plain := demoWelcome(":8080", filepath.Dir(token))
	for _, want := range []string{
		"http://localhost:8080/demo/",
		"http://localhost:8080/dashboard/",
		"npm ci && npm run dev,",
		"http://localhost:4321/routing/",
		"  CLI        DIFFERENTLY_API_KEY=$(cat " + token + ") DIFFERENTLY_PROJECT=prj_default differently sandbox list\n",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("want %q in\n%s", want, plain)
		}
	}

	moved := demoWelcome("127.0.0.1:18090", filepath.Dir(token))
	for _, want := range []string{
		"DIFFERENTLY_URL=http://127.0.0.1:18090 npm run dev",
		"DIFFERENTLY_URL=http://127.0.0.1:18090 DIFFERENTLY_API_KEY=",
	} {
		if !strings.Contains(moved, want) {
			t.Errorf("another address: want %q in\n%s", want, moved)
		}
	}
}
