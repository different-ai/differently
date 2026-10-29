// Package wiring registers the built-in adapter kinds with the pool manager.
package wiring

import (
	"fmt"
	"path/filepath"

	"github.com/different-ai/differently/internal/adapters/agentcore"
	"github.com/different-ai/differently/internal/adapters/azuresandboxes"
	"github.com/different-ai/differently/internal/adapters/azuresessions"
	"github.com/different-ai/differently/internal/adapters/blaxel"
	"github.com/different-ai/differently/internal/adapters/boat"
	"github.com/different-ai/differently/internal/adapters/daytona"
	"github.com/different-ai/differently/internal/adapters/devhost"
	"github.com/different-ai/differently/internal/adapters/e2b"
	"github.com/different-ai/differently/internal/adapters/fake"
	"github.com/different-ai/differently/internal/adapters/fly"
	"github.com/different-ai/differently/internal/adapters/freestyle"
	"github.com/different-ai/differently/internal/adapters/gcpsandboxes"
	"github.com/different-ai/differently/internal/adapters/morph"
	"github.com/different-ai/differently/internal/adapters/northflank"
	"github.com/different-ai/differently/internal/adapters/opensandbox"
	"github.com/different-ai/differently/internal/adapters/vercel"
	"github.com/different-ai/differently/internal/config"
	"github.com/different-ai/differently/internal/pools"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/adapter/adapterhttp"
	"github.com/different-ai/differently/pkg/api"
)

// Register installs every built-in adapter factory.
func Register() {
	pools.Register("remote", func(p api.Pool, d pools.Deps) (adapter.ExecutionAdapter, error) {
		token, err := resolve(p, d)
		if err != nil {
			return nil, err
		}
		return &remote{Client: adapterhttp.NewClient(p.Config["endpoint"], token), trust: p.Trust}, nil
	})
	pools.Register("daytona", func(p api.Pool, d pools.Deps) (adapter.ExecutionAdapter, error) {
		key, err := resolve(p, d)
		if err != nil {
			return nil, err
		}
		return daytona.New(daytona.FromConfig(p.Config, key)), nil
	})
	pools.Register("freestyle", func(p api.Pool, d pools.Deps) (adapter.ExecutionAdapter, error) {
		key, err := resolve(p, d)
		if err != nil {
			return nil, err
		}
		return freestyle.New(freestyle.FromConfig(p.Config, key)), nil
	})
	pools.Register("vercel", func(p api.Pool, d pools.Deps) (adapter.ExecutionAdapter, error) {
		key, err := resolve(p, d)
		if err != nil {
			return nil, err
		}
		opts, err := vercel.FromConfig(p.Config, key)
		if err != nil {
			return nil, err
		}
		return vercel.New(opts), nil
	})
	pools.Register("fly", func(p api.Pool, d pools.Deps) (adapter.ExecutionAdapter, error) {
		key, err := resolve(p, d)
		if err != nil {
			return nil, err
		}
		opts, err := fly.FromConfig(p.Config, key)
		if err != nil {
			return nil, err
		}
		return fly.New(opts), nil
	})
	pools.Register("blaxel", func(p api.Pool, d pools.Deps) (adapter.ExecutionAdapter, error) {
		key, err := resolve(p, d)
		if err != nil {
			return nil, err
		}
		opts, err := blaxel.FromConfig(p.Config, key)
		if err != nil {
			return nil, err
		}
		return blaxel.New(opts), nil
	})
	pools.Register("boat", func(p api.Pool, d pools.Deps) (adapter.ExecutionAdapter, error) {
		key, err := resolve(p, d)
		if err != nil {
			return nil, err
		}
		return boat.New(boat.FromConfig(p.Config, key)), nil
	})
	pools.Register("e2b", func(p api.Pool, d pools.Deps) (adapter.ExecutionAdapter, error) {
		key, err := resolve(p, d)
		if err != nil {
			return nil, err
		}
		return e2b.New(e2b.FromConfig(p.Config, p.Trust, key)), nil
	})
	pools.Register("morph", func(p api.Pool, d pools.Deps) (adapter.ExecutionAdapter, error) {
		key, err := resolve(p, d)
		if err != nil {
			return nil, err
		}
		return morph.New(morph.FromConfig(p.Config, key)), nil
	})
	pools.Register("aws-agentcore", func(p api.Pool, d pools.Deps) (adapter.ExecutionAdapter, error) {
		cred, err := resolve(p, d)
		if err != nil {
			return nil, err
		}
		opts, err := agentcore.FromConfig(p.Config, p.Region, cred)
		if err != nil {
			return nil, err
		}
		return agentcore.New(opts), nil
	})
	pools.Register("azure-sandboxes", func(p api.Pool, d pools.Deps) (adapter.ExecutionAdapter, error) {
		cred, err := resolve(p, d)
		if err != nil {
			return nil, err
		}
		opts, err := azuresandboxes.FromConfig(p.Config, p.Region, cred)
		if err != nil {
			return nil, err
		}
		return azuresandboxes.New(opts), nil
	})
	pools.Register("azure-sessions", func(p api.Pool, d pools.Deps) (adapter.ExecutionAdapter, error) {
		cred, err := resolve(p, d)
		if err != nil {
			return nil, err
		}
		opts, err := azuresessions.FromConfig(p.Config, p.Region, cred)
		if err != nil {
			return nil, err
		}
		return azuresessions.New(opts), nil
	})
	pools.Register("gcp-sandboxes", func(p api.Pool, d pools.Deps) (adapter.ExecutionAdapter, error) {
		cred, err := resolve(p, d)
		if err != nil {
			return nil, err
		}
		opts, err := gcpsandboxes.FromConfig(p.Config, p.Region, cred)
		if err != nil {
			return nil, err
		}
		return gcpsandboxes.New(opts), nil
	})
	pools.Register("northflank", func(p api.Pool, d pools.Deps) (adapter.ExecutionAdapter, error) {
		token, err := resolve(p, d)
		if err != nil {
			return nil, err
		}
		opts, err := northflank.FromConfig(p.Config, token)
		if err != nil {
			return nil, err
		}
		return northflank.New(opts), nil
	})
	pools.Register("opensandbox", func(p api.Pool, d pools.Deps) (adapter.ExecutionAdapter, error) {
		key, err := resolve(p, d)
		if err != nil {
			return nil, err
		}
		opts, err := opensandbox.FromConfig(p.Config, p.Trust, key)
		if err != nil {
			return nil, err
		}
		return opensandbox.New(opts), nil
	})
	pools.Register("devhost", func(p api.Pool, d pools.Deps) (adapter.ExecutionAdapter, error) {
		if !d.Dev {
			return nil, fmt.Errorf("devhost has no isolation and only runs in dev mode")
		}
		root := p.Config["root"]
		if root == "" {
			root = filepath.Join(d.DataDir, "devhost", p.Name)
		}
		return devhost.New(root)
	})
	pools.Register("fake", func(p api.Pool, d pools.Deps) (adapter.ExecutionAdapter, error) {
		if !d.Dev {
			return nil, fmt.Errorf("the fake adapter only runs in dev mode")
		}
		return fake.New(fake.Options{Trust: p.Trust, Region: p.Region}), nil
	})
}

func resolve(p api.Pool, d pools.Deps) (string, error) {
	if p.CredentialRef == "" {
		return "", nil
	}
	if d.Resolve != nil {
		return d.Resolve(p.CredentialRef)
	}
	return config.ParseCredentialRef(p.CredentialRef).Resolve()
}

// remote wraps the HTTP plugin client, discovering capabilities lazily.
type remote struct {
	*adapterhttp.Client
	trust string
}

func (r *remote) Info() adapter.Info {
	info := r.Client.Info()
	if info.Kind == "" {
		info = adapter.Info{Kind: "remote", Protocol: adapter.ProtocolVersion}
	}
	info.Trust = r.trust // the operator's declaration wins over the plugin's claim
	return info
}

// Endpoint is reported in the trust report.
func (r *remote) Endpoint() string { return r.Client.BaseURL }
