package server_test

import (
	"testing"

	"github.com/different-ai/differently/pkg/api"
)

// A negative ttlSeconds used to create a sandbox that never expires, under any
// maxTTLSeconds quota, and a PATCH to 0 expired a running sandbox at once.
func TestInvalidTTLIsRejected(t *testing.T) {
	h := newHarness(t, api.PlacementPolicy{})
	_, _, token := h.tenant("acme", api.Quota{MaxTTLSeconds: 600})
	for _, ttl := range []int{-1, -3600} {
		var env api.ErrorEnvelope
		if code := h.call("POST", "/v1/sandboxes", token, api.CreateSandboxRequest{TTLSeconds: ttl}, &env); code != 400 || env.Error == nil || env.Error.Code != api.CodeInvalidRequest {
			t.Fatalf("create with ttlSeconds %d: status %d, body %+v", ttl, code, env.Error)
		}
	}
	sb := h.create(token, api.CreateSandboxRequest{TTLSeconds: 300})
	h.tick()
	for _, ttl := range []int{0, -1} {
		if code := h.call("PATCH", "/v1/sandboxes/"+sb.ID, token, map[string]int{"ttlSeconds": ttl}, nil); code != 400 {
			t.Fatalf("update to ttlSeconds %d: status %d, want 400", ttl, code)
		}
	}
	h.tick()
	if got := h.get(token, sb.ID); got.Status != api.StatusRunning || got.TTLSeconds != 300 {
		t.Fatalf("a rejected TTL update changed the sandbox: %+v", got)
	}
	h.must(200, "PATCH", "/v1/sandboxes/"+sb.ID, token, map[string]int{"ttlSeconds": 120}, nil)
	if got := h.get(token, sb.ID); got.TTLSeconds != 120 {
		t.Fatalf("a valid TTL update was not applied: %+v", got)
	}
}
