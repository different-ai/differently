// Package config loads and validates the server configuration.
//
// Secrets never appear in the file: the database DSN, bootstrap token and
// backend credentials are referenced by environment variable or file path
// and resolved at runtime, so the config can live in a ConfigMap or in Git.
package config

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/different-ai/differently/internal/store"
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
	CORS            CORSConfig           `json:"cors"`
}

// StoreConfig selects persistence.
type StoreConfig struct {
	// Driver names a registered store driver: memory and postgres in the
	// default build. See internal/store.Register.
	Driver string `json:"driver"`
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
	// MaxRunSeconds pauses (or stops) sandboxes that have run this long
	// without a break, so long runners give their slot back (0: off unless a
	// request asks). At least 60 when set.
	MaxRunSeconds int `json:"maxRunSeconds"`
	// MaxPausedSeconds deletes sandboxes left paused or stopped this long
	// (0: off unless a request asks). At least 60 when set.
	MaxPausedSeconds int `json:"maxPausedSeconds"`
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
	// Preemption lets the scheduler pause running sandboxes to make room for
	// waiting work (pools that checkpoint on pause only).
	Preemption *api.Preemption `json:"preemption,omitempty"`
	// Attestations record what whoever runs this pool is certified for or
	// has signed (soc2-type2, hipaa-baa, ...). Placement requires them where
	// a policy, classification or request says so.
	Attestations map[string]api.Attestation `json:"attestations,omitempty"`
	// Budget caps the pool's estimated spend per calendar month. It needs a
	// costModel, which is what the estimate is computed from.
	Budget *api.Budget `json:"budget,omitempty"`
}

// ToPool converts to the API representation.
func (p PoolConfig) ToPool() api.Pool {
	enabled := p.Enabled == nil || *p.Enabled
	return api.Pool{
		Name: p.Name, Adapter: p.Adapter, Trust: p.Trust, Region: p.Region, Labels: p.Labels,
		Enabled: enabled, MaxConcurrency: p.MaxConcurrency, CostModel: p.CostModel, Config: p.Config,
		CredentialRef: p.Credentials.String(), Source: "config", Preemption: p.Preemption,
		Attestations: p.Attestations, Budget: normalBudget(p.Budget),
	}
}

// normalBudget stores "no budget" one way: nil.
func normalBudget(b *api.Budget) *api.Budget {
	if b == nil || b.Monthly == 0 {
		return nil
	}
	return b
}

// ValidateBudget checks a pool's budget against the cost model its spend is
// estimated from. A budget on a pool with no prices would never be reached,
// so it is refused rather than silently doing nothing.
func ValidateBudget(b *api.Budget, cost api.CostModel) error {
	switch {
	case b == nil || b.Monthly == 0:
		return nil
	case b.Monthly < 0 || math.IsNaN(b.Monthly) || math.IsInf(b.Monthly, 0):
		return fmt.Errorf("budget.monthly must be a positive amount, not %v", b.Monthly)
	case cost.CPUHour <= 0 && cost.MemoryGiBHour <= 0 && cost.DiskGiBHour <= 0 && cost.SandboxHour <= 0:
		return errors.New("a budget needs a costModel: spend is estimated from the pool's prices, and without any it would always be 0")
	}
	return nil
}

// ValidateAttestationNames checks names used to declare or require
// attestations.
func ValidateAttestationNames(names ...string) error {
	for _, n := range names {
		if !api.ValidAttestationName(n) {
			return fmt.Errorf("attestation name %q must be 1 to 63 lowercase letters, digits, '.', '_' or '-', starting with a letter or digit", n)
		}
	}
	return nil
}

// ValidateAttestations checks the attestations declared on a pool.
func ValidateAttestations(m map[string]api.Attestation) error {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	slices.Sort(names)
	return ValidateAttestationNames(names...)
}

// ValidatePreemption checks a pool's preemption policy.
func ValidatePreemption(p *api.Preemption) error {
	if p == nil {
		return nil
	}
	switch p.Mode {
	case "", api.PreemptOff, api.PreemptIdle, api.PreemptLRU:
	default:
		return fmt.Errorf("preemption.mode must be off, idle or lru, not %q", p.Mode)
	}
	if p.MinRunSeconds < 0 || p.IdleSeconds < 0 {
		return fmt.Errorf("preemption seconds must not be negative")
	}
	return nil
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

// CORSConfig lets browser apps on other origins call the API, such as the
// console served from its own host. Requests authenticate with bearer
// tokens, not cookies, so an allowed origin acts only with a key its user
// enters.
type CORSConfig struct {
	// AllowedOrigins are exact origins (scheme://host[:port]); no wildcards.
	AllowedOrigins []string `json:"allowedOrigins,omitempty"`
}

// DashboardConfig toggles the built-in web dashboard.
type DashboardConfig struct {
	Enabled *bool `json:"enabled,omitempty"`
}

// On reports whether the dashboard is served.
func (d DashboardConfig) On() bool { return d.Enabled == nil || *d.Enabled }

// Adapter kinds that always run on third-party infrastructure.
var externalKinds = []string{"daytona", "freestyle", "vercel", "fly", "boat", "morph", "blaxel", "groundbox"}

// DefaultAdapters lists the adapter kinds in every build: the backends
// Differently has run against for real, the remote plugin protocol, and the
// two dev-mode kinds.
var DefaultAdapters = []string{"opensandbox", "daytona", "freestyle", "remote", "devhost", "fake"}

// ExperimentalTag is the Go build tag that compiles ExperimentalAdapters in.
const ExperimentalTag = "experimental_adapters"

// ExperimentalAdapters lists the adapter kinds left out of the default build.
// Apart from agent-sandbox, which ran live against a kind cluster, they are
// tested only against fakes of each provider's published API. A server built
// with -tags experimental_adapters has them.
var ExperimentalAdapters = []string{"vercel", "fly", "agent-sandbox", "cubesandbox", "aws-agentcore", "azure-sandboxes", "azure-sessions", "gcp-sandboxes", "northflank", "boat", "e2b", "morph", "blaxel", "groundbox"}

// KnownAdapters lists adapter kinds this binary can build.
var KnownAdapters = knownAdapters()

func knownAdapters() []string {
	if experimentalBuilt {
		return slices.Concat(DefaultAdapters, ExperimentalAdapters)
	}
	return slices.Clone(DefaultAdapters)
}

// ExperimentalBuilt reports whether this binary has the experimental adapters.
func ExperimentalBuilt() bool { return experimentalBuilt }

// UnavailableAdapter says why this binary cannot build an adapter of kind:
// an experimental kind names the build tag that adds it. It returns "" for a
// kind in KnownAdapters.
func UnavailableAdapter(kind string) string {
	switch {
	case slices.Contains(KnownAdapters, kind):
		return ""
	case slices.Contains(ExperimentalAdapters, kind):
		return fmt.Sprintf("the %s adapter is experimental and not in this build of differently-server; build the server with -tags %s to use it (docs/providers/README.md)", kind, ExperimentalTag)
	}
	return fmt.Sprintf("unknown adapter %q (known: %s)", kind, strings.Join(KnownAdapters, ", "))
}

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
	c, err := Read(path)
	if err != nil {
		return c, err
	}
	return c, c.Validate()
}

// Read reads and defaults a YAML or JSON config file without validating
// it, for a caller that changes the result first (command-line overrides
// such as --dev, which decide whether a devhost pool is allowed). That
// caller must call Validate before using the config.
func Read(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return decode(b)
}

// Parse defaults and validates raw YAML or JSON.
func Parse(b []byte) (Config, error) {
	c, err := decode(b)
	if err != nil {
		return c, err
	}
	return c, c.Validate()
}

func decode(b []byte) (Config, error) {
	var c Config
	if err := yaml.UnmarshalStrict(b, &c); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	return c, nil
}

// Validate reports every configuration problem at once.
func (c Config) Validate() error {
	if d := c.Defaults.IdleTimeoutSeconds; d != 0 && d < 60 {
		return fmt.Errorf("defaults.idleTimeoutSeconds must be 0 (off) or at least 60, not %d", d)
	}
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	for name, d := range map[string]int{"maxRunSeconds": c.Defaults.MaxRunSeconds, "maxPausedSeconds": c.Defaults.MaxPausedSeconds} {
		if d != 0 && d < 60 {
			bad("defaults.%s must be 0 (off) or at least 60, not %d", name, d)
		}
	}
	for _, p := range c.Pools {
		if err := ValidatePreemption(p.Preemption); err != nil {
			bad("pool %s: %v", p.Name, err)
		}
		if err := ValidateAttestations(p.Attestations); err != nil {
			bad("pool %s: %v", p.Name, err)
		}
		if err := ValidateBudget(p.Budget, p.CostModel); err != nil {
			bad("pool %s: %v", p.Name, err)
		}
	}
	if err := ValidateAttestationNames(c.Policy.RequiredAttestations...); err != nil {
		bad("policy.requiredAttestations: %v", err)
	}
	for _, o := range c.CORS.AllowedOrigins {
		u, err := url.Parse(o)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.Contains(u.Host, "*") || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
			bad("cors.allowedOrigins: %q must be an exact origin such as https://console.example.com", o)
		}
	}
	// The drivers are whatever this build registered: the name is checked
	// against them, not against a fixed list.
	if _, ok := store.Lookup(c.Store.Driver); !ok {
		bad("store.driver %q is not in this build (it has: %s)", c.Store.Driver, strings.Join(store.Drivers(), ", "))
	}
	classes := map[string]bool{}
	for _, cl := range c.Classifications {
		classes[cl.Name] = true
		if _, ok := map[string]bool{api.ExternalNever: true, api.ExternalOverflow: true, api.ExternalAllowed: true}[cl.External]; !ok {
			bad("classification %s: external must be never, overflow or allowed", cl.Name)
		}
		if err := ValidateAttestationNames(cl.RequiredAttestations...); err != nil {
			bad("classification %s: %v", cl.Name, err)
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
		if why := UnavailableAdapter(p.Adapter); why != "" {
			bad("pool %s: %s", p.Name, why)
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
			bad("pool %s: the devhost adapter has no isolation and is only allowed in dev mode (dev: true or --dev)", p.Name)
		}
		if p.Adapter == "remote" && p.Config["endpoint"] == "" {
			bad("pool %s: remote adapter needs config.endpoint", p.Name)
		}
	}
	templates := map[string]bool{}
	for _, t := range c.Templates {
		if templates[t.Name] && t.Name != "" {
			bad("duplicate template %s", t.Name)
		}
		templates[t.Name] = true
		errs = append(errs, ValidateTemplate(t)...)
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

// ValidateTemplate reports what is wrong with one template. The config file
// and the admin API (PUT /v1/admin/templates/{name}) hold a template to the
// same rules: it has a name, no image is empty, and images name only pools
// its pools list allows.
func ValidateTemplate(t api.Template) []error {
	if t.Name == "" {
		return []error{errors.New("every template needs a name")}
	}
	var errs []error
	pools := make([]string, 0, len(t.Images))
	for pool := range t.Images {
		pools = append(pools, pool)
	}
	slices.Sort(pools)
	for _, pool := range pools {
		if t.Images[pool] == "" {
			errs = append(errs, fmt.Errorf("template %s: images.%s is empty", t.Name, pool))
		}
		if len(t.Pools) > 0 && !slices.Contains(t.Pools, pool) {
			errs = append(errs, fmt.Errorf("template %s: images names pool %s, which its pools list leaves out", t.Name, pool))
		}
	}
	return errs
}

// Template returns a template the config file declares, by name. Templates
// set through the API live in the store; the scheduler's Template and
// Templates give both as one list.
func (c Config) Template(name string) (api.Template, bool) {
	for _, t := range c.Templates {
		if t.Name == name {
			t.Source, t.UpdatedAt, t.UpdatedBy = api.TemplateSourceConfig, nil, ""
			return t, true
		}
	}
	return api.Template{}, false
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
