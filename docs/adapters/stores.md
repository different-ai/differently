# Store drivers

| | |
|---|---|
| Status | Describes the code as of 2026-11-27. The default build holds two drivers, `memory` and `postgres`. |
| Code | `internal/store` (the contract and the registry), `internal/store/storetest` (the conformance suite) |

Differently keeps its own records (organizations, projects, keys, pools,
policies, templates, sandbox and snapshot records, stored credentials and the
audit chain) in a **store**. The server does not know which one: it opens the
driver named by `store.driver` in the config file, and every driver gives it
the same `store.Store` interface. No query language is part of the contract.
A driver may use SQL, a key-value database or a file.

Sandbox files, processes and memory are never in the store. They belong to
the runtime behind each pool ([storage and lifecycle](../storage-and-lifecycle.md)).

## Drivers in the default build

| `store.driver` | Survives a restart | Several replicas | Needs |
|---|---|---|---|
| `memory` | No | No | Nothing. For trying Differently and for tests; the server warns at start that state is lost when it stops. |
| `postgres` | Yes | Yes | A PostgreSQL database, its URL in the environment variable `store.dsnEnv` names (`DIFFERENTLY_DATABASE_URL` by default). |

A name the build does not hold is refused at start, with the names it does
hold. The server never falls back to another driver: that would put state
somewhere the operator did not choose.

No driver yet keeps state on one machine without a database. That is the
next one planned; until it exists, anything that must survive a restart needs
`postgres`.

## The contract

A driver is a `store.Driver` registered under a name:

```go
func init() {
	store.Register("mydb", store.Driver{
		Summary: "one line for messages and docs",
		Durable: true,  // state survives a restart of the server
		Shared:  false, // several servers may use the same state at once
		Open: func(ctx context.Context, o store.Options) (store.Store, error) {
			// o.DSN is the value of the variable store.dsnEnv names;
			// o.DataDir is the server's data directory.
			return open(ctx, o)
		},
	})
}
```

What the store it opens must do:

1. **Implement `store.Store`** (`internal/store/store.go`).
2. **Pass `storetest.Run`.** This is the contract that matters. Admitting a
   request and activating a queued sandbox onto a pool are single atomic
   operations that check tenant quota and pool capacity; the suite runs them
   concurrently and fails a store that lets two requests take the same
   capacity. Quota use is derived from sandbox records, never kept in a
   separate counter that a crash could leave wrong.
3. **Keep the audit chain in order.** `AppendAudit` seals each event to the
   one before it; two appends must never see the same predecessor.
4. **Be honest in `Durable` and `Shared`.** With `Shared: false`, `TryLeader`
   answers true and the installation runs one replica. With `Shared: true`,
   `TryLeader` must give leadership to one server at a time, because only the
   leader schedules and reconciles.
5. **Implement `store.Migrator`** if there is a schema to bring up to date.
   The server calls `Migrate` at start and for `differently-server migrate`.

## Adding one

1. Write the driver in its own package under `internal/store/` and register
   it from `init`.
2. Run the conformance suite from its tests, the way
   `internal/store/postgres/postgres_test.go` does:
   `storetest.Run(t, func(t *testing.T) store.Store { … })`.
3. Import the package in `cmd/differently-server/main.go` for its
   registration. A driver that brings a large dependency can sit behind a
   build tag instead, as the experimental backend adapters do.
4. Run the server acceptance scenarios on it as well
   (`internal/server`, as `make test-postgres` does for Postgres), add its
   name to the chart's `values.schema.json`, and add a row to the table
   above saying what was run and what was not.

There is no out-of-process protocol for stores, unlike backend adapters
([adapter specification](specification.md)). The atomic operations above
would have to hold across a network call, and nothing has asked for it yet.
