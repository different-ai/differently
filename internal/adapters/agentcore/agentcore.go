// Package agentcore adapts Amazon Bedrock AgentCore Code Interpreter: each
// Differently sandbox is one code-interpreter session.
//
// AWS runs the sessions in its own account, so pools default to
// TrustExternal; an operator whose policy treats their own AWS account as
// private may declare trust: private. Requests are signed with SigV4 using
// a static access key from the pool's credential (no AWS SDK).
//
// Mapping, from the AgentCore data-plane API (botocore model 2024-02-28,
// read 2026-10-28):
//
//   - Create starts a session named "dly:<installation>:<sandbox>" with a
//     clientToken derived from both, after looking for a READY session with
//     that name, so a replay returns the same session;
//   - List pages through READY sessions and keeps those with this
//     installation's name prefix;
//   - Delete stops the session (stopped sessions cannot restart);
//   - Exec runs executeCommand, with cwd, env, stdin and timeout folded into
//     the command line (shellcmd); files move as base64 through commands.
//
// Not available: stop/start, pause/resume, snapshots, ports, streaming.
// Sessions end after at most 8 hours whatever the sandbox's TTL.
//
// Tested against a fake server built from the published API, not yet
// against a live account.
package agentcore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/different-ai/differently/internal/adapters/cloudhttp"
	"github.com/different-ai/differently/internal/adapters/shellcmd"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// DefaultInterpreter is AWS's built-in code interpreter.
const DefaultInterpreter = "aws.codeinterpreter.v1"

// maxSessionSeconds is AgentCore's session lifetime limit (8 hours).
const maxSessionSeconds = 28800

// backstopGrace keeps the session's end behind Differently's own, audited
// expiry.
const backstopGrace = 5 * 60

// Options configures the adapter. Pool config keys: region (or the pool's
// region), codeInterpreter, apiURL. The credential is JSON:
// {"accessKeyId", "secretAccessKey", optional "sessionToken"}.
type Options struct {
	Region      string
	APIURL      string
	Interpreter string
	Credential  cloudhttp.AWSCredential
}

// FromConfig maps pool config to options.
func FromConfig(cfg map[string]string, poolRegion, credential string) (Options, error) {
	o := Options{Region: cfg["region"], APIURL: cfg["apiURL"], Interpreter: cfg["codeInterpreter"]}
	if o.Region == "" {
		o.Region = poolRegion
	}
	if o.Region == "" {
		return o, errors.New("aws-agentcore needs config.region (an AWS region such as us-east-1)")
	}
	if o.APIURL == "" {
		o.APIURL = "https://bedrock-agentcore." + o.Region + ".amazonaws.com"
	}
	if o.Interpreter == "" {
		o.Interpreter = DefaultInterpreter
	}
	c, err := cloudhttp.ParseAWS(credential)
	if err != nil {
		return o, fmt.Errorf("aws-agentcore: %w", err)
	}
	o.Credential = c
	return o, nil
}

// Adapter talks to AgentCore Code Interpreter.
type Adapter struct {
	opts Options
	c    *cloudhttp.Client
}

// New returns an AgentCore adapter.
func New(o Options) *Adapter {
	c := cloudhttp.New("aws-agentcore", o.APIURL, cloudhttp.SigV4(o.Credential, o.Region, "bedrock-agentcore"))
	c.Classify = func(_ int, code string) string {
		switch code {
		case "ServiceQuotaExceededException", "ThrottlingException":
			return adapter.CodeInsufficientCapacity // sent as 402 and 429: retry later
		}
		return ""
	}
	return &Adapter{opts: o, c: c}
}

// Endpoint is reported in the trust report.
func (a *Adapter) Endpoint() string { return a.opts.APIURL }

func (a *Adapter) Info() adapter.Info {
	return adapter.Info{Kind: "aws-agentcore", Version: "1", Trust: api.TrustExternal, Isolation: "microVM per session (AWS-managed)", Protocol: adapter.ProtocolVersion,
		Description: "Amazon Bedrock AgentCore Code Interpreter " + a.opts.Interpreter + " in " + a.opts.Region}
}

func (a *Adapter) Capabilities(context.Context) (adapter.Capabilities, error) {
	// Sessions are fixed at 2 vCPU, 8 GB memory and 10 GB disk.
	return adapter.Capabilities{Features: []string{adapter.FeatureExec, adapter.FeatureFiles, adapter.FeatureMicroVM},
		MaxCPU: 2, MaxMemoryMiB: 8192, MaxDiskGiB: 10, Regions: []string{a.opts.Region}}, nil
}

func (a *Adapter) Capacity(context.Context) (adapter.Capacity, error) { return adapter.Capacity{}, nil }

func (a *Adapter) Health(ctx context.Context) error {
	return a.c.JSON(ctx, http.MethodPost, a.base()+"/sessions/list", map[string]any{"maxResults": 1, "status": "READY"}, nil)
}

func (a *Adapter) base() string { return "/code-interpreters/" + url.PathEscape(a.opts.Interpreter) }

type session struct {
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	Status    string `json:"status"`
}

func sessionName(installation, sandboxID string) string {
	return "dly:" + installation + ":" + sandboxID
}

// token derives an idempotency token: 64 hex characters fit the API's
// 33-256 character [a-zA-Z0-9-] pattern.
func token(parts ...string) string {
	sum := sha256.Sum256([]byte("differently:" + strings.Join(parts, ":")))
	return hex.EncodeToString(sum[:])
}

func (a *Adapter) instance(s session) adapter.Instance {
	_, sandboxID, _ := strings.Cut(strings.TrimPrefix(s.Name, "dly:"), ":")
	return adapter.Instance{Ref: s.SessionID, SandboxID: sandboxID, State: state(s.Status), Region: a.opts.Region}
}

// state maps AgentCore's session status. The model's enum is READY or
// TERMINATED; the descriptions also name ACTIVE, STOPPING and STOPPED.
func state(status string) string {
	switch status {
	case "READY", "ACTIVE":
		return adapter.StateRunning
	case "TERMINATED", "STOPPING", "STOPPED":
		return adapter.StateStopped
	}
	return adapter.StatePending
}

func gone(status string) bool { return state(status) == adapter.StateStopped }

// ready lists every READY session, failing if any page fails.
func (a *Adapter) ready(ctx context.Context) ([]session, error) {
	var out []session
	next := ""
	for {
		body := map[string]any{"maxResults": 100, "status": "READY"}
		if next != "" {
			body["nextToken"] = next
		}
		var page struct {
			Items     []session `json:"items"`
			NextToken string    `json:"nextToken"`
		}
		if err := a.c.JSON(ctx, http.MethodPost, a.base()+"/sessions/list", body, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.NextToken == "" {
			return out, nil
		}
		next = page.NextToken
	}
}

func (a *Adapter) Create(ctx context.Context, req adapter.CreateRequest) (adapter.Instance, error) {
	if req.FromSnapshot != "" {
		return adapter.Instance{}, adapter.Unsupported("snapshots")
	}
	name := sessionName(req.Installation, req.ID)
	sessions, err := a.ready(ctx)
	if err != nil {
		return adapter.Instance{}, err
	}
	var s session
	for _, it := range sessions {
		if it.Name == name {
			s = it
			break
		}
	}
	if s.SessionID == "" {
		ttl := req.TTLSeconds
		if ttl > 0 {
			ttl += backstopGrace
		}
		if ttl <= 0 || ttl > maxSessionSeconds {
			ttl = maxSessionSeconds
		}
		body := map[string]any{"name": name, "sessionTimeoutSeconds": max(ttl, 60), "clientToken": token(req.Installation, req.ID)}
		if err := a.c.JSON(ctx, http.MethodPut, a.base()+"/sessions/start", body, &s); err != nil {
			return adapter.Instance{}, err
		}
		s.Name, s.Status = name, "READY"
	}
	if len(req.Env) > 0 {
		// Rewritten on a replay too, so a retried create still gets its env.
		r, err := a.command(ctx, s.SessionID, shellcmd.EnvScript(req.Env))
		if err != nil {
			return adapter.Instance{}, err
		}
		if r.ExitCode != 0 {
			return adapter.Instance{}, adapter.SandboxFailed(fmt.Errorf("writing the sandbox environment failed: %s", strings.TrimSpace(r.Stderr)))
		}
	}
	return a.instance(s), nil
}

func (a *Adapter) get(ctx context.Context, ref string) (session, error) {
	var s session
	err := a.c.JSON(ctx, http.MethodGet, a.base()+"/sessions/get?sessionId="+url.QueryEscape(ref), nil, &s)
	return s, err
}

func (a *Adapter) Get(ctx context.Context, ref string) (adapter.Instance, error) {
	s, err := a.get(ctx, ref)
	if err != nil {
		return adapter.Instance{}, err
	}
	if gone(s.Status) {
		return adapter.Instance{}, adapter.NotFound(ref)
	}
	return a.instance(s), nil
}

func (a *Adapter) List(ctx context.Context, installation string) ([]adapter.Instance, error) {
	sessions, err := a.ready(ctx)
	if err != nil {
		return nil, err
	}
	prefix := "dly:" + installation + ":"
	var out []adapter.Instance
	for _, s := range sessions {
		if strings.HasPrefix(s.Name, prefix) {
			out = append(out, a.instance(s))
		}
	}
	return out, nil
}

// Delete stops the session. Stopping a stopped or unknown session succeeds.
func (a *Adapter) Delete(ctx context.Context, ref string) error {
	body := map[string]any{"clientToken": token("stop", ref)}
	err := a.c.JSON(ctx, http.MethodPut, a.base()+"/sessions/stop?sessionId="+url.QueryEscape(ref), body, nil)
	if err == nil || api.IsCode(err, api.CodeNotFound) {
		return nil
	}
	if api.IsCode(err, api.CodeInvalidState) || api.IsCode(err, api.CodeInvalidRequest) {
		// A conflict usually means it already stopped; confirm before succeeding.
		s, gerr := a.get(ctx, ref)
		if api.IsCode(gerr, api.CodeNotFound) || gerr == nil && gone(s.Status) {
			return nil
		}
	}
	return err
}

// result is the payload of a "result" event from InvokeCodeInterpreter.
type result struct {
	Content []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Resource *struct {
			Text string `json:"text"`
			Blob []byte `json:"blob"`
		} `json:"resource"`
	} `json:"content"`
	StructuredContent *struct {
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		ExitCode *int   `json:"exitCode"`
	} `json:"structuredContent"`
	IsError bool `json:"isError"`
}

// invoke calls one code-interpreter tool and collects its result events.
func (a *Adapter) invoke(ctx context.Context, ref, tool string, args map[string]any) ([]result, error) {
	body, _ := json.Marshal(map[string]any{"name": tool, "arguments": args})
	h := http.Header{"x-amzn-code-interpreter-session-id": {ref}, "Accept": {"application/vnd.amazon.eventstream"}}
	resp, err := a.c.Do(ctx, http.MethodPost, a.base()+"/tools/invoke", body, "application/json", h)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out []result
	err = cloudhttp.ReadEvents(resp.Body, func(e cloudhttp.Event) error {
		switch e.Headers[":message-type"] {
		case "exception":
			return a.streamError(e.Headers[":exception-type"], e.Payload)
		case "error":
			return adapter.Unavailable(fmt.Errorf("aws-agentcore: %s: %s", e.Headers[":error-code"], e.Headers[":error-message"]))
		}
		if e.Headers[":event-type"] != "result" {
			return nil
		}
		var r result
		if err := json.Unmarshal(e.Payload, &r); err != nil {
			return adapter.Unavailable(fmt.Errorf("aws-agentcore: unreadable result: %w", err))
		}
		out = append(out, r)
		return nil
	})
	if err != nil {
		var ae *api.Error
		if !errors.As(err, &ae) {
			err = adapter.Unavailable(fmt.Errorf("aws-agentcore: %w", err))
		}
		return nil, err
	}
	return out, nil
}

// streamError classifies an exception event inside a 200 response the same
// way as an HTTP error with that error type.
func (a *Adapter) streamError(kind string, payload []byte) error {
	var body struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(payload, &body)
	status := map[string]int{
		"validationException": 400, "accessDeniedException": 403, "resourceNotFoundException": 404,
		"conflictException": 409, "throttlingException": 429, "serviceQuotaExceededException": 402,
	}[kind]
	if status == 0 {
		status = 500
	}
	code := strings.ToUpper(kind[:1]) + kind[1:]
	raw, _ := json.Marshal(map[string]string{"__type": code, "message": body.Message})
	return a.c.Error(status, http.Header{}, raw)
}

// command runs one shell command line with executeCommand.
func (a *Adapter) command(ctx context.Context, ref, cmd string) (api.ExecResult, error) {
	start := time.Now()
	results, err := a.invoke(ctx, ref, "executeCommand", map[string]any{"command": cmd})
	if err != nil {
		return api.ExecResult{}, err
	}
	res := api.ExecResult{DurationMs: time.Since(start).Milliseconds()}
	exit := -1
	for _, r := range results {
		if sc := r.StructuredContent; sc != nil {
			res.Stdout += sc.Stdout
			res.Stderr += sc.Stderr
			if sc.ExitCode != nil {
				exit = *sc.ExitCode
			}
			continue
		}
		for _, c := range r.Content {
			if c.Type == "text" {
				res.Stdout += c.Text
			}
		}
		if r.IsError {
			exit = 1
		}
	}
	if exit < 0 {
		if len(results) == 0 {
			return api.ExecResult{}, adapter.SandboxFailed(errors.New("aws-agentcore returned no result for the command"))
		}
		exit = 0
	}
	res.ExitCode = exit
	return res, nil
}

func (a *Adapter) Exec(ctx context.Context, ref string, req api.ExecRequest) (api.ExecResult, error) {
	return a.command(ctx, ref, shellcmd.Script(req))
}

func (a *Adapter) run(ref string) shellcmd.Run {
	return func(ctx context.Context, cmd string) (api.ExecResult, error) { return a.command(ctx, ref, cmd) }
}

func (a *Adapter) ReadFile(ctx context.Context, ref, path string) ([]byte, error) {
	return shellcmd.ReadFile(ctx, a.run(ref), path)
}

func (a *Adapter) WriteFile(ctx context.Context, ref, path string, data []byte) error {
	return shellcmd.WriteFile(ctx, a.run(ref), path, data)
}

func (a *Adapter) ListFiles(ctx context.Context, ref, path string) ([]api.FileInfo, error) {
	return shellcmd.ListFiles(ctx, a.run(ref), path)
}

var (
	_ adapter.ExecutionAdapter = (*Adapter)(nil)
	_ adapter.FileSystem       = (*Adapter)(nil)
)
