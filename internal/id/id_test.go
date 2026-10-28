package id

import (
	"strings"
	"testing"
)

func TestNewIsSortableAndUnique(t *testing.T) {
	seen := map[string]bool{}
	prev := ""
	for i := 0; i < 5000; i++ {
		v := New("sbx")
		if !strings.HasPrefix(v, "sbx_") || len(v) != 30 {
			t.Fatalf("bad id %q", v)
		}
		if seen[v] {
			t.Fatalf("duplicate %q", v)
		}
		if v <= prev {
			t.Fatalf("not monotonic: %q after %q", v, prev)
		}
		seen[v], prev = true, v
	}
}
