package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/different-ai/differently/pkg/api"
)

func TestReportErrorNeverSilent(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"usage", errors.New("unknown flag: --org"), "error: unknown flag: --org\n"},
		{"network", fmt.Errorf("get /v1/me: %w", errors.New("connection refused")), "error: get /v1/me: connection refused\n"},
		{"api", api.Errorf(api.CodePermissionDenied, "operator (admin) key required").With("scope", "admin"),
			"error [permission_denied]: operator (admin) key required\n  scope: admin\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out strings.Builder
			reportError(&out, tc.err)
			if out.String() != tc.want {
				t.Fatalf("got %q, want %q", out.String(), tc.want)
			}
		})
	}
}

// The dev operator token goes to this machine only.
func TestDevTokenOnlyForLoopback(t *testing.T) {
	for raw, want := range map[string]bool{
		"http://localhost:8080":               true,
		"http://LOCALHOST:8080/":              true,
		"http://api.localhost:8080":           true,
		"http://127.0.0.1:18180":              true,
		"http://[::1]:8080":                   true,
		"https://localhost.example.com":       false,
		"https://differently.example.com":     false,
		"https://example.com/localhost":       false,
		"https://example.com/?next=localhost": false,
		"https://notlocalhost":                false,
		"http://10.0.0.5:8080":                false,
		"://bad":                              false,
	} {
		if got := isLoopbackURL(raw); got != want {
			t.Errorf("isLoopbackURL(%q) = %v, want %v", raw, got, want)
		}
	}
}
