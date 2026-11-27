# Store drivers

| | |
|---|---|
| Status | Describes the code as of 2026-11-27. The default build holds four drivers: `memory`, `file`, `sqlite` and `postgres`. |
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
| `file` | Yes | No | A directory the server can write: `<dataDir>/store`. No database. See [the file driver](#the-file-driver). |
| `sqlite` | Yes | No | A file the server can write: `<dataDir>/sqlite/differently.db`. No database service. See [the sqlite driver](#the-sqlite-driver). |
| `postgres` | Yes | Yes | A PostgreSQL database, its URL in the environment variable `store.dsnEnv` names (`DIFFERENTLY_DATABASE_URL` by default). |

A name the build does not hold is refused at start, with the names it does
hold. The server never falls back to another driver: that would put state
somewhere the operator did not choose.

`memory` is still what a config file with no `store` section gets. Name
`file`, `sqlite` or `postgres` to keep state.

Which one:

- **`file`** when you want nothing but the server. It is the memory store's
  code with a journal, and adds no dependency.
- **`sqlite`** when you want to look at the records with SQL or use SQLite's
  tools. It holds on disk what `file` also holds in memory, so it suits an
  installation whose history has grown large.
- **`postgres`** when more than one replica must share the state.

## The file driver

```yaml
store: {driver: file}
dataDir: /var/lib/differently   # the default
```

It keeps the same records the other drivers do, in four files under
`<dataDir>/store`, readable only by the server's user:

| File | Holds |
|---|---|
| `snapshot.json` | Every table as of the last compaction |
| `journal.jsonl` | Changes since then, one line per write |
| `audit.jsonl` | The audit chain, one event per line, only ever appended |
| `lock` | Held while a server has the directory open |

The rules are the memory store's own code: the driver repeats none of them.
It records each change the memory store makes and writes it down.

- **A call returns after its changes are synced to disk.** Calls that arrive
  together share one sync.
- **A server killed outright loses nothing it reported as done.** At start
  the driver reads the snapshot, then the journal, then the audit log. A
  last line with no newline is a write that was cut short and never
  reported; it is cut off.
- **A damaged file is refused**, with the file and line. The server does not
  start with part of what was kept.
- **If a write to disk fails** (a full disk), that call fails and the store
  stops answering until the server is restarted: memory would be ahead of
  the disk, and anything it said could be gone after a restart.
- **One server per directory.** A second one is refused at start. This
  driver cannot be shared by replicas; `postgres` can.
- **All state is also in memory**, as with the memory store, and the journal
  is folded into the snapshot at start and each time it passes 8 MiB.

To back it up, copy the directory while the server is stopped. A copy taken
while it runs can catch the journal mid-write; that last line is then cut
off at restore, so the copy is usable but may lack the newest writes.

What was run: the conformance suite, with the directory reopened and
compared after every test, and again with the snapshot rewritten after
every write; the server acceptance scenarios (`make test-stores`); a process
killed with SIGKILL while writing; cut-short and damaged files; and a real
`differently-server` killed and restarted on macOS, where the audit chain
verified afterwards. What was not: a power cut or a full disk on real
hardware, network filesystems, and Linux outside CI. The Helm chart does not
offer `file` yet, because its data directory is not on a volume that
outlives the pod.

## The sqlite driver

```yaml
store: {driver: sqlite}
dataDir: /var/lib/differently   # the default
```

The database is `<dataDir>/sqlite/differently.db`, or the file named by the
environment variable `store.dsnEnv` names. It has the postgres driver's
tables: one row per record, the record as JSON in `doc`. The schema is
created at start (`differently-server migrate` does the same).

- **Every write is one transaction, and one runs at a time.** Where the
  postgres driver locks rows so replicas cannot take the same capacity, one
  server has this file, so a single writer is the whole lock. Reads go on
  beside it (write-ahead logging).
- **A commit is synced to disk** (`synchronous=FULL`): a call that returned
  survives the server being killed.
- **One server per file.** A second one is refused at start, by a lock file
  beside the database.
- **You can read it while the server runs**, for example
  `sqlite3 -readonly differently.db "select doc from pools"`. Do not write to
  it: the server's checks are in its code, not in the database, and the
  audit chain would no longer verify.
- The files are readable only by the server's user. To back up, use
  SQLite's own `.backup` or `VACUUM INTO`, or copy the files while the
  server is stopped.

It is built on `modernc.org/sqlite`, a SQLite translated to Go, so the
server stays one static binary with no C library; it adds about 4 MB to it.

What was run: the conformance suite; the server acceptance scenarios
(`make test-stores`); a process killed with SIGKILL while writing; and a real
`differently-server` killed and restarted on macOS, read with `sqlite3` while
running, where the audit chain verified afterwards. What was not: a power
cut on real hardware, network filesystems, Linux outside CI, and a database
larger than these tests make. The Helm chart does not offer `sqlite` yet,
for the same reason as `file`.

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
4. Run the server acceptance scenarios on it as well: add it to
   `STORE_DRIVERS` in the Makefile for `make test-stores` if it needs no
   service, or do as `make test-postgres` does for Postgres. Add its
   name to the chart's `values.schema.json`, and add a row to the table
   above saying what was run and what was not.

There is no out-of-process protocol for stores, unlike backend adapters
([adapter specification](specification.md)). The atomic operations above
would have to hold across a network call, and nothing has asked for it yet.
