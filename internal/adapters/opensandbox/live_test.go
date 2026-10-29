//go:build live

package opensandbox

// Live check against a real OpenSandbox server, optionally recording it for
// TestReplayRecordings (see recording_test.go):
//
//	OPENSANDBOX_URL=http://localhost:8090/v1 go test -tags live -v -run TestLiveOpenSandbox ./internal/adapters/opensandbox/
//
// Settings:
//
//	OPENSANDBOX_URL           lifecycle API base URL (required)
//	OPENSANDBOX_API_KEY       server API key (default: test/opensandbox/config.toml)
//	OPENSANDBOX_RUNTIME       docker (default), kubernetes (no snapshot registry)
//	                          or kubernetes-registry: what the server runs
//	OPENSANDBOX_IMAGE         default image (debian:bookworm-slim)
//	OPENSANDBOX_LINE_ENDINGS  kept (default, execd release-1.2.0) or stripped
//	OPENSANDBOX_RECORD        write testdata/recordings/<name>.json
//	OPENSANDBOX_SERVER, OPENSANDBOX_EXECD, OPENSANDBOX_HOST
//	                          versions and host described in the recording

import (
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/different-ai/differently/pkg/api"
)

func TestLiveOpenSandbox(t *testing.T) {
	base := os.Getenv("OPENSANDBOX_URL")
	if base == "" {
		t.Skip("set OPENSANDBOX_URL to a running OpenSandbox lifecycle API")
	}
	env := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	key := env("OPENSANDBOX_API_KEY", "osb-live-test-key-0123456789") // test/opensandbox/config.toml
	// docker, kubernetes (no snapshot registry), kubernetes-registry or
	// fastsandbox (OPENSANDBOX_TEMPLATE, and OPENSANDBOX_PORT_SCHEME=https
	// when its gateway is behind TLS).
	runtime := env("OPENSANDBOX_RUNTIME", "docker")
	cfg := map[string]string{
		"image":       env("OPENSANDBOX_IMAGE", "debian:bookworm-slim"),
		"isolation":   "container",
		"lineEndings": env("OPENSANDBOX_LINE_ENDINGS", "kept"),
	}
	switch runtime {
	case "kubernetes-registry":
		cfg["runtime"] = "kubernetes"
	case runtimeFastSandbox:
		cfg = map[string]string{"runtime": runtimeFastSandbox, "templateId": os.Getenv("OPENSANDBOX_TEMPLATE"), "lineEndings": cfg["lineEndings"]}
		if s := os.Getenv("OPENSANDBOX_PORT_SCHEME"); s != "" {
			cfg["portScheme"] = s
		}
		// The SandboxPool's size (e.g. 2 and 4Gi), which a fork must send.
		if c := os.Getenv("OPENSANDBOX_POOL_CPU"); c != "" {
			cfg["poolCPU"], cfg["poolMemory"] = c, os.Getenv("OPENSANDBOX_POOL_MEMORY")
		}
	}
	// OPENSANDBOX_EGRESS_ALLOW=pypi.org records an allowlist pool.
	if allow := os.Getenv("OPENSANDBOX_EGRESS_ALLOW"); allow != "" {
		cfg["egress"], cfg["egressAllow"] = "allowlist", allow
	}
	// OPENSANDBOX_PORTS=signed with OPENSANDBOX_PORT_SCHEME (http behind a tunnel).
	if os.Getenv("OPENSANDBOX_PORTS") == "signed" {
		cfg["ports"], cfg["portScheme"] = "signed", env("OPENSANDBOX_PORT_SCHEME", "https")
		livePortCheck = func(t *testing.T, u string) {
			t.Helper()
			status := func(u string) int {
				resp, err := http.Get(u + "/")
				if err != nil {
					t.Fatalf("GET %s: %v", u, err)
				}
				resp.Body.Close()
				return resp.StatusCode
			}
			// Nothing listens on 8080, so the gateway answers 5xx once it has
			// let the request through; 401 would mean the signature failed.
			if got := status(u); got == http.StatusUnauthorized || got == http.StatusForbidden {
				t.Fatalf("signed URL refused without headers: %d", got)
			}
			if got := status(u[:len(u)-1] + "x"); got != http.StatusUnauthorized {
				t.Fatalf("a tampered signature should be refused with 401, got %d", got)
			}
		}
	}
	withURL := map[string]string{"apiURL": base}
	for k, v := range cfg {
		withURL[k] = v
	}
	o, err := FromConfig(withURL, api.TrustPrivate, key)
	if err != nil {
		t.Fatal(err)
	}
	a := New(o)
	step := func(string) {}
	var rec *recorder
	name := os.Getenv("OPENSANDBOX_RECORD")
	if name != "" {
		u, err := url.Parse(base)
		if err != nil {
			t.Fatal(err)
		}
		// OPENSANDBOX_GATEWAY_HOST: the ingress gateway execd is reached
		// through when it is not the server's own address (Fast Sandbox).
		rec = &recorder{next: http.DefaultTransport, host: u.Host, gateway: os.Getenv("OPENSANDBOX_GATEWAY_HOST"), secret: key}
		a.c.HTTP.Transport = rec
		step = rec.mark
	}
	start := time.Now()
	scenario(t, a, runtime, step)
	t.Logf("scenario passed against %s (%s runtime) in %s", base, runtime, time.Since(start).Round(time.Second))
	if rec != nil {
		writeRecording(t, name, recording{
			Recorded:  time.Now().UTC().Format("2006-01-02"),
			Server:    env("OPENSANDBOX_SERVER", "opensandbox/server:release-1.2.0"),
			Execd:     env("OPENSANDBOX_EXECD", "opensandbox/execd:release-1.2.0"),
			Runtime:   runtime,
			Host:      os.Getenv("OPENSANDBOX_HOST"),
			Config:    cfg,
			Exchanges: rec.exchanges,
		})
	}
}
