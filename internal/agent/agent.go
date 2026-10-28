// Package agent is the Differently node agent. It runs on one KVM host,
// manages Firecracker microVMs there and serves them to the control plane
// through the adapter protocol (pkg/adapter/adapterhttp).
//
// Per microVM it keeps a state directory with a copy-on-write-friendly copy
// of the template rootfs, the Firecracker API socket and the vsock socket used
// to reach the in-guest agent. Each VM gets its own tap device and /30
// subnet; host firewall rules isolate VMs from each other, from the node and,
// by default, from cloud metadata and private address ranges.
package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/different-ai/differently/internal/firecracker"
	"github.com/different-ai/differently/internal/guest"
	"github.com/different-ai/differently/internal/id"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// GuestPort is the vsock port the in-guest agent listens on.
const GuestPort = 10000

// Config configures the agent.
type Config struct {
	Pool             string
	Node             string
	StateDir         string
	ImagesDir        string
	Kernel           string
	Firecracker      string
	TotalCPU         int
	TotalMemoryMiB   int
	ReserveCPU       int
	ReserveMemoryMiB int
	CPUOvercommit    float64
	Egress           string   // allow | deny
	DenyCIDRs        []string // blocked even when egress is allowed
	DNS              []string // nameservers written into each guest
	BootTimeout      time.Duration
	Log              *slog.Logger
}

// DefaultDenyCIDRs keeps sandboxes away from cloud metadata, the node and
// private networks (including typical cluster ranges).
var DefaultDenyCIDRs = []string{"169.254.0.0/16", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10"}

// Launcher starts and stops Firecracker processes. The default execs the
// binary; tests substitute an in-process fake.
type Launcher interface {
	Launch(vm *VM) (pid int, err error)
	Kill(pid int) error
	Alive(pid int) bool
}

// Runner executes host commands (ip, iptables, resize2fs).
type Runner interface {
	Run(name string, args ...string) error
}

// VM is the persisted state of one microVM.
type VM struct {
	ID           string            `json:"id"`
	SandboxID    string            `json:"sandboxId"`
	Installation string            `json:"installation"`
	Template     string            `json:"template"`
	Resources    api.Resources     `json:"resources"`
	Env          map[string]string `json:"env,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	State        string            `json:"state"`
	PID          int               `json:"pid"`
	Index        int               `json:"index"`
	Tap          string            `json:"tap"`
	HostIP       string            `json:"hostIP"`
	GuestIP      string            `json:"guestIP"`
	MAC          string            `json:"mac"`
	Dir          string            `json:"dir"`
	CreatedAt    time.Time         `json:"createdAt"`
}

// APISocket is the Firecracker API socket path.
func (v *VM) APISocket() string { return filepath.Join(v.Dir, "fc.sock") }

// VsockPath is the host side of the guest vsock device.
func (v *VM) VsockPath() string { return filepath.Join(v.Dir, "vsock.sock") }

// Rootfs is the VM's writable root disk.
func (v *VM) Rootfs() string { return filepath.Join(v.Dir, "rootfs.ext4") }

// Manager owns every microVM on the node.
type Manager struct {
	cfg      Config
	launcher Launcher
	runner   Runner
	log      *slog.Logger

	mu  sync.Mutex
	vms map[string]*VM
}

// New loads persisted VMs from the state directory and returns a manager.
func New(cfg Config, l Launcher, r Runner) (*Manager, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.CPUOvercommit <= 0 {
		cfg.CPUOvercommit = 1
	}
	if cfg.BootTimeout == 0 {
		cfg.BootTimeout = 30 * time.Second
	}
	if cfg.Egress == "" {
		cfg.Egress = "allow"
	}
	if cfg.DenyCIDRs == nil {
		cfg.DenyCIDRs = DefaultDenyCIDRs
	}
	if cfg.TotalCPU == 0 {
		cfg.TotalCPU = runtime.NumCPU()
	}
	if cfg.TotalMemoryMiB == 0 {
		cfg.TotalMemoryMiB = hostMemoryMiB()
	}
	m := &Manager{cfg: cfg, launcher: l, runner: r, log: cfg.Log, vms: map[string]*VM{}}
	for _, d := range []string{"vms", "snapshots"} {
		if err := os.MkdirAll(filepath.Join(cfg.StateDir, d), 0o700); err != nil {
			return nil, err
		}
	}
	return m, m.recover()
}

// recover reloads VM records after an agent restart. VMs whose Firecracker
// process is gone are reported as stopped; their disks are kept.
func (m *Manager) recover() error {
	entries, err := os.ReadDir(filepath.Join(m.cfg.StateDir, "vms"))
	if err != nil {
		return err
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(m.cfg.StateDir, "vms", e.Name(), "vm.json"))
		if err != nil {
			continue
		}
		var vm VM
		if json.Unmarshal(b, &vm) != nil {
			continue
		}
		switch {
		case vm.State == adapter.StatePending:
			// Interrupted mid-create: release its capacity; the disk is kept so
			// a retried create can be adopted or the reconciler can clean up.
			m.log.Warn("microVM was being created when the agent stopped; marking stopped", "vm", vm.ID)
			if vm.PID > 0 {
				_ = m.launcher.Kill(vm.PID)
			}
			vm.State, vm.PID = adapter.StateStopped, 0
			_ = m.save(&vm)
		case (vm.State == adapter.StateRunning || vm.State == adapter.StatePaused) && !m.launcher.Alive(vm.PID):
			m.log.Warn("microVM process is gone after restart; marking stopped", "vm", vm.ID)
			vm.State, vm.PID = adapter.StateStopped, 0
			_ = m.save(&vm)
		}
		m.vms[vm.ID] = &vm
	}
	return nil
}

func (m *Manager) save(vm *VM) error {
	b, _ := json.MarshalIndent(vm, "", "  ")
	tmp := filepath.Join(vm.Dir, "vm.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(vm.Dir, "vm.json"))
}

// Templates lists rootfs images available on this node.
func (m *Manager) Templates() []string {
	matches, _ := filepath.Glob(filepath.Join(m.cfg.ImagesDir, "*.ext4"))
	var out []string
	for _, p := range matches {
		out = append(out, strings.TrimSuffix(filepath.Base(p), ".ext4"))
	}
	sort.Strings(out)
	return out
}

func (m *Manager) allocatable() (cpu, mem int) {
	cpu = int(float64(m.cfg.TotalCPU-m.cfg.ReserveCPU) * m.cfg.CPUOvercommit)
	mem = m.cfg.TotalMemoryMiB - m.cfg.ReserveMemoryMiB
	return max(cpu, 0), max(mem, 0)
}

func (m *Manager) usedLocked() (cpu, mem int) {
	for _, vm := range m.vms {
		if vm.State == adapter.StateRunning || vm.State == adapter.StatePaused || vm.State == adapter.StatePending {
			cpu += vm.Resources.CPU
			mem += vm.Resources.MemoryMiB
		}
	}
	return
}

// Info implements adapter.ExecutionAdapter.
func (m *Manager) Info() adapter.Info {
	return adapter.Info{Kind: "firecracker-node", Version: "1", Trust: api.TrustPrivate, Isolation: "firecracker microVM (KVM)", Protocol: adapter.ProtocolVersion}
}

// Features are what this node supports. Memory snapshots are not advertised
// until restore is proven (see docs/roadmap.md).
func (m *Manager) Features() []string {
	f := []string{
		adapter.FeatureExec, adapter.FeatureExecStream, adapter.FeatureFiles, adapter.FeatureStopStart, adapter.FeaturePersistDisk,
		adapter.FeaturePauseResume, adapter.FeatureSnapshotDisk, adapter.FeatureFork, adapter.FeatureEgressControl, adapter.FeatureMicroVM,
	}
	if runtime.GOARCH == "arm64" {
		return append(f, adapter.FeatureArchARM64)
	}
	return append(f, adapter.FeatureArchAMD64)
}

// Capabilities implements adapter.ExecutionAdapter.
func (m *Manager) Capabilities(context.Context) (adapter.Capabilities, error) {
	cpu, mem := m.allocatable()
	return adapter.Capabilities{Features: m.Features(), MaxCPU: cpu, MaxMemoryMiB: mem, Templates: m.Templates()}, nil
}

// Capacity implements adapter.ExecutionAdapter.
func (m *Manager) Capacity(context.Context) (adapter.Capacity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cpu, mem := m.allocatable()
	ucpu, umem := m.usedLocked()
	return adapter.Capacity{Bounded: true, TotalCPU: cpu, TotalMemoryMiB: mem, FreeCPU: max(cpu-ucpu, 0), FreeMemoryMiB: max(mem-umem, 0), Nodes: 1}, nil
}

// Health implements adapter.ExecutionAdapter.
func (m *Manager) Health(context.Context) error {
	if _, err := os.Stat(m.cfg.Kernel); err != nil {
		return fmt.Errorf("guest kernel: %w", err)
	}
	if len(m.Templates()) == 0 {
		return fmt.Errorf("no rootfs templates in %s", m.cfg.ImagesDir)
	}
	return nil
}

func instance(vm *VM) adapter.Instance {
	return adapter.Instance{Ref: vm.ID, SandboxID: vm.SandboxID, State: vm.State}
}

// Create implements adapter.ExecutionAdapter. It is idempotent on req.ID.
func (m *Manager) Create(ctx context.Context, req adapter.CreateRequest) (adapter.Instance, error) {
	m.mu.Lock()
	if vm, ok := m.vms[req.ID]; ok {
		m.mu.Unlock()
		return instance(vm), nil
	}
	cpu, mem := m.allocatable()
	ucpu, umem := m.usedLocked()
	if ucpu+req.Resources.CPU > cpu || umem+req.Resources.MemoryMiB > mem {
		m.mu.Unlock()
		return adapter.Instance{}, adapter.InsufficientCapacity("node " + m.cfg.Node + " is full")
	}
	template := req.Template
	if template == "" {
		template = "default"
	}
	src := filepath.Join(m.cfg.ImagesDir, template+".ext4")
	if req.FromSnapshot != "" {
		src = filepath.Join(m.cfg.StateDir, "snapshots", filepath.Base(req.FromSnapshot), "rootfs.ext4")
	}
	if _, err := os.Stat(src); err != nil {
		m.mu.Unlock()
		return adapter.Instance{}, api.Errorf(api.CodeInvalidRequest, "image %s is not available on node %s", filepath.Base(src), m.cfg.Node)
	}
	vm := &VM{
		ID: req.ID, SandboxID: req.ID, Installation: req.Installation, Template: template, Resources: req.Resources,
		Env: req.Env, Labels: adapter.OwnershipLabels(req), State: adapter.StatePending, Index: m.freeIndexLocked(),
		Dir: filepath.Join(m.cfg.StateDir, "vms", filepath.Base(req.ID)), CreatedAt: time.Now().UTC(),
	}
	vm.Tap, vm.HostIP, vm.GuestIP, vm.MAC = addressing(vm.Index)
	m.vms[vm.ID] = vm // reserves capacity and the index while we build
	m.mu.Unlock()

	if err := m.build(ctx, vm, src); err != nil {
		m.log.Error("microVM create failed", "vm", vm.ID, "err", err)
		m.destroy(vm)
		m.mu.Lock()
		delete(m.vms, vm.ID)
		m.mu.Unlock()
		return adapter.Instance{}, adapter.Unavailable(err)
	}
	return instance(vm), nil
}

func (m *Manager) freeIndexLocked() int {
	used := map[int]bool{}
	for _, vm := range m.vms {
		used[vm.Index] = true
	}
	for i := 0; ; i++ {
		if !used[i] {
			return i
		}
	}
}

// addressing derives the tap name, /30 subnet and MAC for VM slot i. Slots
// live in 172.30.0.0/16, giving 16384 VMs per node.
func addressing(i int) (tap, hostIP, guestIP, mac string) {
	base := i * 4
	o3, o4 := (base>>8)&0xff, base&0xff
	return fmt.Sprintf("dly%d", i), fmt.Sprintf("172.30.%d.%d", o3, o4+1), fmt.Sprintf("172.30.%d.%d", o3, o4+2),
		fmt.Sprintf("06:00:ac:1e:%02x:%02x", o3, o4+2)
}

func (m *Manager) build(ctx context.Context, vm *VM, src string) error {
	if err := os.MkdirAll(vm.Dir, 0o700); err != nil {
		return err
	}
	if err := copyFile(src, vm.Rootfs()); err != nil {
		return fmt.Errorf("copy rootfs: %w", err)
	}
	if want := int64(vm.Resources.DiskGiB) << 30; want > 0 {
		if st, err := os.Stat(vm.Rootfs()); err == nil && st.Size() < want {
			if err := os.Truncate(vm.Rootfs(), want); err != nil {
				return err
			}
			if err := m.runner.Run("resize2fs", vm.Rootfs()); err != nil {
				return fmt.Errorf("resize2fs: %w", err)
			}
		}
	}
	if err := m.setupNetwork(vm); err != nil {
		return fmt.Errorf("network: %w", err)
	}
	return m.boot(ctx, vm)
}

func (m *Manager) boot(ctx context.Context, vm *VM) error {
	_ = os.Remove(vm.APISocket())
	_ = os.Remove(vm.VsockPath())
	pid, err := m.launcher.Launch(vm)
	if err != nil {
		return fmt.Errorf("launch firecracker: %w", err)
	}
	vm.PID = pid
	if err := waitForSocket(ctx, vm.APISocket(), 5*time.Second); err != nil {
		return err
	}
	fc := firecracker.New(vm.APISocket())
	bootArgs := fmt.Sprintf("console=ttyS0 reboot=k panic=1 pci=off ip=%s::%s:255.255.255.252::eth0:off", vm.GuestIP, vm.HostIP)
	err = fc.Configure(ctx,
		firecracker.MachineConfig{VCPUCount: vm.Resources.CPU, MemSizeMiB: vm.Resources.MemoryMiB},
		firecracker.BootSource{KernelImagePath: m.cfg.Kernel, BootArgs: bootArgs},
		[]firecracker.Drive{{DriveID: "rootfs", PathOnHost: vm.Rootfs(), IsRootDevice: true}},
		[]firecracker.NetworkInterface{{IfaceID: "eth0", HostDevName: vm.Tap, GuestMAC: vm.MAC}},
		&firecracker.Vsock{GuestCID: 3, UDSPath: vm.VsockPath()})
	if err == nil {
		err = fc.Start(ctx)
	}
	if err != nil {
		return err
	}
	if err := m.guest(vm).WaitReady(ctx, m.cfg.BootTimeout); err != nil {
		return err
	}
	if len(m.cfg.DNS) > 0 {
		// Node and cluster resolvers are unreachable from the VM by design.
		var conf strings.Builder
		for _, ns := range m.cfg.DNS {
			conf.WriteString("nameserver " + ns + "\n")
		}
		if err := m.guest(vm).WriteFile(ctx, "/etc/resolv.conf", []byte(conf.String())); err != nil {
			m.log.Warn("could not configure guest DNS", "vm", vm.ID, "err", err)
		}
	}
	vm.State = adapter.StateRunning
	return m.save(vm)
}

func waitForSocket(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("unix", path); err == nil {
			c.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	return fmt.Errorf("firecracker API socket %s did not appear", path)
}

// guest returns a client for the in-VM agent over Firecracker's vsock
// multiplexer: connect to the host socket, send "CONNECT <port>", expect "OK".
func (m *Manager) guest(vm *VM) *guest.Client {
	return guest.NewClient(func(ctx context.Context) (net.Conn, error) {
		c, err := (&net.Dialer{}).DialContext(ctx, "unix", vm.VsockPath())
		if err != nil {
			return nil, err
		}
		if _, err := fmt.Fprintf(c, "CONNECT %d\n", GuestPort); err != nil {
			c.Close()
			return nil, err
		}
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		br := bufio.NewReader(c)
		line, err := br.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "OK ") {
			c.Close()
			return nil, fmt.Errorf("vsock handshake failed: %q %v", line, err)
		}
		_ = c.SetReadDeadline(time.Time{})
		return &bufferedConn{Conn: c, r: br}, nil
	})
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func (m *Manager) lookup(ref string) (*VM, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	vm, ok := m.vms[ref]
	if !ok {
		return nil, adapter.NotFound(ref)
	}
	return vm, nil
}

func (m *Manager) running(ref string) (*VM, error) {
	vm, err := m.lookup(ref)
	if err != nil {
		return nil, err
	}
	if vm.State != adapter.StateRunning {
		return nil, api.Errorf(api.CodeInvalidState, "microVM %s is %s", ref, vm.State)
	}
	return vm, nil
}

// Get implements adapter.ExecutionAdapter.
func (m *Manager) Get(_ context.Context, ref string) (adapter.Instance, error) {
	vm, err := m.lookup(ref)
	if err != nil {
		return adapter.Instance{}, err
	}
	return instance(vm), nil
}

// List implements adapter.ExecutionAdapter.
func (m *Manager) List(_ context.Context, installation string) ([]adapter.Instance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []adapter.Instance{}
	for _, vm := range m.vms {
		if vm.State != adapter.StatePending && (installation == "" || vm.Installation == installation) {
			out = append(out, instance(vm))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, nil
}

// Delete implements adapter.ExecutionAdapter.
func (m *Manager) Delete(_ context.Context, ref string) error {
	vm, err := m.lookup(ref)
	if err != nil {
		return nil
	}
	m.destroy(vm)
	m.mu.Lock()
	delete(m.vms, ref)
	m.mu.Unlock()
	return nil
}

func (m *Manager) destroy(vm *VM) {
	if vm.PID > 0 {
		_ = m.launcher.Kill(vm.PID)
	}
	m.teardownNetwork(vm)
	_ = os.RemoveAll(vm.Dir)
}

func (m *Manager) execRequest(vm *VM, req api.ExecRequest) api.ExecRequest {
	env := map[string]string{}
	for k, v := range vm.Env {
		env[k] = v
	}
	for k, v := range req.Env {
		env[k] = v
	}
	req.Env = env
	return req
}

// Exec implements adapter.ExecutionAdapter.
func (m *Manager) Exec(ctx context.Context, ref string, req api.ExecRequest) (api.ExecResult, error) {
	vm, err := m.running(ref)
	if err != nil {
		return api.ExecResult{}, err
	}
	return m.guest(vm).Exec(ctx, m.execRequest(vm, req))
}

// ExecStream implements adapter.Streamer.
func (m *Manager) ExecStream(ctx context.Context, ref string, req api.ExecRequest, emit func(api.ExecEvent) error) (int, error) {
	vm, err := m.running(ref)
	if err != nil {
		return -1, err
	}
	return m.guest(vm).ExecStream(ctx, m.execRequest(vm, req), emit)
}

// ReadFile implements adapter.FileSystem.
func (m *Manager) ReadFile(ctx context.Context, ref, path string) ([]byte, error) {
	vm, err := m.running(ref)
	if err != nil {
		return nil, err
	}
	return m.guest(vm).ReadFile(ctx, path)
}

// WriteFile implements adapter.FileSystem.
func (m *Manager) WriteFile(ctx context.Context, ref, path string, data []byte) error {
	vm, err := m.running(ref)
	if err != nil {
		return err
	}
	return m.guest(vm).WriteFile(ctx, path, data)
}

// ListFiles implements adapter.FileSystem.
func (m *Manager) ListFiles(ctx context.Context, ref, path string) ([]api.FileInfo, error) {
	vm, err := m.running(ref)
	if err != nil {
		return nil, err
	}
	return m.guest(vm).ListFiles(ctx, path)
}

// Stop shuts the guest down and keeps its disk.
func (m *Manager) Stop(ctx context.Context, ref string) error {
	vm, err := m.lookup(ref)
	if err != nil {
		return err
	}
	if vm.State == adapter.StateStopped {
		return nil
	}
	_ = firecracker.New(vm.APISocket()).CtrlAltDel(ctx)
	deadline := time.Now().Add(10 * time.Second)
	for m.launcher.Alive(vm.PID) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	_ = m.launcher.Kill(vm.PID)
	m.mu.Lock()
	vm.State, vm.PID = adapter.StateStopped, 0
	m.mu.Unlock()
	return m.save(vm)
}

// Start boots a stopped VM from its retained disk.
func (m *Manager) Start(ctx context.Context, ref string) error {
	vm, err := m.lookup(ref)
	if err != nil {
		return err
	}
	if vm.State == adapter.StateRunning {
		return nil
	}
	m.mu.Lock()
	cpu, mem := m.allocatable()
	ucpu, umem := m.usedLocked()
	if ucpu+vm.Resources.CPU > cpu || umem+vm.Resources.MemoryMiB > mem {
		m.mu.Unlock()
		return adapter.InsufficientCapacity("node " + m.cfg.Node + " is full")
	}
	vm.State = adapter.StatePending
	m.mu.Unlock()
	// Networking does not survive a host reboot; recreate it every start.
	if err := m.setupNetwork(vm); err != nil {
		vm.State = adapter.StateStopped
		_ = m.save(vm)
		return adapter.Unavailable(err)
	}
	if err := m.boot(ctx, vm); err != nil {
		vm.State = adapter.StateStopped
		_ = m.save(vm)
		return adapter.Unavailable(err)
	}
	return nil
}

// Pause freezes the VM's vCPUs.
func (m *Manager) Pause(ctx context.Context, ref string) error {
	vm, err := m.running(ref)
	if err != nil {
		return err
	}
	if err := firecracker.New(vm.APISocket()).Pause(ctx); err != nil {
		return err
	}
	vm.State = adapter.StatePaused
	return m.save(vm)
}

// Resume unfreezes a paused VM.
func (m *Manager) Resume(ctx context.Context, ref string) error {
	vm, err := m.lookup(ref)
	if err != nil {
		return err
	}
	if vm.State != adapter.StatePaused {
		return api.Errorf(api.CodeInvalidState, "microVM %s is %s", ref, vm.State)
	}
	if err := firecracker.New(vm.APISocket()).Resume(ctx); err != nil {
		return err
	}
	vm.State = adapter.StateRunning
	return m.save(vm)
}

// Snapshot captures a crash-consistent copy of the root disk: the VM is
// paused for the copy so the guest cannot write mid-snapshot.
func (m *Manager) Snapshot(ctx context.Context, ref, kind string) (adapter.Snapshot, error) {
	if kind == "memory" {
		return adapter.Snapshot{}, adapter.Unsupported("snapshots.memory")
	}
	vm, err := m.lookup(ref)
	if err != nil {
		return adapter.Snapshot{}, err
	}
	fc := firecracker.New(vm.APISocket())
	if vm.State == adapter.StateRunning {
		if err := fc.Pause(ctx); err != nil {
			return adapter.Snapshot{}, err
		}
		defer fc.Resume(context.WithoutCancel(ctx))
	}
	snap := id.New("snap")
	dir := filepath.Join(m.cfg.StateDir, "snapshots", snap)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return adapter.Snapshot{}, err
	}
	if err := copyFile(vm.Rootfs(), filepath.Join(dir, "rootfs.ext4")); err != nil {
		_ = os.RemoveAll(dir)
		return adapter.Snapshot{}, err
	}
	st, _ := os.Stat(filepath.Join(dir, "rootfs.ext4"))
	return adapter.Snapshot{Ref: snap, Kind: "disk", SizeBytes: st.Size()}, nil
}

// DeleteSnapshot removes a snapshot directory.
func (m *Manager) DeleteSnapshot(_ context.Context, ref string) error {
	return os.RemoveAll(filepath.Join(m.cfg.StateDir, "snapshots", filepath.Base(ref)))
}

// Status reports the node for heartbeats.
func (m *Manager) Status() api.Agent {
	c, _ := m.Capacity(context.Background())
	m.mu.Lock()
	n := len(m.vms)
	m.mu.Unlock()
	return api.Agent{
		Pool: m.cfg.Pool, Node: m.cfg.Node, Arch: runtime.GOARCH, TotalCPU: c.TotalCPU, FreeCPU: c.FreeCPU,
		TotalMemoryMiB: c.TotalMemoryMiB, FreeMemoryMiB: c.FreeMemoryMiB, Sandboxes: n, Capabilities: m.Features(), Templates: m.Templates(),
	}
}

func copyFile(src, dst string) error {
	// Prefer a reflink/sparse copy where coreutils supports it (Linux).
	if runtime.GOOS == "linux" {
		if exec.Command("cp", "--reflink=auto", "--sparse=always", src, dst).Run() == nil {
			return nil
		}
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func hostMemoryMiB() int {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "MemTotal:" {
			kb, _ := strconv.Atoi(f[1])
			return kb / 1024
		}
	}
	return 0
}

// ExecLauncher starts the real Firecracker binary. Firecracker runs in its
// own session, but it shares the agent's cgroup: when the agent's container
// (or systemd unit) restarts, its microVMs stop too and are recovered as
// stopped, restartable from their retained disks.
type ExecLauncher struct {
	Binary string
}

// Launch starts firecracker with its API socket in the VM directory.
func (l ExecLauncher) Launch(vm *VM) (int, error) {
	logf, err := os.OpenFile(filepath.Join(vm.Dir, "firecracker.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	defer logf.Close()
	cmd := exec.Command(l.Binary, "--api-sock", vm.APISocket(), "--id", strings.ReplaceAll(vm.ID, "_", "-"))
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	go func() { _ = cmd.Wait() }()
	return cmd.Process.Pid, nil
}

// Kill terminates a Firecracker process.
func (ExecLauncher) Kill(pid int) error {
	if pid <= 0 {
		return nil
	}
	err := syscall.Kill(pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// Alive reports whether pid is running.
func (ExecLauncher) Alive(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

// ExecRunner runs host commands.
type ExecRunner struct{}

// Run executes name with args and returns combined output on failure.
func (ExecRunner) Run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

var (
	_ adapter.ExecutionAdapter = (*Manager)(nil)
	_ adapter.Streamer         = (*Manager)(nil)
	_ adapter.FileSystem       = (*Manager)(nil)
	_ adapter.Lifecycle        = (*Manager)(nil)
	_ adapter.Pauser           = (*Manager)(nil)
	_ adapter.Snapshotter      = (*Manager)(nil)
)
