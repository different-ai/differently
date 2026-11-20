package daytona

import (
	"testing"

	"github.com/different-ai/differently/internal/acid"
	"github.com/different-ai/differently/internal/acid/acidtest"
)

// TestAcidAgainstFake runs the acid test against the fake Daytona API. Only
// level 0 is checked: it exercises the adapter's own logic (idempotency,
// listing by label, delete, error classes, honest features). The fake does
// not run commands or list files, so levels 1 and 2 are logged, not judged;
// a live run judges them.
func TestAcidAgainstFake(t *testing.T) {
	f := newFake(t)
	a := New(Options{APIURL: f.srv.URL + "/api", APIKey: "k", Snapshot: "daytona-small"})
	acidtest.Check(t, a, acidtest.Quick(), acidtest.All(acid.Pass, 0))
}
