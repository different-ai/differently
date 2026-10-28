// Command differently-agent runs on each KVM node. It manages Firecracker
// microVMs locally, serves them to the control plane over the adapter
// protocol and heartbeats its capacity.
//
//	differently-agent --server URL --pool NAME [flags]   serve
//	differently-agent doctor                              check this node
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/different-ai/differently/internal/agent"
	"github.com/different-ai/differently/internal/version"
	"github.com/different-ai/differently/pkg/adapter/adapterhttp"
	"github.com/different-ai/differently/pkg/api"
)

type flags struct {
	server, pool, listen, advertise, node, stateDir, imagesDir, kernel, firecracker, jailer, egress, dns string
	reserveCPU, reserveMem                                                                               int
	overcommit                                                                                           float64
}

func main() {
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	host, _ := os.Hostname()
	f := flags{}
	fs := flag.NewFlagSet("differently-agent", flag.ExitOnError)
	fs.StringVar(&f.server, "server", os.Getenv("DIFFERENTLY_SERVER"), "control plane URL")
	fs.StringVar(&f.pool, "pool", os.Getenv("DIFFERENTLY_POOL"), "firecracker pool this node serves")
	fs.StringVar(&f.listen, "listen", ":9090", "adapter API listen address")
	fs.StringVar(&f.advertise, "advertise-url", os.Getenv("DIFFERENTLY_ADVERTISE_URL"), "URL the control plane uses to reach this agent")
	fs.StringVar(&f.node, "node", envOr("NODE_NAME", host), "node name")
	fs.StringVar(&f.stateDir, "state-dir", "/var/lib/differently/agent", "microVM state directory")
	fs.StringVar(&f.imagesDir, "images-dir", "/var/lib/differently/images", "directory of <template>.ext4 rootfs images")
	fs.StringVar(&f.kernel, "kernel", "/var/lib/differently/images/vmlinux", "guest kernel image")
	fs.StringVar(&f.firecracker, "firecracker", "/usr/local/bin/firecracker", "firecracker binary")
	fs.StringVar(&f.jailer, "jailer", "", "jailer binary (accepted for forward compatibility; microVMs do not run under the jailer yet)")
	fs.StringVar(&f.egress, "egress", "allow", "sandbox internet egress: allow (private ranges and metadata stay blocked) or deny")
	fs.StringVar(&f.dns, "dns", "1.1.1.1,9.9.9.9", "comma-separated public nameservers written into guests (empty to leave resolv.conf alone)")
	fs.IntVar(&f.reserveCPU, "reserve-cpu", 1, "CPUs kept for the host")
	fs.IntVar(&f.reserveMem, "reserve-memory-mib", 2048, "memory kept for the host")
	fs.Float64Var(&f.overcommit, "cpu-overcommit", 1.0, "vCPU overcommit factor")
	_ = fs.Parse(args)

	var err error
	switch cmd {
	case "serve":
		err = serve(f)
	case "doctor":
		err = doctor(f)
	case "version":
		fmt.Println(version.Version, version.Commit)
	default:
		err = fmt.Errorf("unknown command %q (serve, doctor, version)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func serve(f flags) error {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	token := os.Getenv("DIFFERENTLY_AGENT_TOKEN")
	switch {
	case f.server == "" || f.pool == "":
		return errors.New("--server and --pool are required")
	case token == "":
		return errors.New("DIFFERENTLY_AGENT_TOKEN is required")
	case f.advertise == "":
		return errors.New("--advertise-url is required (e.g. http://$(POD_IP):9090)")
	}
	runner := agent.ExecRunner{}
	if runtime.GOOS == "linux" {
		if err := runner.Run("sysctl", "-w", "net.ipv4.ip_forward=1"); err != nil {
			log.Warn("could not enable IP forwarding; sandboxes will have no network", "err", err)
		}
	}
	// The shared token only registers this node; the control plane must call
	// this agent with its own secret, pinned at first registration.
	secret, err := agent.LoadOrCreateSecret(f.stateDir)
	if err != nil {
		return fmt.Errorf("agent secret: %w", err)
	}
	m, err := agent.New(agent.Config{
		Pool: f.pool, Node: f.node, StateDir: f.stateDir, ImagesDir: f.imagesDir, Kernel: f.kernel, Firecracker: f.firecracker,
		ReserveCPU: f.reserveCPU, ReserveMemoryMiB: f.reserveMem, CPUOvercommit: f.overcommit, Egress: f.egress, DNS: splitList(f.dns), Log: log,
	}, agent.ExecLauncher{Binary: f.firecracker}, runner)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go heartbeat(ctx, log, f, token, secret, m)

	mux := http.NewServeMux()
	mux.Handle("/adapter/", adapterhttp.Handler(m, secret))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := m.Health(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	srv := &http.Server{Addr: f.listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Info("differently-agent started", "node", f.node, "pool", f.pool, "listen", f.listen, "version", version.Version)
	// MicroVMs share this process's cgroup: restarting the agent stops them,
	// and they come back as stopped sandboxes that can be started from disk.
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func heartbeat(ctx context.Context, log *slog.Logger, f flags, token, secret string, m *agent.Manager) {
	client := &http.Client{Timeout: 10 * time.Second}
	id := strings.NewReplacer(":", "-", "/", "-").Replace(f.node)
	for {
		st := m.Status()
		st.ID, st.Endpoint, st.Version = id, f.advertise, version.Version
		body, _ := json.Marshal(st)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(f.server, "/")+"/internal/v1/agents/heartbeat", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set(api.AgentSecretHeader, secret)
		req.Header.Set("Content-Type", "application/json")
		if resp, err := client.Do(req); err != nil {
			log.Warn("heartbeat failed", "err", err)
		} else {
			if resp.StatusCode == http.StatusConflict {
				log.Error("control plane refused this node's identity; if the node was rebuilt, an operator must remove the old agent (DELETE /v1/admin/agents/" + id + ")")
			} else if resp.StatusCode != 200 {
				log.Warn("heartbeat rejected", "status", resp.StatusCode)
			}
			resp.Body.Close()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}

// doctor checks everything the agent needs and explains failures.
func doctor(f flags) error {
	failed := 0
	check := func(ok bool, what, fix string) {
		if ok {
			fmt.Printf("[ok  ] %s\n", what)
			return
		}
		failed++
		fmt.Printf("[FAIL] %s\n       fix: %s\n", what, fix)
	}
	kvm, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err == nil {
		kvm.Close()
	}
	check(err == nil, "/dev/kvm is readable and writable",
		"use bare metal or a nested-virtualization instance type, mount /dev/kvm into the pod and run privileged (see docs/deploy/prerequisites.md)")
	tun, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
	if err == nil {
		tun.Close()
	}
	check(err == nil, "/dev/net/tun is available", "load the tun module (modprobe tun) and mount /dev/net/tun")
	out, err := exec.Command(f.firecracker, "--version").Output()
	check(err == nil, fmt.Sprintf("firecracker binary %s runs (%s)", f.firecracker, strings.TrimSpace(firstLine(string(out)))), "install a Firecracker release binary matching the node architecture")
	if f.jailer != "" {
		_, err := os.Stat(f.jailer)
		check(err == nil, "jailer binary "+f.jailer+" exists (not used yet)", "install the jailer from the same Firecracker release")
	}
	_, err = os.Stat(f.kernel)
	check(err == nil, "guest kernel "+f.kernel+" exists", "build or download a kernel with deploy/images/build-kernel.sh")
	_, err = os.Stat(f.imagesDir + "/default.ext4")
	check(err == nil, "default rootfs "+f.imagesDir+"/default.ext4 exists", "build one with deploy/images/build-rootfs.sh")
	for _, bin := range []string{"ip", "iptables"} {
		_, err := exec.LookPath(bin)
		check(err == nil, bin+" is installed", "install iproute2 and iptables in the agent image")
	}
	if failed > 0 {
		return fmt.Errorf("%d check(s) failed", failed)
	}
	fmt.Println("node is ready for Differently microVMs")
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
