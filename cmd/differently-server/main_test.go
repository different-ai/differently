package main

import (
	"os"
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

// --dev takes effect before the config file is validated, so a file with
// devhost and fake pools runs under the flag without saying dev: true. The
// same file is still refused without the flag, by serve, migrate and
// preflight alike.
func TestDevFlagAllowsDevAdaptersInAConfigFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	pools := "pools:\n  - {name: local, adapter: devhost}\n  - {name: sim, adapter: fake, trust: external}\n"
	path := write("pools.yaml", "store: {driver: memory}\n"+pools)
	dataDir := filepath.Join(dir, "data")

	cfg, err := loadConfig(path, true, dataDir)
	if err != nil {
		t.Fatalf("--dev with a devhost pool: %v", err)
	}
	if !cfg.Dev || cfg.DataDir != dataDir || len(cfg.Pools) != 2 {
		t.Fatalf("--dev and --data-dir not applied: dev=%v dataDir=%q pools=%d", cfg.Dev, cfg.DataDir, len(cfg.Pools))
	}

	const refused = "the devhost adapter has no isolation"
	for name, load := range map[string]func() error{
		"serve":     func() error { _, err := loadConfig(path, false, dataDir); return err },
		"migrate":   func() error { return migrate(path) },
		"preflight": func() error { return preflight(path) },
	} {
		if err := load(); err == nil || !strings.Contains(err.Error(), refused) {
			t.Errorf("%s without --dev or dev: true: want %q, got %v", name, refused, err)
		}
	}

	inFile := write("dev.yaml", "dev: true\nstore: {driver: memory}\n"+pools)
	if cfg, err := loadConfig(inFile, false, ""); err != nil || !cfg.Dev {
		t.Fatalf("dev: true in the file: dev=%v err=%v", cfg.Dev, err)
	}
}
