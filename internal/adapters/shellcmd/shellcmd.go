// Package shellcmd builds POSIX shell scripts for backends whose only
// primitive is "run this command line": the exec request's working
// directory, environment, stdin and timeout become part of the script, and
// files move as base64 through the command line and its output. It assumes
// the image has sh, base64, mkdir, cat and (for timeouts) coreutils timeout.
package shellcmd

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/different-ai/differently/internal/adapters/httpx"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// EnvFile is where adapters keep a sandbox's environment, written at create
// and sourced by every Script.
const EnvFile = "/tmp/.differently/env"

// ChunkBytes is the most file data sent in one command. Linux limits one
// argument to 128 KiB and backends run the command as `sh -c <command>`.
const ChunkBytes = 64 << 10

// Exit codes the file scripts use to report a missing path or a directory.
const (
	exitMissing = 44
	exitIsDir   = 45
)

// Run executes one shell command line in the sandbox.
type Run func(ctx context.Context, command string) (api.ExecResult, error)

// q quotes one shell word.
func q(s string) string { return httpx.ShellQuote([]string{s}) }

// Script renders an exec request as one command line: it loads EnvFile,
// exports the request's env, changes directory, feeds stdin and applies the
// timeout (exit 124 when it fires), then runs the command under /bin/sh.
func Script(req api.ExecRequest) string {
	cmd := req.Command
	if cmd == "" {
		cmd = httpx.ShellQuote(req.Args)
	}
	var b strings.Builder
	b.WriteString("set -a; [ -r " + EnvFile + " ] && . " + EnvFile + "; set +a; ")
	keys := make([]string, 0, len(req.Env))
	for k := range req.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString("export " + k + "=" + q(req.Env[k]) + "; ")
	}
	if req.Cwd != "" {
		b.WriteString("cd " + q(req.Cwd) + " || exit 1; ")
	}
	run := "/bin/sh -c " + q(cmd)
	if req.TimeoutSeconds > 0 {
		run = "timeout -k 5 " + strconv.Itoa(req.TimeoutSeconds) + " " + run
	}
	if req.Stdin != "" {
		run = "printf %s " + q(base64.StdEncoding.EncodeToString([]byte(req.Stdin))) + " | base64 -d | " + run
	} else {
		run += " </dev/null"
	}
	b.WriteString(run)
	return b.String()
}

// EnvScript writes env to EnvFile (mode 0600) as shell assignments.
func EnvScript(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var lines strings.Builder
	for _, k := range keys {
		lines.WriteString(k + "=" + q(env[k]) + "\n")
	}
	dir := EnvFile[:strings.LastIndex(EnvFile, "/")]
	return "umask 077 && mkdir -p " + dir + " && printf %s " + q(base64.StdEncoding.EncodeToString([]byte(lines.String()))) + " | base64 -d > " + EnvFile
}

// fail turns a file script's exit status into an error.
func fail(path string, r api.ExecResult) error {
	switch r.ExitCode {
	case 0:
		return nil
	case exitMissing:
		return api.Errorf(api.CodeNotFound, "%s does not exist", path)
	case exitIsDir:
		return api.Errorf(api.CodeInvalidRequest, "%s is a directory", path)
	}
	return api.Errorf(api.CodeInvalidRequest, "%s: %s", path, strings.TrimSpace(r.Stderr))
}

// ReadFile returns a file's content, read as base64 through stdout.
func ReadFile(ctx context.Context, run Run, path string) ([]byte, error) {
	p := q(path)
	r, err := run(ctx, fmt.Sprintf("[ -e %s ] || exit %d; [ -d %s ] && exit %d; base64 < %s", p, exitMissing, p, exitIsDir, p))
	if err != nil {
		return nil, err
	}
	if err := fail(path, r); err != nil {
		return nil, err
	}
	data, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(r.Stdout), ""))
	if err != nil {
		return nil, adapter.SandboxFailed(fmt.Errorf("unreadable file content for %s: %w", path, err))
	}
	return data, nil
}

// WriteFile creates the parent directories and writes data in ChunkBytes
// pieces, so large files take several commands.
func WriteFile(ctx context.Context, run Run, path string, data []byte) error {
	p := q(path)
	first := true
	for first || len(data) > 0 {
		n := min(len(data), ChunkBytes)
		redirect := ">>"
		prefix := ""
		if first {
			redirect, prefix = ">", "mkdir -p \"$(dirname "+p+")\" && "
		}
		cmd := prefix + "printf %s " + q(base64.StdEncoding.EncodeToString(data[:n])) + " | base64 -d " + redirect + " " + p
		r, err := run(ctx, cmd)
		if err != nil {
			return err
		}
		if err := fail(path, r); err != nil {
			return err
		}
		data, first = data[n:], false
	}
	return nil
}

// ListFiles lists a directory's entries with GNU find, or names and types
// only with ls where find has no -printf (busybox, BSD).
func ListFiles(ctx context.Context, run Run, path string) ([]api.FileInfo, error) {
	p := q(path)
	r, err := run(ctx, fmt.Sprintf(`[ -e %s ] || exit %d; [ -d %s ] || exit 46; cd %s && { find . -mindepth 1 -maxdepth 1 -printf '%%y\t%%s\t%%T@\t%%m\t%%f\n' 2>/dev/null || ls -1Ap | awk '{ t = sub(/\/$/, "") ? "d" : "f"; print t "\t0\t0\t0\t" $0 }'; }`, p, exitMissing, p, p))
	if err != nil {
		return nil, err
	}
	if r.ExitCode == 46 {
		return nil, api.Errorf(api.CodeInvalidRequest, "%s is not a directory", path)
	}
	if err := fail(path, r); err != nil {
		return nil, err
	}
	base := strings.TrimRight(path, "/")
	var out []api.FileInfo
	for _, line := range strings.Split(strings.TrimSpace(r.Stdout), "\n") {
		f := strings.SplitN(line, "\t", 5)
		if len(f) != 5 {
			continue
		}
		size, _ := strconv.ParseInt(f[1], 10, 64)
		secs, _ := strconv.ParseFloat(f[2], 64)
		fi := api.FileInfo{Path: base + "/" + f[4], Name: f[4], Size: size, IsDir: f[0] == "d", Mode: f[3]}
		if secs > 0 {
			fi.ModTime = time.Unix(int64(secs), 0).UTC()
		}
		out = append(out, fi)
	}
	return out, nil
}
