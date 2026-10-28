package guest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/different-ai/differently/pkg/api"
)

// Client talks to a guest agent. Dial opens a fresh connection to the guest,
// for example through a Firecracker vsock socket.
type Client struct {
	Dial func(ctx context.Context) (net.Conn, error)
	http *http.Client
}

// NewClient returns a client using dial for every connection.
func NewClient(dial func(ctx context.Context) (net.Conn, error)) *Client {
	c := &Client{Dial: dial}
	c.http = &http.Client{Transport: &http.Transport{
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return c.Dial(ctx) },
		DisableKeepAlives: true,
	}}
	return c
}

// Health returns nil when the guest agent answers.
func (c *Client) Health(ctx context.Context) error {
	resp, err := c.send(ctx, http.MethodGet, "/healthz", nil)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// WaitReady polls Health until it succeeds or timeout passes.
func (c *Client) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		hctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		last = c.Health(hctx)
		cancel()
		if last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("guest agent not ready after %s: %w", timeout, last)
}

// Exec runs a command synchronously.
func (c *Client) Exec(ctx context.Context, req api.ExecRequest) (api.ExecResult, error) {
	var out api.ExecResult
	body, _ := json.Marshal(req)
	resp, err := c.send(ctx, http.MethodPost, "/v1/exec", bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	// The guest is untrusted: never decode more than two full output buffers.
	return out, json.NewDecoder(io.LimitReader(resp.Body, 2*MaxOutputBytes+1<<20)).Decode(&out)
}

// ExecStream runs a command and forwards output events.
func (c *Client) ExecStream(ctx context.Context, req api.ExecRequest, emit func(api.ExecEvent) error) (int, error) {
	body, _ := json.Marshal(req)
	resp, err := c.send(ctx, http.MethodPost, "/v1/exec?stream=1", bytes.NewReader(body))
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		var ev api.ExecEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			return -1, err
		}
		switch {
		case ev.Error != "":
			return -1, errors.New(ev.Error)
		case ev.ExitCode != nil:
			return *ev.ExitCode, nil
		}
		if err := emit(ev); err != nil {
			return -1, err
		}
	}
	return -1, errors.New("guest stream ended without exit code")
}

// ReadFile downloads a file.
func (c *Client) ReadFile(ctx context.Context, p string) ([]byte, error) {
	resp, err := c.send(ctx, http.MethodGet, "/v1/files/content?path="+url.QueryEscape(p), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, MaxFileBytes))
}

// WriteFile uploads a file.
func (c *Client) WriteFile(ctx context.Context, p string, data []byte) error {
	resp, err := c.send(ctx, http.MethodPut, "/v1/files/content?path="+url.QueryEscape(p), bytes.NewReader(data))
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// ListFiles lists a directory.
func (c *Client) ListFiles(ctx context.Context, p string) ([]api.FileInfo, error) {
	resp, err := c.send(ctx, http.MethodGet, "/v1/files?path="+url.QueryEscape(p), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out []api.FileInfo
	return out, json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&out)
}

func (c *Client) send(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://guest"+path, body)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		var env api.ErrorEnvelope
		if json.NewDecoder(resp.Body).Decode(&env) == nil && env.Error != nil {
			return nil, env.Error
		}
		return nil, fmt.Errorf("guest %s %s: HTTP %d", method, path, resp.StatusCode)
	}
	return resp, nil
}
