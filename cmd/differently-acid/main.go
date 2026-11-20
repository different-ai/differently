// Command differently-acid runs the sandbox acid test (docs/acid-test.md)
// against one pool and reports what the backend really does.
//
//	differently-acid run -config pools.yaml -pool daytona -out daytona.json
//	differently-acid run -config pools.yaml -pool local -dev -only L1.04
//	differently-acid report daytona.json e2b.json
//
// The config file is a Differently server config, or just its pools list.
// Credentials are references (env: or file:), as in the server; secret:
// references need the server's store and are not supported here.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/different-ai/differently/internal/acid"
	"github.com/different-ai/differently/internal/config"
	"github.com/different-ai/differently/internal/pools"
	"github.com/different-ai/differently/internal/wiring"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

const usage = `usage:
  differently-acid run -config FILE -pool NAME [flags]   run the suite against a pool
  differently-acid report [-matrix] RESULT.json...       print results as Markdown
  differently-acid list                                  list the tests

Run 'differently-acid run -h' for the run flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = run(os.Args[2:])
	case "report":
		err = report(os.Args[2:], os.Stdout)
	case "list":
		for _, t := range acid.Tests() {
			feature := ""
			if t.Feature != "" {
				feature = " (" + t.Feature + ")"
			}
			fmt.Printf("%s  %s%s\n", t.ID, t.Title, feature)
		}
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "differently-acid:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", "", "server config or pools file (required)")
	poolName := fs.String("pool", "", "pool to test (required)")
	dev := fs.Bool("dev", false, "allow the dev-only kinds devhost (no isolation) and fake")
	level := fs.Int("level", acid.MaxLevel, "highest level to run")
	only := fs.String("only", "", "comma-separated test IDs to run instead of whole levels")
	out := fs.String("out", "", "write the JSON result here")
	seed := fs.Uint64("seed", 0, "seed for sandbox names and payloads (0: random)")
	cpu := fs.Int("cpu", 1, "CPUs per sandbox")
	memory := fs.Int("memory", 1024, "memory per sandbox, MiB")
	disk := fs.Int("disk", 5, "disk per sandbox, GiB")
	template := fs.String("template", "", "template (image) to create sandboxes from; empty: the pool's default")
	long := fs.Duration("long", 10*time.Minute, "how long L1.07's command runs")
	largeOutput := fs.Int("large-output", 10<<20, "L1.06 output size, bytes")
	largeFile := fs.Int("large-file", 100<<20, "L2.05 file size, bytes")
	testTimeout := fs.Duration("test-timeout", 15*time.Minute, "timeout for one test")
	quiet := fs.Bool("q", false, "no progress on stderr")
	fs.Parse(args)
	if *cfgPath == "" || *poolName == "" {
		fs.Usage()
		return errors.New("-config and -pool are required")
	}
	defer func() {
		for _, d := range dataDirs {
			os.RemoveAll(d)
		}
	}()
	pool, err := loadPool(*cfgPath, *poolName)
	if err != nil {
		return err
	}
	a, err := build(pool, *dev)
	if err != nil {
		return err
	}

	opts := acid.Options{
		Resources: api.Resources{CPU: *cpu, MemoryMiB: *memory, DiskGiB: *disk},
		Template:  *template, MaxLevel: *level, Seed: *seed, TestTimeout: *testTimeout,
		LongCommand: *long, LargeOutputBytes: *largeOutput, LargeFileBytes: *largeFile,
	}
	if *only != "" {
		opts.Only = strings.Split(*only, ",")
	} else if *level == 0 {
		for _, t := range acid.Tests() {
			if t.Level == 0 {
				opts.Only = append(opts.Only, t.ID)
			}
		}
	}
	if pool.CredentialRef != "" {
		opts.BadCredentials = func() (adapter.ExecutionAdapter, string, error) { return wrongCredential(pool, *dev) }
	}
	if !*quiet {
		fmt.Fprintf(os.Stderr, "acid: %s on pool %s (%s)\n", acid.Suite, pool.Name, pool.Adapter)
		opts.Progress = func(r acid.Result) {
			fmt.Fprintf(os.Stderr, "  %s %-5s %-22s %6.1fs  %s\n", acid.Symbol(r.Outcome), r.ID, r.Title, r.Seconds, r.Evidence)
		}
	}

	// Interrupting stops the tests; the run still deletes what it created.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	rep := acid.Run(ctx, a, opts)
	rep.Pool = pool.Name
	rep.Environment = environment()

	if *out != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.WriteFile(*out, append(b, '\n'), 0o644); err != nil {
			return err
		}
	}
	acid.WriteMarkdown(os.Stdout, rep)
	if len(rep.Leaked) > 0 {
		return fmt.Errorf("%d instances could not be deleted: %s", len(rep.Leaked), strings.Join(rep.Leaked, ", "))
	}
	return nil
}

// loadPool reads one pool from a server config or a file holding only
// `pools:`. Other server settings are ignored.
func loadPool(path, name string) (api.Pool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return api.Pool{}, err
	}
	var file struct {
		Pools []config.PoolConfig `json:"pools"`
	}
	if err := yaml.Unmarshal(b, &file); err != nil {
		return api.Pool{}, fmt.Errorf("%s: %w", path, err)
	}
	var names []string
	for _, p := range file.Pools {
		if p.Name == name {
			if p.Credentials.Secret != "" {
				return api.Pool{}, fmt.Errorf("pool %s uses a stored secret (secret:%s); give the CLI an env: or file: reference", name, p.Credentials.Secret)
			}
			return p.ToPool(), nil
		}
		names = append(names, p.Name)
	}
	return api.Pool{}, fmt.Errorf("%s has no pool %q (pools: %s)", path, name, strings.Join(names, ", "))
}

// dataDirs are removed when the run ends.
var dataDirs []string

func build(p api.Pool, dev bool) (adapter.ExecutionAdapter, error) {
	wiring.Register()
	dir, err := os.MkdirTemp("", "differently-acid-")
	if err != nil {
		return nil, err
	}
	dataDirs = append(dataDirs, dir)
	m := pools.NewManager(pools.Deps{Dev: dev, DataDir: dir, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	m.Sync([]api.Pool{p})
	a, err := m.Adapter(p.Name)
	if err != nil {
		return nil, fmt.Errorf("pool %s (%s): %w", p.Name, p.Adapter, err)
	}
	return a, nil
}

// wrongCredential builds the same pool with a credential that cannot be
// right, read from an environment variable like a real one.
func wrongCredential(p api.Pool, dev bool) (adapter.ExecutionAdapter, string, error) {
	const env = "DIFFERENTLY_ACID_WRONG_CREDENTIAL"
	secret := fmt.Sprintf("acid-wrong-credential-%d", time.Now().UnixNano())
	if err := os.Setenv(env, secret); err != nil {
		return nil, "", err
	}
	p.Name += "-wrong-credential"
	p.CredentialRef = "env:" + env
	a, err := build(p, dev)
	return a, secret, err
}

func environment() map[string]string {
	env := map[string]string{"runner": runtime.GOOS + "/" + runtime.GOARCH, "go": runtime.Version()}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				env["commit"] = s.Value
			case "vcs.modified":
				if s.Value == "true" {
					env["modified"] = "true"
				}
			}
		}
	}
	return env
}

func report(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	matrix := fs.Bool("matrix", false, "one table with a column per result, instead of each result in full")
	fs.Parse(args)
	if fs.NArg() == 0 {
		return errors.New("report needs at least one result file")
	}
	var reps []acid.Report
	for _, path := range fs.Args() {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var r acid.Report
		if err := json.Unmarshal(b, &r); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if r.Suite != acid.Suite {
			return fmt.Errorf("%s is %q; this build reads %s", path, r.Suite, acid.Suite)
		}
		reps = append(reps, r)
	}
	if *matrix || len(reps) > 1 {
		acid.WriteMatrix(w, reps)
		if !*matrix {
			for _, r := range reps {
				fmt.Fprintln(w)
				acid.WriteMarkdown(w, r)
			}
		}
		return nil
	}
	acid.WriteMarkdown(w, reps[0])
	return nil
}
