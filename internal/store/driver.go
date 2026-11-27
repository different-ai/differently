package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// A Driver is one way to keep the control plane's state: in memory, in
// Postgres, or in whatever a build registers. The server picks one by name
// (store.driver in the config file) and knows nothing else about it.
//
// A driver's Store must pass storetest.Run. That suite is the contract: it
// holds the atomic admission, activation and quota rules that keep two
// requests from taking the same capacity, so a driver that passes it can be
// trusted with them and one that does not cannot.
type Driver struct {
	// Summary is one line for messages and docs.
	Summary string
	// Durable says state survives a restart of the server.
	Durable bool
	// Shared says several servers can use the same state at once, with
	// TryLeader choosing one scheduler among them. A driver that is not
	// shared supports one replica.
	Shared bool
	// Open connects to the state, or creates it.
	Open func(ctx context.Context, o Options) (Store, error)
}

// Options is what the server hands a driver. A driver reads what it needs
// and ignores the rest.
type Options struct {
	// DSN is the value of the environment variable the config names
	// (store.dsnEnv), and DSNEnv is that variable's name, for messages. The
	// config file never holds the value.
	DSN, DSNEnv string
	// DataDir is the server's data directory, for drivers that keep files.
	DataDir string
}

// Migrator is implemented by stores with a schema to bring up to date.
// The server calls Migrate at start and for its migrate command.
type Migrator interface {
	Migrate(ctx context.Context) error
}

var (
	driversMu sync.RWMutex
	drivers   = map[string]Driver{}
)

// Register makes a driver available under name. Packages call it from init;
// a build holds the drivers it imports. Registering a name twice, or a
// driver that cannot open, is a programming error.
func Register(name string, d Driver) {
	driversMu.Lock()
	defer driversMu.Unlock()
	if name == "" || d.Open == nil {
		panic("store: Register needs a name and an Open function")
	}
	if _, dup := drivers[name]; dup {
		panic("store: driver " + name + " registered twice")
	}
	drivers[name] = d
}

// Lookup returns the driver registered under name.
func Lookup(name string) (Driver, bool) {
	driversMu.RLock()
	defer driversMu.RUnlock()
	d, ok := drivers[name]
	return d, ok
}

// Drivers lists the registered names, sorted.
func Drivers() []string {
	driversMu.RLock()
	defer driversMu.RUnlock()
	names := make([]string, 0, len(drivers))
	for n := range drivers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Open opens the store the named driver provides. An unknown name is an
// error that lists what this build has: it never falls back to another
// driver, because that would quietly put state somewhere else.
func Open(ctx context.Context, name string, o Options) (Store, error) {
	d, ok := Lookup(name)
	if !ok {
		return nil, fmt.Errorf("store.driver %q is not in this build (it has: %s)", name, strings.Join(Drivers(), ", "))
	}
	return d.Open(ctx, o)
}

func init() {
	Register("memory", Driver{
		Summary: "in this process's memory; lost on restart",
		Open:    func(context.Context, Options) (Store, error) { return NewMemory(), nil },
	})
}
