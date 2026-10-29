package shellcmd

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/different-ai/differently/pkg/api"
)

// local runs scripts with the host's /bin/sh, the way a backend would run
// `sh -c <command>`. envFile replaces EnvFile so tests stay in a temp dir.
func local(t *testing.T, envFile string) Run {
	return func(ctx context.Context, command string) (api.ExecResult, error) {
		command = strings.ReplaceAll(command, EnvFile, envFile)
		command = strings.ReplaceAll(command, filepath.Dir(EnvFile), filepath.Dir(envFile))
		var out, errb bytes.Buffer
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		var ee *exec.ExitError
		if err != nil && !errors.As(err, &ee) {
			return api.ExecResult{}, err
		}
		return api.ExecResult{ExitCode: cmd.ProcessState.ExitCode(), Stdout: out.String(), Stderr: errb.String()}, nil
	}
}

func TestScriptRunsWithEnvCwdAndStdin(t *testing.T) {
	dir := t.TempDir()
	run := local(t, filepath.Join(dir, "env", "env"))
	ctx := context.Background()
	if r, err := run(ctx, EnvScript(map[string]string{"GREETING": "it's here", "B": "x y"})); err != nil || r.ExitCode != 0 {
		t.Fatalf("env script %+v %v", r, err)
	}
	r, err := run(ctx, Script(api.ExecRequest{
		Command: `printf '%s|%s|%s|' "$GREETING" "$B" "$EXTRA"; pwd; cat; exit 3`,
		Env:     map[string]string{"EXTRA": "$(not expanded)"}, Cwd: dir, Stdin: "line one\nline two",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := "it's here|x y|$(not expanded)|"
	if r.ExitCode != 3 || !strings.HasPrefix(r.Stdout, want) || !strings.HasSuffix(r.Stdout, "line one\nline two") || !strings.Contains(r.Stdout, filepath.Base(dir)) {
		t.Fatalf("result %+v", r)
	}
	r, _ = run(ctx, Script(api.ExecRequest{Args: []string{"printf", "%s", "a b;c"}}))
	if r.Stdout != "a b;c" {
		t.Fatalf("args are passed without a shell: %q", r.Stdout)
	}
	if s := Script(api.ExecRequest{Command: "sleep 9", TimeoutSeconds: 5}); !strings.Contains(s, "timeout -k 5 5 /bin/sh -c") {
		t.Fatalf("timeout missing: %s", s)
	}
}

func TestFilesOverCommands(t *testing.T) {
	dir := t.TempDir()
	run := local(t, filepath.Join(dir, "env"))
	ctx := context.Background()
	big := bytes.Repeat([]byte("0123456789abcdef\x00\xff"), ChunkBytes/8) // just over 2 chunks, binary
	path := filepath.Join(dir, "a b", "c.bin")
	var calls int
	counting := func(ctx context.Context, c string) (api.ExecResult, error) { calls++; return run(ctx, c) }
	if err := WriteFile(ctx, counting, path, big); err != nil {
		t.Fatal(err)
	}
	if want := (len(big) + ChunkBytes - 1) / ChunkBytes; want != 3 || calls != want {
		t.Fatalf("expected 3 chunks, sent %d", calls)
	}
	got, err := ReadFile(ctx, run, path)
	if err != nil || !bytes.Equal(got, big) {
		t.Fatalf("read back %d bytes (want %d): %v", len(got), len(big), err)
	}
	if err := WriteFile(ctx, run, filepath.Join(dir, "empty"), nil); err != nil {
		t.Fatal(err)
	}
	if b, err := ReadFile(ctx, run, filepath.Join(dir, "empty")); err != nil || len(b) != 0 {
		t.Fatalf("empty file %q %v", b, err)
	}
	if _, err := ReadFile(ctx, run, filepath.Join(dir, "missing")); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("missing file: %v", err)
	}
	if _, err := ReadFile(ctx, run, dir); !api.IsCode(err, api.CodeInvalidRequest) {
		t.Fatalf("directory: %v", err)
	}
	list, err := ListFiles(ctx, run, dir)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]api.FileInfo{}
	for _, f := range list {
		names[f.Name] = f
	}
	if !names["a b"].IsDir || names["empty"].IsDir || names["a b"].Path != dir+"/a b" {
		t.Fatalf("listing %+v", list)
	}
	if _, err := ListFiles(ctx, run, filepath.Join(dir, "nope")); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("missing dir: %v", err)
	}
}
