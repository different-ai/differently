// Package adapterhttp serves an adapter.ExecutionAdapter over HTTP and
// provides a client that implements the same interface.
//
// This is the out-of-process plugin protocol (the `remote` pool adapter): any
// backend can implement it in any language. Routes live
// under /adapter/v1, bodies are JSON, errors use the api.ErrorEnvelope shape,
// and streamed exec output is newline-delimited JSON (one api.ExecEvent per
// line). An optional bearer token authenticates the control plane.
package adapterhttp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// ProtocolHeader carries the protocol version on every response.
const ProtocolHeader = "Differently-Adapter-Protocol"

// MaxFileBytes bounds file transfers through the protocol.
const MaxFileBytes = 100 << 20

// InfoResponse is returned by GET /adapter/v1/info.
type InfoResponse struct {
	Info         adapter.Info         `json:"info"`
	Capabilities adapter.Capabilities `json:"capabilities"`
}

type snapshotRequest struct {
	Kind string `json:"kind"`
}

type portRequest struct {
	Port int `json:"port"`
}

type portResponse struct {
	URL string `json:"url"`
}

// Handler serves a. If token is non-empty every request must present it as a
// bearer token.
func Handler(a adapter.ExecutionAdapter, token string) http.Handler {
	mux := http.NewServeMux()
	h := &handler{a: a}
	mux.HandleFunc("GET /adapter/v1/info", h.info)
	mux.HandleFunc("GET /adapter/v1/capacity", h.capacity)
	mux.HandleFunc("GET /adapter/v1/health", h.health)
	mux.HandleFunc("POST /adapter/v1/instances", h.create)
	mux.HandleFunc("GET /adapter/v1/instances", h.list)
	mux.HandleFunc("GET /adapter/v1/instances/{ref}", h.get)
	mux.HandleFunc("DELETE /adapter/v1/instances/{ref}", h.delete)
	mux.HandleFunc("POST /adapter/v1/instances/{ref}/exec", h.exec)
	mux.HandleFunc("GET /adapter/v1/instances/{ref}/files", h.listFiles)
	mux.HandleFunc("GET /adapter/v1/instances/{ref}/files/content", h.readFile)
	mux.HandleFunc("PUT /adapter/v1/instances/{ref}/files/content", h.writeFile)
	mux.HandleFunc("POST /adapter/v1/instances/{ref}/{action}", h.action)
	mux.HandleFunc("POST /adapter/v1/instances/{ref}/snapshots", h.snapshot)
	mux.HandleFunc("DELETE /adapter/v1/snapshots/{ref}", h.deleteSnapshot)
	mux.HandleFunc("POST /adapter/v1/instances/{ref}/ports", h.port)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(ProtocolHeader, adapter.ProtocolVersion)
		if token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				writeErr(w, api.Errorf(api.CodeUnauthenticated, "invalid adapter token"))
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

type handler struct{ a adapter.ExecutionAdapter }

func (h *handler) info(w http.ResponseWriter, r *http.Request) {
	caps, err := h.a.Capabilities(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	caps.Features = adapter.EffectiveFeatures(h.a, caps.Features)
	info := h.a.Info()
	info.Protocol = adapter.ProtocolVersion
	writeJSON(w, http.StatusOK, InfoResponse{Info: info, Capabilities: caps})
}

func (h *handler) capacity(w http.ResponseWriter, r *http.Request) {
	c, err := h.a.Capacity(r.Context())
	respond(w, c, err)
}

func (h *handler) health(w http.ResponseWriter, r *http.Request) {
	if err := h.a.Health(r.Context()); err != nil {
		writeErr(w, adapter.Unavailable(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *handler) create(w http.ResponseWriter, r *http.Request) {
	var req adapter.CreateRequest
	if !decode(w, r, &req) {
		return
	}
	inst, err := h.a.Create(r.Context(), req)
	respond(w, inst, err)
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	items, err := h.a.List(r.Context(), r.URL.Query().Get("installation"))
	if items == nil {
		items = []adapter.Instance{}
	}
	respond(w, items, err)
}

func (h *handler) get(w http.ResponseWriter, r *http.Request) {
	inst, err := h.a.Get(r.Context(), r.PathValue("ref"))
	respond(w, inst, err)
}

func (h *handler) delete(w http.ResponseWriter, r *http.Request) {
	if err := h.a.Delete(r.Context(), r.PathValue("ref")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) exec(w http.ResponseWriter, r *http.Request) {
	var req api.ExecRequest
	if !decode(w, r, &req) {
		return
	}
	ref := r.PathValue("ref")
	if r.URL.Query().Get("stream") == "" {
		res, err := h.a.Exec(r.Context(), ref, req)
		respond(w, res, err)
		return
	}
	s, ok := h.a.(adapter.Streamer)
	if !ok {
		writeErr(w, adapter.Unsupported("exec.stream"))
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	emit := func(ev api.ExecEvent) error {
		if err := enc.Encode(ev); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	code, err := s.ExecStream(r.Context(), ref, req, emit)
	if err != nil {
		_ = emit(api.ExecEvent{Error: err.Error()})
		return
	}
	_ = emit(api.ExecEvent{ExitCode: &code})
}

func (h *handler) fs(w http.ResponseWriter) (adapter.FileSystem, bool) {
	fs, ok := h.a.(adapter.FileSystem)
	if !ok {
		writeErr(w, adapter.Unsupported("files"))
	}
	return fs, ok
}

func (h *handler) listFiles(w http.ResponseWriter, r *http.Request) {
	fs, ok := h.fs(w)
	if !ok {
		return
	}
	items, err := fs.ListFiles(r.Context(), r.PathValue("ref"), r.URL.Query().Get("path"))
	if items == nil {
		items = []api.FileInfo{}
	}
	respond(w, items, err)
}

func (h *handler) readFile(w http.ResponseWriter, r *http.Request) {
	fs, ok := h.fs(w)
	if !ok {
		return
	}
	data, err := fs.ReadFile(r.Context(), r.PathValue("ref"), r.URL.Query().Get("path"))
	if err != nil {
		writeErr(w, err)
		return
	}
	// Sandbox files are untrusted: force a download and stop browsers from
	// sniffing or rendering them on the API origin.
	hdr := w.Header()
	hdr.Set("Content-Type", "application/octet-stream")
	hdr.Set("Content-Disposition", "attachment")
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	_, _ = w.Write(data)
}

func (h *handler) writeFile(w http.ResponseWriter, r *http.Request) {
	fs, ok := h.fs(w)
	if !ok {
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, MaxFileBytes+1))
	if err != nil {
		writeErr(w, err)
		return
	}
	if len(data) > MaxFileBytes {
		writeErr(w, api.Errorf(api.CodeInvalidRequest, "file larger than %d bytes", MaxFileBytes))
		return
	}
	if err := fs.WriteFile(r.Context(), r.PathValue("ref"), r.URL.Query().Get("path"), data); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) action(w http.ResponseWriter, r *http.Request) {
	ref, action := r.PathValue("ref"), r.PathValue("action")
	var err error
	switch action {
	case "stop", "start":
		l, ok := h.a.(adapter.Lifecycle)
		if !ok {
			writeErr(w, adapter.Unsupported(action))
			return
		}
		if action == "stop" {
			err = l.Stop(r.Context(), ref)
		} else {
			err = l.Start(r.Context(), ref)
		}
	case "pause", "resume":
		p, ok := h.a.(adapter.Pauser)
		if !ok {
			writeErr(w, adapter.Unsupported(action))
			return
		}
		if action == "pause" {
			err = p.Pause(r.Context(), ref)
		} else {
			err = p.Resume(r.Context(), ref)
		}
	default:
		writeErr(w, api.Errorf(api.CodeNotFound, "unknown action %s", action))
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) snapshot(w http.ResponseWriter, r *http.Request) {
	s, ok := h.a.(adapter.Snapshotter)
	if !ok {
		writeErr(w, adapter.Unsupported("snapshot"))
		return
	}
	var req snapshotRequest
	if !decode(w, r, &req) {
		return
	}
	snap, err := s.Snapshot(r.Context(), r.PathValue("ref"), req.Kind)
	respond(w, snap, err)
}

func (h *handler) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	s, ok := h.a.(adapter.Snapshotter)
	if !ok {
		writeErr(w, adapter.Unsupported("snapshot"))
		return
	}
	if err := s.DeleteSnapshot(r.Context(), r.PathValue("ref")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) port(w http.ResponseWriter, r *http.Request) {
	p, ok := h.a.(adapter.PortExposer)
	if !ok {
		writeErr(w, adapter.Unsupported("ports.expose"))
		return
	}
	var req portRequest
	if !decode(w, r, &req) {
		return
	}
	u, err := p.ExposePort(r.Context(), r.PathValue("ref"), req.Port)
	respond(w, portResponse{URL: u}, err)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, api.Errorf(api.CodeInvalidRequest, "invalid JSON body: %v", err))
		return false
	}
	return true
}

func respond(w http.ResponseWriter, v any, err error) {
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	e := api.AsError(err)
	status := e.HTTPStatus()
	if e.Code == adapter.CodeInsufficientCapacity || e.Code == adapter.CodeSandboxFailed {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, api.ErrorEnvelope{Error: e})
}

// Client implements adapter.ExecutionAdapter and every optional interface
// against a remote adapter. Optional operations the remote does not support
// return unsupported_operation errors.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client

	info InfoResponse
}

// NewClient returns a client for baseURL.
func NewClient(baseURL, token string) *Client {
	// No short client timeout: exec and transfers may run for hours and are
	// bounded by the caller's context instead.
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: &http.Client{Timeout: 2*time.Hour + 15*time.Minute}}
}

// Discover fetches the remote info and capabilities.
func (c *Client) Discover(ctx context.Context) (InfoResponse, error) {
	var out InfoResponse
	err := c.do(ctx, http.MethodGet, "/adapter/v1/info", nil, &out)
	if err == nil {
		c.info = out
	}
	return out, err
}

// Info returns the last discovered info.
func (c *Client) Info() adapter.Info { return c.info.Info }

// Capabilities fetches capabilities from the remote.
func (c *Client) Capabilities(ctx context.Context) (adapter.Capabilities, error) {
	out, err := c.Discover(ctx)
	return out.Capabilities, err
}

// Capacity implements adapter.ExecutionAdapter.
func (c *Client) Capacity(ctx context.Context) (adapter.Capacity, error) {
	var out adapter.Capacity
	return out, c.do(ctx, http.MethodGet, "/adapter/v1/capacity", nil, &out)
}

// Health implements adapter.ExecutionAdapter.
func (c *Client) Health(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/adapter/v1/health", nil, nil)
}

// Create implements adapter.ExecutionAdapter.
func (c *Client) Create(ctx context.Context, req adapter.CreateRequest) (adapter.Instance, error) {
	var out adapter.Instance
	return out, c.do(ctx, http.MethodPost, "/adapter/v1/instances", req, &out)
}

// Get implements adapter.ExecutionAdapter.
func (c *Client) Get(ctx context.Context, ref string) (adapter.Instance, error) {
	var out adapter.Instance
	return out, c.do(ctx, http.MethodGet, inst(ref, ""), nil, &out)
}

// List implements adapter.ExecutionAdapter.
func (c *Client) List(ctx context.Context, installation string) ([]adapter.Instance, error) {
	var out []adapter.Instance
	return out, c.do(ctx, http.MethodGet, "/adapter/v1/instances?installation="+url.QueryEscape(installation), nil, &out)
}

// Delete implements adapter.ExecutionAdapter.
func (c *Client) Delete(ctx context.Context, ref string) error {
	err := c.do(ctx, http.MethodDelete, inst(ref, ""), nil, nil)
	if api.IsCode(err, api.CodeNotFound) {
		return nil
	}
	return err
}

// Exec implements adapter.ExecutionAdapter.
func (c *Client) Exec(ctx context.Context, ref string, req api.ExecRequest) (api.ExecResult, error) {
	var out api.ExecResult
	return out, c.do(ctx, http.MethodPost, inst(ref, "/exec"), req, &out)
}

// ExecStream implements adapter.Streamer.
func (c *Client) ExecStream(ctx context.Context, ref string, req api.ExecRequest, emit func(api.ExecEvent) error) (int, error) {
	body, _ := json.Marshal(req)
	resp, err := c.send(ctx, http.MethodPost, inst(ref, "/exec?stream=1"), bytes.NewReader(body), "application/json")
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		var ev api.ExecEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			return -1, fmt.Errorf("decode stream: %w", err)
		}
		switch {
		case ev.Error != "":
			return -1, errors.New(ev.Error)
		case ev.ExitCode != nil:
			return *ev.ExitCode, nil
		}
		if err := emit(ev); err != nil {
			return -1, err
		}
	}
	if err := sc.Err(); err != nil {
		return -1, err
	}
	return -1, errors.New("stream ended without exit code")
}

// ReadFile implements adapter.FileSystem.
func (c *Client) ReadFile(ctx context.Context, ref, path string) ([]byte, error) {
	resp, err := c.send(ctx, http.MethodGet, inst(ref, "/files/content?path="+url.QueryEscape(path)), nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, MaxFileBytes))
}

// WriteFile implements adapter.FileSystem.
func (c *Client) WriteFile(ctx context.Context, ref, path string, data []byte) error {
	resp, err := c.send(ctx, http.MethodPut, inst(ref, "/files/content?path="+url.QueryEscape(path)), bytes.NewReader(data), "application/octet-stream")
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// ListFiles implements adapter.FileSystem.
func (c *Client) ListFiles(ctx context.Context, ref, path string) ([]api.FileInfo, error) {
	var out []api.FileInfo
	return out, c.do(ctx, http.MethodGet, inst(ref, "/files?path="+url.QueryEscape(path)), nil, &out)
}

// Stop implements adapter.Lifecycle.
func (c *Client) Stop(ctx context.Context, ref string) error {
	return c.do(ctx, http.MethodPost, inst(ref, "/stop"), nil, nil)
}

// Start implements adapter.Lifecycle.
func (c *Client) Start(ctx context.Context, ref string) error {
	return c.do(ctx, http.MethodPost, inst(ref, "/start"), nil, nil)
}

// Pause implements adapter.Pauser.
func (c *Client) Pause(ctx context.Context, ref string) error {
	return c.do(ctx, http.MethodPost, inst(ref, "/pause"), nil, nil)
}

// Resume implements adapter.Pauser.
func (c *Client) Resume(ctx context.Context, ref string) error {
	return c.do(ctx, http.MethodPost, inst(ref, "/resume"), nil, nil)
}

// Snapshot implements adapter.Snapshotter.
func (c *Client) Snapshot(ctx context.Context, ref, kind string) (adapter.Snapshot, error) {
	var out adapter.Snapshot
	return out, c.do(ctx, http.MethodPost, inst(ref, "/snapshots"), snapshotRequest{Kind: kind}, &out)
}

// DeleteSnapshot implements adapter.Snapshotter.
func (c *Client) DeleteSnapshot(ctx context.Context, ref string) error {
	return c.do(ctx, http.MethodDelete, "/adapter/v1/snapshots/"+url.PathEscape(ref), nil, nil)
}

// ExposePort implements adapter.PortExposer.
func (c *Client) ExposePort(ctx context.Context, ref string, port int) (string, error) {
	var out portResponse
	return out.URL, c.do(ctx, http.MethodPost, inst(ref, "/ports"), portRequest{Port: port}, &out)
}

func inst(ref, suffix string) string { return "/adapter/v1/instances/" + url.PathEscape(ref) + suffix }

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ct = bytes.NewReader(b), "application/json"
	}
	resp, err := c.send(ctx, method, path, body, ct)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) send(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, adapter.Unavailable(err)
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		var env api.ErrorEnvelope
		if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&env) == nil && env.Error != nil {
			return nil, env.Error
		}
		return nil, adapter.Unavailable(fmt.Errorf("%s %s: HTTP %s", method, path, strconv.Itoa(resp.StatusCode)))
	}
	return resp, nil
}

var (
	_ adapter.ExecutionAdapter = (*Client)(nil)
	_ adapter.Streamer         = (*Client)(nil)
	_ adapter.FileSystem       = (*Client)(nil)
	_ adapter.Lifecycle        = (*Client)(nil)
	_ adapter.Pauser           = (*Client)(nil)
	_ adapter.Snapshotter      = (*Client)(nil)
	_ adapter.PortExposer      = (*Client)(nil)
)
