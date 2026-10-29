// Package audit seals audit events into a hash chain.
//
// Each event's hash covers its content and the previous event's hash, so any
// edit, deletion or reordering of stored events is detectable by Verify. This
// is tamper-evident, not tamper-proof: an attacker with write access to the
// database can rewrite the whole chain. Exporting events (or the latest hash)
// to write-once storage outside the database closes that gap.
package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/different-ai/differently/pkg/api"
)

// Genesis is the PrevHash of the first event.
const Genesis = "0000000000000000000000000000000000000000000000000000000000000000"

// Seal sets ev.PrevHash and ev.Hash given the previous event's hash.
func Seal(prev string, ev api.AuditEvent) api.AuditEvent {
	if prev == "" {
		prev = Genesis
	}
	ev.PrevHash = prev
	ev.Hash = ""
	ev.Hash = digest(ev)
	return ev
}

func digest(ev api.AuditEvent) string {
	ev.Hash = ""
	// encoding/json sorts map keys, which makes the encoding canonical for
	// this struct.
	b, _ := json.Marshal(ev)
	sum := sha256.Sum256(append([]byte(ev.PrevHash), b...))
	return hex.EncodeToString(sum[:])
}

// VerifyResult reports the outcome of a chain check.
type VerifyResult struct {
	OK       bool   `json:"ok"`
	Events   int    `json:"events"`
	BrokenAt int64  `json:"brokenAt,omitempty"`
	Problem  string `json:"problem,omitempty"`
	HeadHash string `json:"headHash,omitempty"`
}

// Verify checks a contiguous, ascending run of events. The first event's
// PrevHash is trusted as the anchor, so a partial range can be verified.
func Verify(events []api.AuditEvent) VerifyResult {
	res := VerifyResult{OK: true, Events: len(events)}
	for i, ev := range events {
		if i > 0 {
			prev := events[i-1]
			if ev.Seq != prev.Seq+1 {
				return VerifyResult{Events: len(events), BrokenAt: ev.Seq, Problem: fmt.Sprintf("sequence gap after %d", prev.Seq)}
			}
			if ev.PrevHash != prev.Hash {
				return VerifyResult{Events: len(events), BrokenAt: ev.Seq, Problem: "previous hash mismatch"}
			}
		}
		if digest(ev) != ev.Hash {
			return VerifyResult{Events: len(events), BrokenAt: ev.Seq, Problem: "event content does not match its hash"}
		}
		res.HeadHash = ev.Hash
	}
	return res
}
