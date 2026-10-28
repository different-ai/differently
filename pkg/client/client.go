// Package client is the Go SDK for the Differently API.
//
//	c := client.New("https://differently.example.com", os.Getenv("DIFFERENTLY_API_KEY"))
//	sb, err := c.CreateSandbox(ctx, api.CreateSandboxRequest{Profile: "small"}, true)
//	res, err := c.Exec(ctx, sb.ID, api.ExecRequest{Command: "node --version"})
//	err = c.DeleteSandbox(ctx, sb.ID)
//
// The client never names a backend: placement is decided by the server's
// policies unless the request narrows it.
package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/different-ai/differently/pkg/api"
)

// Client calls one Differently installation.
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
	// Retries is how many times idempotent requests are retried on 429/503.
	Retries int
}

// New returns a client.
func New(baseURL, apiKey string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), APIKey: apiKey, HTTP: &http.Client{Timeout: 10 * time.Minute}, Retries: 3}
}

// Do sends a JSON request. in may be nil, []byte (sent raw) or any JSON value.
func (c *Client) Do(ctx context.Context, method, path string, in, out any, header ...http.Header) error {
	resp, err := c.send(ctx, method, path, in, header...)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if b, ok := out.(*[]byte); ok {
		*b, err = io.ReadAll(resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) send(ctx context.Context, method, path string, in any, header ...http.Header) (*http.Response, error) {
	var body []byte
	ct := ""
	switch v := in.(type) {
	case nil:
	case []byte:
		body, ct = v, "application/octet-stream"
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		body, ct = b, "application/json"
	}
	retryable := method == http.MethodGet || method == http.MethodDelete || method == http.MethodPut
	for _, h := range header {
		retryable = retryable || h.Get("Idempotency-Key") != ""
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
		req.Header.Set("User-Agent", "differently-go")
		for _, h := range header {
			for k, vs := range h {
				for _, v := range vs {
					req.Header.Add(k, v)
				}
			}
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < 300 {
			return resp, nil
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if retryable && attempt < c.Retries && (resp.StatusCode == 429 || resp.StatusCode == 503) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(200<<attempt) * time.Millisecond):
			}
			continue
		}
		var env api.ErrorEnvelope
		if json.Unmarshal(raw, &env) == nil && env.Error != nil {
			return nil, env.Error
		}
		return nil, &api.Error{Code: api.CodeInternal, Message: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))}
	}
}

func newIdempotencyKey() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// CreateSandbox requests a sandbox; with wait it blocks until it is running
// (or failed). Each call carries a fresh idempotency key so retries are safe.
func (c *Client) CreateSandbox(ctx context.Context, req api.CreateSandboxRequest, wait bool) (api.Sandbox, error) {
	var sb api.Sandbox
	if err := c.Do(ctx, http.MethodPost, "/v1/sandboxes", req, &sb, http.Header{"Idempotency-Key": {newIdempotencyKey()}}); err != nil {
		return sb, err
	}
	if !wait {
		return sb, nil
	}
	return c.WaitSandbox(ctx, sb.ID, api.StatusRunning, 5*time.Minute)
}

// WaitSandbox long-polls until status is reached, the sandbox ends or timeout.
func (c *Client) WaitSandbox(ctx context.Context, id string, status api.SandboxStatus, timeout time.Duration) (api.Sandbox, error) {
	deadline := time.Now().Add(timeout)
	for {
		var sb api.Sandbox
		left := int(time.Until(deadline).Seconds())
		q := fmt.Sprintf("/v1/sandboxes/%s/wait?status=%s&timeoutSeconds=%d", url.PathEscape(id), status, min(max(left, 1), 60))
		if err := c.Do(ctx, http.MethodGet, q, nil, &sb); err != nil {
			return sb, err
		}
		switch {
		case sb.Status == status:
			return sb, nil
		case sb.Status.Terminal():
			return sb, api.Errorf(api.CodeInvalidState, "sandbox %s is %s: %s", id, sb.Status, sb.StatusReason)
		case time.Now().After(deadline):
			return sb, api.Errorf(api.CodeInvalidState, "timed out waiting for %s; sandbox is %s: %s", status, sb.Status, sb.StatusReason)
		}
	}
}

// GetSandbox fetches a sandbox.
func (c *Client) GetSandbox(ctx context.Context, id string) (api.Sandbox, error) {
	var sb api.Sandbox
	return sb, c.Do(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(id), nil, &sb)
}

// ListSandboxes lists one page; pass the returned cursor for the next.
func (c *Client) ListSandboxes(ctx context.Context, query url.Values) (api.List[api.Sandbox], error) {
	var out api.List[api.Sandbox]
	return out, c.Do(ctx, http.MethodGet, "/v1/sandboxes?"+query.Encode(), nil, &out)
}

// DeleteSandbox requests termination.
func (c *Client) DeleteSandbox(ctx context.Context, id string) error {
	return c.Do(ctx, http.MethodDelete, "/v1/sandboxes/"+url.PathEscape(id), nil, nil)
}

// Exec runs a command and waits for it.
func (c *Client) Exec(ctx context.Context, id string, req api.ExecRequest) (api.ExecResult, error) {
	var out api.ExecResult
	return out, c.Do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/exec", req, &out)
}

// ExecStream runs a command and calls fn for each output event. It returns
// the exit code.
func (c *Client) ExecStream(ctx context.Context, id string, req api.ExecRequest, fn func(api.ExecEvent)) (int, error) {
	resp, err := c.send(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/exec?stream=1", req)
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	event := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			var ev api.ExecEvent
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
				return -1, err
			}
			switch event {
			case "exit":
				return *ev.ExitCode, nil
			case "error":
				return -1, api.Errorf(api.CodeProviderUnavailable, "%s", ev.Error)
			default:
				fn(ev)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return -1, err
	}
	return -1, fmt.Errorf("stream ended without an exit event")
}

// ReadFile downloads a file from a sandbox.
func (c *Client) ReadFile(ctx context.Context, id, path string) ([]byte, error) {
	var out []byte
	return out, c.Do(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(id)+"/files/content?path="+url.QueryEscape(path), nil, &out)
}

// WriteFile uploads a file into a sandbox.
func (c *Client) WriteFile(ctx context.Context, id, path string, data []byte) error {
	return c.Do(ctx, http.MethodPut, "/v1/sandboxes/"+url.PathEscape(id)+"/files/content?path="+url.QueryEscape(path), data, nil)
}

// ListFiles lists a directory in a sandbox.
func (c *Client) ListFiles(ctx context.Context, id, path string) ([]api.FileInfo, error) {
	var out api.List[api.FileInfo]
	return out.Items, c.Do(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(id)+"/files?path="+url.QueryEscape(path), nil, &out)
}

// Action runs stop, start, pause or resume.
func (c *Client) Action(ctx context.Context, id, action string) (api.Sandbox, error) {
	var sb api.Sandbox
	return sb, c.Do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/"+action, nil, &sb)
}

// Snapshot captures a sandbox (kind disk or memory).
func (c *Client) Snapshot(ctx context.Context, id, kind string) (api.Snapshot, error) {
	var out api.Snapshot
	return out, c.Do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/snapshots", api.CreateSnapshotRequest{Kind: kind}, &out)
}

// Fork snapshots a sandbox and starts a copy.
func (c *Client) Fork(ctx context.Context, id string) (api.Sandbox, error) {
	var sb api.Sandbox
	return sb, c.Do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/fork", nil, &sb)
}

// ExposePort returns a URL for a sandbox port where the backend supports it.
func (c *Client) ExposePort(ctx context.Context, id string, port int) (api.PortURL, error) {
	var out api.PortURL
	return out, c.Do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/ports", map[string]int{"port": port}, &out)
}

// Placement explains where a sandbox runs and why.
func (c *Client) Placement(ctx context.Context, id string) (map[string]any, error) {
	var out map[string]any
	return out, c.Do(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(id)+"/placement", nil, &out)
}

// CreateAPIKey mints a key; the token is only returned here.
func (c *Client) CreateAPIKey(ctx context.Context, req api.CreateAPIKeyRequest) (api.CreateAPIKeyResponse, error) {
	var out api.CreateAPIKeyResponse
	return out, c.Do(ctx, http.MethodPost, "/v1/api-keys", req, &out)
}
