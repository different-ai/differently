// Package firecracker is a minimal client for the Firecracker VMM API, which
// Firecracker serves over a unix socket (one socket per microVM).
//
// Only the calls Differently needs are implemented. See
// https://github.com/firecracker-microvm/firecracker/blob/main/src/firecracker/swagger/firecracker.yaml
package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Client talks to one Firecracker process.
type Client struct {
	Socket string
	http   *http.Client
}

// New returns a client for the API socket at path.
func New(path string) *Client {
	return &Client{Socket: path, http: &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}},
	}}
}

// MachineConfig is PUT /machine-config.
type MachineConfig struct {
	VCPUCount  int  `json:"vcpu_count"`
	MemSizeMiB int  `json:"mem_size_mib"`
	SMT        bool `json:"smt"`
}

// BootSource is PUT /boot-source.
type BootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args,omitempty"`
}

// Drive is PUT /drives/{id}.
type Drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

// NetworkInterface is PUT /network-interfaces/{id}.
type NetworkInterface struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
	GuestMAC    string `json:"guest_mac,omitempty"`
}

// Vsock is PUT /vsock.
type Vsock struct {
	GuestCID int    `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

// SnapshotCreate is PUT /snapshot/create.
type SnapshotCreate struct {
	SnapshotType string `json:"snapshot_type"`
	SnapshotPath string `json:"snapshot_path"`
	MemFilePath  string `json:"mem_file_path"`
}

func (c *Client) do(ctx context.Context, method, path string, body any) error {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://firecracker"+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var fault struct {
			FaultMessage string `json:"fault_message"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&fault)
		return fmt.Errorf("firecracker %s %s: HTTP %d: %s", method, path, resp.StatusCode, fault.FaultMessage)
	}
	return nil
}

// Configure applies the boot-time configuration of a fresh microVM.
func (c *Client) Configure(ctx context.Context, m MachineConfig, b BootSource, drives []Drive, nics []NetworkInterface, v *Vsock) error {
	if err := c.do(ctx, http.MethodPut, "/machine-config", m); err != nil {
		return err
	}
	if err := c.do(ctx, http.MethodPut, "/boot-source", b); err != nil {
		return err
	}
	for _, d := range drives {
		if err := c.do(ctx, http.MethodPut, "/drives/"+d.DriveID, d); err != nil {
			return err
		}
	}
	for _, n := range nics {
		if err := c.do(ctx, http.MethodPut, "/network-interfaces/"+n.IfaceID, n); err != nil {
			return err
		}
	}
	if v != nil {
		return c.do(ctx, http.MethodPut, "/vsock", v)
	}
	return nil
}

// Start boots the configured microVM.
func (c *Client) Start(ctx context.Context) error {
	return c.do(ctx, http.MethodPut, "/actions", map[string]string{"action_type": "InstanceStart"})
}

// CtrlAltDel asks the guest to shut down (x86 only).
func (c *Client) CtrlAltDel(ctx context.Context) error {
	return c.do(ctx, http.MethodPut, "/actions", map[string]string{"action_type": "SendCtrlAltDel"})
}

// Pause freezes vCPUs; memory stays resident.
func (c *Client) Pause(ctx context.Context) error {
	return c.do(ctx, http.MethodPatch, "/vm", map[string]string{"state": "Paused"})
}

// Resume unfreezes vCPUs.
func (c *Client) Resume(ctx context.Context) error {
	return c.do(ctx, http.MethodPatch, "/vm", map[string]string{"state": "Resumed"})
}

// CreateSnapshot writes a full snapshot of a paused microVM.
func (c *Client) CreateSnapshot(ctx context.Context, s SnapshotCreate) error {
	if s.SnapshotType == "" {
		s.SnapshotType = "Full"
	}
	return c.do(ctx, http.MethodPut, "/snapshot/create", s)
}
