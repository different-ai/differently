// Package wiring registers the built-in adapter kinds with the pool manager.
package wiring

import (
	"fmt"
	"path/filepath"

	"github.com/different-ai/differently/internal/adapters/daytona"
	"github.com/different-ai/differently/internal/adapters/devhost"
	"github.com/different-ai/differently/internal/adapters/fake"
	"github.com/different-ai/differently/internal/adapters/fly"
	"github.com/different-ai/differently/internal/adapters/freestyle"
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
