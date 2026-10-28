// Package auditlog records audit events to the store's hash chain and,
// optionally, mirrors them as JSON lines to a file for shipping to
// write-once storage or a SIEM.
package auditlog

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/pkg/api"
)

// Recorder appends audit events.
type Recorder struct {
	Store store.Store
	Log   *slog.Logger
	mu    sync.Mutex
	file  *os.File
}

// New returns a recorder; path may be empty.
func New(st store.Store, log *slog.Logger, path string) (*Recorder, error) {
	r := &Recorder{Store: st, Log: log}
	if path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		r.file = f
	}
	return r, nil
}

// Record stores ev. Audit failures are logged, never silently dropped.
func (r *Recorder) Record(ctx context.Context, ev api.AuditEvent) {
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	if ev.Outcome == "" {
		ev.Outcome = "success"
	}
	sealed, err := r.Store.AppendAudit(context.WithoutCancel(ctx), ev)
	if err != nil {
		r.Log.Error("audit append failed", "action", ev.Action, "resource", ev.Resource, "err", err)
		return
	}
	if r.file != nil {
		r.mu.Lock()
		b, _ := json.Marshal(sealed)
		_, _ = r.file.Write(append(b, '\n'))
		r.mu.Unlock()
	}
}

// Close closes the mirror file.
func (r *Recorder) Close() error {
	if r.file != nil {
		return r.file.Close()
	}
	return nil
}
