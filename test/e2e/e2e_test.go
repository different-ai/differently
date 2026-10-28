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
	if sb.Adapter != "opensandbox" || sb.Trust != api.TrustPrivate || sb.Pool != "opensandbox" {
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
	if res, err := c.Exec(ctx, sb.ID, api.ExecRequest{Args: []string{"sh", "-c", "pwd"}, Cwd: "/tmp"}); err != nil || strings.TrimSpace(res.Stdout) != "/tmp" {
		t.Fatalf("argv + cwd %+v %v", res, err)
	}
	start := time.Now()
	res, err = c.Exec(ctx, sb.ID, api.ExecRequest{Command: "sleep 30", TimeoutSeconds: 1})
	if err == nil && res.ExitCode == 0 || time.Since(start) > 15*time.Second {
		t.Fatalf("timeout not enforced: %+v %v after %s", res, err, time.Since(start))
	}
	if _, err := c.Exec(ctx, sb.ID, api.ExecRequest{Command: "cat", Stdin: "x"}); !api.IsCode(err, api.CodeUnsupportedOperation) {
		t.Fatalf("stdin should be reported unsupported, got %v", err)
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
	// OpenSandbox has no stop; Differently says so precisely.
	if _, err := c.Action(ctx(t), sb.ID, "stop"); !api.IsCode(err, api.CodeUnsupportedOperation) {
		t.Fatalf("stop should be unsupported_operation, got %v", err)
	}
}

func TestSnapshotAndFork(t *testing.T) {
	c := tenant(t, "fork", api.Quota{})
	parent := create(t, c, api.CreateSandboxRequest{})
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
func TestCapabilityMismatchIsPrecise(t *testing.T) {
	c := tenant(t, "caps", api.Quota{})
	_, err := c.CreateSandbox(ctx(t), api.CreateSandboxRequest{Capabilities: []string{"lifecycle.stop-start"}}, false)
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
	code := osb(t, "POST", "/sandboxes", map[string]any{
		"image": map[string]string{"uri": "debian:bookworm-slim"}, "entrypoint": []string{"tail", "-f", "/dev/null"},
		"timeout": 600, "resourceLimits": map[string]string{"cpu": "1", "memory": "256Mi"},
		"metadata": map[string]string{"differently-sandbox-id": "sbx_orphan_e2e", "differently-installation": v.Installation},
	}, &orphan)
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
	if !strings.Contains(string(body), `differently_sandbox_transitions_total{pool="opensandbox",to="running"}`) {
		t.Fatal("metrics listener has no sandbox transitions for the OpenSandbox pool")
	}
}
