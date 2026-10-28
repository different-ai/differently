// Package localexec runs commands and file operations under a directory on
// the local host. It backs the devhost adapter (development only, no
// isolation).
package localexec

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/different-ai/differently/pkg/api"
)

const (
	// MaxOutputBytes caps each captured stream of a synchronous exec.
	MaxOutputBytes = 10 << 20
	// DefaultTimeout applies when an exec request sets none.
	DefaultTimeout = 10 * time.Minute
	// MaxTimeout caps any exec request.
	MaxTimeout = 2 * time.Hour
	// MaxFileBytes bounds file reads and writes.
	MaxFileBytes = 100 << 20
)

// Runner executes commands and file operations below Root.
type Runner struct {
	// Root is the directory every path resolves under. "/" inside a VM.
	Root string
	// DefaultCwd is used when a request has no cwd. Relative to Root.
	DefaultCwd string
	// BaseEnv is applied to every command before request env.
	BaseEnv map[string]string
}

// Resolve maps a sandbox path to a host path, refusing escapes from Root.
func (r *Runner) Resolve(p string) (string, error) {
	if p == "" {
		p = "/"
	}
	clean := filepath.Clean("/" + p)
	full := filepath.Join(r.Root, clean)
	root := filepath.Clean(r.Root)
	if full != root && !strings.HasPrefix(full, root+string(filepath.Separator)) && root != string(filepath.Separator) {
		return "", api.Errorf(api.CodeInvalidRequest, "path %q escapes the sandbox root", p)
	}
	return full, nil
}

func (r *Runner) command(ctx context.Context, req api.ExecRequest) (*exec.Cmd, context.CancelFunc, error) {
	if req.Command == "" && len(req.Args) == 0 {
		return nil, nil, api.Errorf(api.CodeInvalidRequest, "command or args is required")
	}
	timeout := DefaultTimeout
	if req.TimeoutSeconds > 0 {
		timeout = time.Duration(req.TimeoutSeconds) * time.Second
	}
	timeout = min(timeout, MaxTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	var cmd *exec.Cmd
	if len(req.Args) > 0 {
		cmd = exec.CommandContext(ctx, req.Args[0], req.Args[1:]...)
	} else {
		cmd = exec.CommandContext(ctx, "/bin/sh", "-c", req.Command)
	}
	cwd := req.Cwd
	if cwd == "" {
		cwd = r.DefaultCwd
	}
	dir, err := r.Resolve(cwd)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		dir = r.Root
	}
	cmd.Dir = dir
	env := map[string]string{
		"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME": r.Root,
		"LANG": "C.UTF-8",
	}
	if p := os.Getenv("PATH"); p != "" && r.Root != "/" {
		env["PATH"] = p
	}
	for k, v := range r.BaseEnv {
		env[k] = v
	}
	for k, v := range req.Env {
		env[k] = v
	}
	cmd.Env = make([]string, 0, len(env))
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	sort.Strings(cmd.Env)
	if req.Stdin != "" {
		cmd.Stdin = strings.NewReader(req.Stdin)
	}
	configureProcessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second
	return cmd, cancel, nil
}

// Exec runs a command to completion and captures its output.
func (r *Runner) Exec(ctx context.Context, req api.ExecRequest) (api.ExecResult, error) {
	cmd, cancel, err := r.command(ctx, req)
	if err != nil {
		return api.ExecResult{}, err
	}
	defer cancel()
	stdout, stderr := &capped{max: MaxOutputBytes}, &capped{max: MaxOutputBytes}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	start := time.Now()
	code, err := exitCode(cmd.Run())
	if err != nil {
		return api.ExecResult{}, err
	}
	return api.ExecResult{
		ExitCode:   code,
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		DurationMs: time.Since(start).Milliseconds(),
		Truncated:  stdout.truncated || stderr.truncated,
	}, nil
}

// ExecStream runs a command and emits output chunks as they arrive.
func (r *Runner) ExecStream(ctx context.Context, req api.ExecRequest, emit func(api.ExecEvent) error) (int, error) {
	cmd, cancel, err := r.command(ctx, req)
	if err != nil {
		return -1, err
	}
	defer cancel()
	var mu sync.Mutex
	var emitErr error
	send := func(stream string) io.Writer {
		return writerFunc(func(p []byte) (int, error) {
			mu.Lock()
			defer mu.Unlock()
			if emitErr == nil {
				emitErr = emit(api.ExecEvent{Stream: stream, Data: string(p)})
			}
			return len(p), nil
		})
	}
	cmd.Stdout, cmd.Stderr = send("stdout"), send("stderr")
	code, err := exitCode(cmd.Run())
	if err != nil {
		return -1, err
	}
	return code, emitErr
}

func exitCode(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return 127, nil
	}
	return -1, err
}

// ReadFile returns a file's contents.
func (r *Runner) ReadFile(p string) ([]byte, error) {
	full, err := r.Resolve(p)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, fileErr(p, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFileBytes {
		return nil, api.Errorf(api.CodeInvalidRequest, "file larger than %d bytes", MaxFileBytes)
	}
	return data, nil
}

// WriteFile writes a file, creating parent directories.
func (r *Runner) WriteFile(p string, data []byte) error {
	full, err := r.Resolve(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fileErr(p, err)
	}
	return fileErr(p, os.WriteFile(full, data, 0o644))
}

// ListFiles lists a directory.
func (r *Runner) ListFiles(p string) ([]api.FileInfo, error) {
	full, err := r.Resolve(p)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		return nil, fileErr(p, err)
	}
	base := filepath.Clean("/" + p)
	out := make([]api.FileInfo, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, api.FileInfo{
			Path:    filepath.Join(base, e.Name()),
			Name:    e.Name(),
			Size:    info.Size(),
			Mode:    info.Mode().String(),
			IsDir:   e.IsDir(),
			ModTime: info.ModTime().UTC(),
		})
	}
	return out, nil
}

func fileErr(p string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return api.Errorf(api.CodeNotFound, "%s does not exist", p)
	case errors.Is(err, fs.ErrPermission):
		return api.Errorf(api.CodePermissionDenied, "%s: permission denied", p)
	}
	return api.Errorf(api.CodeInvalidRequest, "%s: %v", p, err)
}

type capped struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (c *capped) Write(p []byte) (int, error) {
	room := c.max - c.buf.Len()
	if room <= 0 {
		c.truncated = c.truncated || len(p) > 0
		return len(p), nil
	}
	if len(p) > room {
		c.buf.Write(p[:room])
		c.truncated = true
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *capped) String() string { return c.buf.String() }

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
