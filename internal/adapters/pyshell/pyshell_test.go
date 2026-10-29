package pyshell

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// python runs a program the way a code interpreter would, if python3 is
// installed; the parsing tests below need no interpreter.
func python(t *testing.T, src string) (string, string) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	var out, errb bytes.Buffer
	cmd := exec.Command(py, "-I", "-c", src)
	cmd.Stdout, cmd.Stderr = &out, &errb
	_ = cmd.Run()
	return out.String(), errb.String()
}

func TestProgramRunsCommands(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "d", "env.json")
	if _, e := python(t, EnvProgram(envFile, map[string]string{"FROM_FILE": "yes", "OVERRIDE": "file"})); e != "" {
		t.Fatalf("env program: %s", e)
	}
	stdout, stderr := python(t, Program(api.ExecRequest{
		Command: `printf '%s %s %s ' "$FROM_FILE" "$OVERRIDE" "$(pwd)"; cat; echo oops >&2; exit 7`,
		Env:     map[string]string{"OVERRIDE": "req \"quoted\""}, Cwd: dir, Stdin: "in\n",
	}, envFile))
	r, err := Parse(stdout, stderr)
	if err != nil {
		t.Fatal(err)
	}
	if r.ExitCode != 7 || !strings.HasPrefix(r.Stdout, `yes req "quoted" `) || !strings.HasSuffix(r.Stdout, "in\n") || r.Stderr != "oops\n" {
		t.Fatalf("result %+v", r)
	}
	stdout, stderr = python(t, Program(api.ExecRequest{Args: []string{"sleep", "5"}, TimeoutSeconds: 1}, ""))
	if r, err := Parse(stdout, stderr); err != nil || r.ExitCode != 124 {
		t.Fatalf("timeout %+v %v", r, err)
	}
	stdout, stderr = python(t, Program(api.ExecRequest{Args: []string{"/no/such/binary"}}, ""))
	if r, err := Parse(stdout, stderr); err != nil || r.ExitCode != 127 {
		t.Fatalf("missing binary %+v %v", r, err)
	}
}

func TestParse(t *testing.T) {
	r, err := Parse("out", "before\n"+marker+"0\nafter")
	if err != nil || r.ExitCode != 0 || r.Stdout != "out" || r.Stderr != "before\nafter" {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := Parse("", "Traceback: KeyboardInterrupt"); !api.IsCode(err, adapter.CodeSandboxFailed) || !strings.Contains(err.Error(), "KeyboardInterrupt") {
		t.Fatalf("no marker: %v", err)
	}
	if _, err := Parse("", marker+"x\n"); !api.IsCode(err, adapter.CodeSandboxFailed) {
		t.Fatalf("bad marker: %v", err)
	}
}
