package store

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/different-ai/differently/pkg/api"
)

// Change tracking lets a driver keep the memory store's state somewhere
// durable without repeating any of its rules. Every write in memory.go goes
// through put, drop or note, which record the record as it now is. A driver
// takes those changes after each call and writes them down; at start it
// gives them back through Apply. internal/store/file is such a driver.

// The tables a Change can name.
const (
	TableMeta     = "meta"
	TableOrg      = "org"
	TableProject  = "project"
	TableKey      = "key"
	TablePool     = "pool"
	TableSecret   = "secret"
	TableStack    = "stack"
	TableTemplate = "template"
	TablePolicy   = "policy"
	TableSandbox  = "sandbox"
	TableSnapshot = "snapshot"
	TableAudit    = "audit"

	metaInstallation = "installation"
)

// Change is one record as it is after a write, or its removal. Applying the
// same Change twice leaves the same state, except in TableAudit, where each
// Change appends one event.
type Change struct {
	Table   string          `json:"t"`
	ID      string          `json:"id,omitempty"`
	Value   json.RawMessage `json:"v,omitempty"`
	Deleted bool            `json:"d,omitempty"`
}

// put stores a copy of v and records the change. Every write to a table goes
// through put or drop; TestMemoryWritesGoThroughPutAndDrop holds that.
func put[T any](m *Memory, table map[string]T, name, id string, v T) {
	c := clone(v)
	table[id] = c
	m.note(name, id, c, false)
}

// drop removes a record and records the removal.
func drop[T any](m *Memory, table map[string]T, name, id string) {
	if _, ok := table[id]; !ok {
		return
	}
	delete(table, id)
	m.note(name, id, nil, true)
}

// note records one change. The caller holds m.mu.
func (m *Memory) note(table, id string, v any, deleted bool) {
	if !m.track {
		return
	}
	c := Change{Table: table, ID: id, Deleted: deleted}
	if !deleted {
		b, err := json.Marshal(v)
		if err != nil {
			panic(err) // clone already marshalled the same value
		}
		c.Value = b
	}
	m.pending = append(m.pending, c)
	m.seq.Add(1)
}

// TrackChanges starts recording changes. Call it once, after Apply has
// restored what was kept, and before the store is used.
func (m *Memory) TrackChanges() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.track = true
}

// ChangeSeq counts the changes recorded so far. It only grows. A caller
// that reads it before and after an operation knows whether anything was
// written in between, and up to where it must be kept before the operation
// may be reported as done.
func (m *Memory) ChangeSeq() uint64 { return m.seq.Load() }

// TakeChanges returns the changes recorded since the last call, in order,
// and the ChangeSeq they reach. Changes of one store operation are never
// split between two calls, because both hold the same lock.
func (m *Memory) TakeChanges() ([]Change, uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := m.pending
	m.pending = nil
	return out, m.seq.Load()
}

// TakeChangesAndDump is TakeChanges and Dump under one lock: the changes not
// yet taken, and the whole state they lead to. A driver that writes the
// changes down and then replaces its kept state with the dump has both
// describe the same moment.
func (m *Memory) TakeChangesAndDump() (pending []Change, seq uint64, state []Change) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pending, m.pending = m.pending, nil
	return pending, m.seq.Load(), m.dumpLocked()
}

// Apply restores changes that were kept, in order. It records nothing.
func (m *Memory) Apply(changes []Change) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range changes {
		if err := m.apply(c); err != nil {
			return fmt.Errorf("%s %s: %w", c.Table, c.ID, err)
		}
	}
	return nil
}

func (m *Memory) apply(c Change) error {
	switch c.Table {
	case TableMeta:
		if c.ID != metaInstallation {
			return fmt.Errorf("unknown meta record")
		}
		return json.Unmarshal(c.Value, &m.installation)
	case TableOrg:
		return applyTo(m.orgs, c)
	case TableProject:
		return applyTo(m.projects, c)
	case TableKey:
		return applyTo(m.keys, c)
	case TablePool:
		return applyTo(m.pools, c)
	case TableSecret:
		return applyTo(m.secrets, c)
	case TableStack:
		return applyTo(m.stacks, c)
	case TableTemplate:
		return applyTo(m.templates, c)
	case TablePolicy:
		return applyTo(m.policies, c)
	case TableSandbox:
		return applyTo(m.sandboxes, c)
	case TableSnapshot:
		return applyTo(m.snapshots, c)
	case TableAudit:
		var ev api.AuditEvent
		if err := json.Unmarshal(c.Value, &ev); err != nil {
			return err
		}
		// The chain is a sequence with no gaps: an event out of place means
		// the kept log lost or repeated one, and going on would hide it.
		if want := int64(len(m.audit) + 1); ev.Seq != want {
			return fmt.Errorf("audit event has sequence %d where %d belongs", ev.Seq, want)
		}
		m.audit = append(m.audit, ev)
		return nil
	}
	return fmt.Errorf("unknown table")
}

func applyTo[T any](table map[string]T, c Change) error {
	if c.Deleted {
		delete(table, c.ID)
		return nil
	}
	var v T
	if err := json.Unmarshal(c.Value, &v); err != nil {
		return err
	}
	table[c.ID] = v
	return nil
}

// Dump returns the whole state as changes that rebuild it, without the
// audit chain: tables in a fixed order and records sorted by ID, so the same
// state always gives the same bytes. The audit chain only grows, so a driver
// keeps it apart and never rewrites it.
func (m *Memory) Dump() []Change {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dumpLocked()
}

func (m *Memory) dumpLocked() []Change {
	var out []Change
	if m.installation != "" {
		b, _ := json.Marshal(m.installation)
		out = append(out, Change{Table: TableMeta, ID: metaInstallation, Value: b})
	}
	out = dumpTable(out, TableOrg, m.orgs)
	out = dumpTable(out, TableProject, m.projects)
	out = dumpTable(out, TableKey, m.keys)
	out = dumpTable(out, TablePool, m.pools)
	out = dumpTable(out, TableSecret, m.secrets)
	out = dumpTable(out, TableStack, m.stacks)
	out = dumpTable(out, TableTemplate, m.templates)
	out = dumpTable(out, TablePolicy, m.policies)
	out = dumpTable(out, TableSandbox, m.sandboxes)
	out = dumpTable(out, TableSnapshot, m.snapshots)
	return out
}

// DumpAudit returns the audit chain as changes, oldest first.
func (m *Memory) DumpAudit() []Change {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Change, 0, len(m.audit))
	for _, ev := range m.audit {
		b, _ := json.Marshal(ev)
		out = append(out, Change{Table: TableAudit, Value: b})
	}
	return out
}

func dumpTable[T any](out []Change, name string, table map[string]T) []Change {
	ids := make([]string, 0, len(table))
	for id := range table {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		b, err := json.Marshal(table[id])
		if err != nil {
			panic(err)
		}
		out = append(out, Change{Table: name, ID: id, Value: b})
	}
	return out
}
