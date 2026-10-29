// Package httpx is a small JSON-over-HTTP helper for provider adapters. It
// maps provider HTTP failures onto Differently error codes so the scheduler
// can tell transient capacity limits from outages.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// Client calls one provider API.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	// Provider names the backend in error messages.
	Provider string
}

// New returns a client with a sane default timeout.
func New(provider, baseURL, token string) *Client {
	return &Client{Provider: provider, BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: &http.Client{Timeout: 2*time.Hour + 15*time.Minute}}
}

// JSON sends a JSON request and decodes a JSON response into out (may be nil).
func (c *Client) JSON(ctx context.Context, method, url string, in, out any) error {
	var body io.Reader
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ct = bytes.NewReader(b), "application/json"
	}
	resp, err := c.Do(ctx, method, url, body, ct, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Do sends a request; url may be absolute or relative to BaseURL. Non-2xx
// responses become *api.Error.
func (c *Client) Do(ctx context.Context, method, url string, body io.Reader, contentType string, header http.Header) (*http.Response, error) {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = c.BaseURL + url
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, adapter.Unavailable(fmt.Errorf("%s: %w", c.Provider, err))
	}
	if resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return nil, c.mapError(resp.StatusCode, raw)
}

// mapError converts a provider error into a Differently error. The message
// keeps the provider's own text but never echoes request credentials.
func (c *Client) mapError(status int, raw []byte) error {
	var body struct {
		Code    string `json:"code"`
		Message any    `json:"message"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal(raw, &body)
	msg := fmt.Sprint(body.Message)
	if body.Message == nil {
		msg = body.Error
	}
	if msg == "" {
		msg = http.StatusText(status)
	}
	msg = c.Provider + ": " + msg
	e := &api.Error{Message: msg, Details: map[string]string{"provider": c.Provider, "providerStatus": fmt.Sprint(status)}}
	if body.Code != "" {
		e.Message += " (" + body.Code + ")"
		e.Details["providerCode"] = body.Code
	}
	switch {
	case status == http.StatusNotFound:
		e.Code = api.CodeNotFound
	case status == http.StatusTooManyRequests:
		e.Code = adapter.CodeInsufficientCapacity // provider plan or rate cap: retry later
	case status == http.StatusConflict:
		e.Code = api.CodeInvalidState
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		e.Code = api.CodeInvalidRequest
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		e.Code = api.CodeProviderUnavailable
		e.Message = c.Provider + ": credentials were rejected; check the pool's credential reference"
	default:
		e.Code = api.CodeProviderUnavailable
	}
	return e
}

// ShellQuote joins args into a POSIX shell command line.
func ShellQuote(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		if a != "" && strings.IndexFunc(a, func(r rune) bool {
			return !(r == '-' || r == '_' || r == '.' || r == '/' || r == '=' || r == ':' || r == ',' || r == '+' ||
				(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
		}) < 0 {
			out[i] = a
			continue
		}
		out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(out, " ")
}

// CommandLine renders an ExecRequest as one shell command, honouring cwd.
func CommandLine(req api.ExecRequest) string {
	cmd := req.Command
	if cmd == "" {
		cmd = ShellQuote(req.Args)
	}
	if req.Cwd != "" {
		cmd = "cd " + ShellQuote([]string{req.Cwd}) + " && " + cmd
	}
	return cmd
}

// Poll calls check until it reports done, ctx ends or timeout passes.
func Poll(ctx context.Context, every, timeout time.Duration, check func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	for {
		done, err := check()
		if err != nil || done {
			return err
		}
		if time.Now().After(deadline) {
			return adapter.Unavailable(fmt.Errorf("timed out after %s", timeout))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(every):
		}
	}
}
