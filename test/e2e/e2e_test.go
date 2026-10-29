//go:build e2e

// Package e2e drives the real stack: Postgres, the Differently control plane
// and an OpenSandbox server as its private pool (test/e2e/compose.yaml).
//
//	make e2e-prepare e2e-up e2e
//
// Settings (defaults match compose.yaml):
//
//	DIFFERENTLY_E2E_URL          API URL                (http://localhost:18090)
//	DIFFERENTLY_E2E_TOKEN        operator token         (the compose test token)
//	DIFFERENTLY_E2E_METRICS_URL  metrics listener       (http://localhost:19091)
//	OPENSANDBOX_E2E_URL          OpenSandbox lifecycle  (http://localhost:18091/v1)
//	OPENSANDBOX_E2E_KEY          OpenSandbox API key    (test/opensandbox/config.toml)
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/different-ai/differently/pkg/api"
	"github.com/different-ai/differently/pkg/client"
)

var (
	baseURL    = env("DIFFERENTLY_E2E_URL", "http://localhost:18090")
	operator   = client.New(baseURL, env("DIFFERENTLY_E2E_TOKEN", "dly_e2eoperator_e2etoken0123456789abcdefghjkmnpqrstvwxyz"))
	metricsURL = env("DIFFERENTLY_E2E_METRICS_URL", "http://localhost:19091")
	osbURL     = env("OPENSANDBOX_E2E_URL", "http://localhost:18091/v1")
	osbKey     = env("OPENSANDBOX_E2E_KEY", "osb-live-test-key-0123456789")
	// osbInternalURL is OpenSandbox as the Differently server reaches it, for
	// pools the tests register.
	osbInternalURL = env("OPENSANDBOX_E2E_INTERNAL_URL", "http://opensandbox-server:8090/v1")
	// mainPool is the private pool the server's config defines.
	mainPool = env("DIFFERENTLY_E2E_POOL", "opensandbox")
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	t.Cleanup(cancel)
	return c
}

// tenant creates an organization and returns a client bound to a project key.
func tenant(t *testing.T, name string, quota api.Quota) *client.Client {
	t.Helper()
	var created struct {
		Organization api.Organization `json:"organization"`
		Project      api.Project      `json:"project"`
	}
	body := map[string]any{"name": name, "quota": quota, "defaultProject": "e2e"}
	if err := operator.Do(ctx(t), "POST", "/v1/admin/organizations", body, &created); err != nil {
		t.Fatal(err)
	}
	key, err := operator.CreateAPIKey(ctx(t), api.CreateAPIKeyRequest{
		Name: "e2e", OrgID: created.Organization.ID, ProjectID: created.Project.ID,
		Scopes: []string{"sandboxes:read", "sandboxes:write", "sandboxes:exec", "usage:read"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return client.New(baseURL, key.Token)
}

func create(t *testing.T, c *client.Client, req api.CreateSandboxRequest) api.Sandbox {
	t.Helper()
	start := time.Now()
	sb, err := c.CreateSandbox(ctx(t), req, true)
	if err != nil {
		t.Fatalf("create: %v (%+v)", err, sb)
	}
	t.Logf("sandbox %s running on %s after %s", sb.ID, sb.Pool, time.Since(start).Round(time.Millisecond))
	t.Cleanup(func() { _ = c.DeleteSandbox(context.Background(), sb.ID) })
	return sb
}

func sh(t *testing.T, c *client.Client, id, command string) api.ExecResult {
	t.Helper()
	res, err := c.Exec(ctx(t), id, api.ExecRequest{Command: command, TimeoutSeconds: 60})
	if err != nil {
		t.Fatalf("exec %q: %v", command, err)
	}
	return res
}

func eventually(t *testing.T, timeout time.Duration, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// advertises reports whether the pool advertises a capability. The suite
// runs against any OpenSandbox runtime: where an operation is not offered
// (pause on the Kubernetes runtime without a snapshot registry, say), it
// checks that Differently refuses it precisely instead.
func advertises(t *testing.T, feature string) bool {
	t.Helper()
	var caps api.List[struct {
		Pool         string   `json:"pool"`
		Capabilities []string `json:"capabilities"`
	}]
	if err := operator.Do(ctx(t), "GET", "/v1/capabilities", nil, &caps); err != nil {
		t.Fatalf("capabilities: %v", err)
	}
	for _, p := range caps.Items {
		if p.Pool == mainPool {
			return slices.Contains(p.Capabilities, feature)
		}
	}
	t.Fatalf("pool %s not in %+v", mainPool, caps.Items)
	return false
}

// poolHealthy reports the private pool's health as placement sees it.
func poolHealthy(t *testing.T) (bool, string) {
	t.Helper()
	var p api.Pool
	if err := operator.Do(ctx(t), "GET", "/v1/admin/pools/"+mainPool, nil, &p); err != nil || p.Health == nil {
		t.Fatalf("pool %s: %+v %v", mainPool, p, err)
	}
	return p.Health.Status == "healthy", p.Health.Message
}

// osb calls OpenSandbox directly, as an operator would behind Differently's back.
func osb(t *testing.T, method, path string, body, out any) int {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequestWithContext(ctx(t), method, osbURL+path, r)
	req.Header.Set("OPEN-SANDBOX-API-KEY", osbKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func osbExists(t *testing.T, id string) bool {
	var sb struct {
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	}
	code := osb(t, "GET", "/sandboxes/"+id, nil, &sb)
	return code == 200 && sb.Status.State != "Terminated"
}

func backendRef(t *testing.T, id string) string {
	t.Helper()
	var explain struct {
		Decision *api.PlacementDecision `json:"decision"`
	}
	if err := operator.Do(ctx(t), "GET", "/v1/sandboxes/"+id+"/placement", nil, &explain); err != nil {
		t.Fatal(err)
	}
	// The backend ID is OpenSandbox's; find it through the ownership metadata.
	var list struct {
		Items []struct {
			ID       string            `json:"id"`
			Metadata map[string]string `json:"metadata"`
		} `json:"items"`
	}
	osb(t, "GET", "/sandboxes?page=1&pageSize=200", nil, &list)
	for _, it := range list.Items {
		if it.Metadata["differently-sandbox-id"] == id {
			return it.ID
		}
	}
	t.Fatalf("no OpenSandbox sandbox carries Differently ID %s", id)
	return ""
}

func TestMain(m *testing.M) {
	deadline := time.Now().Add(90 * time.Second)
	for {
		var pools api.List[api.Pool]
		err := operator.Do(context.Background(), "GET", "/v1/admin/pools", nil, &pools)
		if err == nil && len(pools.Items) > 0 && pools.Items[0].Health != nil && pools.Items[0].Health.Status == "healthy" {
			break
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "the OpenSandbox pool at %s is not healthy: %v (is the e2e stack up?)\n", baseURL, err)
			os.Exit(1)
		}
		time.Sleep(time.Second)
	}
	os.Exit(m.Run())
}

func TestSandboxRunsOnOpenSandboxWithLimits(t *testing.T) {
	c := tenant(t, "boot", api.Quota{})
	sb := create(t, c, api.CreateSandboxRequest{Profile: "medium"})
	if sb.Adapter != "opensandbox" || sb.Trust != api.TrustPrivate || sb.Pool != mainPool {
		t.Fatalf("placed on %s/%s/%s", sb.Pool, sb.Adapter, sb.Trust)
	}
	if !osbExists(t, backendRef(t, sb.ID)) {
		t.Fatal("OpenSandbox does not know the sandbox")
	}
	// Differently's resource envelope reaches the runtime as cgroup limits.
	mem := strings.TrimSpace(sh(t, c, sb.ID, "cat /sys/fs/cgroup/memory.max").Stdout)
	if mem != strconv.Itoa(1024<<20) {
		t.Fatalf("memory.max = %q, want 1 GiB", mem)
	}
	cpu := strings.Fields(sh(t, c, sb.ID, "cat /sys/fs/cgroup/cpu.max").Stdout)
	if len(cpu) != 2 || cpu[0] == "max" {
		t.Fatalf("cpu.max = %v, want a 2-CPU quota", cpu)
	}
	quota, _ := strconv.Atoi(cpu[0])
	period, _ := strconv.Atoi(cpu[1])
	if quota/period != 2 {
		t.Fatalf("cpu.max = %v, want 2 CPUs", cpu)
	}
}

func TestExecSemantics(t *testing.T) {
	c := tenant(t, "exec", api.Quota{})
	sb := create(t, c, api.CreateSandboxRequest{Env: map[string]string{"GREETING": "hello"}})
	ctx := ctx(t)
	res, err := c.Exec(ctx, sb.ID, api.ExecRequest{Command: `echo "$GREETING $WHO"; echo oops >&2; exit 7`, Env: map[string]string{"WHO": "world"}})
	if err != nil || res.ExitCode != 7 || strings.TrimSpace(res.Stdout) != "hello world" || strings.TrimSpace(res.Stderr) != "oops" {
		t.Fatalf("exec %+v %v", res, err)
	}
	if res := sh(t, c, sb.ID, `printf 'a\nb\n'`); res.Stdout != "a\nb\n" {
		t.Fatalf("multi-line output %q", res.Stdout)
	}
	// lineEndings: kept with execd release-1.2.0: output is byte-exact.
	if res := sh(t, c, sb.ID, `printf 'a\n\nno newline'; printf 'err' >&2`); res.Stdout != "a\n\nno newline" || res.Stderr != "err" {
		t.Fatalf("output is not exact: %q %q", res.Stdout, res.Stderr)
	}
	if res, err := c.Exec(ctx, sb.ID, api.ExecRequest{Args: []string{"sh", "-c", "pwd"}, Cwd: "/tmp"}); err != nil || strings.TrimSpace(res.Stdout) != "/tmp" {
		t.Fatalf("argv + cwd %+v %v", res, err)
	}
	start := time.Now()
	res, err = c.Exec(ctx, sb.ID, api.ExecRequest{Command: "sleep 30", TimeoutSeconds: 1})
	if err == nil && res.ExitCode == 0 || time.Since(start) > 15*time.Second {
		t.Fatalf("timeout not enforced: %+v %v after %s", res, err, time.Since(start))
	}
	if res, err := c.Exec(ctx, sb.ID, api.ExecRequest{Command: "wc -l", Stdin: "a\nb\nc\n"}); err != nil || strings.TrimSpace(res.Stdout) != "3" {
		t.Fatalf("stdin: %+v %v", res, err)
	}
	var chunks []string
	code, err := c.ExecStream(ctx, sb.ID, api.ExecRequest{Command: "for i in 1 2 3; do echo line$i; sleep 0.2; done; echo err >&2; exit 3"}, func(ev api.ExecEvent) {
		chunks = append(chunks, ev.Stream+":"+strings.TrimSpace(ev.Data))
	})
	joined := strings.Join(chunks, ",")
	if err != nil || code != 3 || !strings.Contains(joined, "stdout:line1") || !strings.Contains(joined, "stderr:err") {
		t.Fatalf("stream code=%d chunks=%v err=%v", code, chunks, err)
	}
}

func TestFiles(t *testing.T) {
	c := tenant(t, "files", api.Quota{})
	sb := create(t, c, api.CreateSandboxRequest{})
	data := bytes.Repeat([]byte("differently\x00\xff"), 80_000) // ~1 MiB, binary
	if err := c.WriteFile(ctx(t), sb.ID, "/tmp/work/blob.bin", data); err != nil {
		t.Fatal(err)
	}
	got, err := c.ReadFile(ctx(t), sb.ID, "/tmp/work/blob.bin")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read back %d bytes (want %d): %v", len(got), len(data), err)
	}
	if n := strings.TrimSpace(sh(t, c, sb.ID, "wc -c < /tmp/work/blob.bin").Stdout); n != strconv.Itoa(len(data)) {
		t.Fatalf("sandbox sees %s bytes", n)
	}
	items, err := c.ListFiles(ctx(t), sb.ID, "/tmp/work")
	if err != nil || len(items) != 1 || items[0].Name != "blob.bin" {
		t.Fatalf("list %+v %v", items, err)
	}
}

func TestPauseResumeAndUnsupportedStop(t *testing.T) {
	c := tenant(t, "pause", api.Quota{})
	sb := create(t, c, api.CreateSandboxRequest{})
	if advertises(t, "lifecycle.stop-start") {
		// OpenSandbox's Kubernetes runtime: stop commits the disk and frees
		// the pod, start brings it back from that image.
		sh(t, c, sb.ID, "echo kept > /root/disk-state && sync")
		start := time.Now()
		if _, err := c.Action(ctx(t), sb.ID, "stop"); err != nil {
			t.Fatal(err)
		}
		stopped := time.Since(start)
		start = time.Now()
		if _, err := c.Action(ctx(t), sb.ID, "start"); err != nil {
			t.Fatal(err)
		}
		if _, err := c.WaitSandbox(ctx(t), sb.ID, api.StatusRunning, 3*time.Minute); err != nil {
			t.Fatal(err)
		}
		t.Logf("stop took %s, start %s", stopped.Round(100*time.Millisecond), time.Since(start).Round(100*time.Millisecond))
		if got := strings.TrimSpace(sh(t, c, sb.ID, "cat /root/disk-state").Stdout); got != "kept" {
			t.Fatalf("disk not kept across stop/start: %q", got)
		}
	} else if _, err := c.Action(ctx(t), sb.ID, "stop"); !api.IsCode(err, api.CodeUnsupportedOperation) {
		// OpenSandbox's Docker runtime has no stop; Differently says so precisely.
		t.Fatalf("stop should be unsupported_operation, got %v", err)
	}
	if !advertises(t, "lifecycle.pause-resume") {
		if _, err := c.Action(ctx(t), sb.ID, "pause"); !api.IsCode(err, api.CodeUnsupportedOperation) {
			t.Fatalf("pause on a pool without pause-resume: want unsupported_operation, got %v", err)
		}
		if got, _ := c.GetSandbox(ctx(t), sb.ID); got.Status != api.StatusRunning {
			t.Fatalf("a refused pause changed the sandbox: %+v", got)
		}
		t.Log("pool does not advertise pause-resume: checked the refusal")
		return
	}
	sh(t, c, sb.ID, "echo 42 > /dev/shm/in-memory")
	if _, err := c.Action(ctx(t), sb.ID, "pause"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx(t), sb.ID, api.ExecRequest{Command: "true"}); !api.IsCode(err, api.CodeInvalidState) {
		t.Fatalf("exec on a paused sandbox: %v", err)
	}
	if _, err := c.Action(ctx(t), sb.ID, "resume"); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(sh(t, c, sb.ID, "cat /dev/shm/in-memory").Stdout); got != "42" {
		t.Fatalf("memory not kept across pause/resume: %q", got)
	}
}

func TestSnapshotAndFork(t *testing.T) {
	c := tenant(t, "fork", api.Quota{})
	parent := create(t, c, api.CreateSandboxRequest{})
	if !advertises(t, "snapshots.disk") {
		if _, err := c.Fork(ctx(t), parent.ID); !api.IsCode(err, api.CodeUnsupportedOperation) && !api.IsCode(err, api.CodeUnsupportedCapability) {
			t.Fatalf("fork on a pool without snapshots: want a precise refusal, got %v", err)
		}
		t.Log("pool does not advertise snapshots.disk: checked the refusal")
		return
	}
	sh(t, c, parent.ID, "echo v1 > /root/state && sync")
	child, err := c.Fork(ctx(t), parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.DeleteSandbox(context.Background(), child.ID) })
	if child, err = c.WaitSandbox(ctx(t), child.ID, api.StatusRunning, 3*time.Minute); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(sh(t, c, child.ID, "cat /root/state").Stdout); got != "v1" {
		t.Fatalf("fork lost the parent's filesystem: %q", got)
	}
	sh(t, c, child.ID, "echo child > /root/state")
	if got := strings.TrimSpace(sh(t, c, parent.ID, "cat /root/state").Stdout); got != "v1" {
		t.Fatalf("fork is not independent: parent sees %q", got)
	}
}

// Scenario I against a real backend: the only pool cannot stop and start.
// A sandbox whose image cannot be pulled fails or is retried with a reason
// that names the image, and the pool stays healthy for everyone else
// throughout: the backend itself is fine.
func TestUnpullableImageKeepsPoolHealthy(t *testing.T) {
	c := tenant(t, "badimage", api.Quota{})
	sb, err := c.CreateSandbox(ctx(t), api.CreateSandboxRequest{Template: "docker.io/library/debian:no-such-tag-differently"}, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.DeleteSandbox(context.Background(), sb.ID) })
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if ok, msg := poolHealthy(t); !ok {
			t.Fatalf("one sandbox's unpullable image marked the pool unhealthy: %s", msg)
		}
		got, err := c.GetSandbox(ctx(t), sb.ID)
		if err != nil {
			t.Fatal(err)
		}
		failedOnce := got.Status == api.StatusFailed || got.Status == api.StatusQueued && got.Attempts > 0
		if failedOnce {
			if !strings.Contains(got.StatusReason, "image") {
				t.Fatalf("the reason should name the image: %q", got.StatusReason)
			}
			if strings.Contains(got.StatusReason, "provider_unavailable") {
				t.Fatalf("an image problem is reported as a provider outage: %q", got.StatusReason)
			}
			t.Logf("%s after %d attempt(s): %s", got.Status, got.Attempts, got.StatusReason)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the sandbox never failed: %+v", got)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// Where the pool offers port URLs (OpenSandbox's ingress gateway with signed
// URLs), a tenant opens a service in its sandbox with the returned URL alone:
// no header, no key. Otherwise the request is refused precisely.
func TestPortURL(t *testing.T) {
	c := tenant(t, "ports", api.Quota{})
	if !advertises(t, "ports.expose") {
		sb := create(t, c, api.CreateSandboxRequest{})
		if _, err := c.ExposePort(ctx(t), sb.ID, 8080); !api.IsCode(err, api.CodeUnsupportedOperation) && !api.IsCode(err, api.CodeUnsupportedCapability) {
			t.Fatalf("ports on a pool without ports.expose: want a precise refusal, got %v", err)
		}
		t.Log("pool does not advertise ports.expose: checked the refusal")
		return
	}
	sb := create(t, c, api.CreateSandboxRequest{Template: "python:3.12-slim"})
	sh(t, c, sb.ID, "mkdir -p /srv && echo served-by-sandbox > /srv/index.html && cd /srv && (nohup python3 -m http.server 8080 >/dev/null 2>&1 &) && sleep 1")
	port, err := c.ExposePort(ctx(t), sb.ID, 8080)
	if err != nil {
		t.Fatal(err)
	}
	var body string
	eventually(t, 30*time.Second, "the port URL to serve the sandbox's page", func() bool {
		resp, err := http.Get(port.URL + "/")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		body = string(b)
		return resp.StatusCode == http.StatusOK
	})
	if !strings.Contains(body, "served-by-sandbox") {
		t.Fatalf("port URL served %q", body)
	}
	if strings.Contains(port.URL, osbKey) {
		t.Fatal("the port URL carries the OpenSandbox key")
	}
}

// Idle auto-stop: after its idle timeout with no calls, the sandbox is
// stopped (Kubernetes runtime, disk kept) or paused (Docker runtime, memory
// kept), and the next exec wakes it with its files intact.
func TestIdleAutoStopAndWake(t *testing.T) {
	if !advertises(t, "lifecycle.stop-start") && !advertises(t, "lifecycle.pause-resume") {
		t.Skip("the pool can neither stop nor pause, so idle sandboxes stay running")
	}
	c := tenant(t, "idle", api.Quota{})
	sb := create(t, c, api.CreateSandboxRequest{IdleTimeoutSeconds: 60})
	sh(t, c, sb.ID, "echo before-idle > /root/idle-state && sync")
	var idled api.Sandbox
	eventually(t, 3*time.Minute, "the idle sandbox to be stopped or paused", func() bool {
		got, err := c.GetSandbox(context.Background(), sb.ID)
		idled = got
		return err == nil && got.IdledAt != nil && (got.Status == api.StatusStopped || got.Status == api.StatusPaused)
	})
	t.Logf("%s after idling: %s", idled.Status, idled.StatusReason)
	start := time.Now()
	res := sh(t, c, sb.ID, "cat /root/idle-state")
	t.Logf("exec woke it in %s", time.Since(start).Round(100*time.Millisecond))
	if strings.TrimSpace(res.Stdout) != "before-idle" {
		t.Fatalf("files lost across idle and wake: %q", res.Stdout)
	}
	if got, _ := c.GetSandbox(ctx(t), sb.ID); got.Status != api.StatusRunning || got.IdledAt != nil {
		t.Fatalf("after waking: %+v", got)
	}
}

func TestCapabilityMismatchIsPrecise(t *testing.T) {
	c := tenant(t, "caps", api.Quota{})
	_, err := c.CreateSandbox(ctx(t), api.CreateSandboxRequest{Capabilities: []string{"gpu"}}, false)
	if !api.IsCode(err, api.CodeUnsupportedCapability) {
		t.Fatalf("want unsupported_capability, got %v", err)
	}
}

func TestQuotaQueuesAndReleases(t *testing.T) {
	c := tenant(t, "quota", api.Quota{MaxActiveSandboxes: 1})
	first := create(t, c, api.CreateSandboxRequest{})
	second, err := c.CreateSandbox(ctx(t), api.CreateSandboxRequest{}, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.DeleteSandbox(context.Background(), second.ID) })
	time.Sleep(2 * time.Second)
	if got, _ := c.GetSandbox(ctx(t), second.ID); got.Status != api.StatusQueued || !strings.Contains(got.StatusReason, "quota") {
		t.Fatalf("second sandbox should wait for quota: %s %q", got.Status, got.StatusReason)
	}
	if err := c.DeleteSandbox(ctx(t), first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.WaitSandbox(ctx(t), second.ID, api.StatusRunning, 2*time.Minute); err != nil {
		t.Fatalf("queued sandbox did not start after release: %v", err)
	}
}

func TestDeleteCleansUpBackend(t *testing.T) {
	c := tenant(t, "cleanup", api.Quota{})
	sb := create(t, c, api.CreateSandboxRequest{})
	ref := backendRef(t, sb.ID)
	if err := c.DeleteSandbox(ctx(t), sb.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.WaitSandbox(ctx(t), sb.ID, api.StatusTerminated, time.Minute); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "OpenSandbox to drop the sandbox", func() bool { return !osbExists(t, ref) })
}

// Scenario J against a real backend: drift made directly in OpenSandbox is
// reconciled. An instance carrying this installation's ownership metadata
// but no Differently sandbox is deleted; a sandbox whose instance was
// deleted behind Differently's back is failed, and its quota released.
func TestReconcilesDriftInTheBackend(t *testing.T) {
	var v api.VersionInfo
	if err := operator.Do(ctx(t), "GET", "/version", nil, &v); err != nil {
		t.Fatal(err)
	}
	var orphan struct {
		ID string `json:"id"`
	}
	// OpenSandbox's Kubernetes runtime can still be busy with an earlier
	// test's slow create (504 POD_READY_TIMEOUT); retry the plant itself.
	var code int
	for try := 0; try < 3; try++ {
		code = osb(t, "POST", "/sandboxes", map[string]any{
			"image": map[string]string{"uri": "debian:bookworm-slim"}, "entrypoint": []string{"tail", "-f", "/dev/null"},
			"timeout": 600, "resourceLimits": map[string]string{"cpu": "1", "memory": "256Mi"},
			"metadata": map[string]string{"differently-sandbox-id": "sbx_orphan_e2e", "differently-installation": v.Installation},
		}, &orphan)
		if code != http.StatusGatewayTimeout {
			break
		}
	}
	if code != 202 || orphan.ID == "" {
		t.Fatalf("could not plant an orphan: %d", code)
	}
	eventually(t, 60*time.Second, "the reconciler to delete the orphan", func() bool { return !osbExists(t, orphan.ID) })

	c := tenant(t, "drift", api.Quota{MaxActiveSandboxes: 1})
	sb := create(t, c, api.CreateSandboxRequest{})
	osb(t, "DELETE", "/sandboxes/"+backendRef(t, sb.ID), nil, nil)
	eventually(t, 90*time.Second, "the lost sandbox to be failed", func() bool {
		got, _ := c.GetSandbox(context.Background(), sb.ID)
		return got.Status == api.StatusFailed
	})
	create(t, c, api.CreateSandboxRequest{}) // quota was released
}

func TestAuditTrustAndMetrics(t *testing.T) {
	var verify struct {
		OK     bool `json:"ok"`
		Events int  `json:"events"`
	}
	if err := operator.Do(ctx(t), "GET", "/v1/admin/audit/verify", nil, &verify); err != nil || !verify.OK || verify.Events == 0 {
		t.Fatalf("audit chain %+v %v", verify, err)
	}
	var requests api.List[api.AuditEvent]
	if err := operator.Do(ctx(t), "GET", "/v1/audit-events?action=api.request&limit=5", nil, &requests); err != nil || len(requests.Items) == 0 {
		t.Fatalf("audit.failClosed should record requests before acting: %v %v", requests.Items, err)
	}
	var report struct {
		Pools []struct {
			Name, Adapter, Trust, Isolation, Endpoint, CredentialRef string
		} `json:"pools"`
		Limitations []string `json:"limitations"`
	}
	if err := operator.Do(ctx(t), "GET", "/v1/admin/trust-report", nil, &report); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(report)
	if strings.Contains(string(raw), osbKey) {
		t.Fatal("trust report leaks the OpenSandbox API key")
	}
	if len(report.Pools) != 1 || report.Pools[0].Adapter != "opensandbox" || !strings.Contains(report.Pools[0].Isolation, "container") || report.Pools[0].CredentialRef != "env:DIFFERENTLY_OPENSANDBOX_API_KEY" {
		t.Fatalf("trust report pools %+v", report.Pools)
	}
	if !strings.Contains(strings.Join(report.Limitations, " "), "Differently does not isolate sandboxes itself") {
		t.Fatal("trust report does not state that isolation is the runtime's")
	}
	resp, err := http.Get(baseURL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("/metrics still served on the API listener: %d", resp.StatusCode)
	}
	resp, err = http.Get(metricsURL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `differently_sandbox_transitions_total{pool="`+mainPool+`",to="running"}`) {
		t.Fatal("metrics listener has no sandbox transitions for the OpenSandbox pool")
	}
}

// A provider key stored in Differently drives a real backend; switching sends
// new sandboxes there; rotating the stored key takes effect without a restart.
// The second pool fronts the same OpenSandbox server, as an operator adding
// a provider from the dashboard would.
func TestStoredProviderKeyAndSwitching(t *testing.T) {
	c := tenant(t, "providers", api.Quota{})
	pool := func() api.Pool {
		var p api.Pool
		if err := operator.Do(ctx(t), "GET", "/v1/admin/pools/osb-stored", nil, &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if _, err := operator.PutSecret(ctx(t), "osb-stored-key", osbKey); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"name": "osb-stored", "adapter": "opensandbox", "trust": "private", "credentials": map[string]string{"secret": "osb-stored-key"},
		"config": map[string]string{"apiURL": osbInternalURL, "image": "debian:bookworm-slim", "isolation": "container", "features": "exec,exec.stream,files"}}
	if err := operator.Do(ctx(t), "POST", "/v1/admin/pools", body, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = operator.UsePool(context.Background(), mainPool, false)
		_ = operator.Do(context.Background(), "DELETE", "/v1/admin/pools/osb-stored", nil, nil)
		_ = operator.Do(context.Background(), "DELETE", "/v1/admin/secrets/osb-stored-key", nil, nil)
	})
	eventually(t, 30*time.Second, "the new pool is healthy with its stored key", func() bool { return pool().Health.Status == "healthy" })

	switched, err := operator.UsePool(ctx(t), "osb-stored", false)
	if err != nil || len(switched.Drained) != 1 || switched.Drained[0] != mainPool {
		t.Fatalf("switch: %+v %v", switched, err)
	}
	sb := create(t, c, api.CreateSandboxRequest{})
	if sb.Pool != "osb-stored" {
		t.Fatalf("new sandbox placed on %q, want osb-stored", sb.Pool)
	}
	if res := sh(t, c, sb.ID, "echo via stored key"); strings.TrimSpace(res.Stdout) != "via stored key" {
		t.Fatalf("exec %+v", res)
	}

	// Rotate the stored key: a wrong value takes the pool down, the right one brings it back.
	if _, err := operator.PutSecret(ctx(t), "osb-stored-key", "osb-wrong-key"); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "the pool notices the rotated (wrong) key", func() bool { return pool().Health.Status != "healthy" })
	if _, err := operator.PutSecret(ctx(t), "osb-stored-key", osbKey); err != nil {
		t.Fatal(err)
	}
	eventually(t, 30*time.Second, "the pool recovers with the right key", func() bool { return pool().Health.Status == "healthy" })
	if res := sh(t, c, sb.ID, "echo still here"); strings.TrimSpace(res.Stdout) != "still here" {
		t.Fatalf("exec after rotation %+v", res)
	}

	// Switch back: new sandboxes go to the original pool, the running one stays.
	if _, err := operator.UsePool(ctx(t), mainPool, false); err != nil {
		t.Fatal(err)
	}
	back := create(t, c, api.CreateSandboxRequest{})
	if back.Pool != mainPool {
		t.Fatalf("after switching back, placed on %q", back.Pool)
	}
	for _, id := range []string{sb.ID, back.ID} {
		if err := c.DeleteSandbox(ctx(t), id); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, 60*time.Second, "the sandboxes are gone", func() bool {
		s, err := c.GetSandbox(ctx(t), sb.ID)
		return err == nil && s.Status == api.StatusTerminated
	})
}

// On a pool that pauses with memory kept (Fast Sandbox microVMs, or the
// Docker runtime), a paused sandbox resumes with its processes intact: a
// background process keeps its PID and /dev/shm keeps its content.
func TestPauseKeepsMemory(t *testing.T) {
	if !advertises(t, "lifecycle.pause-resume") {
		t.Skip("the pool does not pause; TestPauseResumeAndUnsupportedStop checks the refusal")
	}
	c := tenant(t, "memory", api.Quota{})
	sb := create(t, c, api.CreateSandboxRequest{})
	sh(t, c, sb.ID, "head -c 1048576 /dev/urandom > /dev/shm/blob && md5sum /dev/shm/blob | cut -d' ' -f1 > /root/blob.md5")
	pid := strings.TrimSpace(sh(t, c, sb.ID, "nohup sleep 100000 >/dev/null 2>&1 & echo $!").Stdout)
	if pid == "" {
		t.Fatal("background process did not start")
	}
	start := time.Now()
	if _, err := c.Action(ctx(t), sb.ID, "pause"); err != nil {
		t.Fatal(err)
	}
	paused := time.Since(start)
	start = time.Now()
	if _, err := c.Action(ctx(t), sb.ID, "resume"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.WaitSandbox(ctx(t), sb.ID, api.StatusRunning, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	t.Logf("pause took %s, resume %s", paused.Round(10*time.Millisecond), time.Since(start).Round(10*time.Millisecond))
	if got := sh(t, c, sb.ID, "tr '\\0' ' ' < /proc/"+pid+"/cmdline"); !strings.Contains(got.Stdout, "sleep 100000") {
		t.Fatalf("background process %s lost across pause/resume: %+v", pid, got)
	}
	if got := sh(t, c, sb.ID, "md5sum /dev/shm/blob | cut -d' ' -f1 | cmp - /root/blob.md5 && echo same"); strings.TrimSpace(got.Stdout) != "same" {
		t.Fatalf("/dev/shm content changed across pause/resume: %+v", got)
	}
}

// On a microVM pool that pauses, an idle sandbox is paused (not stopped) and
// asking for a port URL wakes it: the server it was running answers again
// without being restarted, because its memory was kept.
func TestIdlePauseAndWakeThroughPortURL(t *testing.T) {
	if !advertises(t, "isolation.microvm") || !advertises(t, "lifecycle.pause-resume") || !advertises(t, "ports.expose") {
		t.Skip("needs a microVM pool that pauses and exposes ports")
	}
	c := tenant(t, "idlevm", api.Quota{})
	sb := create(t, c, api.CreateSandboxRequest{Template: "python:3.12-slim", IdleTimeoutSeconds: 60})
	sh(t, c, sb.ID, "mkdir -p /srv && cd /srv && (nohup python3 -c 'import http.server,os\nclass H(http.server.BaseHTTPRequestHandler):\n def do_GET(s):\n  s.send_response(200);s.end_headers();s.wfile.write(str(os.getpid()).encode())\nhttp.server.HTTPServer((\"\",8080),H).serve_forever()' >/dev/null 2>&1 &) && sleep 1")
	get := func(u string) (int, string) {
		resp, err := http.Get(u + "/")
		if err != nil {
			return 0, ""
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	port, err := c.ExposePort(ctx(t), sb.ID, 8080)
	if err != nil {
		t.Fatal(err)
	}
	var before string
	eventually(t, 30*time.Second, "the port URL to answer", func() bool {
		code, body := get(port.URL)
		before = body
		return code == http.StatusOK
	})
	eventually(t, 3*time.Minute, "the idle microVM to be paused", func() bool {
		got, err := c.GetSandbox(context.Background(), sb.ID)
		return err == nil && got.IdledAt != nil && got.Status == api.StatusPaused
	})
	start := time.Now()
	if port, err = c.ExposePort(ctx(t), sb.ID, 8080); err != nil {
		t.Fatal(err)
	}
	var after string
	eventually(t, 30*time.Second, "the woken port URL to answer", func() bool {
		code, body := get(port.URL)
		after = body
		return code == http.StatusOK
	})
	t.Logf("port URL woke it and answered in %s", time.Since(start).Round(10*time.Millisecond))
	if after != before {
		t.Fatalf("the server was restarted, not resumed: pid %q before, %q after", before, after)
	}
}

// A memory snapshot, where the pool offers one, is taken and becomes ready.
func TestMemorySnapshot(t *testing.T) {
	c := tenant(t, "memsnap", api.Quota{})
	sb := create(t, c, api.CreateSandboxRequest{})
	if !advertises(t, "snapshots.memory") {
		if _, err := c.Snapshot(ctx(t), sb.ID, "memory"); !api.IsCode(err, api.CodeUnsupportedOperation) && !api.IsCode(err, api.CodeUnsupportedCapability) {
			t.Fatalf("memory snapshot on a pool without snapshots.memory: want a precise refusal, got %v", err)
		}
		t.Log("pool does not advertise snapshots.memory: checked the refusal")
		return
	}
	start := time.Now()
	snap, err := c.Snapshot(ctx(t), sb.ID, "memory")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Kind != "memory" {
		t.Fatalf("asked for a memory snapshot, got %+v", snap)
	}
	t.Logf("memory snapshot %s in %s", snap.ID, time.Since(start).Round(10*time.Millisecond))
}
