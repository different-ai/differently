package config

import (
	"strings"
	"testing"

	"github.com/different-ai/differently/pkg/api"
)

// Cloud sandbox services run in the operator's own cloud account, so the
// operator states the trust: external, or private if their policy says so.
func TestCloudAccountKindsDeclareTrust(t *testing.T) {
	for _, kind := range []string{"aws-agentcore", "azure-sandboxes", "azure-sessions", "gcp-sandboxes", "northflank"} {
		pool := "pools:\n  - {name: p, adapter: " + kind + ", credentials: {env: KEY}"
		if _, err := Parse([]byte(pool + "}\n")); err == nil || !strings.Contains(err.Error(), "set trust explicitly") {
			t.Errorf("%s without trust: %v", kind, err)
		}
		for _, trust := range []string{api.TrustExternal, api.TrustPrivate} {
			c, err := Parse([]byte(pool + ", trust: " + trust + "}\n"))
			if err != nil || c.Pools[0].Trust != trust {
				t.Errorf("%s with trust %s: %+v %v", kind, trust, c.Pools, err)
			}
		}
	}
}
