package opensandbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// A container the Docker runtime cannot start concerns that one sandbox,
// never the server: the acid test (L0.06) found an oversized request
// answered as provider_unavailable, which counts against the pool's health.
// The bodies are what OpenSandbox release-1.2.0 sends.
func TestCreateStartFailureIsTheSandboxsFault(t *testing.T) {
	for _, c := range []struct {
		message, want string
	}{
		{`Failed to create or start container: 400 Client Error for http+docker://localhost/v1.54/containers/create?name=sandbox-1: Bad Request ("range of CPUs is from 0.01 to 8.00, as there are only 8 CPUs available")`, api.CodeInvalidRequest},
		{`Failed to create or start container: 500 Server Error for http+docker://localhost/v1.54/containers/sandbox-1/start: Internal Server Error ("failed to set up container networking")`, adapter.CodeSandboxFailed},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodGet {
				json.NewEncoder(w).Encode(map[string]any{"items": []any{}, "pagination": map[string]any{"page": 1, "pageSize": 100, "totalItems": 0, "totalPages": 1, "hasNextPage": false}})
				return
			}
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"code": "DOCKER::SANDBOX_START_FAILED", "message": c.message})
		}))
		a := New(Options{APIURL: srv.URL + "/v1", APIKey: apiKey, Image: "debian:bookworm-slim", UseServerProxy: true})
		_, err := a.Create(context.Background(), adapter.CreateRequest{ID: "sbx_1", Installation: "inst", Resources: api.Resources{CPU: 4096, MemoryMiB: 1024}})
		if !api.IsCode(err, c.want) {
			t.Errorf("start failure answers %v, want %s", err, c.want)
		}
		srv.Close()
	}
}
