package server_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/different-ai/differently/internal/config"
	"github.com/different-ai/differently/pkg/api"
)

// A console on an allowed origin gets CORS headers on the API; other origins
// and other paths get none, and a server without origins adds nothing.
func TestCORSAllowsOnlyConfiguredOrigins(t *testing.T) {
	h := newHarnessWith(t, api.PlacementPolicy{}, func(c *config.Config) {
		c.CORS.AllowedOrigins = []string{"https://console.example.com"}
	}, nil)
	send := func(method, path, origin string, hdr map[string]string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, h.srv.URL+path, nil)
		req.Header.Set("Origin", origin)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}

	pre := send(http.MethodOptions, "/v1/sandboxes", "https://console.example.com", map[string]string{
		"Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "authorization, content-type",
	})
	if pre.StatusCode != http.StatusNoContent || pre.Header.Get("Access-Control-Allow-Origin") != "https://console.example.com" ||
		!strings.Contains(pre.Header.Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Fatalf("preflight: %d %v", pre.StatusCode, pre.Header)
	}
	got := send(http.MethodGet, "/v1/me", "https://console.example.com", map[string]string{"Authorization": "Bearer " + operatorToken})
	if got.StatusCode != http.StatusOK || got.Header.Get("Access-Control-Allow-Origin") != "https://console.example.com" ||
		!strings.Contains(got.Header.Get("Access-Control-Expose-Headers"), "X-Request-Id") {
		t.Fatalf("allowed GET: %d %v", got.StatusCode, got.Header)
	}
	if other := send(http.MethodGet, "/v1/me", "https://evil.example", map[string]string{"Authorization": "Bearer " + operatorToken}); other.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("another origin got CORS headers: %v", other.Header)
	}
	if off := send(http.MethodGet, "/healthz", "https://console.example.com", nil); off.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("paths outside the API get no CORS headers")
	}

	plain := newHarness(t, api.PlacementPolicy{})
	req, _ := http.NewRequest(http.MethodGet, plain.srv.URL+"/v1/me", nil)
	req.Header.Set("Origin", "https://console.example.com")
	req.Header.Set("Authorization", "Bearer "+operatorToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("no origins configured means no CORS headers")
	}
}
