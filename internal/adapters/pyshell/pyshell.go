// Package pyshell runs Differently exec requests on code-interpreter
// backends that execute Python rather than shell commands. The request
// becomes a short Python program that runs the command with subprocess,
// writes one marked line with the exit code to stderr first, then passes
// the command's stdout and stderr through as text. The exit code comes
// first so a backend that cuts long output at the end still reports it.
package pyshell

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// marker prefixes the exit-code line.
const marker = "__differently_exit__"

// program reads the request from a base64 JSON literal, so no part of it
// needs Python string escaping. A missing command or cwd is exit 127, a
// timeout exit 124 with the output so far, a signal 128+N, as in a shell.
const program = `import base64, json, os, subprocess, sys
s = json.loads(base64.b64decode("%s"))
env = dict(os.environ)
if s["envFile"]:
    try:
        with open(s["envFile"]) as f:
            env.update(json.load(f))
    except OSError:
        pass
env.update(s["env"])
argv = s["args"] or ["/bin/sh", "-c", s["command"]]
try:
    p = subprocess.run(argv, input=s["stdin"].encode(), cwd=s["cwd"] or None, env=env, capture_output=True, timeout=s["timeout"] or None)
    code, out, err = (p.returncode if p.returncode >= 0 else 128 - p.returncode), p.stdout, p.stderr
except subprocess.TimeoutExpired as e:
    code, out, err = 124, e.stdout or b"", e.stderr or b""
except OSError as e:
    code, out, err = 127, b"", (str(e) + "\n").encode()
sys.stderr.write("` + marker + `%%d\n" %% code)
sys.stderr.flush()
sys.stdout.write(out.decode("utf-8", "replace"))
sys.stdout.flush()
sys.stderr.write(err.decode("utf-8", "replace"))
sys.stderr.flush()
`

// Program returns Python source that runs req. envFile, when set, is a JSON
// object of environment variables in the sandbox, merged in before req.Env
// (see EnvProgram).
func Program(req api.ExecRequest, envFile string) string {
	env := req.Env
	if env == nil {
		env = map[string]string{}
	}
	b, _ := json.Marshal(map[string]any{
		"command": req.Command, "args": req.Args, "env": env, "cwd": req.Cwd,
		"stdin": req.Stdin, "timeout": req.TimeoutSeconds, "envFile": envFile,
	})
	return fmt.Sprintf(program, base64.StdEncoding.EncodeToString(b))
}

// EnvProgram returns Python source that stores env as the JSON file path
// (mode 0600), for Program to merge into every later command.
func EnvProgram(path string, env map[string]string) string {
	b, _ := json.Marshal(map[string]any{"path": path, "env": env})
	return fmt.Sprintf(`import base64, json, os
s = json.loads(base64.b64decode("%s"))
os.makedirs(os.path.dirname(s["path"]), exist_ok=True)
fd = os.open(s["path"], os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
with os.fdopen(fd, "w") as f:
    json.dump(s["env"], f)
`, base64.StdEncoding.EncodeToString(b))
}

// Parse takes the interpreter's stdout and stderr for a Program run and
// returns the command's result. Without the exit-code line the program never
// ran to the end (the interpreter's own timeout, or a crash), which is
// sandbox_failed with what the backend said.
func Parse(stdout, stderr string) (api.ExecResult, error) {
	i := strings.Index(stderr, marker)
	if i < 0 {
		msg := strings.TrimSpace(stderr)
		if msg == "" {
			msg = "no exit status"
		}
		if len(msg) > 500 {
			msg = msg[:500] + "…"
		}
		return api.ExecResult{}, adapter.SandboxFailed(fmt.Errorf("the command did not finish: %s", msg))
	}
	rest := stderr[i+len(marker):]
	line, after, _ := strings.Cut(rest, "\n")
	code, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		return api.ExecResult{}, adapter.SandboxFailed(fmt.Errorf("unreadable exit status %q", line))
	}
	return api.ExecResult{ExitCode: code, Stdout: stdout, Stderr: stderr[:i] + after}, nil
}
