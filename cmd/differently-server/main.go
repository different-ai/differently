// Command differently-server runs the Differently control plane: the API,
// scheduler, reconciler and dashboard.
//
//	differently-server [--config FILE] [--dev]   serve
//	differently-server migrate                    apply database migrations
//	differently-server preflight                  validate config and environment
//	differently-server token                      print a new random API token
//	differently-server secrets-key                print a new key for sealing stored credentials
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/different-ai/differently/internal/auditlog"
	"github.com/different-ai/differently/internal/auth"
	"github.com/different-ai/differently/internal/config"
	"github.com/different-ai/differently/internal/demo"
	"github.com/different-ai/differently/internal/pools"
	"github.com/different-ai/differently/internal/scheduler"
	"github.com/different-ai/differently/internal/secrets"
	"github.com/different-ai/differently/internal/server"
	"github.com/different-ai/differently/internal/stacks"
	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/internal/store/postgres"
	"github.com/different-ai/differently/internal/version"
	"github.com/different-ai/differently/internal/wiring"
)

func main() {
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("differently-server", flag.ExitOnError)
	cfgPath := fs.String("config", os.Getenv("DIFFERENTLY_CONFIG"), "path to the YAML config file")
	dev := fs.Bool("dev", false, "zero-setup local mode: memory store and a devhost pool with NO isolation")
	demoMode := fs.Bool("demo", false, "demo mode: mock connectors and generated traffic, nothing real is called (implies --dev)")
	listen := fs.String("listen", "", "override the listen address")
	dataDir := fs.String("data-dir", "", "override the data directory")
	_ = fs.Parse(args)

	var err error
	switch cmd {
	case "serve":
		err = serve(*cfgPath, *dev || *demoMode, *demoMode, *listen, *dataDir)
	case "migrate":
		err = migrate(*cfgPath)
	case "preflight":
		err = preflight(*cfgPath)
	case "token":
		_, token, _ := auth.NewToken()
		fmt.Println(token)
	case "secrets-key":
		fmt.Println(secrets.NewKey())
	case "version":
		fmt.Println(version.Version, version.Commit)
	default:
		err = fmt.Errorf("unknown command %q (serve, migrate, preflight, token, secrets-key, version)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func loadConfig(path string, dev bool, dataDir string) (config.Config, error) {
	if path == "" {
		if !dev {
			return config.Config{}, errors.New("--config is required (or --dev for local development)")
		}
		if dataDir == "" {
			home, _ := os.UserHomeDir()
			dataDir = filepath.Join(home, ".differently", "dev")
		}
		return config.DevDefault(dataDir), nil
	}
	// The flags change what is valid (a devhost pool needs dev mode), so
	// apply them first and validate once, after them.
	cfg, err := config.Read(path)
	if err != nil {
		return cfg, err
	}
	if dev {
		cfg.Dev = true
	}
	if dataDir != "" {
		cfg.DataDir = dataDir
	}
	return cfg, cfg.Validate()
}

func openStore(ctx context.Context, cfg config.Config) (store.Store, error) {
	if cfg.Store.Driver == "postgres" {
		dsn := os.Getenv(cfg.Store.DSNEnv)
		if dsn == "" {
			return nil, fmt.Errorf("store.driver is postgres but %s is empty", cfg.Store.DSNEnv)
		}
		return postgres.Open(ctx, dsn)
	}
	return store.NewMemory(), nil
}

func serve(cfgPath string, dev, demoMode bool, listen, dataDir string) error {
	cfg, err := loadConfig(cfgPath, dev, dataDir)
	if demoMode {
		// The demo is its own installation: a config file would mix real
		// pools with generated traffic.
		if cfgPath != "" {
			return errors.New("--demo brings its own configuration; do not combine it with --config")
		}
		if dataDir == "" {
			home, _ := os.UserHomeDir()
			dataDir = filepath.Join(home, ".differently", "demo")
		}
		cfg = demo.Config(dataDir)
		err = cfg.Validate()
	}
	if err != nil {
		return err
	}
	if listen != "" {
		cfg.Listen = listen
	}
	var handler slog.Handler = slog.NewJSONHandler(os.Stderr, nil)
	if cfg.Dev {
		handler = slog.NewTextHandler(os.Stderr, nil)
	}
	log := slog.New(handler)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	if pg, ok := st.(*postgres.Store); ok {
		if err := pg.Migrate(ctx); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	installation, err := st.Installation(ctx)
	if err != nil {
		return err
	}
	rec, err := auditlog.New(st, log, cfg.Audit.File)
	if err != nil {
		return err
	}
	defer rec.Close()

	wiring.Register()
	box, err := secrets.Load(cfg.Secrets.KeyEnv, cfg.Secrets.KeyFile)
	if err != nil {
		return err
	}
	if box == nil {
		log.Info("no secrets key; provider credentials can be referenced (env:, file:) but not stored")
	}
	pm := pools.NewManager(pools.Deps{Store: st, Dev: cfg.Dev, DataDir: cfg.DataDir, Logger: log, Resolve: server.CredentialResolver(st, box)})
	sch := scheduler.New(st, pm, cfg, installation, rec, log)
	srv := server.New(cfg, st, pm, sch, rec, log, installation)
	srv.Secrets = box
	srv.Stacks = stacks.New(stacks.Deps{Store: st, Audit: rec, Log: log, Resolve: server.CredentialResolver(st, box)})
	if err := srv.Bootstrap(ctx); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	apiLn, metricsLn, err := bindPorts(cfg)
	if err != nil {
		return err
	}
	go sch.Run(ctx)
	go srv.Stacks.Run(ctx)

	handlerHTTP := srv.Handler()
	if demoMode {
		// The generator calls the API as the operator: with the bootstrap
		// token when one is set, otherwise with the one dev mode wrote.
		token := os.Getenv(cfg.Auth.BootstrapTokenEnv)
		if token == "" {
			b, err := os.ReadFile(filepath.Join(cfg.DataDir, "token"))
			if err != nil {
				return fmt.Errorf("demo: read the operator token: %w", err)
			}
			token = string(b)
		}
		gen := demo.NewGenerator(localURL(cfg.Listen), strings.TrimSpace(token), log)
		mux := http.NewServeMux()
		mux.Handle("/demo/state", gen.Handler())
		mux.Handle("GET /demo/{$}", demo.PageHandler())
		mux.Handle("/", handlerHTTP)
		handlerHTTP = mux
		go gen.Run(ctx) // the port is already bound: its first requests wait for Serve
	}
	httpSrv := &http.Server{Addr: cfg.Listen, Handler: handlerHTTP, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	errc := make(chan error, 2)
	go func() { errc <- httpSrv.Serve(apiLn) }()
	var metricsSrv *http.Server
	if metricsLn != nil {
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", srv.MetricsHandler())
		metricsSrv = &http.Server{Addr: cfg.Metrics.Listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() { errc <- metricsSrv.Serve(metricsLn) }()
	}
	log.Info("differently-server started", "listen", cfg.Listen, "version", version.Version, "installation", installation, "store", cfg.Store.Driver, "dev", cfg.Dev)
	if demoMode {
		log.Warn("DEMO MODE: mock connectors and generated traffic; no real provider is called",
			"presenter", localURL(cfg.Listen)+"/demo/", "operatorTokenFile", filepath.Join(cfg.DataDir, "token"))
		fmt.Fprint(os.Stderr, demoWelcome(cfg.Listen, cfg.DataDir))
	} else if cfg.Dev {
		log.Warn("DEV MODE: the devhost pool runs commands directly on this machine with no isolation",
			"operatorTokenFile", filepath.Join(cfg.DataDir, "token"), "dashboard", dashboardURL(cfg.Listen))
		fmt.Fprint(os.Stderr, devWelcome(cfg.Listen, cfg.DataDir))
	}
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	log.Info("shutting down")
	if metricsSrv != nil {
		_ = metricsSrv.Shutdown(shutdown)
	}
	return httpSrv.Shutdown(shutdown)
}

func migrate(cfgPath string) error {
	cfg, err := loadConfig(cfgPath, false, "")
	if err != nil {
		return err
	}
	if cfg.Store.Driver != "postgres" {
		fmt.Println("store.driver is memory: nothing to migrate")
		return nil
	}
	ctx := context.Background()
	st, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.(*postgres.Store).Migrate(ctx); err != nil {
		return err
	}
	fmt.Println("migrations applied")
	return nil
}

// preflight validates the config and the secrets it references.
func preflight(cfgPath string) error {
	cfg, err := loadConfig(cfgPath, false, "")
	if err != nil {
		return err
	}
	var problems []error
	check := func(ok bool, format string, args ...any) {
		status := "ok  "
		if !ok {
			status = "FAIL"
			problems = append(problems, fmt.Errorf(format, args...))
		}
		fmt.Printf("[%s] %s\n", status, fmt.Sprintf(format, args...))
	}
	token := os.Getenv(cfg.Auth.BootstrapTokenEnv)
	_, _, valid := auth.ParseToken(token)
	check(token == "" || valid, "bootstrap token %s is a valid dly_ token (or unset)", cfg.Auth.BootstrapTokenEnv)
	box, err := secrets.Load(cfg.Secrets.KeyEnv, cfg.Secrets.KeyFile)
	check(err == nil, "secrets key is valid base64 of %d bytes (or unset)", secrets.KeySize)
	for _, p := range cfg.Pools {
		if p.Credentials.String() == "" {
			continue
		}
		if p.Credentials.Secret != "" {
			// The store is checked when the server starts; here only the key is needed.
			check(box != nil, "pool %s credential %s has a secrets key to open it", p.Name, p.Credentials.String())
			continue
		}
		_, err := p.Credentials.Resolve()
		check(err == nil, "pool %s credential %s resolves", p.Name, p.Credentials.String())
	}
	if cfg.Store.Driver == "postgres" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		st, err := openStore(ctx, cfg)
		check(err == nil, "postgres reachable via %s", cfg.Store.DSNEnv)
		if err == nil {
			_ = st.Close()
		}
	}
	check(cfg.Policy.External == "never" || len(cfg.Policy.ExternalPools) > 0, "external placement is off, or names approved externalPools")
	return errors.Join(problems...)
}

// bindPorts takes the API port, and the metrics port when one is set, before
// the scheduler starts or the server says it is running, so a port that is
// already in use stops the start with an error instead of after the welcome.
func bindPorts(cfg config.Config) (apiLn, metricsLn net.Listener, err error) {
	if apiLn, err = net.Listen("tcp", cfg.Listen); err != nil {
		return nil, nil, fmt.Errorf("%w (is another server using %s? stop it, or pass --listen with another address)", err, cfg.Listen)
	}
	if cfg.Metrics.Listen != "" {
		if metricsLn, err = net.Listen("tcp", cfg.Metrics.Listen); err != nil {
			_ = apiLn.Close()
			return nil, nil, fmt.Errorf("metrics: %w (is another server using %s? stop it, or change metrics.listen)", err, cfg.Metrics.Listen)
		}
	}
	return apiLn, metricsLn, nil
}

// dashboardURL is where a local operator opens the console for a listen
// address such as ":8080" or "127.0.0.1:18180".
func dashboardURL(listen string) string {
	return localURL(listen) + "/dashboard/"
}

// localURL is the API's address as seen from this machine.
func localURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://" + listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// devWelcome tells a first-time user what just started and the next command
// to type. The CLI and the SDKs find a dev server at the default address and
// data directory by themselves; anywhere else they need to be told, so the
// command shown always works as printed.
func devWelcome(listen, dataDir string) string {
	api, token := localURL(listen), filepath.Join(dataDir, "token")
	try := cliCommand(listen, dataDir, "differently run -- echo hello")
	return fmt.Sprintf(`
Differently is running in dev mode.

  API        %s
  Dashboard  %s/dashboard/
  Token      %s
  Try        %s

Dev mode keeps everything in memory and runs commands on this machine as
you, with no isolation. Do not run code you do not trust.

`, api, api, token, try)
}

// cliCommand prefixes a CLI command with the settings it needs to reach this
// server: none at the default address and dev data directory, where the CLI
// and the SDKs look by themselves.
func cliCommand(listen, dataDir, command string) string {
	if home, err := os.UserHomeDir(); err != nil || dataDir != filepath.Join(home, ".differently", "dev") {
		command = fmt.Sprintf("DIFFERENTLY_API_KEY=$(cat %s) DIFFERENTLY_PROJECT=prj_default %s", filepath.Join(dataDir, "token"), command)
	}
	if api := localURL(listen); api != "http://localhost:8080" {
		command = "DIFFERENTLY_URL=" + api + " " + command
	}
	return command
}

// demoWelcome says where to watch the demo and how to drive it. The console's
// dev server forwards /v1 to DIFFERENTLY_URL, http://localhost:8080 by default.
func demoWelcome(listen, dataDir string) string {
	api, token := localURL(listen), filepath.Join(dataDir, "token")
	console := "npm run dev"
	if api != "http://localhost:8080" {
		console = "DIFFERENTLY_URL=" + api + " " + console
	}
	return fmt.Sprintf(`
Differently is running in demo mode: six mock providers and generated
traffic. Every sandbox is simulated and no real provider is called.

  Presenter  %s/demo/
  Dashboard  %s/dashboard/
  Token      %s (sign in with it)
  Console    in console/: npm ci && %s, then open
             http://localhost:4321/routing/ for Live routing
  CLI        %s

The presenter page switches situations: a normal day, a quarter-end peak,
a regulated workload, EU data residency and a vendor incident.

`, api, api, token, console, cliCommand(listen, dataDir, "differently sandbox list"))
}
