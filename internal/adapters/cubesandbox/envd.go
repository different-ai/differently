package cubesandbox

// envd is E2B's agent inside each sandbox (CubeSandbox builds it from
// e2b-dev/infra). Commands go through the Connect protocol's server-streaming
// process.Process/Start, files through envd's plain HTTP /files and the unary
// filesystem.Filesystem RPCs. Nothing here is CubeSandbox-specific apart from
// how CubeProxy is addressed, so an E2B adapter could share it.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/different-ai/differently/internal/adapters/httpx"
	"github.com/different-ai/differently/internal/id"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// EnvdPort is envd's port in every sandbox.
const EnvdPort = 49983

// Connect protocol framing: a flag byte and a big-endian length per message.
const (
	flagCompressed = 0x01
	flagEndStream  = 0x02
	maxMessage     = 16 << 20
	maxFile        = 100 << 20
)

// domain is where CubeProxy serves a sandbox's ports: the pool's setting, or
// what CubeAPI reported for the sandbox, or CubeSandbox's default.
func (a *Adapter) domain(ctx context.Context, ref string) string {
	if a.opts.Domain != "" {
		return a.opts.Domain
	}
	a.mu.Lock()
	d := a.domains[ref]
	a.mu.Unlock()
	if d == "" {
		if s, err := a.get(ctx, ref); err == nil {
			a.inst(s)
			d = s.Domain
		}
	}
	if d == "" {
		d = "cube.app"
	}
	return d
}

// target returns the base URL and routing headers for a sandbox port: host
// routing (<port>-<id>.<domain>) by default, or header routing at sandboxURL.
func (a *Adapter) target(ctx context.Context, ref string, port int) (string, http.Header) {
	h := http.Header{}
	if a.opts.SandboxURL != "" {
		h.Set("E2b-Sandbox-Id", ref)
		h.Set("E2b-Sandbox-Port", strconv.Itoa(port))
		return a.opts.SandboxURL, h
	}
	return fmt.Sprintf("%s://%d-%s.%s", a.opts.ProxyScheme, port, ref, a.domain(ctx, ref)), h
}

// envd sends one request to the sandbox's envd.
func (a *Adapter) envd(ctx context.Context, ref, method, p string, body io.Reader, contentType string, extra http.Header) (*http.Response, error) {
	base, h := a.target(ctx, ref, EnvdPort)
	for k, vs := range extra {
		h[k] = vs
	}
	return a.data.Do(ctx, method, base+p, body, contentType, h)
}

// rpcHeaders are the Connect headers, with envd's user as Basic auth.
func (a *Adapter) rpcHeaders() http.Header {
	h := http.Header{}
	h.Set("Connect-Protocol-Version", "1")
	h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(a.opts.User+":")))
	return h
}

// waitEnvd polls envd's health endpoint until it answers.
func (a *Adapter) waitEnvd(ctx context.Context, ref string) error {
	return httpx.Poll(ctx, 200*time.Millisecond, a.opts.ReadyTimeout, func() (bool, error) {
		resp, err := a.envd(ctx, ref, http.MethodGet, "/health", nil, "", nil)
		if err != nil {
			return false, nil // not reachable yet
		}
		resp.Body.Close()
		return true, nil
	})
}

func envelope(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(b) > maxMessage {
		return nil, api.Errorf(api.CodeInvalidRequest, "cubesandbox: the command request is %d bytes; the limit is %d", len(b), maxMessage)
	}
	out := make([]byte, 5, 5+len(b))
	binary.BigEndian.PutUint32(out[1:], uint32(len(b)))
	return append(out, b...), nil
}

type processEvent struct {
	Event *struct {
		Start *struct {
			PID int `json:"pid"`
		} `json:"start"`
		Data *struct {
			Stdout string `json:"stdout"`
			Stderr string `json:"stderr"`
		} `json:"data"`
		End *struct {
			ExitCode *int   `json:"exitCode"`
			Exited   bool   `json:"exited"`
			Status   string `json:"status"`
			Error    string `json:"error"`
		} `json:"end"`
	} `json:"event"`
}

type endStream struct {
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// run starts a command and passes output to emit as envd streams it. envd
// takes stdin only as later SendInput calls, so stdin is staged in a file
// the command reads, as for OpenSandbox.
func (a *Adapter) run(ctx context.Context, ref string, req api.ExecRequest, emit func(api.ExecEvent) error) (int, error) {
	proc := map[string]any{}
	switch {
	case req.Stdin != "":
		file := "/tmp/.differently-stdin-" + id.New("in")
		if err := a.WriteFile(ctx, ref, file, []byte(req.Stdin)); err != nil {
			return -1, err
		}
		cmd := req.Command
		if cmd == "" {
			cmd = httpx.ShellQuote(req.Args)
		}
		q := httpx.ShellQuote([]string{file})
		proc["cmd"], proc["args"] = "/bin/sh", []string{"-c", "(" + cmd + ") < " + q + "; rc=$?; rm -f " + q + "; exit $rc"}
	case req.Command != "":
		proc["cmd"], proc["args"] = "/bin/sh", []string{"-c", req.Command}
	case len(req.Args) > 0:
		proc["cmd"], proc["args"] = req.Args[0], req.Args[1:]
	default:
		return -1, api.Errorf(api.CodeInvalidRequest, "cubesandbox: exec needs a command or args")
	}
	if len(req.Env) > 0 {
		proc["envs"] = req.Env
	}
	if req.Cwd != "" {
		proc["cwd"] = req.Cwd
	}
	body, err := envelope(map[string]any{"process": proc, "stdin": false})
	if err != nil {
		return -1, err
	}
	h := a.rpcHeaders()
	if req.TimeoutSeconds > 0 {
		h.Set("Connect-Timeout-Ms", strconv.Itoa(req.TimeoutSeconds*1000))
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutSeconds+10)*time.Second)
		defer cancel()
	}
	started := time.Now()
	resp, err := a.envd(ctx, ref, http.MethodPost, "/process.Process/Start", bytes.NewReader(body), "application/connect+json", h)
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()
	r := bufio.NewReaderSize(resp.Body, 64<<10)
	code, ended := -1, false
	for {
		var hdr [5]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) && ended {
				return code, nil
			}
			return -1, adapter.Unavailable(fmt.Errorf("cubesandbox: exec stream ended early: %w", err))
		}
		n := binary.BigEndian.Uint32(hdr[1:])
		if n > maxMessage {
			return -1, fmt.Errorf("cubesandbox: exec message of %d bytes", n)
		}
		msg := make([]byte, n)
		if _, err := io.ReadFull(r, msg); err != nil {
			return -1, adapter.Unavailable(fmt.Errorf("cubesandbox: exec stream ended early: %w", err))
		}
		if hdr[0]&flagCompressed != 0 {
			return -1, errors.New("cubesandbox: envd sent a compressed message, which was not requested")
		}
		if hdr[0]&flagEndStream != 0 {
			var end endStream
			_ = json.Unmarshal(msg, &end)
			switch {
			case end.Error == nil && ended:
				return code, nil
			case end.Error != nil && end.Error.Code == "deadline_exceeded" && req.TimeoutSeconds > 0:
				return 124, nil // as timeout(1) reports it
			case end.Error != nil:
				return startError(end.Error.Code, end.Error.Message)
			}
			return -1, adapter.Unavailable(errors.New("cubesandbox: exec stream ended without an end event"))
		}
		var ev processEvent
		if err := json.Unmarshal(msg, &ev); err != nil {
			return -1, fmt.Errorf("cubesandbox: decode exec event: %w", err)
		}
		switch e := ev.Event; {
		case e == nil:
		case e.Data != nil:
			for stream, b64 := range map[string]string{"stdout": e.Data.Stdout, "stderr": e.Data.Stderr} {
				if b64 == "" {
					continue
				}
				b, err := base64.StdEncoding.DecodeString(b64)
				if err != nil {
					return -1, fmt.Errorf("cubesandbox: decode %s: %w", stream, err)
				}
				if err := emit(api.ExecEvent{Stream: stream, Data: string(b)}); err != nil {
					return -1, err
				}
			}
		case e.End != nil:
			ended = true
			code = endCode(e.End.ExitCode, e.End.Status, e.End.Exited)
			if !e.End.Exited && req.TimeoutSeconds > 0 && time.Since(started) >= time.Duration(req.TimeoutSeconds)*time.Second {
				code = 124 // envd killed it at the deadline
			}
		}
	}
}

// endCode reads an end event. proto3 JSON omits a zero exitCode, so the code
// comes from the field, else from "exit status N", else from exited.
func endCode(exitCode *int, status string, exited bool) int {
	if exitCode != nil {
		return *exitCode
	}
	if s, ok := strings.CutPrefix(strings.TrimSpace(status), "exit status "); ok {
		if n, err := strconv.Atoi(s); err == nil {
			return n
		}
	}
	if exited {
		return 0
	}
	return 128 + 9 // killed by a signal (envd reports "signal: killed")
}

// startError reports a command envd could not start as a shell would.
func startError(code, msg string) (int, error) {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "executable file not found") || strings.Contains(m, "no such file or directory"):
		return 127, nil
	case strings.Contains(m, "permission denied"):
		return 126, nil
	case code == "not_found":
		return -1, api.Errorf(api.CodeNotFound, "cubesandbox: %s", msg)
	case code == "unavailable":
		return -1, adapter.Unavailable(fmt.Errorf("cubesandbox: %s", msg))
	}
	return -1, api.Errorf(api.CodeInvalidRequest, "cubesandbox: exec failed: %s (%s)", msg, code)
}

// Exec implements adapter.ExecutionAdapter.
func (a *Adapter) Exec(ctx context.Context, ref string, req api.ExecRequest) (api.ExecResult, error) {
	var stdout, stderr strings.Builder
	start := time.Now()
	code, err := a.run(ctx, ref, req, func(ev api.ExecEvent) error {
		if ev.Stream == "stderr" {
			stderr.WriteString(ev.Data)
		} else {
			stdout.WriteString(ev.Data)
		}
		return nil
	})
	if err != nil {
		return api.ExecResult{}, err
	}
	return api.ExecResult{ExitCode: code, Stdout: stdout.String(), Stderr: stderr.String(), DurationMs: time.Since(start).Milliseconds()}, nil
}

// ExecStream implements adapter.Streamer.
func (a *Adapter) ExecStream(ctx context.Context, ref string, req api.ExecRequest, emit func(api.ExecEvent) error) (int, error) {
	return a.run(ctx, ref, req, emit)
}

func (a *Adapter) fileQuery(p string) string {
	return "/files?" + url.Values{"path": {p}, "username": {a.opts.User}}.Encode()
}

func (a *Adapter) ReadFile(ctx context.Context, ref, p string) ([]byte, error) {
	resp, err := a.envd(ctx, ref, http.MethodGet, a.fileQuery(p), nil, "", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxFile+1))
	if err == nil && len(b) > maxFile {
		return nil, api.Errorf(api.CodeInvalidRequest, "cubesandbox: %s is larger than %d bytes", p, maxFile)
	}
	return b, err
}

// WriteFile creates the parent directory, then uploads the file as a
// multipart form, which every envd version accepts.
func (a *Adapter) WriteFile(ctx context.Context, ref, p string, data []byte) error {
	if err := a.rpc(ctx, ref, "filesystem.Filesystem/MakeDir", map[string]string{"path": path.Dir(p)}, nil); err != nil && !api.IsCode(err, api.CodeInvalidState) {
		return err // 409 already_exists is fine
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("file", p)
	if err != nil {
		return err
	}
	if _, err := part.Write(data); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	resp, err := a.envd(ctx, ref, http.MethodPost, a.fileQuery(p), &buf, w.FormDataContentType(), nil)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// rpc calls a unary Connect method with JSON.
func (a *Adapter) rpc(ctx context.Context, ref, method string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	resp, err := a.envd(ctx, ref, http.MethodPost, "/"+method, bytes.NewReader(b), "application/json", a.rpcHeaders())
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out)
}

// int64s arrive as JSON strings in proto3 JSON, but accept numbers too.
type int64s int64

func (n *int64s) UnmarshalJSON(b []byte) error {
	v, err := strconv.ParseInt(strings.Trim(string(b), `"`), 10, 64)
	*n = int64s(v)
	return err
}

func (a *Adapter) ListFiles(ctx context.Context, ref, p string) ([]api.FileInfo, error) {
	var out struct {
		Entries []struct {
			Name         string `json:"name"`
			Type         string `json:"type"`
			Path         string `json:"path"`
			Size         int64s `json:"size"`
			Mode         uint32 `json:"mode"`
			ModifiedTime string `json:"modifiedTime"`
		} `json:"entries"`
	}
	if err := a.rpc(ctx, ref, "filesystem.Filesystem/ListDir", map[string]any{"path": p, "depth": 1}, &out); err != nil {
		return nil, err
	}
	files := make([]api.FileInfo, 0, len(out.Entries))
	for _, e := range out.Entries {
		mt, _ := time.Parse(time.RFC3339Nano, e.ModifiedTime)
		files = append(files, api.FileInfo{Path: e.Path, Name: e.Name, Size: int64(e.Size), Mode: strconv.FormatUint(uint64(e.Mode&0o777), 8),
			IsDir: e.Type == "FILE_TYPE_DIRECTORY", ModTime: mt})
	}
	return files, nil
}

// ExposePort returns CubeProxy's URL for a port: <port>-<id>.<domain>, or
// with header routing its path form, /sandbox/<id>/<port>.
func (a *Adapter) ExposePort(ctx context.Context, ref string, port int) (string, error) {
	if _, err := a.get(ctx, ref); err != nil {
		return "", err
	}
	if a.opts.SandboxURL != "" {
		return fmt.Sprintf("%s/sandbox/%s/%d", a.opts.SandboxURL, url.PathEscape(ref), port), nil
	}
	base, _ := a.target(ctx, ref, port)
	return base, nil
}
