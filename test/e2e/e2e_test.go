//go:build e2e

// Package e2e drives a real Differently stack: Postgres, the control plane
// and a node agent running Firecracker microVMs (test/e2e/compose.yaml).
//
//	DOCKER_CONTEXT=colima-differently make e2e
//
// Settings (defaults match compose.yaml):
//
//	DIFFERENTLY_E2E_URL          API URL            (http://localhost:18090)
//	DIFFERENTLY_E2E_TOKEN        operator token     (the compose test token)
//	DIFFERENTLY_E2E_METRICS_URL  metrics listener   (http://localhost:19091)
//	DIFFERENTLY_E2E_COMPOSE      compose file; enables tests that restart or
//	                             inspect containers (needs the docker CLI)
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
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
	compose    = os.Getenv("DIFFERENTLY_E2E_COMPOSE")
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
func tenant(t *testing.T, name string, quota api.Quota) (*client.Client, string) {
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
	return client.New(baseURL, key.Token), created.Organization.ID
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

func docker(t *testing.T, args ...string) string {
	t.Helper()
	if compose == "" {
		t.Skip("set DIFFERENTLY_E2E_COMPOSE to run container-level checks")
	}
	out, err := exec.Command("docker", append([]string{"compose", "-f", compose}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker compose %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func agents(t *testing.T) []api.Agent {
	t.Helper()
	var list api.List[api.Agent]
	if err := operator.Do(ctx(t), "GET", "/v1/admin/agents", nil, &list); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

func TestMain(m *testing.M) {
	deadline := time.Now().Add(90 * time.Second)
	for {
		var list api.List[api.Agent]
		err := operator.Do(context.Background(), "GET", "/v1/admin/agents", nil, &list)
		if err == nil && len(list.Items) > 0 && list.Items[0].Healthy {
			break
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "no healthy node agent at %s: %v (is the e2e stack up?)\n", baseURL, err)
			os.Exit(1)
		}
		time.Sleep(time.Second)
	}
	os.Exit(m.Run())
}

func TestMicroVMBootsWithRequestedResources(t *testing.T) {
	c, _ := tenant(t, "boot", api.Quota{})
	sb := create(t, c, api.CreateSandboxRequest{Profile: "medium"})
	if sb.Trust != api.TrustPrivate || sb.Adapter != "firecracker" {
		t.Fatalf("placed on %s/%s", sb.Adapter, sb.Trust)
	}
	if got := strings.TrimSpace(sh(t, c, sb.ID, "nproc").Stdout); got != "2" {
		t.Fatalf("nproc = %q, want 2", got)
	}
	mem := strings.Fields(sh(t, c, sb.ID, "grep MemTotal /proc/meminfo").Stdout)
	kb, _ := strconv.Atoi(mem[1])
	if kb < 900_000 || kb > 1_048_576 {
		t.Fatalf("MemTotal %d kB for a 1024 MiB sandbox", kb)
	}
	cmdline := sh(t, c, sb.ID, "cat /proc/cmdline").Stdout
	if !strings.Contains(cmdline, "ip=172.30.") || !strings.Contains(cmdline, "root=/dev/vda") {
		t.Fatalf("not a Differently microVM: %s", cmdline)
	}
	disk := strings.Fields(sh(t, c, sb.ID, "df -B1 --output=size / | tail -1").Stdout)
	size, _ := strconv.ParseInt(disk[0], 10, 64)
	if size < 1_800_000_000 {
		t.Fatalf("root disk %d bytes; the 2 GiB profile was not applied", size)
	}
	t.Logf("guest kernel %s", strings.TrimSpace(sh(t, c, sb.ID, "uname -r").Stdout))
}

func TestExecSemantics(t *testing.T) {
	c, _ := tenant(t, "exec", api.Quota{})
	sb := create(t, c, api.CreateSandboxRequest{Env: map[string]string{"GREETING": "hello"}})
	ctx := ctx(t)
	res, err := c.Exec(ctx, sb.ID, api.ExecRequest{Command: `echo "$GREETING $WHO"; echo oops >&2; exit 7`, Env: map[string]string{"WHO": "world"}})
	if err != nil || res.ExitCode != 7 || strings.TrimSpace(res.Stdout) != "hello world" || strings.TrimSpace(res.Stderr) != "oops" {
		t.Fatalf("exec %+v %v", res, err)
	}
	res, err = c.Exec(ctx, sb.ID, api.ExecRequest{Args: []string{"cat"}, Stdin: "from stdin"})
	if err != nil || res.Stdout != "from stdin" {
		t.Fatalf("stdin %+v %v", res, err)
	}
	start := time.Now()
	res, err = c.Exec(ctx, sb.ID, api.ExecRequest{Command: "sleep 30", TimeoutSeconds: 1})
	if err != nil || res.ExitCode == 0 || time.Since(start) > 10*time.Second {
		t.Fatalf("timeout not enforced: %+v %v after %s", res, err, time.Since(start))
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
	c, _ := tenant(t, "files", api.Quota{})
	sb := create(t, c, api.CreateSandboxRequest{})
	data := bytes.Repeat([]byte("differently\x00\xff"), 80_000) // ~1 MiB, binary
	if err := c.WriteFile(ctx(t), sb.ID, "/work/blob.bin", data); err != nil {
		t.Fatal(err)
	}
	got, err := c.ReadFile(ctx(t), sb.ID, "/work/blob.bin")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read back %d bytes (want %d): %v", len(got), len(data), err)
	}
	if sum := strings.Fields(sh(t, c, sb.ID, "wc -c < /work/blob.bin").Stdout); sum[0] != strconv.Itoa(len(data)) {
		t.Fatalf("guest sees %v bytes", sum)
	}
	items, err := c.ListFiles(ctx(t), sb.ID, "/work")
	if err != nil || len(items) != 1 || items[0].Name != "blob.bin" {
		t.Fatalf("list %+v %v", items, err)
	}
}

func TestNetworkIsolation(t *testing.T) {
	c, _ := tenant(t, "net", api.Quota{})
	a := create(t, c, api.CreateSandboxRequest{})
	b := create(t, c, api.CreateSandboxRequest{})
	if out := sh(t, c, a.ID, `curl -sS -m 15 -o /dev/null -w "%{http_code}" https://example.com`).Stdout; out != "200" {
		t.Fatalf("internet egress (DNS + HTTPS) = %q", out)
	}
	blocked := map[string]string{
		"cloud metadata":            "curl -sS -m 3 http://169.254.169.254/",
		"node agent API on gateway": `curl -sS -m 3 http://$(ip route | awk '/default/{print $3}'):9090/healthz`,
	}
	if compose != "" {
		serverIP := strings.TrimSpace(docker(t, "exec", "-T", "agent", "getent", "hosts", "server"))
		blocked["control plane container"] = "curl -sS -m 3 http://" + strings.Fields(serverIP)[0] + ":8080/healthz"
	}
	bIP := strings.Fields(sh(t, c, b.ID, "ip -4 -o addr show eth0").Stdout)[3]
	bIP = strings.Split(bIP, "/")[0]
	sh(t, c, b.ID, "nohup nc -lk -p 8000 >/dev/null 2>&1 &")
	if res := sh(t, c, b.ID, "nc -z -w 2 127.0.0.1 8000"); res.ExitCode != 0 {
		t.Fatal("listener in sandbox b did not start")
	}
	blocked["another sandbox"] = "nc -z -w 3 " + bIP + " 8000"
	for what, cmd := range blocked {
		if res := sh(t, c, a.ID, cmd); res.ExitCode == 0 {
			t.Errorf("%s reachable from a sandbox: %q -> %+v", what, cmd, res)
		}
	}
}

func TestStopStartKeepsDisk(t *testing.T) {
	c, _ := tenant(t, "stop", api.Quota{})
	sb := create(t, c, api.CreateSandboxRequest{})
	sh(t, c, sb.ID, "echo persisted > /root/state")
	if _, err := c.Action(ctx(t), sb.ID, "stop"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx(t), sb.ID, api.ExecRequest{Command: "true"}); !api.IsCode(err, api.CodeInvalidState) {
		t.Fatalf("exec on a stopped sandbox: %v", err)
	}
	if _, err := c.Action(ctx(t), sb.ID, "start"); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(sh(t, c, sb.ID, "cat /root/state").Stdout); got != "persisted" {
		t.Fatalf("disk not kept across stop/start: %q", got)
	}
}

func TestPauseResume(t *testing.T) {
	c, _ := tenant(t, "pause", api.Quota{})
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
}

func TestSnapshotAndFork(t *testing.T) {
	c, _ := tenant(t, "fork", api.Quota{})
	parent := create(t, c, api.CreateSandboxRequest{})
	sh(t, c, parent.ID, "echo v1 > /root/state && sync")
	child, err := c.Fork(ctx(t), parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.DeleteSandbox(context.Background(), child.ID) })
	child, err = c.WaitSandbox(ctx(t), child.ID, api.StatusRunning, 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(sh(t, c, child.ID, "cat /root/state").Stdout); got != "v1" {
		t.Fatalf("fork lost the parent's disk: %q", got)
	}
	sh(t, c, child.ID, "echo child > /root/state")
	if got := strings.TrimSpace(sh(t, c, parent.ID, "cat /root/state").Stdout); got != "v1" {
		t.Fatalf("fork is not independent: parent sees %q", got)
	}
	if child.Labels["forked-from"] != parent.ID {
		t.Fatalf("labels %v", child.Labels)
	}
}

func TestQuotaQueuesAndReleases(t *testing.T) {
	c, _ := tenant(t, "quota", api.Quota{MaxActiveSandboxes: 1})
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

// The agent restarting kills its microVMs (they share its container). The
// control plane must notice, report them stopped, and boot them again from
// their disks; the agent must keep its identity.
func TestAgentRestartRecovers(t *testing.T) {
	if compose == "" {
		t.Skip("set DIFFERENTLY_E2E_COMPOSE to restart the agent")
	}
	c, _ := tenant(t, "restart", api.Quota{})
	sb := create(t, c, api.CreateSandboxRequest{})
	sh(t, c, sb.ID, "echo survived > /root/state && sync")
	docker(t, "restart", "agent")
	eventually(t, 60*time.Second, "the agent to re-register", func() bool {
		list := agents(t)
		return len(list) == 1 && list[0].Healthy && time.Since(list[0].LastSeen) < 5*time.Second
	})
	eventually(t, 60*time.Second, "the sandbox to be reported stopped", func() bool {
		got, _ := c.GetSandbox(context.Background(), sb.ID)
		return got.Status == api.StatusStopped
	})
	if _, err := c.Action(ctx(t), sb.ID, "start"); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(sh(t, c, sb.ID, "cat /root/state").Stdout); got != "survived" {
		t.Fatalf("disk lost across agent restart: %q", got)
	}
}

func TestDeleteCleansUpNode(t *testing.T) {
	c, _ := tenant(t, "cleanup", api.Quota{})
	sb := create(t, c, api.CreateSandboxRequest{})
	if err := c.DeleteSandbox(ctx(t), sb.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.WaitSandbox(ctx(t), sb.ID, api.StatusTerminated, time.Minute); err != nil {
		t.Fatal(err)
	}
	if compose != "" {
		eventually(t, 30*time.Second, "the VM's firecracker process to exit", func() bool {
			// "[f]" keeps pgrep from matching this shell's own command line.
			out := docker(t, "exec", "-T", "agent", "sh", "-c", "pgrep -f '[f]irecracker.*"+sb.ID+"' >/dev/null && echo alive || echo gone")
			return strings.TrimSpace(out) == "gone"
		})
	}
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
	var report map[string]any
	if err := operator.Do(ctx(t), "GET", "/v1/admin/trust-report", nil, &report); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(report)
	if strings.Contains(string(raw), `"secret"`) || strings.Contains(string(raw), "fail-open") || strings.Contains(string(raw), "/metrics is served without authentication") {
		t.Fatalf("trust report leaks a secret or describes settings this stack does not use: %s", raw)
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
	if !strings.Contains(string(body), `differently_sandbox_transitions_total{pool="fc-local",to="running"}`) {
		t.Fatal("metrics listener has no sandbox transitions for the real pool")
	}
}
