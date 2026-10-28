package firecracker

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/different-ai/differently/internal/adapters/fake"
	"github.com/different-ai/differently/internal/store"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/adapter/adapterhttp"
	"github.com/different-ai/differently/pkg/api"
)

// Two "nodes" speak the agent protocol; the pool aggregates and routes.
func TestPoolPlacesAndRoutesAcrossAgents(t *testing.T) {
	st := store.NewMemory()
	ctx := context.Background()
	nodes := map[string]*fake.Adapter{
		"node-a": fake.New(fake.Options{TotalCPU: 4, TotalMemoryMiB: 4096}),
		"node-b": fake.New(fake.Options{TotalCPU: 8, TotalMemoryMiB: 16384}),
	}
	for name, a := range nodes {
		srv := httptest.NewServer(adapterhttp.Handler(a, "tok"))
		t.Cleanup(srv.Close)
		c, _ := a.Capacity(ctx)
		_ = st.UpsertAgent(ctx, api.Agent{ID: name, Pool: "fc", Endpoint: srv.URL, TotalCPU: c.TotalCPU, FreeCPU: c.FreeCPU,
			TotalMemoryMiB: c.TotalMemoryMiB, FreeMemoryMiB: c.FreeMemoryMiB, Capabilities: fake.DefaultFeatures, LastSeen: time.Now()})
	}
	_ = st.UpsertAgent(ctx, api.Agent{ID: "stale", Pool: "fc", Endpoint: "http://127.0.0.1:1", FreeCPU: 64, FreeMemoryMiB: 1 << 20, LastSeen: time.Now().Add(-time.Hour)})
	p := New("fc", st, "tok", 30*time.Second)

	if err := p.Health(ctx); err != nil {
		t.Fatal(err)
	}
	capa, _ := p.Capacity(ctx)
	if capa.Nodes != 2 || capa.TotalCPU != 12 || capa.LargestFreeMemoryMiB != 16384 {
		t.Fatalf("stale agent counted or totals wrong: %+v", capa)
	}
	inst, err := p.Create(ctx, adapter.CreateRequest{ID: "sbx_1", Installation: "i", Resources: api.Resources{CPU: 2, MemoryMiB: 2048}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(inst.Ref, "node-b:") {
		t.Fatalf("expected the roomier node, got %s", inst.Ref)
	}
	if res, err := p.Exec(ctx, inst.Ref, api.ExecRequest{Command: "uname"}); err != nil || !strings.Contains(res.Stdout, "uname") {
		t.Fatalf("routed exec %+v %v", res, err)
	}
	snap, err := p.Snapshot(ctx, inst.Ref, "disk")
	if err != nil || !strings.HasPrefix(snap.Ref, "node-b:") {
		t.Fatalf("snapshot %+v %v", snap, err)
	}
	restored, err := p.Create(ctx, adapter.CreateRequest{ID: "sbx_2", Installation: "i", Resources: api.Resources{CPU: 1, MemoryMiB: 512}, FromSnapshot: snap.Ref})
	if err != nil || !strings.HasPrefix(restored.Ref, "node-b:") {
		t.Fatalf("restore must stay on the snapshot's node: %+v %v", restored, err)
	}
	list, err := p.List(ctx, "i")
	if err != nil || len(list) != 2 {
		t.Fatalf("list %v %v", list, err)
	}
	if _, err := p.Create(ctx, adapter.CreateRequest{ID: "sbx_3", Resources: api.Resources{CPU: 32, MemoryMiB: 1024}}); !api.IsCode(err, adapter.CodeInsufficientCapacity) {
		t.Fatalf("oversized create: %v", err)
	}
	if err := p.Delete(ctx, inst.Ref); err != nil || nodes["node-b"].Count() != 1 {
		t.Fatalf("delete %v count %d", err, nodes["node-b"].Count())
	}
	// A node that only just went quiet may be partitioned: no partial lists.
	_ = st.UpsertAgent(ctx, api.Agent{ID: "node-c", Pool: "fc", Endpoint: "http://127.0.0.1:1", LastSeen: time.Now().Add(-2 * time.Minute)})
	if _, err := p.List(ctx, "i"); !api.IsCode(err, api.CodeProviderUnavailable) {
		t.Fatalf("list during a possible partition must fail, got %v", err)
	}
}
