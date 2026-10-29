// Package cloudhttp calls cloud providers' sandbox APIs with cloud IAM
// credentials: Microsoft Entra ID tokens, Google OAuth 2.0 tokens and AWS
// Signature Version 4. It uses only the standard library, so the cloud
// adapters need no cloud SDKs.
//
// It differs from httpx in two ways the cloud APIs need: requests are
// authorized one by one (short-lived tokens, signatures over the body), and
// error bodies are read in the providers' nested shapes (Azure and Google's
// {"error": {...}}, AWS's x-amzn-ErrorType) so classification can use the
// provider's own error code.
//
// A pool's credential reference (env:, file: or secret:) resolves to one
// string. Cloud adapters read it as a small JSON document (EntraCredential,
// a Google service-account key, AWSCredential). Errors never include
// credential values.
package cloudhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	// Provider names the backend in error messages.
	Provider string
	BaseURL  string
	HTTP     *http.Client
	// Authorize adds credentials to a request; body is its exact payload.
	Authorize func(r *http.Request, body []byte) error
	// Classify may override the Differently error code for a provider error
	// code (for example a quota error sent as 400). An empty result keeps
	// the status-based default.
	Classify func(status int, providerCode string) string
}

// New returns a client with the same generous timeout as httpx: callers
// bound requests with their context.
func New(provider, baseURL string, authorize func(*http.Request, []byte) error) *Client {
	return &Client{Provider: provider, BaseURL: strings.TrimRight(baseURL, "/"), Authorize: authorize,
		HTTP: &http.Client{Timeout: 2*time.Hour + 15*time.Minute}}
}

// JSON sends in as JSON (when not nil) and decodes the response into out
// (when not nil).
func (c *Client) JSON(ctx context.Context, method, url string, in, out any) error {
	var body []byte
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ct = b, "application/json"
	}
	resp, err := c.Do(ctx, method, url, body, ct, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil && err != io.EOF {
		return adapter.Unavailable(fmt.Errorf("%s: unreadable response: %w", c.Provider, err))
	}
	return nil
}

// Do sends a request; url may be absolute or relative to BaseURL. Non-2xx
// responses become *api.Error.
func (c *Client) Do(ctx context.Context, method, url string, body []byte, contentType string, header http.Header) (*http.Response, error) {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		url = c.BaseURL + url
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
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
	if c.Authorize != nil {
		if err := c.Authorize(req, body); err != nil {
			var ae *api.Error
			if errors.As(err, &ae) {
				return nil, err
			}
			return nil, adapter.Unavailable(fmt.Errorf("%s: %w", c.Provider, err))
		}
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, adapter.Unavailable(fmt.Errorf("%s: %w", c.Provider, err))
	}
	if resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return nil, c.Error(resp.StatusCode, resp.Header, raw)
}

// Error converts a failed provider response into a Differently error. The
// message keeps the provider's own text, except for rejected credentials.
func (c *Client) Error(status int, h http.Header, raw []byte) error {
	code, msg := ProviderError(h, raw)
	if msg == "" {
		msg = http.StatusText(status)
	}
	e := &api.Error{Message: c.Provider + ": " + msg, Details: map[string]string{"provider": c.Provider, "providerStatus": fmt.Sprint(status)}}
	if code != "" {
		e.Message += " (" + code + ")"
		e.Details["providerCode"] = code
	}
	if c.Classify != nil {
		e.Code = c.Classify(status, code)
	}
	if e.Code == "" {
		e.Code = Classify(status)
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		e.Message = c.Provider + ": credentials were rejected or lack permission; check the pool's credential reference and its role assignment"
		if code != "" {
			e.Message += " (" + code + ")"
		}
	}
	return e
}

// Classify is the default status mapping, the same as httpx's: 404
// not_found, 429 insufficient_capacity, 409 invalid_state, 400 and 422
// invalid_request, everything else (401, 403, 5xx) provider_unavailable.
func Classify(status int) string {
	switch status {
	case http.StatusNotFound:
		return api.CodeNotFound
	case http.StatusTooManyRequests:
		return adapter.CodeInsufficientCapacity
	case http.StatusConflict:
		return api.CodeInvalidState
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return api.CodeInvalidRequest
	}
	return api.CodeProviderUnavailable
}

// ProviderError extracts the provider's error code and message from the
// shapes cloud APIs use:
//
//	{"error": {"code": "SessionNotFound", "message": "..."}}         Azure
//	{"error": {"code": 404, "message": "...", "status": "NOT_FOUND"}} Google
//	x-amzn-ErrorType: ThrottlingException, {"message": "..."}        AWS
//	{"code": "...", "message": "..."} or {"error": "..."}             others
func ProviderError(h http.Header, raw []byte) (code, msg string) {
	var body struct {
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
		Type    string          `json:"__type"`
		Error   json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal(raw, &body)
	msg = body.Message
	code = str(body.Code)
	if len(body.Error) > 0 {
		var nested struct {
			Code    json.RawMessage `json:"code"`
			Message string          `json:"message"`
			Status  json.RawMessage `json:"status"`
		}
		if json.Unmarshal(body.Error, &nested) == nil {
			msg = nested.Message
			// Google sends a numeric code and a symbolic status; prefer the symbol.
			code = str(nested.Status)
			if code == "" || isNumber(code) {
				code = str(nested.Code)
			}
		} else if s := str(body.Error); s != "" {
			msg = s
		}
	}
	if t := h.Get("X-Amzn-Errortype"); t != "" {
		code = strings.SplitN(t, ":", 2)[0]
	} else if body.Type != "" {
		code = body.Type[strings.LastIndex(body.Type, "#")+1:]
	}
	if isNumber(code) {
		code = ""
	}
	return code, msg
}

func str(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}

func isNumber(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
