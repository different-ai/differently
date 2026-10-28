package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/different-ai/differently/internal/guest"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/adapter/adapterhttp"
	"github.com/different-ai/differently/pkg/api"
)

// fakeLauncher stands in for the Firecracker binary: it serves a fake VMM
// API on the VM's API socket and a guest agent behind a vsock-style
// "CONNECT <port>" handshake on the VM's vsock socket.
type fakeLauncher struct {
	mu      sync.Mutex
	next    int
	alive   map[int][]io.Closer
	calls   []string
	guestFS map[string]string // vm id -> guest root
}

func newFakeLauncher() *fakeLauncher {
	return &fakeLauncher{next: 100, alive: map[int][]io.Closer{}, guestFS: map[string]string{}}
}

func (f *fakeLauncher) Launch(vm *VM) (int, error) {
	api, err := net.Listen("unix", vm.APISocket())
	if err != nil {
		return 0, err
	}
	vs, err := net.Listen("unix", vm.VsockPath())
	if err != nil {
		return 0, err
	}
	apiSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.calls = append(f.calls, fmt.Sprintf("%s %s %v", r.Method, r.URL.Path, body))
		f.mu.Unlock()
		w.WriteHeader(204)
	})}
	go apiSrv.Serve(api)
	f.mu.Lock()
	root, ok := f.guestFS[vm.ID]
	if !ok {
		root, _ = os.MkdirTemp("", "guest")
		f.guestFS[vm.ID] = root
	}
	f.mu.Unlock()
	guestSrv := &http.Server{Handler: guest.Handler(&guest.Runner{Root: root})}
	go guestSrv.Serve(&handshakeListener{Listener: vs})
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	f.alive[f.next] = []io.Closer{apiSrv, guestSrv}
	return f.next, nil
}

func (f *fakeLauncher) Kill(pid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.alive[pid] {
		c.Close()
	}
	delete(f.alive, pid)
	return nil
}

func (f *fakeLauncher) Alive(pid int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.alive[pid]
	return ok
}

// handshakeListener implements Firecracker's host-initiated vsock protocol.
type handshakeListener struct{ net.Listener }

func (l *handshakeListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil || line != fmt.Sprintf("CONNECT %d\n", GuestPort) {
		c.Close()
		return l.Accept()
	}
	fmt.Fprintf(c, "OK 1073741824\n")
	return &bufferedConn{Conn: c, r: br}, nil
}

type recordingRunner struct {
	mu   sync.Mutex
	cmds []string
}

func (r *recordingRunner) Run(name string, args ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cmds = append(r.cmds, name+" "+strings.Join(args, " "))
	return nil
}

func (r *recordingRunner) has(sub string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.cmds {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

func newManager(t *testing.T, l *fakeLauncher, r *recordingRunner) (*Manager, string) {
	t.Helper()
	// Unix socket paths are length-limited; keep them short.
	dir, err := os.MkdirTemp("/tmp", "dlyag")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	images := filepath.Join(dir, "img")
	_ = os.MkdirAll(images, 0o755)
	_ = os.WriteFile(filepath.Join(images, "default.ext4"), []byte("rootfs-image"), 0o644)
	_ = os.WriteFile(filepath.Join(images, "vmlinux"), []byte("kernel"), 0o644)
	m, err := New(Config{
		Pool: "fc", Node: "node-1", StateDir: filepath.Join(dir, "s"), ImagesDir: images, Kernel: filepath.Join(images, "vmlinux"),
		TotalCPU: 8, TotalMemoryMiB: 16384, ReserveCPU: 1, ReserveMemoryMiB: 1024,
	}, l, r)
	if err != nil {
		t.Fatal(err)
	}
	return m, dir
}

func TestMicroVMLifecycleOverAdapterProtocol(t *testing.T) {
	l, r := newFakeLauncher(), &recordingRunner{}
	m, _ := newManager(t, l, r)
	if err := m.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Drive the manager exactly as the control plane does: over HTTP.
	srv := http.Server{Handler: adapterhttp.Handler(m, "agent-secret")}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go srv.Serve(ln)
	defer srv.Close()
	c := adapterhttp.NewClient("http://"+ln.Addr().String(), "agent-secret")
	ctx := context.Background()

	info, err := c.Discover(ctx)
	if err != nil || !info.Capabilities.Has(adapter.FeaturePauseResume) || info.Capabilities.Has(adapter.FeatureSnapshotMem) {
		t.Fatalf("discover %+v %v", info, err)
	}
	inst, err := c.Create(ctx, adapter.CreateRequest{ID: "sbx_1", Installation: "inst", Resources: api.Resources{CPU: 2, MemoryMiB: 2048}, Env: map[string]string{"GREETING": "hello"}})
	if err != nil || inst.State != adapter.StateRunning {
		t.Fatalf("create %+v %v", inst, err)
	}
	joined := strings.Join(l.calls, "\n")
	for _, want := range []string{"PUT /machine-config map[mem_size_mib:2048 smt:false vcpu_count:2]", "PUT /drives/rootfs", "PUT /network-interfaces/eth0", "PUT /vsock", "InstanceStart"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing VMM call %q in:\n%s", want, joined)
		}
	}
	// Network isolation: no access to the node or metadata, NAT for egress.
	for _, want := range []string{"ip tuntap add dev dly0", "INPUT -i dly0 -j DROP", "-d 169.254.0.0/16 -j DROP", "MASQUERADE"} {
		if !r.has(want) {
			t.Fatalf("missing network rule %q in %v", want, r.cmds)
		}
	}
	res, err := c.Exec(ctx, inst.Ref, api.ExecRequest{Command: "echo $GREETING from guest"})
	if err != nil || strings.TrimSpace(res.Stdout) != "hello from guest" {
		t.Fatalf("exec through vsock %+v %v", res, err)
	}
	if err := c.WriteFile(ctx, inst.Ref, "/work/x", []byte("data")); err != nil {
		t.Fatal(err)
	}
	if b, err := c.ReadFile(ctx, inst.Ref, "/work/x"); err != nil || string(b) != "data" {
		t.Fatalf("read %q %v", b, err)
	}
	capa, _ := c.Capacity(ctx)
	if capa.FreeCPU != 5 || capa.FreeMemoryMiB != 13312 {
		t.Fatalf("capacity accounting %+v", capa)
	}
	if err := c.Pause(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, inst.Ref, api.ExecRequest{Command: "true"}); !api.IsCode(err, api.CodeInvalidState) {
		t.Fatalf("exec while paused: %v", err)
	}
	if err := c.Resume(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	snap, err := c.Snapshot(ctx, inst.Ref, "disk")
	if err != nil || snap.Kind != "disk" {
		t.Fatalf("snapshot %+v %v", snap, err)
	}
	if _, err := c.Snapshot(ctx, inst.Ref, "memory"); !api.IsCode(err, api.CodeUnsupportedOperation) {
		t.Fatalf("memory snapshot should be unsupported: %v", err)
	}
	if again, _ := c.Create(ctx, adapter.CreateRequest{ID: "sbx_1"}); again.Ref != inst.Ref {
		t.Fatal("create is not idempotent")
	}
	if _, err := c.Create(ctx, adapter.CreateRequest{ID: "sbx_big", Resources: api.Resources{CPU: 6, MemoryMiB: 1024}}); !api.IsCode(err, adapter.CodeInsufficientCapacity) {
		t.Fatalf("overcommit allowed: %v", err)
	}
	if err := c.Delete(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	if !r.has("ip link del dly0") || len(l.alive) != 0 {
		t.Fatalf("teardown incomplete: alive=%d", len(l.alive))
	}
	if list, _ := c.List(ctx, "inst"); len(list) != 0 {
		t.Fatalf("list after delete %v", list)
	}
	if _, err := adapterhttp.NewClient("http://"+ln.Addr().String(), "wrong").Capacity(ctx); !api.IsCode(err, api.CodeUnauthenticated) {
		t.Fatalf("agent accepted a bad token: %v", err)
	}
}

func TestRestartRecoversVMs(t *testing.T) {
	l, r := newFakeLauncher(), &recordingRunner{}
	m, dir := newManager(t, l, r)
	ctx := context.Background()
	if _, err := m.Create(ctx, adapter.CreateRequest{ID: "sbx_a", Installation: "i", Resources: api.Resources{CPU: 1, MemoryMiB: 512}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(ctx, adapter.CreateRequest{ID: "sbx_b", Installation: "i", Resources: api.Resources{CPU: 1, MemoryMiB: 512}}); err != nil {
		t.Fatal(err)
	}
	vmB, _ := m.lookup("sbx_b")
	_ = l.Kill(vmB.PID) // VM b dies while the agent is down
	m2, err := New(m.cfg, l, r)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := m2.Get(ctx, "sbx_a")
	b, _ := m2.Get(ctx, "sbx_b")
	if a.State != adapter.StateRunning || b.State != adapter.StateStopped {
		t.Fatalf("recovered states a=%s b=%s (dir %s)", a.State, b.State, dir)
	}
	r.mu.Lock()
	r.cmds = nil
	r.mu.Unlock()
	if err := m2.Start(ctx, "sbx_b"); err != nil {
		t.Fatal(err)
	}
	if !r.has("ip tuntap add dev " + vmB.Tap) {
		t.Fatal("start did not recreate the VM's network")
	}
	if b, _ := m2.Get(ctx, "sbx_b"); b.State != adapter.StateRunning {
		t.Fatalf("restart from retained disk failed: %s", b.State)
	}
}

func TestEgressDeny(t *testing.T) {
	r := &recordingRunner{}
	m := &Manager{cfg: Config{Egress: "deny", DenyCIDRs: DefaultDenyCIDRs}, runner: r}
	vm := &VM{Index: 3}
	vm.Tap, vm.HostIP, vm.GuestIP, vm.MAC = addressing(3)
	rules := fmt.Sprint(m.networkRules(vm))
	if strings.Contains(rules, "MASQUERADE") || !strings.Contains(rules, "-A DLY-3 -j DROP") || subnet(vm) != "172.30.0.12/30" {
		t.Fatalf("deny rules %s subnet %s", rules, subnet(vm))
	}
}
