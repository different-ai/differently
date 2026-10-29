package httpx

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

// ErrorShape wraps next so that a provider's error bodies (any status of 300
// or more) reach the Client in the flat {"code", "message"} shape it reads.
// Use it for providers that nest their error object or name the code field
// differently. flatten returns empty strings to leave a body as it is.
func ErrorShape(next http.RoundTripper, flatten func(raw []byte) (code, message string)) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp, err := next.RoundTrip(r)
		if err != nil || resp.StatusCode < 300 {
			return resp, err
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if code, msg := flatten(raw); code != "" || msg != "" {
			raw, _ = json.Marshal(map[string]string{"code": code, "message": msg})
		}
		resp.Body = io.NopCloser(bytes.NewReader(raw))
		resp.ContentLength = int64(len(raw))
		return resp, nil
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
