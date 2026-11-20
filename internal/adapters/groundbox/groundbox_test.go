//go:build experimental_adapters

package groundbox

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

const testKey = "gbx_test_key"

// fakeGroundbox mimics the /v1 API as the live API answered on 2026-11-18:
// errors come as {"error": {"code", "message"}}, a create with a known
// Idempotency-Key replays the sandbox the key made and says so in the
// Idempotent-Replayed header, the list filters on live, status and metadata
// and pages with a cursor, a deleted sandbox stays readable as "stopped",
// one stopped with its disk kept is "shut_down", and a kept snapshot is a
// copy of the sandbox's files that outlives it.
type fakeGroundbox struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	next      int
	sandboxes map[string]*fakeSandbox
	order     []string // newest last
	keys      map[string]usedKey
	kept      map[string]*fakeKept
	files     map[string][]byte // "<sandbox> <path>"

	// startAs is the status a new sandbox is created in. "starting" becomes
	// "running" once it has been read; "queued" stays until release.
	startAs string
	// pageSize caps a list page, whatever the caller asks for.
	pageSize int
	// fail answers the next matching request ("METHOD /path") with an error.
	fail map[string]fakeError
	// unfiltered makes the list ignore its metadata filter.
	unfiltered bool
	// drops is how many times the exec stream cuts a resumable command's
	// connection after its first chunk.
	drops int

	rec       recorded
	closeCode chan int
}

// idlePause pauses a sandbox as Groundbox does once its idle timeout has
// passed: memory kept, and woken by the next request when it asked for that.
func (f *fakeGroundbox) idlePause(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, reason := f.sandboxes[id], "idle"
	s.Status, s.PauseReason, s.WakesOnRequest = "paused", &reason, s.wakeOnRequest
}

// recorded is what the fake last received.
type recorded struct {
	create, exec, patch, preview, keep, pause map[string]any
	keys, lists                               []string
	attached                                  []map[string]any
}

// seen returns what the fake has received so far.
func (f *fakeGroundbox) seen() recorded {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.rec
	r.keys, r.lists, r.attached = slices.Clone(r.keys), slices.Clone(r.lists), slices.Clone(r.attached)
	return r
}

// count is how many sandboxes exist, ended ones included.
func (f *fakeGroundbox) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sandboxes)
}

// set changes the fake between requests.
func (f *fakeGroundbox) set(change func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change()
}

type fakeSandbox struct {
	ID         string            `json:"id"`
	Status     string            `json:"status"`
	Metadata   map[string]string `json:"metadata"`
	StopReason *string           `json:"stop_reason"`
	ForkedFrom *string           `json:"forked_from"`
	// PauseReason and WakesOnRequest are set while it is paused: "idle"
	// (the fake's idlePause) or "manual" (POST .../pause).
	PauseReason    *string `json:"pause_reason"`
	WakesOnRequest bool    `json:"wakes_on_request"`
	// wakeOnRequest is the create's wake_on_request, for an idle pause.
	wakeOnRequest bool
	// then is the status the sandbox moves to after its next read.
	then string
}

// fakeKept is a kept snapshot: "creating" until it has been read once.
type fakeKept struct {
	ID        string            `json:"id"`
	Status    string            `json:"status"`
	Source    string            `json:"source_sandbox_id"`
	Memory    bool              `json:"memory"`
	SizeBytes int64             `json:"size_bytes"`
	files     map[string][]byte // path -> content
}

type usedKey struct{ body, id string }

type fakeError struct {
	status        int
	code, message string
}

func newFake(t *testing.T) *fakeGroundbox {
	f := &fakeGroundbox{
		t: t, sandboxes: map[string]*fakeSandbox{}, keys: map[string]usedKey{}, kept: map[string]*fakeKept{}, files: map[string][]byte{},
		startAs: "starting", pageSize: 200, fail: map[string]fakeError{}, closeCode: make(chan int, 1),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sandboxes", f.create)
	mux.HandleFunc("GET /v1/sandboxes", f.list)
	mux.HandleFunc("GET /v1/sandboxes/{id}", f.one(func(w http.ResponseWriter, _ *http.Request, s *fakeSandbox) {
		json.NewEncoder(w).Encode(s)
		if s.then != "" {
			s.Status, s.then = s.then, ""
		}
	}))
	mux.HandleFunc("PATCH /v1/sandboxes/{id}", f.one(func(w http.ResponseWriter, r *http.Request, s *fakeSandbox) {
		f.rec.patch = body(r)
		json.NewEncoder(w).Encode(s)
	}))
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", f.one(func(w http.ResponseWriter, _ *http.Request, s *fakeSandbox) {
		if s.Status != "stopped" && s.Status != "failed" {
			reason := "deleted"
			s.Status, s.StopReason, s.then = "stopped", &reason, ""
		}
		json.NewEncoder(w).Encode(s)
	}))
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec", f.one(f.exec))
	mux.HandleFunc("GET /v1/sandboxes/{id}/exec/stream", f.stream)
	mux.HandleFunc("PUT /v1/sandboxes/{id}/files", f.one(func(w http.ResponseWriter, r *http.Request, s *fakeSandbox) {
		p := r.URL.Query().Get("path")
		if !strings.HasPrefix(p, "/") {
			fail(w, 400, "bad_request", "Invalid query: path: Use an absolute path")
			return
		}
		b, _ := io.ReadAll(r.Body)
		f.files[s.ID+" "+p] = b
		json.NewEncoder(w).Encode(map[string]any{"path": p, "size_bytes": len(b)})
	}))
	mux.HandleFunc("GET /v1/sandboxes/{id}/files", f.one(f.readFiles))
	mux.HandleFunc("POST /v1/sandboxes/{id}/pause", f.one(func(w http.ResponseWriter, r *http.Request, s *fakeSandbox) {
		if s.Status != "running" && s.Status != "paused" {
			fail(w, 409, "conflict", "Sandbox "+s.ID+" is "+s.Status+", not running")
			return
		}
		f.rec.pause = body(r)
		reason := "manual"
		s.Status, s.then, s.PauseReason, s.WakesOnRequest = "pausing", "paused", &reason, f.rec.pause["wake_on_request"] == true
		json.NewEncoder(w).Encode(s)
	}))
	mux.HandleFunc("POST /v1/sandboxes/{id}/resume", f.one(func(w http.ResponseWriter, _ *http.Request, s *fakeSandbox) {
		if s.Status != "paused" && s.Status != "running" {
			fail(w, 409, "conflict", "Sandbox "+s.ID+" is "+s.Status+", not paused")
			return
		}
		s.Status, s.then, s.PauseReason, s.WakesOnRequest = "resuming", "running", nil, false
		json.NewEncoder(w).Encode(s)
	}))
	mux.HandleFunc("POST /v1/sandboxes/{id}/stop", f.one(func(w http.ResponseWriter, _ *http.Request, s *fakeSandbox) {
		if s.Status != "running" && s.Status != "shut_down" {
			fail(w, 409, "conflict", "Sandbox "+s.ID+" is "+s.Status+", not running")
			return
		}
		s.Status, s.then = "shutting_down", "shut_down"
		for k := range f.files { // the disk is kept; /tmp is not
			if strings.HasPrefix(k, s.ID+" /tmp/") {
				delete(f.files, k)
			}
		}
		json.NewEncoder(w).Encode(s)
	}))
	mux.HandleFunc("POST /v1/sandboxes/{id}/start", f.one(func(w http.ResponseWriter, _ *http.Request, s *fakeSandbox) {
		if s.Status != "shut_down" && s.Status != "running" {
			fail(w, 409, "conflict", "Sandbox "+s.ID+" is "+s.Status+", not shut down")
			return
		}
		s.Status, s.then = "starting", "running"
		json.NewEncoder(w).Encode(s)
	}))
	mux.HandleFunc("POST /v1/snapshots", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.rec.keep = body(r)
		id, _ := f.rec.keep["sandbox_id"].(string)
		s := f.sandboxes[id]
		switch {
		case s == nil:
			fail(w, 404, "not_found", "No sandbox "+id)
			return
		case s.Status != "running":
			fail(w, 409, "conflict", "Sandbox "+s.ID+" is "+s.Status+". A snapshot can be kept of a running sandbox, or of a save point of a paused one")
			return
		}
		f.next++
		k := &fakeKept{ID: fmt.Sprintf("snap_%08d", f.next), Status: "creating", Source: s.ID, Memory: true, files: map[string][]byte{}}
		for key, b := range f.files {
			if p, ok := strings.CutPrefix(key, s.ID+" "); ok {
				k.files[p] = b
			}
		}
		f.kept[k.ID] = k
		json.NewEncoder(w).Encode(k)
	})
	mux.HandleFunc("GET /v1/snapshots/{id}", f.snapshot(func(w http.ResponseWriter, k *fakeKept) {
		json.NewEncoder(w).Encode(k)
		if k.Status == "creating" {
			k.Status, k.SizeBytes = "ready", 4096
		}
	}))
	mux.HandleFunc("DELETE /v1/snapshots/{id}", f.snapshot(func(w http.ResponseWriter, k *fakeKept) {
		k.Status = "deleted"
		json.NewEncoder(w).Encode(k)
	}))
	mux.HandleFunc("POST /v1/sandboxes/{id}/previews", f.one(func(w http.ResponseWriter, r *http.Request, s *fakeSandbox) {
		f.rec.preview = body(r)
		u := fmt.Sprintf("https://abc123-%v.previews.example/", f.rec.preview["port"])
		out := map[string]any{"url": u, "public": f.rec.preview["public"], "share_url": nil}
		if f.rec.preview["public"] != true {
			out["share_url"] = u + "?gb_token=gbp_share"
		}
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(out)
	}))
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			fail(w, 401, "unauthorized", "Unknown API key")
			return
		}
		f.mu.Lock()
		e, failing := f.fail[r.Method+" "+r.URL.Path]
		delete(f.fail, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		if failing {
			fail(w, e.status, e.code, e.message)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func fail(w http.ResponseWriter, status int, code, message string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": message, "request_id": "req_1"}})
}

func body(r *http.Request) map[string]any {
	var m map[string]any
	_ = json.NewDecoder(r.Body).Decode(&m)
	return m
}

// one runs a handler on the sandbox the path names, under the lock.
func (f *fakeGroundbox) one(h func(http.ResponseWriter, *http.Request, *fakeSandbox)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		s := f.sandboxes[r.PathValue("id")]
		if s == nil {
			fail(w, 404, "not_found", "No sandbox "+r.PathValue("id"))
			return
		}
		h(w, r, s)
	}
}

// snapshot runs a handler on the kept snapshot the path names, under the lock.
func (f *fakeGroundbox) snapshot(h func(http.ResponseWriter, *fakeKept)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		k := f.kept[r.PathValue("id")]
		if k == nil {
			fail(w, 404, "not_found", "No snapshot "+r.PathValue("id"))
			return
		}
		h(w, k)
	}
}

// add makes a sandbox; the caller holds the lock.
func (f *fakeGroundbox) add(status string, metadata map[string]string) *fakeSandbox {
	f.next++
	s := &fakeSandbox{ID: fmt.Sprintf("sbx_%08d", f.next), Status: status, Metadata: metadata}
	switch status {
	case "starting":
		s.then = "running"
	case "failed":
		reason := "error"
		s.StopReason = &reason
	}
	f.sandboxes[s.ID] = s
	f.order = append(f.order, s.ID)
	return s
}

// replay answers a request whose Idempotency-Key was used before; the
// caller holds the lock.
func (f *fakeGroundbox) replay(w http.ResponseWriter, r *http.Request, raw []byte) (id string, seen bool) {
	key := r.Header.Get("Idempotency-Key")
	used, ok := f.keys[key]
	if !ok {
		return "", false
	}
	if used.body != r.URL.Path+string(raw) {
		fail(w, 409, "idempotency_key_reused", "This Idempotency-Key was used for a different request")
		return "", true
	}
	w.Header().Set("Idempotent-Replayed", "true")
	return used.id, true
}

func metadataOf(m map[string]any) map[string]string {
	out := map[string]string{}
	meta, _ := m["metadata"].(map[string]any)
	for k, v := range meta {
		out[k] = fmt.Sprint(v)
	}
	return out
}

func (f *fakeGroundbox) create(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rec.create = m
	f.rec.keys = append(f.rec.keys, r.Header.Get("Idempotency-Key"))
	if id, seen := f.replay(w, r, raw); seen {
		if id != "" {
			json.NewEncoder(w).Encode(f.sandboxes[id])
		}
		return
	}
	cpus, _ := m["cpus"].(float64)
	if mem, _ := m["memory_gb"].(float64); cpus > 0 && mem > 4*cpus {
		fail(w, 400, "bad_request", "Invalid request body: memory_gb: memory_gb can be at most 4 GB per core")
		return
	}
	if m["template"] != nil && m["image"] != nil {
		fail(w, 400, "bad_request", "Invalid request body: image: give image or template, not both")
		return
	}
	if m["template"] == "no-such-template" {
		fail(w, 404, "not_found", "No template no-such-template")
		return
	}
	var from *fakeKept
	if id, ok := m["snapshot_id"].(string); ok {
		if from = f.kept[id]; from == nil {
			fail(w, 404, "not_found", "No snapshot "+id)
			return
		}
		if m["template"] != nil || m["image"] != nil || m["cpus"] != nil {
			fail(w, 400, "bad_request", "Invalid request body: snapshot_id: give one of snapshot_id, image or template")
			return
		}
		if from.Status != "ready" {
			fail(w, 409, "conflict", "Snapshot "+id+" is "+from.Status)
			return
		}
	}
	s := f.add(f.startAs, metadataOf(m))
	s.wakeOnRequest = m["wake_on_request"] != false
	if from != nil {
		for p, b := range from.files {
			f.files[s.ID+" "+p] = b
		}
	}
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		f.keys[key] = usedKey{body: r.URL.Path + string(raw), id: s.ID}
	}
	json.NewEncoder(w).Encode(s)
}

func (f *fakeGroundbox) list(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	f.rec.lists = append(f.rec.lists, r.URL.RawQuery)
	var statuses []string
	for _, v := range q["status"] {
		statuses = append(statuses, strings.Split(v, ",")...)
	}
	var match []*fakeSandbox
next:
	for i := len(f.order) - 1; i >= 0; i-- { // newest first
		s := f.sandboxes[f.order[i]]
		over := s.Status == "stopped" || s.Status == "failed"
		if (q.Get("live") == "true" && over) || (q.Get("live") == "false" && !over) || (len(statuses) > 0 && !slices.Contains(statuses, s.Status)) {
			continue
		}
		for k, v := range q {
			if key, ok := strings.CutPrefix(k, "metadata["); ok && !f.unfiltered && s.Metadata[strings.TrimSuffix(key, "]")] != v[0] {
				continue next
			}
		}
		match = append(match, s)
	}
	from, _ := strconv.Atoi(q.Get("cursor"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit == 0 || limit > f.pageSize {
		limit = f.pageSize
	}
	end := min(from+limit, len(match))
	out := map[string]any{"data": match[min(from, len(match)):end], "next_cursor": nil}
	if end < len(match) {
		out["next_cursor"] = strconv.Itoa(end)
	}
	json.NewEncoder(w).Encode(out)
}

func (f *fakeGroundbox) exec(w http.ResponseWriter, r *http.Request, s *fakeSandbox) {
	if s.Status == "paused" && s.WakesOnRequest { // wake on request, then run
		s.Status, s.PauseReason, s.WakesOnRequest = "running", nil, false
	}
	if s.Status != "running" {
		fail(w, 409, "conflict", "Sandbox "+s.ID+" is "+s.Status+", not running")
		return
	}
	f.rec.exec = body(r)
	if f.rec.exec["cmd"] == "sleep 30" {
		json.NewEncoder(w).Encode(map[string]any{"exit_code": 137, "stdout": "", "stderr": "", "duration_ms": 1000, "timed_out": true})
		return
	}
	if in, ok := f.rec.exec["stdin_base64"].(string); ok { // cat
		b, _ := base64.StdEncoding.DecodeString(in)
		json.NewEncoder(w).Encode(map[string]any{"exit_code": 0, "stdout": string(b), "stderr": "", "duration_ms": 5, "timed_out": false})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"exit_code": 2, "stdout": "out", "stderr": "err", "duration_ms": 5, "timed_out": false})
}

func (f *fakeGroundbox) readFiles(w http.ResponseWriter, r *http.Request, s *fakeSandbox) {
	if dir := r.URL.Query().Get("dir"); dir != "" {
		entries := []map[string]any{}
		seen := map[string]bool{}
		for k, b := range f.files {
			rest, ok := strings.CutPrefix(k, s.ID+" "+strings.TrimRight(dir, "/")+"/")
			if !ok {
				continue
			}
			name, _, nested := strings.Cut(rest, "/")
			if seen[name] {
				continue
			}
			seen[name] = true
			e := map[string]any{"name": name, "path": strings.TrimRight(dir, "/") + "/" + name, "type": "file", "size_bytes": len(b), "modified_at": "2026-11-18T14:18:47.478Z"}
			if nested {
				e["type"], e["size_bytes"] = "dir", 4096
			}
			entries = append(entries, e)
		}
		if len(entries) == 0 {
			fail(w, 404, "not_found", "No such file or folder: "+dir)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"dir": dir, "entries": entries})
		return
	}
	b, ok := f.files[s.ID+" "+r.URL.Query().Get("path")]
	if !ok {
		fail(w, 404, "not_found", "No such file or folder: "+r.URL.Query().Get("path"))
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(b)
}

// stream is the server side of the exec WebSocket. A command with stdin
// echoes it. A resumable command prints "one", "two" and an error line and
// exits 7; with drops left, its connection is cut after "one" (and an attach
// is cut at once) and the rest follows the next attach. "hang" prints one line and waits for the client to
// close; "broken" ends with an error message.
func (f *fakeGroundbox) stream(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	s := f.sandboxes[r.PathValue("id")]
	f.mu.Unlock()
	if s == nil {
		fail(w, 404, "not_found", "No sandbox "+r.PathValue("id"))
		return
	}
	if r.Header.Get("Upgrade") != "websocket" {
		fail(w, 426, "bad_request", "Connect to this endpoint with a WebSocket")
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
	send := func(v map[string]any) {
		b, _ := json.Marshal(v)
		serverFrame(conn, opText, b)
	}
	exit := func(code int, timedOut bool) {
		send(map[string]any{"type": "exit", "exit_code": code, "duration_ms": 3, "timed_out": timedOut})
		serverFrame(conn, opClose, []byte{0x03, 0xe8})
	}
	var first map[string]any
	_, raw := clientFrame(f.t, brw.Reader)
	json.Unmarshal(raw, &first)
	tail := func() {
		// A ping and a fragmented message, as a real server may send.
		serverFrame(conn, opPing, []byte("hi"))
		conn.Write([]byte{opText, 4})
		conn.Write([]byte(`{"ty`))
		rest := []byte(`pe":"stdout","data":"two\n","seq":8}`)
		conn.Write(append([]byte{0x80, byte(len(rest))}, rest...))
		send(map[string]any{"type": "heartbeat"})
		send(map[string]any{"type": "stderr", "data": "warn\n", "seq": 13})
		exit(7, false)
	}
	switch {
	case s.Status != "running":
		send(map[string]any{"type": "error", "code": "conflict", "message": "Sandbox " + s.ID + " is " + s.Status + ", not running"})
	case first["type"] == "attach":
		f.mu.Lock()
		f.rec.attached = append(f.rec.attached, first)
		drop := f.drops > 0
		if drop {
			f.drops--
		}
		f.mu.Unlock()
		if drop {
			return
		}
		if first["exec_id"] != "ex_0123456789abcdef" || first["seq"] != float64(4) {
			send(map[string]any{"type": "error", "message": "No such command"})
			return
		}
		tail()
	case first["type"] != "start":
		send(map[string]any{"type": "error", "message": "Send start first"})
	case first["stdin"] == true:
		f.mu.Lock()
		f.rec.exec = first
		f.mu.Unlock()
		var in strings.Builder
		for {
			var m map[string]any
			_, raw := clientFrame(f.t, brw.Reader)
			if raw == nil {
				return
			}
			json.Unmarshal(raw, &m)
			if m["type"] == "stdin_end" {
				break
			}
			b, _ := base64.StdEncoding.DecodeString(fmt.Sprint(m["data_base64"]))
			in.Write(b)
		}
		send(map[string]any{"type": "stdout", "data": in.String()})
		exit(0, false)
	case first["cmd"] == "broken":
		send(map[string]any{"type": "error", "code": "timeout", "message": "The sandbox's box is not reachable"})
	case first["cmd"] == "sleep 30":
		exit(137, true)
	case first["cmd"] == "hang":
		send(map[string]any{"type": "started", "exec_id": "ex_0123456789abcdef"})
		send(map[string]any{"type": "stdout", "data": "up\n", "seq": 3})
		op, payload := clientFrame(f.t, brw.Reader)
		code := 0
		if op == opClose && len(payload) >= 2 {
			code = int(binary.BigEndian.Uint16(payload))
		}
		f.closeCode <- code
	default:
		f.mu.Lock()
		f.rec.exec = first
		drop := f.drops > 0
		if drop {
			f.drops--
		}
		f.mu.Unlock()
		send(map[string]any{"type": "started", "exec_id": "ex_0123456789abcdef"})
		send(map[string]any{"type": "stdout", "data": "one\n", "seq": 4})
		if drop {
			return // the connection is cut without a close frame
		}
		tail()
	}
}

func serverFrame(w io.Writer, op byte, payload []byte) {
	hdr := []byte{0x80 | op}
	switch n := len(payload); {
	case n < 126:
		hdr = append(hdr, byte(n))
	case n <= 0xffff:
		hdr = append(hdr, 126)
		hdr = binary.BigEndian.AppendUint16(hdr, uint16(n))
	default:
		hdr = append(hdr, 127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	w.Write(append(hdr, payload...))
}

// clientFrame reads one masked client frame, skipping pongs. It returns a
// nil payload when the connection ends.
func clientFrame(t *testing.T, r *bufio.Reader) (op byte, payload []byte) {
	for {
		var h [2]byte
		if _, err := io.ReadFull(r, h[:]); err != nil {
			return 0, nil
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
		return h[0] & 0x0f, p
	}
}

func newAdapter(t *testing.T, f *fakeGroundbox, cfg map[string]string) *Adapter {
	t.Helper()
	if cfg == nil {
		cfg = map[string]string{}
	}
	cfg["apiURL"] = f.srv.URL + "/v1"
	o, err := FromConfig(cfg, testKey)
	if err != nil {
		t.Fatal(err)
	}
	o.ReadyTimeout, o.PollInterval = 5*time.Second, time.Millisecond
	return New(o)
}

func TestGroundboxAdapter(t *testing.T) {
	f := newFake(t)
	a := newAdapter(t, f, map[string]string{"template": "python-ml", "egress": "none"})
	ctx := context.Background()

	if err := a.Health(ctx); err != nil {
		t.Fatal(err)
	}
	if info := a.Info(); info.Kind != "groundbox" || info.Trust != api.TrustExternal || a.Endpoint() != f.srv.URL+"/v1" {
		t.Fatalf("info %+v at %s", info, a.Endpoint())
	}
	caps, _ := a.Capabilities(ctx)
	if got := adapter.EffectiveFeatures(a, caps.Features); len(got) != len(caps.Features) {
		t.Fatalf("declared %v but only %v are implemented", caps.Features, got)
	}

	req := adapter.CreateRequest{
		ID: "sbx_ABC", Installation: "inst_1", TTLSeconds: 600,
		Resources: api.Resources{CPU: 2, MemoryMiB: 3000, DiskGiB: 10},
		Env:       map[string]string{"TOKEN": "it's secret"},
		Labels:    map[string]string{adapter.LabelOrg: "org_1", adapter.LabelProject: "prj_1", "team": "not sent"},
	}
	inst, err := a.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != adapter.StateRunning || inst.SandboxID != "sbx_ABC" || inst.Ref != "sbx_00000001" {
		t.Fatalf("instance %+v", inst)
	}
	b := f.seen().create
	if b["cpus"] != float64(2) || b["memory_gb"] != float64(3) || b["disk_gb"] != float64(10) {
		t.Fatalf("size sent: %v", b)
	}
	// The auto-stop time is the TTL plus the grace: Differently's own expiry runs first.
	if b["timeout_minutes"] != float64(15) || b["template"] != "python-ml" || fmt.Sprint(b["env"]) != "map[TOKEN:it's secret]" {
		t.Fatalf("create body %v", b)
	}
	if hosts, ok := b["allow_hosts"].([]any); !ok || len(hosts) != 0 {
		t.Fatalf("egress none must send an empty allowlist, sent %v", b["allow_hosts"])
	}
	wantMeta := map[string]string{metaSandbox: "sbx_ABC", metaInstallation: "inst_1", metaOrg: "org_1", metaProject: "prj_1"}
	if got := metadataOf(b); fmt.Sprint(got) != fmt.Sprint(wantMeta) {
		t.Fatalf("metadata %v", got)
	}
	if !slices.Equal(f.seen().keys, []string{"differently:inst_1:sbx_ABC:1"}) {
		t.Fatalf("idempotency keys %v", f.seen().keys)
	}
	if again, err := a.Create(ctx, req); err != nil || again.Ref != inst.Ref || f.count() != 1 {
		t.Fatalf("create not idempotent: %v %v", again, err)
	}

	res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "make", Cwd: "/src", Env: map[string]string{"A": "b"}})
	if err != nil || res.ExitCode != 2 || res.Stdout != "out" || res.Stderr != "err" || res.DurationMs != 5 || res.Truncated {
		t.Fatalf("exec %+v %v", res, err)
	}
	// Without a timeout the longest is sent: Groundbox's default is 60 s.
	if e := f.seen().exec; e["cmd"] != "make" || e["cwd"] != "/src" || e["timeout_s"] != float64(3600) || fmt.Sprint(e["env"]) != "map[A:b]" {
		t.Fatalf("exec body %v", e)
	}
	// An argument list runs with no shell: it is sent as it is, with no cmd.
	if _, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Args: []string{"printf", "%s|", "a b", "it's"}}); err != nil || fmt.Sprint(f.seen().exec["args"]) != "[printf %s| a b it's]" || f.seen().exec["cmd"] != nil {
		t.Fatalf("args %v %v", f.seen().exec, err)
	}
	if res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "sleep 30", TimeoutSeconds: 1}); err != nil || res.ExitCode != 124 || f.seen().exec["timeout_s"] != float64(1) {
		t.Fatalf("timeout %+v %v", res, err)
	}
	// Stdin goes with the buffered call as base64, so bytes that are not text
	// arrive as written; more than the call takes goes over the stream.
	in := "piped \x00 \xff"
	if res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "cat", Stdin: in}); err != nil || !strings.HasPrefix(res.Stdout, "piped \x00 ") || res.ExitCode != 0 || f.seen().exec["stdin_base64"] != base64.StdEncoding.EncodeToString([]byte(in)) {
		t.Fatalf("stdin %+v %v (%v)", res, err, f.seen().exec)
	}
	big := strings.Repeat("0123456789abcdef", (maxExecStdin+stdinChunk)/16)
	if res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "cat", Stdin: big}); err != nil || res.Stdout != big || f.seen().exec["type"] != "start" || f.seen().exec["stdin"] != true {
		t.Fatalf("large stdin: %d bytes back, %v (%v)", len(res.Stdout), err, f.seen().exec["type"])
	}
	// So does a command longer than the buffered call waits for.
	if res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "build", TimeoutSeconds: 7200}); err != nil || res.ExitCode != 7 || res.Stdout != "one\ntwo\n" || f.seen().exec["timeout_s"] != float64(7200) {
		t.Fatalf("a two-hour command: %+v %v (%v)", res, err, f.seen().exec)
	}

	if err := a.WriteFile(ctx, inst.Ref, "work/deep/hello.txt", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if b, err := a.ReadFile(ctx, inst.Ref, "/work/deep/hello.txt"); err != nil || string(b) != "hello" {
		t.Fatalf("read %q %v", b, err)
	}
	if _, err := a.ReadFile(ctx, inst.Ref, "/missing"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("missing file: %v", err)
	}
	items, err := a.ListFiles(ctx, inst.Ref, "/work/deep")
	if err != nil || len(items) != 1 || items[0].Path != "/work/deep/hello.txt" || items[0].Name != "hello.txt" || items[0].Size != 5 || items[0].IsDir || items[0].ModTime.IsZero() {
		t.Fatalf("list %+v %v", items, err)
	}
	if items, err := a.ListFiles(ctx, inst.Ref, "/work"); err != nil || len(items) != 1 || !items[0].IsDir {
		t.Fatalf("list of the parent %+v %v", items, err)
	}

	// Stop keeps the disk and nothing else; start boots from it.
	if err := a.WriteFile(ctx, inst.Ref, "/tmp/scratch.txt", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := a.Stop(ctx, inst.Ref); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if in, err := a.Get(ctx, inst.Ref); err != nil || in.State != adapter.StateStopped {
		t.Fatalf("after stop: %+v %v", in, err)
	}
	if list, err := a.List(ctx, "inst_1"); err != nil || len(list) != 1 || list[0].State != adapter.StateStopped {
		t.Fatalf("a stopped sandbox is still the installation's: %+v %v", list, err)
	}
	if _, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "true"}); !api.IsCode(err, api.CodeInvalidState) {
		t.Fatalf("exec on a stopped sandbox: %v", err)
	}
	if again, err := a.Create(ctx, req); err != nil || again.Ref != inst.Ref || again.State != adapter.StateStopped {
		t.Fatalf("a replayed create of a stopped sandbox: %+v %v", again, err)
	}
	if err := a.Start(ctx, inst.Ref); err != nil {
		t.Fatalf("start: %v", err)
	}
	if b, err := a.ReadFile(ctx, inst.Ref, "/work/deep/hello.txt"); err != nil || string(b) != "hello" {
		t.Fatalf("the disk after a start: %q %v", b, err)
	}
	if _, err := a.ReadFile(ctx, inst.Ref, "/tmp/scratch.txt"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("/tmp after a start: %v", err)
	}

	if err := a.Pause(ctx, inst.Ref); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if in, err := a.Get(ctx, inst.Ref); err != nil || in.State != adapter.StatePaused {
		t.Fatalf("after pause: %+v %v", in, err)
	}
	if _, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "true"}); !api.IsCode(err, api.CodeInvalidState) {
		t.Fatalf("exec on a paused sandbox: %v", err)
	}
	if _, err := a.ExecStream(ctx, inst.Ref, api.ExecRequest{Command: "true"}, func(api.ExecEvent) error { return nil }); !api.IsCode(err, api.CodeInvalidState) {
		t.Fatalf("streamed exec on a paused sandbox: %v", err)
	}
	if err := a.Resume(ctx, inst.Ref); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if in, err := a.Get(ctx, inst.Ref); err != nil || in.State != adapter.StateRunning {
		t.Fatalf("after resume: %+v %v", in, err)
	}

	// A snapshot is kept on its own and always holds memory and disk, and
	// says so. Snapshot waits while the box copies it.
	snap, err := a.Snapshot(ctx, inst.Ref, "disk")
	if err != nil || snap.Kind != "memory" || snap.SizeBytes != 4096 || !strings.HasPrefix(snap.Ref, "snap_") || f.seen().keep["sandbox_id"] != inst.Ref {
		t.Fatalf("snapshot %+v %v (%v)", snap, err, f.seen().keep)
	}
	if err := a.WriteFile(ctx, inst.Ref, "/work/later.txt", []byte("later")); err != nil {
		t.Fatal(err)
	}
	// A fork starts from the snapshot at its size, with no template: Groundbox
	// refuses either.
	forkReq := adapter.CreateRequest{ID: "sbx_FORK", Installation: "inst_1", TTLSeconds: 60, FromSnapshot: snap.Ref, Resources: req.Resources, Env: req.Env, Template: "python-ml"}
	fork, err := a.Create(ctx, forkReq)
	if err != nil || fork.Ref == inst.Ref || fork.SandboxID != "sbx_FORK" || fork.State != adapter.StateRunning {
		t.Fatalf("fork %+v %v", fork, err)
	}
	if fb := f.seen().create; fb["snapshot_id"] != snap.Ref || fb["cpus"] != nil || fb["template"] != nil || fb["timeout_minutes"] != float64(6) || metadataOf(fb)[metaSandbox] != "sbx_FORK" || fmt.Sprint(fb["env"]) != "map[TOKEN:it's secret]" {
		t.Fatalf("fork body %v", fb)
	}
	if b, err := a.ReadFile(ctx, fork.Ref, "/work/deep/hello.txt"); err != nil || string(b) != "hello" {
		t.Fatalf("the fork lacks its source's file: %q %v", b, err)
	}
	if _, err := a.ReadFile(ctx, fork.Ref, "/work/later.txt"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("the fork has a file written after the snapshot: %v", err)
	}
	if again, err := a.Create(ctx, forkReq); err != nil || again.Ref != fork.Ref || f.count() != 2 {
		t.Fatalf("fork not idempotent: %+v %v", again, err)
	}

	// A private preview is reached with its share link; a public one has none.
	if u, err := a.ExposePort(ctx, inst.Ref, 8080); err != nil || u != "https://abc123-8080.previews.example/?gb_token=gbp_share" || f.seen().preview["public"] != false {
		t.Fatalf("expose %q %v (%v)", u, err, f.seen().preview)
	}
	if u, err := newAdapter(t, f, map[string]string{"ports": "public"}).ExposePort(ctx, inst.Ref, 8080); err != nil || u != "https://abc123-8080.previews.example/" || f.seen().preview["public"] != true {
		t.Fatalf("public expose %q %v (%v)", u, err, f.seen().preview)
	}

	if err := a.SetExpiry(ctx, inst.Ref, time.Now().Add(20*time.Minute)); err != nil || f.seen().patch["timeout_minutes"] != float64(20) {
		t.Fatalf("set expiry: %v, sent %v", err, f.seen().patch)
	}
	if err := a.SetExpiry(ctx, inst.Ref, time.Now().Add(72*time.Hour)); err != nil || f.seen().patch["timeout_minutes"] != float64(4320) {
		t.Fatalf("an expiry three days away: %v, sent %v", err, f.seen().patch)
	}
	if err := a.SetExpiry(ctx, inst.Ref, time.Now().Add(90*24*time.Hour)); err != nil || f.seen().patch["timeout_minutes"] != float64(43200) {
		t.Fatalf("an expiry past 30 days is the longest Groundbox takes: %v, sent %v", err, f.seen().patch)
	}
	if err := a.SetExpiry(ctx, inst.Ref, time.Now().Add(-time.Hour)); err != nil || f.seen().patch["timeout_minutes"] != float64(1) {
		t.Fatalf("an expiry in the past is one minute: %v, sent %v", err, f.seen().patch)
	}

	if list, err := a.List(ctx, "inst_1"); err != nil || len(list) != 2 {
		t.Fatalf("list %v %v", list, err)
	}
	if list, err := a.List(ctx, "inst_other"); err != nil || len(list) != 0 {
		t.Fatalf("another installation's list %v %v", list, err)
	}
	for i := 0; i < 2; i++ {
		if err := a.Delete(ctx, inst.Ref); err != nil {
			t.Fatalf("delete #%d: %v", i+1, err)
		}
	}
	// Groundbox still answers for a deleted sandbox, as "stopped".
	if _, err := a.Get(ctx, inst.Ref); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	if err := a.Delete(ctx, "sbx_neverexisted"); err != nil {
		t.Fatalf("delete of an unknown sandbox: %v", err)
	}
	if list, err := a.List(ctx, "inst_1"); err != nil || len(list) != 1 || list[0].Ref != fork.Ref {
		t.Fatalf("list after delete %v %v", list, err)
	}
	// The snapshot outlives its sandbox.
	late, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_LATE", Installation: "inst_1", FromSnapshot: snap.Ref})
	if err != nil || late.State != adapter.StateRunning {
		t.Fatalf("a sandbox from the snapshot of a deleted sandbox: %+v %v", late, err)
	}
	if b, err := a.ReadFile(ctx, late.Ref, "/work/deep/hello.txt"); err != nil || string(b) != "hello" {
		t.Fatalf("the snapshot's file: %q %v", b, err)
	}
	for i := 0; i < 2; i++ {
		if err := a.DeleteSnapshot(ctx, snap.Ref); err != nil {
			t.Fatalf("delete snapshot #%d: %v", i+1, err)
		}
	}
	if err := a.DeleteSnapshot(ctx, "snap_neverexisted"); err != nil {
		t.Fatalf("delete of an unknown snapshot: %v", err)
	}
	if _, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_GONE", Installation: "inst_1", FromSnapshot: snap.Ref}); !api.IsCode(err, api.CodeInvalidRequest) || !strings.Contains(err.Error(), "is deleted") {
		t.Fatalf("a sandbox from a deleted snapshot: %v", err)
	}
	if _, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_NONE", Installation: "inst_1", FromSnapshot: "snap_neverexisted"}); !api.IsCode(err, api.CodeInvalidRequest) || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("a sandbox from an unknown snapshot: %v", err)
	}
	if _, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_BAD", Installation: "inst_1", FromSnapshot: "sbx_1:snap_12345678"}); !api.IsCode(err, api.CodeInvalidRequest) {
		t.Fatalf("a foreign snapshot ref: %v", err)
	}
	// Only a running sandbox can be kept as it is now.
	if err := a.Pause(ctx, late.Ref); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Snapshot(ctx, late.Ref, "memory"); !api.IsCode(err, api.CodeInvalidState) {
		t.Fatalf("a snapshot of a paused sandbox: %v", err)
	}
}

// A sandbox that failed is one attempt. The scheduler retries Create with the
// same ID: the retry must get a new sandbox, and a replay of the retry the
// same one, although the first key still answers with the failed sandbox.
func TestGroundboxCreateRetriesGetANewSandbox(t *testing.T) {
	f := newFake(t)
	a := newAdapter(t, f, nil)
	ctx := context.Background()
	req := adapter.CreateRequest{ID: "sbx_RETRY", Installation: "inst_1"}

	f.set(func() { f.startAs = "failed" })
	if _, err := a.Create(ctx, req); !api.IsCode(err, adapter.CodeSandboxFailed) || !strings.Contains(err.Error(), "failed (error)") {
		t.Fatalf("a failed sandbox: %v", err)
	}
	// One Create makes one sandbox, also when it ends before it ran.
	if f.count() != 1 {
		t.Fatalf("a failed create made %d sandboxes", f.count())
	}
	f.set(func() { f.startAs = "starting" })
	second, err := a.Create(ctx, req)
	if err != nil || second.Ref != "sbx_00000002" || second.State != adapter.StateRunning {
		t.Fatalf("retry %+v %v", second, err)
	}
	if again, err := a.Create(ctx, req); err != nil || again.Ref != second.Ref || f.count() != 2 {
		t.Fatalf("replayed retry %+v %v, %d sandboxes", again, err, f.count())
	}
	if want := []string{"differently:inst_1:sbx_RETRY:1", "differently:inst_1:sbx_RETRY:1", "differently:inst_1:sbx_RETRY:2", "differently:inst_1:sbx_RETRY:1", "differently:inst_1:sbx_RETRY:2"}; !slices.Equal(f.seen().keys, want) {
		t.Fatalf("keys %v", f.seen().keys)
	}
	// Without a TTL the auto-stop time is the longest Groundbox takes.
	if f.seen().create["timeout_minutes"] != float64(43200) {
		t.Fatalf("timeout without a TTL: %v", f.seen().create["timeout_minutes"])
	}
	if _, sized := f.seen().create["cpus"]; sized {
		t.Fatalf("no size was asked for, sent %v", f.seen().create)
	}
	if _, listed := f.seen().create["allow_hosts"]; listed {
		t.Fatalf("egress public must send no allowlist, sent %v", f.seen().create["allow_hosts"])
	}

	// Every attempt ended: Create gives up instead of walking keys for ever.
	f.set(func() { f.startAs = "failed" })
	failing := adapter.CreateRequest{ID: "sbx_NEVER", Installation: "inst_1"}
	for i := 0; i < createAttempts; i++ {
		if _, err := a.Create(ctx, failing); !api.IsCode(err, adapter.CodeSandboxFailed) || f.count() != 3+i {
			t.Fatalf("attempt %d: %v, %d sandboxes", i+1, err, f.count())
		}
	}
	before := f.count()
	if _, err := a.Create(ctx, failing); !api.IsCode(err, adapter.CodeSandboxFailed) || f.count() != before {
		t.Fatalf("after %d ended sandboxes: %v, %d new", createAttempts, err, f.count()-before)
	}
}

// A sandbox Groundbox has queued is waiting for room: Create says so, leaves
// it queued, and the scheduler's next call finds the same sandbox.
func TestGroundboxQueuedSandboxIsACapacityWait(t *testing.T) {
	f := newFake(t)
	a := newAdapter(t, f, nil)
	a.opts.ReadyTimeout = 20 * time.Millisecond
	ctx := context.Background()
	req := adapter.CreateRequest{ID: "sbx_WAIT", Installation: "inst_1"}

	f.set(func() { f.startAs = "queued" })
	if _, err := a.Create(ctx, req); !api.IsCode(err, adapter.CodeInsufficientCapacity) {
		t.Fatalf("a queued sandbox: %v", err)
	}
	if list, err := a.List(ctx, "inst_1"); err != nil || len(list) != 1 || list[0].State != adapter.StatePending || list[0].SandboxID != "sbx_WAIT" {
		t.Fatalf("the queued sandbox must be listed for the reconciler: %+v %v", list, err)
	}
	f.mu.Lock()
	f.sandboxes["sbx_00000001"].Status = "running"
	f.mu.Unlock()
	if inst, err := a.Create(ctx, req); err != nil || inst.Ref != "sbx_00000001" || f.count() != 1 {
		t.Fatalf("the next call: %+v %v, %d sandboxes", inst, err, f.count())
	}

	// A sandbox stuck in another status is not a capacity wait.
	f.set(func() { f.startAs = "resuming" })
	if _, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_STUCK", Installation: "inst_1"}); !api.IsCode(err, api.CodeProviderUnavailable) {
		t.Fatalf("a sandbox that never settles: %v", err)
	}
}

// A replay whose body differs (the pool's config changed in between) cannot
// use the key. The sandbox is found by its metadata instead.
func TestGroundboxCreateAfterAConfigChange(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	req := adapter.CreateRequest{ID: "sbx_CFG", Installation: "inst_1"}
	first, err := newAdapter(t, f, nil).Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	changed := newAdapter(t, f, map[string]string{"template": "python-ml"})
	if again, err := changed.Create(ctx, req); err != nil || again.Ref != first.Ref || f.count() != 1 {
		t.Fatalf("replay after a config change: %+v %v, %d sandboxes", again, err, f.count())
	}
	// Once that sandbox is gone, the next key makes a new one.
	if err := changed.Delete(ctx, first.Ref); err != nil {
		t.Fatal(err)
	}
	if next, err := changed.Create(ctx, req); err != nil || next.Ref == first.Ref || f.seen().create["template"] != "python-ml" {
		t.Fatalf("create after the first ended: %+v %v", next, err)
	}
}

// List asks for the installation's live sandboxes in one filtered call per
// page, and still checks the metadata itself: the reconciler deletes what
// List returns and the control plane does not know.
func TestGroundboxListReadsEveryPageAndChecksOwners(t *testing.T) {
	f := newFake(t)
	f.set(func() { f.pageSize = 2 })
	a := newAdapter(t, f, nil)
	mine := func(n int) map[string]string {
		return map[string]string{metaInstallation: "inst_1", metaSandbox: fmt.Sprintf("sbx_%d", n)}
	}
	f.mu.Lock()
	for i, status := range []string{"running", "running", "running", "paused", "queued", "pausing", "resuming", "shutting_down", "shut_down", "stopped", "failed", "stopping"} {
		f.add(status, mine(i+1))
	}
	f.add("running", map[string]string{})
	f.add("running", map[string]string{metaInstallation: "inst_other", metaSandbox: "sbx_theirs"})
	f.add("running", map[string]string{metaInstallation: "inst_1"})
	f.mu.Unlock()
	want := map[string]string{"sbx_1": "running", "sbx_2": "running", "sbx_3": "running", "sbx_4": "paused", "sbx_5": "pending", "sbx_6": "paused", "sbx_7": "pending", "sbx_8": "stopped", "sbx_9": "stopped"}

	for _, unfiltered := range []bool{false, true} { // a server that ignores the filter returns the whole account
		f.set(func() { f.unfiltered, f.rec.lists = unfiltered, nil })
		list, err := a.List(context.Background(), "inst_1")
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, in := range list {
			got[in.SandboxID] = in.State
		}
		if len(list) != len(want) || fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("list (filter ignored: %v) %v, want %v", unfiltered, got, want)
		}
	}
	f.set(func() { f.unfiltered, f.rec.lists = false, nil })
	if _, err := a.List(context.Background(), "inst_1"); err != nil {
		t.Fatal(err)
	}
	calls := f.seen().lists
	if q, _ := url.ParseQuery(calls[0]); len(calls) != 6 || q.Get("live") != "true" || q.Get("metadata["+metaInstallation+"]") != "inst_1" || q.Get("status") != "" {
		t.Fatalf("list calls %q", calls)
	}

	// One page failing fails the list: a partial one would read as lost sandboxes.
	f.mu.Lock()
	f.fail["GET /v1/sandboxes"] = fakeError{status: 500, code: "internal", message: "boom"}
	f.mu.Unlock()
	if list, err := a.List(context.Background(), "inst_1"); !api.IsCode(err, api.CodeProviderUnavailable) || list != nil {
		t.Fatalf("a failed page: %v %v", list, err)
	}
}

func TestGroundboxErrorClassification(t *testing.T) {
	f := newFake(t)
	a := newAdapter(t, f, nil)
	ctx := context.Background()
	for i, c := range []struct {
		err  fakeError
		want string
		msg  string
	}{
		{fakeError{429, "concurrency_limit", "This team can run 5 sandboxes at once"}, adapter.CodeInsufficientCapacity, "5 sandboxes at once"},
		{fakeError{429, "rate_limited", "Slow down"}, adapter.CodeInsufficientCapacity, "Slow down"},
		{fakeError{503, "no_capacity", "No box has room"}, adapter.CodeInsufficientCapacity, "No box has room"},
		{fakeError{402, "insufficient_balance", "Add credit"}, api.CodeProviderUnavailable, "Add credit"},
		{fakeError{402, "paid_plan_required", "Sandboxes are on the paid plan"}, api.CodeProviderUnavailable, "paid plan"},
		{fakeError{403, "missing_scope", "This key lacks sandboxes:write"}, api.CodeProviderUnavailable, "credentials were rejected"},
		{fakeError{400, "bad_request", "Invalid request body: cpus: cpus is a whole number from 1 to 8"}, api.CodeInvalidRequest, "from 1 to 8"},
		{fakeError{500, "internal", "boom"}, api.CodeProviderUnavailable, "boom"},
		{fakeError{504, "timeout", "The box did not answer"}, api.CodeProviderUnavailable, "did not answer"},
	} {
		f.mu.Lock()
		f.fail["POST /v1/sandboxes"] = c.err
		f.mu.Unlock()
		_, err := a.Create(ctx, adapter.CreateRequest{ID: fmt.Sprintf("sbx_E%d", i), Installation: "inst_1"})
		if !api.IsCode(err, c.want) || !strings.Contains(err.Error(), c.msg) || strings.Contains(err.Error(), testKey) {
			t.Errorf("%d %s: got %v, want %s", c.err.status, c.err.code, err, c.want)
		}
	}
	if f.count() != 0 {
		t.Fatalf("%d sandboxes were made", f.count())
	}

	// A rejected key is an outage of the pool, and is never echoed.
	bad := New(Options{APIURL: f.srv.URL + "/v1", APIKey: "gbx_wrong_secret"})
	if err := bad.Health(ctx); !api.IsCode(err, api.CodeProviderUnavailable) || !strings.Contains(err.Error(), "credentials were rejected") || strings.Contains(err.Error(), "gbx_wrong_secret") {
		t.Fatalf("a rejected key: %v", err)
	}
	// The sandbox's own limits are the request's fault, not the pool's.
	if _, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_BIG", Installation: "inst_1", Resources: api.Resources{CPU: 1, MemoryMiB: 8192}}); !api.IsCode(err, api.CodeInvalidRequest) || !strings.Contains(err.Error(), "4 GB per core") {
		t.Fatalf("too much memory per core: %v", err)
	}
	if _, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_TPL", Installation: "inst_1", Template: "no-such-template"}); !api.IsCode(err, api.CodeInvalidRequest) || !strings.Contains(err.Error(), "no-such-template") {
		t.Fatalf("an unknown template: %v", err)
	}

	inst, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_OK", Installation: "inst_1"})
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.fail["POST /v1/sandboxes/"+inst.Ref+"/previews"] = fakeError{503, "not_available", "Previews are not switched on"}
	f.fail["PUT /v1/sandboxes/"+inst.Ref+"/files"] = fakeError{413, "bad_request", "A file can be at most 104857600 bytes"}
	f.fail["POST /v1/sandboxes/"+inst.Ref+"/pause"] = fakeError{429, "concurrency_limit", "This team can keep 10 sandboxes paused"}
	f.mu.Unlock()
	if _, err := a.ExposePort(ctx, inst.Ref, 80); !api.IsCode(err, api.CodeUnsupportedOperation) {
		t.Fatalf("previews switched off: %v", err)
	}
	if err := a.WriteFile(ctx, inst.Ref, "/big", []byte("x")); !api.IsCode(err, api.CodeInvalidRequest) {
		t.Fatalf("a file over the limit: %v", err)
	}
	if err := a.Pause(ctx, inst.Ref); !api.IsCode(err, adapter.CodeInsufficientCapacity) {
		t.Fatalf("too many paused sandboxes: %v", err)
	}
	if _, err := a.Get(ctx, "sbx_unknown"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("get of an unknown sandbox: %v", err)
	}
	// A sandbox that ends while it is being paused is that sandbox's failure.
	f.mu.Lock()
	f.sandboxes[inst.Ref].Status = "paused"
	f.mu.Unlock()
	if err := a.Resume(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.fail["POST /v1/sandboxes/"+inst.Ref+"/pause"] = fakeError{200, "", ""} // the pause is accepted
	reason := "worker_lost"
	f.sandboxes[inst.Ref].Status, f.sandboxes[inst.Ref].StopReason = "failed", &reason
	f.mu.Unlock()
	if err := a.Pause(ctx, inst.Ref); !api.IsCode(err, adapter.CodeSandboxFailed) || !strings.Contains(err.Error(), "worker_lost") {
		t.Fatalf("a sandbox lost during a pause: %v", err)
	}
}

func TestGroundboxBackstopOutlastsTheTTL(t *testing.T) {
	for ttl, want := range map[int]int{0: 43200, 1: 6, 600: 15, 601: 16, 3600: 65, 86400: 1445, 30 * 86400: 43200, 1 << 30: 43200} {
		if got := timeoutMinutes(ttl); got != want {
			t.Errorf("a TTL of %d s: %d minutes, want %d", ttl, got, want)
		}
		if got := timeoutMinutes(ttl); ttl > 0 && ttl < 30*86400-backstopGrace && got*60 < ttl+backstopGrace {
			t.Errorf("a TTL of %d s: Groundbox would stop the sandbox after %d s, before Differently's expiry and its grace", ttl, got*60)
		}
	}
}

func TestGroundboxExecStream(t *testing.T) {
	f := newFake(t)
	a := newAdapter(t, f, nil)
	ctx := context.Background()
	inst, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_STREAM", Installation: "inst_1"})
	if err != nil {
		t.Fatal(err)
	}
	collect := func(events *[]string) func(api.ExecEvent) error {
		return func(e api.ExecEvent) error {
			*events = append(*events, e.Stream+":"+e.Data)
			return nil
		}
	}
	want := []string{"stdout:one\n", "stdout:two\n", "stderr:warn\n"}

	var events []string
	code, err := a.ExecStream(ctx, inst.Ref, api.ExecRequest{Command: "build", Cwd: "/src", TimeoutSeconds: 30}, collect(&events))
	if err != nil || code != 7 || !slices.Equal(events, want) {
		t.Fatalf("stream: exit %d, %v, events %q", code, err, events)
	}
	// The command is resumable, so it survives a dropped connection.
	if s := f.seen().exec; s["cmd"] != "build" || s["cwd"] != "/src" || s["timeout_s"] != float64(30) || s["resumable"] != true || s["stdin"] != false {
		t.Fatalf("start message %v", s)
	}

	// The connection is cut after the first chunk: the adapter attaches to
	// the command again and reads on from the last output it saw.
	f.set(func() { f.drops = 2 })
	events = nil
	code, err = a.ExecStream(ctx, inst.Ref, api.ExecRequest{Command: "build"}, collect(&events))
	if err != nil || code != 7 || !slices.Equal(events, want) {
		t.Fatalf("stream with drops: exit %d, %v, events %q", code, err, events)
	}
	if len(f.seen().attached) != 2 || f.seen().attached[1]["seq"] != float64(4) {
		t.Fatalf("attach messages %v", f.seen().attached)
	}
	// A connection that keeps dropping is an error, not an endless loop.
	f.set(func() { f.drops = 100 })
	if _, err := a.ExecStream(ctx, inst.Ref, api.ExecRequest{Command: "build"}, func(api.ExecEvent) error { return nil }); !api.IsCode(err, api.CodeProviderUnavailable) {
		t.Fatalf("a stream that never holds: %v", err)
	}
	f.set(func() { f.drops = 0 })

	if code, err := a.ExecStream(ctx, inst.Ref, api.ExecRequest{Command: "sleep 30", TimeoutSeconds: 1}, func(api.ExecEvent) error { return nil }); err != nil || code != 124 {
		t.Fatalf("a command killed at its timeout: exit %d, %v", code, err)
	}
	if _, err := a.ExecStream(ctx, inst.Ref, api.ExecRequest{Command: "broken"}, func(api.ExecEvent) error { return nil }); !api.IsCode(err, api.CodeProviderUnavailable) || !strings.Contains(err.Error(), "not reachable") {
		t.Fatalf("an error from the stream: %v", err)
	}
	// Without a timeout a stream may run as long as Groundbox lets one; an
	// argument list is sent as it is.
	if _, err := a.ExecStream(ctx, inst.Ref, api.ExecRequest{Args: []string{"make", "all"}}, func(api.ExecEvent) error { return nil }); err != nil || f.seen().exec["timeout_s"] != float64(maxStreamSeconds) || fmt.Sprint(f.seen().exec["args"]) != "[make all]" {
		t.Fatalf("a stream with no timeout: %v (%v)", err, f.seen().exec)
	}
	if _, err := a.ExecStream(ctx, "sbx_unknown", api.ExecRequest{Command: "true"}, func(api.ExecEvent) error { return nil }); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("a stream for an unknown sandbox: %v", err)
	}
	bad := New(Options{APIURL: f.srv.URL + "/v1", APIKey: "gbx_wrong_secret"})
	if _, err := bad.ExecStream(ctx, inst.Ref, api.ExecRequest{Command: "true"}, func(api.ExecEvent) error { return nil }); !api.IsCode(err, api.CodeProviderUnavailable) || strings.Contains(err.Error(), "gbx_wrong_secret") {
		t.Fatalf("a stream with a rejected key: %v", err)
	}

	// A caller that goes away closes the socket with 1000, which is what
	// makes Groundbox kill a command that would otherwise run on.
	cctx, cancel := context.WithCancel(ctx)
	_, err = a.ExecStream(cctx, inst.Ref, api.ExecRequest{Command: "hang"}, func(api.ExecEvent) error {
		cancel()
		return nil
	})
	if err == nil {
		t.Fatal("a cancelled stream returned no error")
	}
	select {
	case code := <-f.closeCode:
		if code != 1000 {
			t.Fatalf("the socket closed with %d, want 1000", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the socket was not closed")
	}
	// So does an emit that fails: the client stopped reading.
	if _, err := a.ExecStream(ctx, inst.Ref, api.ExecRequest{Command: "hang"}, func(api.ExecEvent) error { return io.ErrClosedPipe }); err != io.ErrClosedPipe {
		t.Fatalf("a failed emit: %v", err)
	}
	select {
	case code := <-f.closeCode:
		if code != 1000 {
			t.Fatalf("the socket closed with %d, want 1000", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the socket was not closed after a failed emit")
	}
}

// What a sandbox starts from: the pool's template or image, or the request's
// template, which is a Groundbox template when it reads as one and a Docker
// image otherwise. The deny list goes with every create.
func TestGroundboxTemplatesImagesAndDenyList(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()
	for i, c := range []struct {
		cfg             map[string]string
		request         string
		template, image any
	}{
		{nil, "", nil, nil},
		{map[string]string{"template": "python-ml"}, "", "python-ml", nil},
		{map[string]string{"image": "ghcr.io/org/app:tag"}, "", nil, "ghcr.io/org/app:tag"},
		{map[string]string{"image": "ghcr.io/org/app:tag"}, "python-ml:3", "python-ml:3", nil},
		{map[string]string{"template": "python-ml"}, "python:3.12-slim", nil, "python:3.12-slim"},
		{nil, "tpl_abcdefgh12", "tpl_abcdefgh12", nil},
		{nil, "docker.io/library/ubuntu", nil, "docker.io/library/ubuntu"},
	} {
		cfg := map[string]string{"egressDeny": "*.example.com"}
		for k, v := range c.cfg {
			cfg[k] = v
		}
		if _, err := newAdapter(t, f, cfg).Create(ctx, adapter.CreateRequest{ID: fmt.Sprintf("sbx_T%d", i), Installation: "inst_1", Template: c.request}); err != nil {
			t.Fatal(err)
		}
		if b := f.seen().create; b["template"] != c.template || b["image"] != c.image || fmt.Sprint(b["deny_hosts"]) != "[*.example.com]" || b["allow_hosts"] != nil {
			t.Errorf("pool %v, request %q: sent template %v, image %v, deny %v", c.cfg, c.request, b["template"], b["image"], b["deny_hosts"])
		}
	}
}

func TestGroundboxFromConfig(t *testing.T) {
	o, err := FromConfig(map[string]string{}, "k")
	if err != nil || o.APIURL != DefaultAPIURL || o.AllowHosts != nil || o.PublicPorts {
		t.Fatalf("defaults %+v %v", o, err)
	}
	o, err = FromConfig(map[string]string{"apiURL": "https://example.test/v1/", "egress": "allowlist", "egressAllow": "github.com, registry.npmjs.org ,", "ports": "public", "template": "tpl"}, "k")
	if err != nil || o.APIURL != "https://example.test/v1" || !slices.Equal(o.AllowHosts, []string{"github.com", "registry.npmjs.org"}) || !o.PublicPorts || o.Template != "tpl" {
		t.Fatalf("options %+v %v", o, err)
	}
	if o, err := FromConfig(map[string]string{"egress": "none"}, "k"); err != nil || o.AllowHosts == nil || len(o.AllowHosts) != 0 {
		t.Fatalf("egress none %+v %v", o, err)
	}
	o, err = FromConfig(map[string]string{"image": "python:3.12-slim", "egressDeny": "*.example.com, 203.0.113.0/24"}, "k")
	if err != nil || o.Image != "python:3.12-slim" || o.AllowHosts != nil || !slices.Equal(o.DenyHosts, []string{"*.example.com", "203.0.113.0/24"}) {
		t.Fatalf("image and deny list %+v %v", o, err)
	}
	for _, cfg := range []map[string]string{{"egress": "allowlist"}, {"egress": "open"}, {"ports": "signed"}, {"template": "a", "image": "b:1"}} {
		if _, err := FromConfig(cfg, "k"); err == nil {
			t.Errorf("%v should be refused", cfg)
		}
	}
}

func TestGroundboxIdlesNatively(t *testing.T) {
	f := newFake(t)
	a := newAdapter(t, f, nil)
	ctx := context.Background()
	if !a.IdlesNatively() {
		t.Fatal("Groundbox pauses idle sandboxes itself")
	}

	// The idle timeout goes with the create, within Groundbox's 30 s to 30
	// days, and asks for wake on request; none is sent without one.
	for _, c := range []struct {
		asked int
		sent  any
	}{{0, nil}, {300, float64(300)}, {10, float64(30)}, {40 * 24 * 3600, float64(30 * 24 * 3600)}} {
		if _, err := a.Create(ctx, adapter.CreateRequest{ID: fmt.Sprintf("sbx_I%d", c.asked), Installation: "inst_1", IdleTimeoutSeconds: c.asked}); err != nil {
			t.Fatal(err)
		}
		wake := any(nil)
		if c.sent != nil {
			wake = true
		}
		if b := f.seen().create; b["idle_timeout_seconds"] != c.sent || b["wake_on_request"] != wake {
			t.Errorf("idle %d: sent idle_timeout_seconds %v, wake_on_request %v", c.asked, b["idle_timeout_seconds"], b["wake_on_request"])
		}
	}

	inst, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_IDLE", Installation: "inst_1", IdleTimeoutSeconds: 120})
	if err != nil {
		t.Fatal(err)
	}

	// Groundbox's idle pause shows as paused in Get and List, which the
	// reconciler follows.
	f.idlePause(inst.Ref)
	if got, err := a.Get(ctx, inst.Ref); err != nil || got.State != adapter.StatePaused {
		t.Fatalf("after an idle pause: %+v, %v", got, err)
	}
	listed, err := a.List(ctx, "inst_1")
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range listed {
		if in.Ref == inst.Ref && in.State != adapter.StatePaused {
			t.Fatalf("listed as %s after an idle pause", in.State)
		}
	}

	// The next exec wakes it on Groundbox's side and runs.
	if res, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "echo hi"}); err != nil || res.Stdout != "out" {
		t.Fatalf("exec after an idle pause: %+v, %v", res, err)
	}
	if got, _ := a.Get(ctx, inst.Ref); got.State != adapter.StateRunning {
		t.Fatalf("after the waking exec: %s", got.State)
	}

	// A pause asked for through the adapter does not wake on request: it
	// stays paused until Resume.
	if err := a.Pause(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}
	if b := f.seen().pause; b["wake_on_request"] != nil {
		t.Fatalf("pause sent wake_on_request %v", b["wake_on_request"])
	}
	if _, err := a.Exec(ctx, inst.Ref, api.ExecRequest{Command: "echo hi"}); err == nil {
		t.Fatal("an exec woke a sandbox paused on purpose")
	}
	if err := a.Resume(ctx, inst.Ref); err != nil {
		t.Fatal(err)
	}

	// A sandbox started from a snapshot gets its idle timeout too.
	snap, err := a.Snapshot(ctx, inst.Ref, "memory")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Create(ctx, adapter.CreateRequest{ID: "sbx_IDLE_FROM", Installation: "inst_1", FromSnapshot: snap.Ref, IdleTimeoutSeconds: 600}); err != nil {
		t.Fatal(err)
	}
	if b := f.seen().create; b["snapshot_id"] != snap.Ref || b["idle_timeout_seconds"] != float64(600) || b["wake_on_request"] != true {
		t.Fatalf("create from a snapshot sent %v", b)
	}
}
