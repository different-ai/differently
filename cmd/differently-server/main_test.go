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
