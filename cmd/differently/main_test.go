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
