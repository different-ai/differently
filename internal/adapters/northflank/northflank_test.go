package northflank

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/different-ai/differently/internal/adapters/shellcmd"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// fakeNorthflank follows Northflank's v1 API for deployment services and
// the command-exec WebSocket protocol (init with the API token, stdInEnd,
// stdOut/stdErr frames, then completion with the exit code as a string).
type fakeNorthflank struct {
	t        *testing.T
	mu       sync.Mutex
	services map[string]map[string]any
	order    []string
	creates  int
	gets     int
	limited  bool
	dir      string
	srv      *httptest.Server
}

const project = "/v1/projects/proj-1"

func nfError(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"status": status, "message": msg, "id": "req-1"}})
}

func newFake(t *testing.T) *fakeNorthflank {
	f := &fakeNorthflank{t: t, services: map[string]map[string]any{}, dir: t.TempDir()}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+project+"/services/deployment", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		name := body["name"].(string)
		if _, ok := f.services[name]; ok {
			nfError(w, 409, "There is already a service with the same derived identifier")
			return
		}
		f.creates++
		svc := map[string]any{"id": name, "name": name, "description": body["description"], "deployment": body["deployment"],
			"createOptions": body["createOptions"], "runtimeEnvironment": body["runtimeEnvironment"],
			"status": map[string]any{"deployment": map[string]any{"status": "IN_PROGRESS"}}, "ports": []any{}}
		f.services[name], f.order = svc, append(f.order, name)
		json.NewEncoder(w).Encode(map[string]any{"data": svc})
	})
	mux.HandleFunc("GET "+project+"/services", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var items []any
		for _, n := range f.order {
			if s, ok := f.services[n]; ok {
				items = append(items, map[string]any{"id": s["id"], "name": s["name"], "status": s["status"]})
			}
		}
		start := 0
		if c := r.URL.Query().Get("cursor"); c != "" {
			start = int(c[0] - '0')
		}
		end := min(start+2, len(items))
		page := map[string]any{"data": map[string]any{"services": items[min(start, len(items)):end]}, "pagination": map[string]any{"hasNextPage": end < len(items), "cursor": string(rune('0' + end)), "count": end - start}}
		json.NewEncoder(w).Encode(page)
	})
	mux.HandleFunc("GET "+project+"/services/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.gets++
		s, ok := f.services[r.PathValue("id")]
		if !ok {
			nfError(w, 404, "Service not found")
			return
		}
		s["status"] = map[string]any{"deployment": map[string]any{"status": "COMPLETED"}}
		json.NewEncoder(w).Encode(map[string]any{"data": s})
	})
	mux.HandleFunc("DELETE "+project+"/services/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.services[r.PathValue("id")]; !ok {
			nfError(w, 404, "Service not found")
			return
		}
		delete(f.services, r.PathValue("id"))
		w.Write([]byte(`{"data":{}}`))
	})
	mux.HandleFunc("PATCH "+project+"/services/deployment/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Ports []map[string]any `json:"ports"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		s := f.services[r.PathValue("id")]
		var ports []any
		for _, p := range body.Ports {
			p["dns"] = p["name"].(string) + "--" + s["name"].(string) + "--abc1.code.run"
			ports = append(ports, p)
		}
		s["ports"] = ports
		json.NewEncoder(w).Encode(map[string]any{"data": s})
	})
	mux.HandleFunc("GET /v1/command-exec/projects/proj-1/services/{id}", f.exec)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		limited := f.limited
		f.mu.Unlock()
		if limited {
			nfError(w, 429, "Rate limit exceeded")
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/v1/command-exec/") && r.Header.Get("Authorization") != "Bearer nf-token" {
			nfError(w, 401, "Unauthorized")
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// exec is the server side of the command-exec WebSocket.
func (f *fakeNorthflank) exec(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Upgrade") != "websocket" {
		nfError(w, 400, "expected a websocket")
		return
	}
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		f.t.Error(err)
		return
	}
	defer conn.Close()
	sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + wsGUID))
	brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n")
	brw.Flush()
	send := func(v any) {
		b, _ := json.Marshal(v)
		serverFrame(conn, opText, b)
	}
	var init struct {
		Data struct {
			Auth struct {
				APIToken string `json:"apiToken"`
			} `json:"auth"`
			Context struct {
				Command []string `json:"command"`
				Shell   string   `json:"shell"`
			} `json:"context"`
		} `json:"data"`
	}
	json.Unmarshal(clientFrame(f.t, brw.Reader), &init)
	if init.Data.Auth.APIToken != "nf-token" {
		send(map[string]any{"type": "init", "data": map[string]any{"auth": "failed", "message": "invalid token"}})
		return
	}
	send(map[string]any{"type": "init", "data": map[string]any{"auth": "successful"}})
	var end struct{ Type string }
	json.Unmarshal(clientFrame(f.t, brw.Reader), &end)
	if end.Type != "stdInEnd" || init.Data.Context.Shell != "none" || len(init.Data.Context.Command) != 3 {
		f.t.Errorf("exec context %+v, then %q", init.Data.Context, end.Type)
	}
	env := filepath.Join(f.dir, r.PathValue("id"), "env")
	cmdline := strings.ReplaceAll(init.Data.Context.Command[2], shellcmd.EnvFile, env)
	cmdline = strings.ReplaceAll(cmdline, filepath.Dir(shellcmd.EnvFile), filepath.Dir(env))
	var out, errb bytes.Buffer
	cmd := exec.Command("/bin/sh", "-c", cmdline)
	cmd.Stdout, cmd.Stderr = &out, &errb
	var ee *exec.ExitError
	if err := cmd.Run(); err != nil && !errors.As(err, &ee) {
		f.t.Errorf("sh: %v", err)
	}
	// Split stdout in two messages to exercise accumulation; send a
	// fragmented message and a ping too.
	o := out.String()
	send(map[string]any{"type": "stdOut", "data": o[:len(o)/2]})
	frag, _ := json.Marshal(map[string]any{"type": "stdOut", "data": o[len(o)/2:]})
	conn.Write(append([]byte{opText, byte(len(frag[:5]))}, frag[:5]...)) // FIN clear
	serverFrame(conn, opPing, []byte("hi"))
	conn.Write(append([]byte{0x80, byte(len(frag[5:]))}, frag[5:]...)) // continuation, FIN
	if errb.Len() > 0 {
		send(map[string]any{"type": "stdErr", "data": errb.String()})
	}
	send(map[string]any{"type": "completion", "data": map[string]any{"exitCode": itoa(cmd.ProcessState.ExitCode()), "status": "Success"}})
	serverFrame(conn, opClose, []byte{0x03, 0xe8})
}

// itoa sends the exit code as a string, as Northflank does in practice.
func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func serverFrame(w io.Writer, op byte, payload []byte) {
	hdr := []byte{0x80 | op}
	if len(payload) < 126 {
		hdr = append(hdr, byte(len(payload)))
	} else {
		hdr = append(hdr, 126)
		hdr = binary.BigEndian.AppendUint16(hdr, uint16(len(payload)))
	}
	w.Write(append(hdr, payload...))
}

// clientFrame reads one masked client frame, skipping pongs.
func clientFrame(t *testing.T, r *bufio.Reader) []byte {
	for {
		var h [2]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return nil
		}
		if h[1]&0x80 == 0 {
			t.Error("client frames must be masked")
		}
		n := int(h[1] & 0x7f)
		switch n {
		case 126:
			var b [2]byte
			io.ReadFull(r, b[:])
			n = int(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			io.ReadFull(r, b[:])
			n = int(binary.BigEndian.Uint64(b[:]))
		}
		mask := make([]byte, 4)
		io.ReadFull(r, mask)
		p := make([]byte, n)
		io.ReadFull(r, p)
		for i := range p {
			p[i] ^= mask[i%4]
		}
		if h[0]&0x0f == opPong {
			continue
		}
		return p
	}
}

func newAdapter(t *testing.T, url, token string) *Adapter {
	o, err := FromConfig(map[string]string{"projectId": "proj-1", "apiURL": url}, token)
	if err != nil {
		t.Fatal(err)
	}
	o.PollEvery = time.Millisecond
	return New(o)
}

func TestNorthflankAdapter(t *testing.T) {
	f := newFake(t)
	a := newAdapter(t, f.srv.URL, "nf-token")
	ctx := context.Background()
	if err := a.Health(ctx); err != nil {
		t.Fatal(err)
	}
	req := adapter.CreateRequest{ID: "sbx_01jb2c3d4e5f6g7h8j9k0m1n2p", Installation: "inst_1", TTLSeconds: 3600, Env: map[string]string{"MODE": "test"}}
	inst, err := a.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(inst.Ref, "dly-") || len(inst.Ref) > 54 || inst.SandboxID != req.ID || inst.State != adapter.StateRunning {
		t.Fatalf("instance %+v", inst)
	}
	svc := f.services[inst.Ref]
	if co, _ := json.Marshal(svc["createOptions"]); string(co) != `{"expiryTime":3900}` {
		t.Fatalf("expiry %s", co)
	}
	if again, err := a.Create(ctx, req); err != nil || again.Ref != inst.Ref || f.creates != 1 {
		t.Fatalf("replayed create %+v %v (%d creates)", again, err, f.creates)
	}
	if got, err := a.Get(ctx, inst.Ref); err != nil || got.SandboxID != req.ID {
		t.Fatalf("get %+v %v", got, err)
	}

	res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: `echo out-$X; cat; echo err >&2; exit 6`, Env: map[string]string{"X": "1"}, Stdin: "in"})
	if err != nil || res.ExitCode != 6 || res.Stdout != "out-1\nin" || res.Stderr != "err\n" {
		t.Fatalf("exec %+v %v", res, err)
	}
	var events []api.ExecEvent
	code, err := a.ExecStream(ctx, inst.Ref, api.ExecRequest{Command: "printf abcdefghij"}, func(e api.ExecEvent) error { events = append(events, e); return nil })
	if err != nil || code != 0 || len(events) != 2 || events[0].Data+events[1].Data != "abcdefghij" {
		t.Fatalf("stream %d %+v %v", code, events, err)
	}
	path := filepath.Join(f.dir, "files", "x.txt")
	if err := a.WriteFile(ctx, inst.Ref, path, []byte("payload")); err != nil {
		t.Fatal(err)
	}
	if b, err := a.ReadFile(ctx, inst.Ref, path); err != nil || string(b) != "payload" {
		t.Fatalf("read %q %v", b, err)
	}

	u, err := a.ExposePort(ctx, inst.Ref, 3000)
	if err != nil || u != "https://p3000--"+inst.Ref+"--abc1.code.run" {
		t.Fatalf("port %q %v", u, err)
	}
	if _, err := a.ExposePort(ctx, inst.Ref, 4000); err != nil || len(f.services[inst.Ref]["ports"].([]any)) != 2 {
		t.Fatalf("a second port must keep the first: %v %v", err, f.services[inst.Ref]["ports"])
	}

	for _, id := range []string{"sbx_01jb2c3d4e5f6g7h8j9k0m1n2q", "sbx_01jb2c3d4e5f6g7h8j9k0m1n2r"} {
		if _, err := a.Create(ctx, adapter.CreateRequest{ID: id, Installation: "inst_1"}); err != nil {
			t.Fatal(err)
		}
	}
	if f.services[f.order[1]]["createOptions"] != nil {
		t.Fatal("no TTL, no expiry")
	}
	if _, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_01jb2c3d4e5f6g7h8j9k0m1n2s", Installation: "inst_2"}); err != nil {
		t.Fatal(err)
	}
	list, err := a.List(ctx, "inst_1")
	if err != nil || len(list) != 3 || list[0].SandboxID != req.ID {
		t.Fatalf("paged list %+v %v", list, err)
	}
	if err := a.Delete(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	if err := a.Delete(ctx, inst.Ref); err != nil {
		t.Fatalf("second delete must succeed: %v", err)
	}
	caps, _ := a.Capabilities(ctx)
	if caps.Has(adapter.FeatureStopStart) || caps.Has(adapter.FeatureSnapshotDisk) || !caps.Has(adapter.FeatureExecStream) {
		t.Fatalf("capabilities %+v", caps)
	}
}

func TestNorthflankErrorsAndHealthCache(t *testing.T) {
	f := newFake(t)
	bad := newAdapter(t, f.srv.URL, "wrong")
	ctx := context.Background()
	if err := bad.Health(ctx); !api.IsCode(err, api.CodeProviderUnavailable) || strings.Contains(err.Error(), "wrong") {
		t.Fatalf("401: %v", err)
	}
	if _, err := bad.Exec(ctx, "svc", api.ExecRequest{Command: "true"}); !api.IsCode(err, api.CodeProviderUnavailable) {
		t.Fatalf("exec auth failure: %v", err)
	}
	// Health answers from its cache for 30 s.
	good := newAdapter(t, f.srv.URL, "nf-token")
	good.Health(ctx)
	f.mu.Lock()
	f.limited = true
	f.mu.Unlock()
	if err := good.Health(ctx); err != nil {
		t.Fatalf("cached health: %v", err)
	}
	good.checkedAt = time.Now().Add(-time.Minute)
	if err := good.Health(ctx); !api.IsCode(err, adapter.CodeInsufficientCapacity) {
		t.Fatalf("429: %v", err)
	}
	if _, err := good.Create(ctx, adapter.CreateRequest{ID: "sbx_x", Installation: "inst_1"}); !api.IsCode(err, adapter.CodeInsufficientCapacity) {
		t.Fatalf("429 on create: %v", err)
	}
	if _, err := FromConfig(map[string]string{}, "t"); err == nil {
		t.Fatal("projectId is required")
	}
}

func TestWebSocketRejectsBadAccept(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		bufio.NewReader(c).ReadString('\n')
		c.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: nope\r\n\r\n"))
		time.Sleep(100 * time.Millisecond)
		c.Close()
	}()
	if _, err := dialWS(context.Background(), &http.Client{}, "ws://"+ln.Addr().String()+"/x"); err == nil || !strings.Contains(err.Error(), "Accept") {
		t.Fatalf("bad accept: %v", err)
	}
}
