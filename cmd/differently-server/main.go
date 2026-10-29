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
	"syscall"
	"time"

	"github.com/different-ai/differently/internal/auditlog"
	"github.com/different-ai/differently/internal/auth"
	"github.com/different-ai/differently/internal/config"
	"github.com/different-ai/differently/internal/pools"
	"github.com/different-ai/differently/internal/scheduler"
	"github.com/different-ai/differently/internal/secrets"
	"github.com/different-ai/differently/internal/server"
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
	listen := fs.String("listen", "", "override the listen address")
	dataDir := fs.String("data-dir", "", "override the data directory")
	_ = fs.Parse(args)

	var err error
	switch cmd {
	case "serve":
		err = serve(*cfgPath, *dev, *listen, *dataDir)
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
	cfg, err := config.Load(path)
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

func serve(cfgPath string, dev bool, listen, dataDir string) error {
	cfg, err := loadConfig(cfgPath, dev, dataDir)
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
	if err := srv.Bootstrap(ctx); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	go sch.Run(ctx)

	httpSrv := &http.Server{Addr: cfg.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second}
	errc := make(chan error, 2)
	go func() { errc <- httpSrv.ListenAndServe() }()
	var metricsSrv *http.Server
	if cfg.Metrics.Listen != "" {
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", srv.MetricsHandler())
		metricsSrv = &http.Server{Addr: cfg.Metrics.Listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() { errc <- metricsSrv.ListenAndServe() }()
	}
	log.Info("differently-server started", "listen", cfg.Listen, "version", version.Version, "installation", installation, "store", cfg.Store.Driver, "dev", cfg.Dev)
	if cfg.Dev {
		log.Warn("DEV MODE: the devhost pool runs commands directly on this machine with no isolation",
			"operatorTokenFile", filepath.Join(cfg.DataDir, "token"), "dashboard", dashboardURL(cfg.Listen))
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

// dashboardURL is where a local operator opens the console for a listen
// address such as ":8080" or "127.0.0.1:18180".
func dashboardURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://" + listen + "/dashboard/"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port) + "/dashboard/"
}
