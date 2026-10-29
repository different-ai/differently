package config

import (
	"testing"

	"github.com/different-ai/differently/pkg/api"
)

func TestPlatformProvidersAreExternal(t *testing.T) {
	c, err := Parse([]byte(`
pools:
  - name: vercel
    adapter: vercel
    trust: private   # ignored: Vercel runs the sandboxes
    config: {project: prj_1}
    credentials: {secret: vercel-token}
  - name: fly
    adapter: fly
    config: {app: dly-sandboxes}
    credentials: {env: FLY_API_TOKEN}
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range c.Pools {
		if p.Trust != api.TrustExternal {
			t.Errorf("pool %s: trust %q, want external", p.Name, p.Trust)
		}
	}
}
