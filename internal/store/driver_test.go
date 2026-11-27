package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestOpenUsesTheNamedDriverAndNeverAnother(t *testing.T) {
	st, err := Open(context.Background(), "memory", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.(*Memory); !ok {
		t.Fatalf("memory driver opened a %T", st)
	}
	if d, _ := Lookup("memory"); d.Durable || d.Shared {
		t.Errorf("memory claims durable=%v shared=%v", d.Durable, d.Shared)
	}

	// An unknown name is refused with the list of what the build has. It
	// must not fall back to memory: state would silently not be kept.
	st, err = Open(context.Background(), "sqlite", Options{})
	if err == nil || st != nil {
		t.Fatalf("unknown driver opened %T, err %v", st, err)
	}
	if !strings.Contains(err.Error(), `"sqlite" is not in this build`) || !strings.Contains(err.Error(), "memory") {
		t.Errorf("error = %v", err)
	}
}

func TestRegisterAddsADriverAndRefusesMistakes(t *testing.T) {
	boom := errors.New("cannot open")
	var got Options
	Register("test-driver", Driver{Summary: "for this test", Durable: true, Open: func(_ context.Context, o Options) (Store, error) {
		got = o
		return nil, boom
	}})
	t.Cleanup(func() {
		driversMu.Lock()
		delete(drivers, "test-driver")
		driversMu.Unlock()
	})
	if !strings.Contains(strings.Join(Drivers(), ","), "test-driver") {
		t.Errorf("Drivers() = %v", Drivers())
	}
	want := Options{DSN: "dsn", DSNEnv: "ENV", DataDir: "/data"}
	if _, err := Open(context.Background(), "test-driver", want); !errors.Is(err, boom) || got != want {
		t.Errorf("err = %v, options = %+v", err, got)
	}
	for name, d := range map[string]Driver{
		"test-driver": {Open: func(context.Context, Options) (Store, error) { return nil, nil }}, // twice
		"":            {Open: func(context.Context, Options) (Store, error) { return nil, nil }},
		"no-open":     {},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Register(%q) did not panic", name)
				}
			}()
			Register(name, d)
		}()
	}
}

// This package's tests do not hold the file driver, so the default is the
// one every build has. internal/store/file checks the other half.
func TestDefaultDriverIsMemoryWithoutTheFileDriver(t *testing.T) {
	if _, ok := Lookup("file"); ok {
		t.Skip("the file driver is linked into this test")
	}
	if got := DefaultDriver(); got != "memory" {
		t.Errorf("DefaultDriver() = %q", got)
	}
}
