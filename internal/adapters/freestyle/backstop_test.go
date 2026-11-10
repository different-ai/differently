package freestyle

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/different-ai/differently/pkg/adapter"
)

// Freestyle's deletion deadline counts from the create request and
// Differently's TTL from the moment the VM is ready, so the deadline is sent
// with a grace: Differently's own, audited expiry must run first.
func TestFreestyleBackstopOutlastsTheTTL(t *testing.T) {
	var body map[string]any
	vm := map[string]any{"id": "vm-1", "state": "running", "metadata": map[string]string{metaSandbox: "sbx_ttl", metaInstallation: "inst_1"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v5/vms":
			_ = json.NewDecoder(r.Body).Decode(&body)
			json.NewEncoder(w).Encode(vm)
		case r.Method == http.MethodGet && body != nil:
			json.NewEncoder(w).Encode(vm)
		default:
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"message": "not found"})
		}
	}))
	defer srv.Close()
	a := New(Options{APIURL: srv.URL, APIKey: "k", ReadyTimeout: 5 * time.Second})
	// The VM comes up; later setup steps need endpoints this server lacks.
	_, _ = a.Create(context.Background(), adapter.CreateRequest{ID: "sbx_ttl", Installation: "inst_1", TTLSeconds: 600})
	if got := body["ttlSeconds"]; got != float64(600+backstopGrace) {
		t.Fatalf("ttlSeconds %v for a 600 s TTL, want %d (the TTL plus the grace)", got, 600+backstopGrace)
	}
}
