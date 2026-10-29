// Package config loads and validates the server configuration.
//
// Secrets never appear in the file: the database DSN, bootstrap token and
// backend credentials are referenced by environment variable or file path
// and resolved at runtime, so the config can live in a ConfigMap or in Git.
package config

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/different-ai/differently/pkg/api"
)

// Config is the server configuration file.
type Config struct {
	Listen          string               `json:"listen"`
	PublicURL       string               `json:"publicURL,omitempty"`
	Dev             bool                 `json:"dev,omitempty"`
	DataDir         string               `json:"dataDir,omitempty"`
	Store           StoreConfig          `json:"store"`
	Auth            AuthConfig           `json:"auth"`
	Secrets         SecretsConfig        `json:"secrets"`
	Defaults        Defaults             `json:"defaults"`
	Classifications []api.Classification `json:"classifications"`
	Profiles        []api.Profile        `json:"profiles"`
	Templates       []api.Template       `json:"templates,omitempty"`
	Pools           []PoolConfig         `json:"pools"`
	Policy          api.PlacementPolicy  `json:"policy"`
	Scheduler       SchedulerConfig      `json:"scheduler"`
	RateLimit       RateLimitConfig      `json:"rateLimit"`
	Audit           AuditConfig          `json:"audit"`
	Metrics         MetricsConfig        `json:"metrics"`
	Dashboard       DashboardConfig      `json:"dashboard"`
}

// StoreConfig selects persistence.
type StoreConfig struct {
	Driver string `json:"driver"` // memory | postgres
	DSNEnv string `json:"dsnEnv,omitempty"`
}

// SecretsConfig names where the key that seals stored provider credentials
// comes from. Without a key, credentials can still be referenced as env: or
// file:, but none can be stored in Differently.
type SecretsConfig struct {
	KeyEnv  string `json:"keyEnv"`
	KeyFile string `json:"keyFile,omitempty"`
}

// AuthConfig names the environment variable holding the bootstrap secret.
type AuthConfig struct {
	BootstrapTokenEnv string `json:"bootstrapTokenEnv"`
}

// Defaults apply when an organization, project or request omits a value.
type Defaults struct {
	OrgQuota       api.Quota     `json:"orgQuota"`
	ProjectQuota   api.Quota     `json:"projectQuota"`
	Classification string        `json:"classification"`
	Profile        string        `json:"profile"`
	Template       string        `json:"template,omitempty"`
	Resources      api.Resources `json:"resources"`
	TTLSeconds     int           `json:"ttlSeconds"`
	// IdleTimeoutSeconds stops or pauses sandboxes left idle this long
	// (0: off unless a request asks). At least 60 when set.
	IdleTimeoutSeconds int `json:"idleTimeoutSeconds"`
}

// CredentialRef points at a secret without containing it.
type CredentialRef struct {
	Env  string `json:"env,omitempty"`
	File string `json:"file,omitempty"`
	// Secret names a credential stored in Differently (PUT /v1/admin/secrets/{name}).
	Secret string `json:"secret,omitempty"`
}

// String renders the reference for display.
func (c CredentialRef) String() string {
	switch {
	case c.Env != "":
		return "env:" + c.Env
	case c.File != "":
		return "file:" + c.File
	case c.Secret != "":
		return "secret:" + c.Secret
	}
	return ""
}

// ParseCredentialRef parses "env:NAME", "file:/path" or "secret:name".
func ParseCredentialRef(s string) CredentialRef {
	switch {
	case strings.HasPrefix(s, "env:"):
		return CredentialRef{Env: strings.TrimPrefix(s, "env:")}
	case strings.HasPrefix(s, "file:"):
		return CredentialRef{File: strings.TrimPrefix(s, "file:")}
	case strings.HasPrefix(s, "secret:"):
		return CredentialRef{Secret: strings.TrimPrefix(s, "secret:")}
	}
	return CredentialRef{}
}

// ErrStoredSecret reports a secret: reference, which only the server can
// resolve (it needs the store and the secrets key).
var ErrStoredSecret = errors.New("stored secrets resolve through the server")

// Resolve reads the secret. A missing reference resolves to "".
func (c CredentialRef) Resolve() (string, error) {
	switch {
	case c.Env != "":
		v := os.Getenv(c.Env)
		if v == "" {
			return "", fmt.Errorf("environment variable %s is empty", c.Env)
		}
		return v, nil
	case c.File != "":
		b, err := os.ReadFile(c.File)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	case c.Secret != "":
		return "", ErrStoredSecret
	}
	return "", nil
}

// PoolConfig declares a pool in the config file.
type PoolConfig struct {
	Name           string            `json:"name"`
	Adapter        string            `json:"adapter"`
	Trust          string            `json:"trust,omitempty"`
	Region         string            `json:"region,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	Enabled        *bool             `json:"enabled,omitempty"`
	MaxConcurrency int               `json:"maxConcurrency,omitempty"`
	CostModel      api.CostModel     `json:"costModel,omitempty"`
	Config         map[string]string `json:"config,omitempty"`
	Credentials    CredentialRef     `json:"credentials,omitempty"`
}

// ToPool converts to the API representation.
func (p PoolConfig) ToPool() api.Pool {
	enabled := p.Enabled == nil || *p.Enabled
	return api.Pool{
		Name: p.Name, Adapter: p.Adapter, Trust: p.Trust, Region: p.Region, Labels: p.Labels,
		Enabled: enabled, MaxConcurrency: p.MaxConcurrency, CostModel: p.CostModel, Config: p.Config,
		CredentialRef: p.Credentials.String(), Source: "config",
	}
}

// SchedulerConfig tunes the dispatch and reconcile loops.
type SchedulerConfig struct {
	IntervalMs              int `json:"intervalMs"`
	ProvisionTimeoutSeconds int `json:"provisionTimeoutSeconds"`
	MaxAttempts             int `json:"maxAttempts"`
	ReconcileIntervalSecs   int `json:"reconcileIntervalSeconds"`
}

// RateLimitConfig is a per-key token bucket.
type RateLimitConfig struct {
	RequestsPerSecond float64 `json:"requestsPerSecond"`
	Burst             int     `json:"burst"`
}

// AuditConfig optionally mirrors audit events to a JSON-lines file.
type AuditConfig struct {
	File string `json:"file,omitempty"`
	// RecordCommands stores exec command lines in audit events. Commands can
	// contain secrets, so the default records only the program name.
	RecordCommands bool `json:"recordCommands,omitempty"`
	// FailClosed refuses mutating API requests (503) when the audit event
	// recording them cannot be stored. The default logs and counts the
	// failure and lets the request proceed.
	FailClosed bool `json:"failClosed,omitempty"`
}

// MetricsConfig controls where Prometheus metrics are served.
type MetricsConfig struct {
	// Listen serves /metrics on its own address (for example ":9091") and
	// removes it from the API listener, so it can stay off the ingress.
	// Empty serves /metrics on the API listener without authentication.
	Listen string `json:"listen,omitempty"`
}

// DashboardConfig toggles the built-in web dashboard.
type DashboardConfig struct {
	Enabled *bool `json:"enabled,omitempty"`
}

// On reports whether the dashboard is served.
func (d DashboardConfig) On() bool { return d.Enabled == nil || *d.Enabled }

// Adapter kinds that always run on third-party infrastructure.
var externalKinds = []string{"daytona", "freestyle", "vercel", "fly", "boat", "morph", "blaxel"}

// KnownAdapters lists adapter kinds the server can build.
var KnownAdapters = []string{"opensandbox", "daytona", "freestyle", "remote", "devhost", "fake", "vercel", "fly", "agent-sandbox", "cubesandbox", "aws-agentcore", "azure-sandboxes", "azure-sessions", "gcp-sandboxes", "northflank", "boat", "e2b", "morph", "blaxel"}

// Adapter kinds that run where the operator put them (a plugin they deploy,
// the dev host, the test fake), so they are private unless declared.
var privateByDefaultKinds = []string{"remote", "devhost", "fake"}

// Adapter kinds whose trust boundary depends on who operates the backend, so
// the operator must declare it. Any kind in none of these lists is treated the
// same way: a new adapter never becomes private by omission.
var declaredTrustKinds = []string{"opensandbox", "agent-sandbox", "cubesandbox", "aws-agentcore", "azure-sandboxes", "azure-sessions", "gcp-sandboxes", "northflank", "e2b"}

// Default returns a configuration with every default applied.
func Default() Config {
	c := Config{}
	c.applyDefaults()
	return c
}

// DevDefault is the zero-setup local configuration used by --dev.
func DevDefault(dataDir string) Config {
	c := Config{
		Dev:     true,
		DataDir: dataDir,
		Store:   StoreConfig{Driver: "memory"},
		Pools: []PoolConfig{{
			Name: "local", Adapter: "devhost", Trust: api.TrustPrivate, Region: "local",
			MaxConcurrency: 20, Config: map[string]string{"root": dataDir + "/devhost"},
		}},
	}
	c.applyDefaults()
	return c
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.DataDir == "" {
		c.DataDir = "/var/lib/differently"
	}
	if c.Store.Driver == "" {
		c.Store.Driver = "memory"
	}
	if c.Store.Driver == "postgres" && c.Store.DSNEnv == "" {
		c.Store.DSNEnv = "DIFFERENTLY_DATABASE_URL"
	}
	if c.Auth.BootstrapTokenEnv == "" {
		c.Auth.BootstrapTokenEnv = "DIFFERENTLY_BOOTSTRAP_TOKEN"
	}
	if c.Secrets.KeyEnv == "" && c.Secrets.KeyFile == "" {
		c.Secrets.KeyEnv = "DIFFERENTLY_SECRETS_KEY"
	}
	if len(c.Classifications) == 0 {
		c.Classifications = []api.Classification{
			{Name: "private-only", Description: "Must never leave private pools.", External: api.ExternalNever},
			{Name: "internal", Description: "May overflow to approved external pools.", External: api.ExternalOverflow},
			{Name: "public", Description: "No residency restriction beyond policy.", External: api.ExternalAllowed},
		}
	}
	if c.Defaults.Classification == "" {
		c.Defaults.Classification = "internal"
	}
	if len(c.Profiles) == 0 {
		c.Profiles = []api.Profile{
			{Name: "small", Resources: api.Resources{CPU: 1, MemoryMiB: 1024, DiskGiB: 5}},
			{Name: "medium", Resources: api.Resources{CPU: 2, MemoryMiB: 4096, DiskGiB: 10}},
			{Name: "large", Resources: api.Resources{CPU: 4, MemoryMiB: 8192, DiskGiB: 20}},
		}
	}
	if c.Defaults.Profile == "" {
		c.Defaults.Profile = "small"
	}
	if c.Defaults.TTLSeconds == 0 {
		c.Defaults.TTLSeconds = 3600
	}
	if c.Policy.Scope == "" {
		c.Policy.Scope = "global"
	}
	if c.Policy.External == "" {
		c.Policy.External = api.ExternalNever
	}
	if c.Policy.Strategy == "" {
		c.Policy.Strategy = api.StrategyPrivateFirst
	}
	for i := range c.Pools {
		p := &c.Pools[i]
		if slices.Contains(externalKinds, p.Adapter) {
			p.Trust = api.TrustExternal
		}
		if p.Trust == "" && slices.Contains(privateByDefaultKinds, p.Adapter) {
			p.Trust = api.TrustPrivate
		}
	}
	s := &c.Scheduler
	if s.IntervalMs == 0 {
		s.IntervalMs = 250
	}
	if s.ProvisionTimeoutSeconds == 0 {
		s.ProvisionTimeoutSeconds = 120
	}
	if s.MaxAttempts == 0 {
		s.MaxAttempts = 3
	}
	if s.ReconcileIntervalSecs == 0 {
		s.ReconcileIntervalSecs = 30
	}
	if c.RateLimit.RequestsPerSecond == 0 {
		c.RateLimit.RequestsPerSecond = 50
	}
	if c.RateLimit.Burst == 0 {
		c.RateLimit.Burst = 100
	}
}

// Load reads, defaults and validates a YAML or JSON config file.
func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return Parse(b)
}

// Parse defaults and validates raw YAML or JSON.
func Parse(b []byte) (Config, error) {
	var c Config
	if err := yaml.UnmarshalStrict(b, &c); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	return c, c.Validate()
}

// Validate reports every configuration problem at once.
func (c Config) Validate() error {
	if d := c.Defaults.IdleTimeoutSeconds; d != 0 && d < 60 {
		return fmt.Errorf("defaults.idleTimeoutSeconds must be 0 (off) or at least 60, not %d", d)
	}
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if c.Store.Driver != "memory" && c.Store.Driver != "postgres" {
		bad("store.driver must be memory or postgres, got %q", c.Store.Driver)
	}
	classes := map[string]bool{}
	for _, cl := range c.Classifications {
		classes[cl.Name] = true
		if _, ok := map[string]bool{api.ExternalNever: true, api.ExternalOverflow: true, api.ExternalAllowed: true}[cl.External]; !ok {
			bad("classification %s: external must be never, overflow or allowed", cl.Name)
		}
	}
	if !classes[c.Defaults.Classification] {
		bad("defaults.classification %q is not a defined classification", c.Defaults.Classification)
	}
	profiles := map[string]bool{}
	for _, p := range c.Profiles {
		profiles[p.Name] = true
		if p.Resources.CPU <= 0 || p.Resources.MemoryMiB <= 0 {
			bad("profile %s needs positive cpu and memoryMiB", p.Name)
		}
	}
	if !profiles[c.Defaults.Profile] {
		bad("defaults.profile %q is not a defined profile", c.Defaults.Profile)
	}
	names := map[string]bool{}
	for _, p := range c.Pools {
		switch {
		case p.Name == "":
			bad("every pool needs a name")
		case names[p.Name]:
			bad("duplicate pool %s", p.Name)
		}
		names[p.Name] = true
		if !slices.Contains(KnownAdapters, p.Adapter) {
			bad("pool %s: unknown adapter %q (known: %s)", p.Name, p.Adapter, strings.Join(KnownAdapters, ", "))
		}
		switch {
		case p.Trust == "":
			bad("pool %s: set trust explicitly for the %s adapter: private if you operate it, external if someone else does", p.Name, p.Adapter)
		case p.Trust != api.TrustPrivate && p.Trust != api.TrustExternal:
			bad("pool %s: trust must be private or external", p.Name)
		}
		if p.Adapter == "opensandbox" && p.Config["apiURL"] == "" {
			bad("pool %s: the opensandbox adapter needs config.apiURL", p.Name)
		}
		if p.Adapter == "cubesandbox" && p.Config["apiURL"] == "" {
			bad("pool %s: the cubesandbox adapter needs config.apiURL (CubeAPI, e.g. http://cube-api:3000)", p.Name)
		}
		if p.Adapter == "devhost" && !c.Dev {
			bad("pool %s: the devhost adapter has no isolation and is only allowed with dev: true", p.Name)
		}
		if p.Adapter == "remote" && p.Config["endpoint"] == "" {
			bad("pool %s: remote adapter needs config.endpoint", p.Name)
		}
	}
	templates := map[string]bool{}
	for _, t := range c.Templates {
		switch {
		case t.Name == "":
			bad("every template needs a name")
		case templates[t.Name]:
			bad("duplicate template %s", t.Name)
		}
		templates[t.Name] = true
		for pool, image := range t.Images {
			if image == "" {
				bad("template %s: images.%s is empty", t.Name, pool)
			}
			if len(t.Pools) > 0 && !slices.Contains(t.Pools, pool) {
				bad("template %s: images names pool %s, which its pools list leaves out", t.Name, pool)
			}
		}
	}
	if c.Metrics.Listen != "" && c.Metrics.Listen == c.Listen {
		bad("metrics.listen must differ from listen")
	}
	for _, ep := range c.Policy.ExternalPools {
		if !names[ep] {
			bad("policy.externalPools names unknown pool %s", ep)
		}
	}
	return errors.Join(errs...)
}

// Classification returns a classification by name.
func (c Config) Classification(name string) (api.Classification, bool) {
	for _, cl := range c.Classifications {
		if cl.Name == name {
			return cl, true
		}
	}
	return api.Classification{}, false
}

// Template returns a configured template by name.
func (c Config) Template(name string) (api.Template, bool) {
	for _, t := range c.Templates {
		if t.Name == name {
			return t, true
		}
	}
	return api.Template{}, false
}

// BackendTemplate is what the backend of pool should boot for template name:
// the pool's mapped image for a configured template, otherwise the name as
// given.
func (c Config) BackendTemplate(name, pool string) string {
	if t, ok := c.Template(name); ok && name != "" {
		return t.ImageFor(pool)
	}
	return name
}

// Profile returns a profile by name.
func (c Config) Profile(name string) (api.Profile, bool) {
	for _, p := range c.Profiles {
		if p.Name == name {
			return p, true
		}
	}
	return api.Profile{}, false
}
