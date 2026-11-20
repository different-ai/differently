package acid

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// Level 1: run a command and get back exactly what it did.
func level1() []Test {
	return []Test{
		{ID: "L1.01", Level: 1, Title: "Exit codes", Feature: adapter.FeatureExec, Run: exitCodes},
		{ID: "L1.02", Level: 1, Title: "Separate streams", Feature: adapter.FeatureExec, Run: separateStreams},
		{ID: "L1.03", Level: 1, Title: "cwd, env and argv", Feature: adapter.FeatureExec, Run: cwdEnvArgv},
		{ID: "L1.04", Level: 1, Title: "Timeout", Feature: adapter.FeatureExec, Run: timeout},
		{ID: "L1.05", Level: 1, Title: "Bytes, not text", Feature: adapter.FeatureExec, Run: exactBytes},
		{ID: "L1.06", Level: 1, Title: "Large output", Feature: adapter.FeatureExec, Run: largeOutput},
		{ID: "L1.07", Level: 1, Title: "Long command", Feature: adapter.FeatureExec, Run: longCommand},
		{ID: "L1.08", Level: 1, Title: "stdin", Feature: adapter.FeatureExec, Run: stdin},
	}
}

func exitCodes(t *T) {
	ref := t.Shared()
	for _, c := range []struct {
		cmd  string
		want int
	}{{"true", 0}, {"false", 1}, {"exit 3", 3}, {"exit 255", 255}} {
		res := t.Exec(ref, api.ExecRequest{Command: c.cmd, TimeoutSeconds: 60})
		if res.ExitCode != c.want {
			t.Fail("%q returned exit code %d, want %d", c.cmd, res.ExitCode, c.want)
		}
	}
	// Exec round trip for a command that does nothing.
	var rtt []float64
	for range 5 {
		start := time.Now()
		t.Exec(ref, api.ExecRequest{Command: "true", TimeoutSeconds: 60})
		rtt = append(rtt, time.Since(start).Seconds())
	}
	slices.Sort(rtt)
	t.Measure("execRoundTripP50Seconds", rtt[len(rtt)/2])
	t.Note("exit codes 0, 1, 3 and 255 come back as results; exec round trip p50 %.0f ms", rtt[len(rtt)/2]*1000)
}

func separateStreams(t *T) {
	res := t.Exec(t.Shared(), api.ExecRequest{Command: "printf acid-out; printf acid-err >&2", TimeoutSeconds: 60})
	switch {
	case res.Stdout == "acid-out" && res.Stderr == "acid-err":
		t.Note("stdout and stderr arrive separately")
	case strings.Contains(res.Stdout, "acid-err"):
		t.Fail("stderr is mixed into stdout (%s)", clip(res.Stdout))
	default:
		t.Fail("stdout %s, stderr %s; want \"acid-out\" and \"acid-err\"", clip(res.Stdout), clip(res.Stderr))
	}
}

func cwdEnvArgv(t *T) {
	ref := t.Shared()
	t.Sh(ref, "mkdir -p /tmp/acid-cwd")
	value := t.Name("v") + " with spaces $HOME"
	res := t.Exec(ref, api.ExecRequest{
		Command: `pwd; printf '%s' "$ACID_VALUE"`, Cwd: "/tmp/acid-cwd",
		Env: map[string]string{"ACID_VALUE": value}, TimeoutSeconds: 60,
	})
	dir, got, _ := strings.Cut(res.Stdout, "\n")
	if dir != "/tmp/acid-cwd" {
		t.Fail("with cwd /tmp/acid-cwd, pwd prints %s", clip(dir))
	}
	if got != value {
		t.Fail("env ACID_VALUE arrives as %s, want %s", clip(got), clip(value))
	}
	// argv is passed as is: no shell splitting or expansion.
	arg := "a b  $HOME 'q' \"d\""
	res = t.Exec(ref, api.ExecRequest{Args: []string{"printf", "%s", arg}, TimeoutSeconds: 60})
	if res.Stdout != arg {
		t.Fail("argv %s arrives as %s", clip(arg), clip(res.Stdout))
	}
	t.Note("cwd, env and argv are honoured exactly")
}

func timeout(t *T) {
	ref := t.Shared()
	start := time.Now()
	res, err := t.Adapter().Exec(t.Ctx(), ref, api.ExecRequest{Command: "sleep 60; echo late", TimeoutSeconds: 2})
	took := time.Since(start).Seconds()
	t.Measure("timeoutReturnSeconds", took)
	switch {
	case took > 20:
		t.Fail("a 2s timeout returned after %.0fs", took)
	case err == nil && res.ExitCode == 0:
		t.Fail("a command past its timeout reported success (%s)", clip(res.Stdout))
	case strings.Contains(res.Stdout, "late"):
		t.Fail("the command kept running past its timeout (%s)", clip(res.Stdout))
	}
	if res := t.Exec(ref, api.ExecRequest{Command: "echo ok", TimeoutSeconds: 60}); strings.TrimSpace(res.Stdout) != "ok" {
		t.Fail("the sandbox does not answer after a timeout (%s)", clip(res.Stdout))
	}
	how := "exit code " + strconv.Itoa(res.ExitCode)
	if err != nil {
		how = describe(err)
	}
	t.Note("stopped after %.1fs (%s); the sandbox still answers", took, how)
}

func exactBytes(t *T) {
	// NUL, control bytes and multi-byte UTF-8. Invalid UTF-8 cannot cross
	// the JSON contract, so it is not asked for.
	res := t.Exec(t.Shared(), api.ExecRequest{Command: `printf 'a\000b\001\177 h\303\251llo \342\234\223 \344\270\226\r\n'`, TimeoutSeconds: 60})
	want := "a\x00b\x01\x7f héllo ✓ 世\r\n"
	if res.Stdout != want {
		t.Fail("output arrives as %s, want %s", clip(res.Stdout), clip(want))
	}
	t.Note("NUL, control bytes, CRLF and UTF-8 arrive unchanged")
}

func largeOutput(t *T) {
	n := t.Options().LargeOutputBytes
	start := time.Now()
	res := t.Exec(t.Shared(), api.ExecRequest{Command: "head -c " + strconv.Itoa(n) + " /dev/zero | tr '\\000' a", TimeoutSeconds: 300})
	t.Measure("largeOutputSeconds", time.Since(start).Seconds())
	got := len(res.Stdout)
	switch {
	case got == n && strings.Count(res.Stdout, "a") == n:
		t.Note("%d bytes arrive complete in %.1fs", n, time.Since(start).Seconds())
	case got < n && res.Truncated:
		t.Note("cut at %d of %d bytes, and flagged as truncated", got, n)
	case got < n:
		t.Dishonest("only %d of %d bytes arrive, and the result does not say it was truncated", got, n)
	default:
		t.Fail("%d bytes arrive for %d sent, or the content changed", got, n)
	}
}

func longCommand(t *T) {
	d := t.Options().LongCommand
	secs := int(d.Seconds())
	start := time.Now()
	res := t.Exec(t.Shared(), api.ExecRequest{Command: "sleep " + strconv.Itoa(secs) + "; echo done", TimeoutSeconds: secs + 120})
	took := time.Since(start)
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "done" {
		t.Fail("a %ds command returned exit %d, %s", secs, res.ExitCode, clip(res.Stdout))
	}
	if took < d {
		t.Fail("a %ds command returned after %.1fs", secs, took.Seconds())
	}
	t.Note("a %ds command completes", secs)
}

func stdin(t *T) {
	input := t.Name("acid-stdin") + "\nsecond line\n"
	res, err := t.Adapter().Exec(t.Ctx(), t.Shared(), api.ExecRequest{Command: "cat", Stdin: input, TimeoutSeconds: 60})
	switch {
	case api.IsCode(err, api.CodeUnsupportedOperation):
		t.Unsupported("stdin answers unsupported_operation")
	case err != nil:
		t.Fail("exec with stdin: %s", describe(err))
	case res.Stdout == input:
		t.Note("stdin reaches the command")
	case res.Stdout == "" && res.ExitCode == 0:
		t.Dishonest("stdin was accepted and silently dropped")
	default:
		t.Fail("cat echoed %s for stdin %s", clip(res.Stdout), clip(input))
	}
}
