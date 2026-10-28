// Package id generates sortable, prefixed resource identifiers.
//
// IDs are a type prefix plus a 26-character ULID-style body (48-bit
// millisecond timestamp, 80 random bits, Crockford base32), so they sort by
// creation time and are safe in URLs, labels and file names.
package id

import (
	"crypto/rand"
	"encoding/binary"
	"strings"
	"sync"
	"time"
)

const alphabet = "0123456789abcdefghjkmnpqrstvwxyz"

var (
	mu       sync.Mutex
	lastMs   uint64
	lastRand [10]byte
)

// New returns prefix_<ulid>.
func New(prefix string) string {
	mu.Lock()
	ms := uint64(time.Now().UnixMilli())
	var r [10]byte
	if ms == lastMs {
		// Monotonic within a millisecond: increment the previous random part.
		r = lastRand
		for i := len(r) - 1; i >= 0; i-- {
			r[i]++
			if r[i] != 0 {
				break
			}
		}
	} else {
		_, _ = rand.Read(r[:])
	}
	lastMs, lastRand = ms, r
	mu.Unlock()

	var b [16]byte
	binary.BigEndian.PutUint16(b[0:2], uint16(ms>>32))
	binary.BigEndian.PutUint32(b[2:6], uint32(ms))
	copy(b[6:], r[:])
	return prefix + "_" + encode(b)
}

// Secret returns n random bytes encoded in the same alphabet.
func Secret(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	var sb strings.Builder
	for _, c := range buf {
		sb.WriteByte(alphabet[c&31])
	}
	return sb.String()
}

func encode(b [16]byte) string {
	// 128 bits -> 26 base32 characters (first char carries 3 bits).
	hi := binary.BigEndian.Uint64(b[0:8])
	lo := binary.BigEndian.Uint64(b[8:16])
	out := make([]byte, 26)
	for i := 25; i >= 0; i-- {
		out[i] = alphabet[lo&31]
		lo = lo>>5 | (hi&31)<<59
		hi >>= 5
	}
	return string(out)
}
