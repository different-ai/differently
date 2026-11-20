package acid_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/different-ai/differently/internal/acid"
	"github.com/different-ai/differently/internal/acid/acidtest"
	"github.com/different-ai/differently/internal/adapters/devhost"
	"github.com/different-ai/differently/internal/adapters/fake"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/adapter/adapterhttp"
	"github.com/different-ai/differently/pkg/api"
)

// The suite is checked against itself: each expectation below is a known
// property of the adapter, so a test that passes everything, or fails
// everything, shows up here before it misjudges a provider in public.

func merge(ms ...map[string]acid.Outcome) map[string]acid.Outcome {
	out := map[string]acid.Outcome{}
	for _, m := range ms {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// devhost runs real commands on the host, so most of levels 1 and 2 pass.
// What fails is true of devhost: it accepts any size, and maps paths under
// its root for files and cwd but not inside commands.
func TestDevhost(t *testing.T) {
	a, err := devhost.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rep := acidtest.Check(t, a, acidtest.Quick(), merge(acidtest.All(acid.Pass, 0, 1, 2), map[string]acid.Outcome{
		"L0.06": acid.Fail,
		"L1.03": acid.Fail,
		"L2.02": acid.Fail,
	}))
	if rep.Level != -1 || rep.Passed != 18 || rep.Total != 21 {
		t.Errorf("headline %s, want no level · 18/21", rep.Headline())
	}
}

// The fake answers every command with its own text and exit code 0: level 1
// must catch that, including output that is "complete" but wrong.
func TestFake(t *testing.T) {
	acidtest.Check(t, fake.New(fake.Options{}), acidtest.Quick(), merge(acidtest.All(acid.Fail, 1), map[string]acid.Outcome{
		"L0.01": acid.Pass, "L0.02": acid.Pass, "L0.03": acid.Pass, "L0.04": acid.Pass, "L0.05": acid.Pass, "L0.07": acid.Pass,
		"L0.06": acid.Fail,      // accepts 4096 CPUs
		"L1.06": acid.Dishonest, // short output, not flagged as truncated
		"L2.01": acid.Pass, "L2.03": acid.Pass, "L2.05": acid.Pass, "L2.06": acid.Pass,
		"L2.02": acid.Fail, // commands do not see files
		"L2.04": acid.Fail, // no directories in listings
	}))
}

// A pool with declared limits refuses an impossible size before the backend.
func TestDeclaredLimitsRefuseImpossibleSize(t *testing.T) {
	acidtest.Check(t, fake.New(fake.Options{TotalCPU: 64, TotalMemoryMiB: 256 << 10}), withOnly("L0.06"), map[string]acid.Outcome{"L0.06": acid.Pass})
}

func withOnly(ids ...string) acid.Options {
	o := acidtest.Quick()
	o.Only = ids
	return o
}

// leaky never deletes anything.
type leaky struct{ *fake.Adapter }

func (leaky) Delete(context.Context, string) error { return nil }

func TestLeaksAreReported(t *testing.T) {
	opts := withOnly("L0.02", "L0.05")
	opts.ReadyTimeout = time.Second
	rep := acid.Run(context.Background(), leaky{fake.New(fake.Options{})}, opts)
	if r := rep.Results[1]; r.ID != "L0.05" || r.Outcome != acid.Fail {
		t.Errorf("L0.05 %s (%s), want fail", r.Outcome, r.Evidence)
	}
	if len(rep.Leaked) != 2 {
		t.Errorf("leaked %v, want the two instances", rep.Leaked)
	}
}

// wrongCredential answers like a backend that refused a credential, and
// echoes it when echo is set.
type wrongCredential struct {
	*fake.Adapter
	secret string
	echo   bool
}

func (w wrongCredential) refuse() error {
	msg := "401 unauthorized"
	if w.echo {
		msg += " for key " + w.secret
	}
	return adapter.Unavailable(errors.New(msg))
}

func (w wrongCredential) Health(context.Context) error { return w.refuse() }
func (w wrongCredential) Create(context.Context, adapter.CreateRequest) (adapter.Instance, error) {
	return adapter.Instance{}, w.refuse()
}

func TestWrongCredentials(t *testing.T) {
	for _, echo := range []bool{false, true} {
		opts := withOnly("L0.01", "L0.06")
		opts.BadCredentials = func() (adapter.ExecutionAdapter, string, error) {
			return wrongCredential{fake.New(fake.Options{}), "acid-wrong-key-123", echo}, "acid-wrong-key-123", nil
		}
		a := fake.New(fake.Options{TotalCPU: 64, TotalMemoryMiB: 256 << 10})
		want := acid.Pass
		if echo {
			want = acid.Fail
		}
		rep := acidtest.Check(t, a, opts, map[string]acid.Outcome{"L0.01": want, "L0.06": want})
		if echo && !strings.Contains(rep.Results[0].Evidence, "contains the credential") {
			t.Errorf("evidence %q", rep.Results[0].Evidence)
		}
	}
}

// dropsStdin runs commands but silently ignores stdin.
type dropsStdin struct{ *devhost.Adapter }

func (d dropsStdin) Exec(ctx context.Context, ref string, req api.ExecRequest) (api.ExecResult, error) {
	req.Stdin = ""
	return d.Adapter.Exec(ctx, ref, req)
}

func TestSilentlyDroppedStdinIsDishonest(t *testing.T) {
	a, err := devhost.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	acidtest.Check(t, dropsStdin{a}, withOnly("L1.08"), map[string]acid.Outcome{"L1.08": acid.Dishonest})
}

func TestUndeclaredFeatureIsUnsupported(t *testing.T) {
	a := fake.New(fake.Options{Features: []string{adapter.FeatureExec}})
	rep := acidtest.Check(t, a, withOnly("L0.07", "L2.01"), map[string]acid.Outcome{"L2.01": acid.Unsupported})
	// The fake implements files, pause and the rest without declaring them,
	// and they work: honest enough to pass, and noted.
	if r := rep.Results[0]; r.Outcome != acid.Pass || !strings.Contains(r.Evidence, "works although undeclared") {
		t.Errorf("L0.07 %s (%s)", r.Outcome, r.Evidence)
	}
}

func TestTestsAreOrderedAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, tt := range acid.Tests() {
		if seen[tt.ID] {
			t.Errorf("duplicate %s", tt.ID)
		}
		seen[tt.ID] = true
		if !strings.HasPrefix(tt.ID, "L") || tt.ID[1]-'0' != byte(tt.Level) {
			t.Errorf("%s is in level %d", tt.ID, tt.Level)
		}
	}
}

// A backend behind the HTTP plugin protocol (a remote pool) gets the same
// verdicts as the same backend in process: the protocol loses nothing the
// suite checks, and a wrong token is refused like a wrong credential.
func TestRemoteProtocolMatchesInProcess(t *testing.T) {
	a, err := devhost.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(adapterhttp.Handler(a, "plugin-token"))
	defer srv.Close()
	opts := acidtest.Quick()
	opts.BadCredentials = func() (adapter.ExecutionAdapter, string, error) {
		return adapterhttp.NewClient(srv.URL, "acid-wrong-token"), "acid-wrong-token", nil
	}
	acidtest.Check(t, adapterhttp.NewClient(srv.URL, "plugin-token"), opts, merge(acidtest.All(acid.Pass, 0, 1, 2), map[string]acid.Outcome{
		"L0.06": acid.Fail,
		"L1.03": acid.Fail,
		"L2.02": acid.Fail,
	}))
}
