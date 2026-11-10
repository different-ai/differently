//go:build experimental_adapters

package wiring

import (
	"github.com/different-ai/differently/internal/adapters/agentcore"
	"github.com/different-ai/differently/internal/adapters/agentsandbox"
	"github.com/different-ai/differently/internal/adapters/azuresandboxes"
	"github.com/different-ai/differently/internal/adapters/azuresessions"
	"github.com/different-ai/differently/internal/adapters/blaxel"
	"github.com/different-ai/differently/internal/adapters/boat"
	"github.com/different-ai/differently/internal/adapters/cubesandbox"
	"github.com/different-ai/differently/internal/adapters/e2b"
	"github.com/different-ai/differently/internal/adapters/fly"
	"github.com/different-ai/differently/internal/adapters/gcpsandboxes"
	"github.com/different-ai/differently/internal/adapters/morph"
	"github.com/different-ai/differently/internal/adapters/northflank"
	"github.com/different-ai/differently/internal/adapters/vercel"
	"github.com/different-ai/differently/internal/pools"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// registerExperimental installs the adapter kinds listed in
// config.ExperimentalAdapters. Apart from agent-sandbox, which ran live
// against a kind cluster, they are tested only against fakes of each
// provider's published API, so they stay out of the default build.
func registerExperimental() {
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
	pools.Register("agent-sandbox", func(p api.Pool, d pools.Deps) (adapter.ExecutionAdapter, error) {
		token, err := resolve(p, d)
		if err != nil {
			return nil, err
		}
		opts, err := agentsandbox.FromConfig(p.Config, p.Trust, token)
		if err != nil {
			return nil, err
		}
		return agentsandbox.New(opts)
	})
	pools.Register("cubesandbox", func(p api.Pool, d pools.Deps) (adapter.ExecutionAdapter, error) {
		key, err := resolve(p, d)
		if err != nil {
			return nil, err
		}
		opts, err := cubesandbox.FromConfig(p.Config, p.Trust, key)
		if err != nil {
			return nil, err
		}
		return cubesandbox.New(opts)
	})
}
