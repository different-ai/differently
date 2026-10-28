package firecracker

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestConfigureAndStart(t *testing.T) {
	dir, _ := os.MkdirTemp("/tmp", "fc")
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "api.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var calls []string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/vm" && body["state"] == "Bogus" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"fault_message": "bad state"})
			return
		}
		w.WriteHeader(204)
	})}
	go srv.Serve(ln)
	defer srv.Close()

	c := New(sock)
	ctx := context.Background()
	err = c.Configure(ctx, MachineConfig{VCPUCount: 2, MemSizeMiB: 512},
		BootSource{KernelImagePath: "/k", BootArgs: "console=ttyS0"},
		[]Drive{{DriveID: "rootfs", PathOnHost: "/r.ext4", IsRootDevice: true}},
		[]NetworkInterface{{IfaceID: "eth0", HostDevName: "dly0"}},
		&Vsock{GuestCID: 3, UDSPath: "/v.sock"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{"PUT /machine-config", "PUT /boot-source", "PUT /drives/rootfs", "PUT /network-interfaces/eth0", "PUT /vsock", "PUT /actions"}
	if len(calls) != len(want) {
		t.Fatalf("calls %v", calls)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("call %d = %s, want %s", i, calls[i], want[i])
		}
	}
	if err := c.do(ctx, http.MethodPatch, "/vm", map[string]string{"state": "Bogus"}); err == nil || !strings.Contains(err.Error(), "bad state") {
		t.Fatalf("fault message not surfaced: %v", err)
	}
}
