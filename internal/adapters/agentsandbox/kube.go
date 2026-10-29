package agentsandbox

// A small Kubernetes API client on the standard library: JSON requests with
// a bearer token, and pod exec over WebSocket (v5.channel.k8s.io, falling
// back to v4.channel.k8s.io). It replaces client-go for the handful of calls
// the adapter makes.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/different-ai/differently/internal/adapters/httpx"
	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// In-cluster service account files, read when the pool sets no apiServer.
const (
	serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"
	inClusterToken    = serviceAccountDir + "/token"
	inClusterCA       = serviceAccountDir + "/ca.crt"
)

// kube calls one Kubernetes API server.
type kube struct {
	c  *httpx.Client // JSON calls; the token is set per request
	ws *http.Client  // exec upgrades, without a client timeout: it would make a 101 body read-only

	token     string // a fixed token (the pool's credential), or
	tokenFile string // a projected token re-read as it rotates

	mu       sync.Mutex
	cached   string
	cachedAt time.Time
}

// newKube builds a client for server. caFile and tokenFile may be empty.
func newKube(server, caFile, token, tokenFile string) (*kube, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("agent-sandbox: read config.caFile: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("agent-sandbox: config.caFile %s holds no PEM certificate", caFile)
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	c := httpx.New("agent-sandbox", server, "")
	c.HTTP.Transport = tr
	ws := tr.Clone()
	ws.ForceAttemptHTTP2 = false
	ws.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{} // WebSocket needs HTTP/1.1
	return &kube{c: c, ws: &http.Client{Transport: acceptAny{ws}}, token: token, tokenFile: tokenFile}, nil
}

// acceptAny replaces the JSON Accept header httpx sets: the API server
// answers an exec upgrade that only accepts JSON with 406.
type acceptAny struct{ next http.RoundTripper }

func (t acceptAny) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Accept", "*/*")
	return t.next.RoundTrip(r)
}

// auth returns the Authorization header. A projected service account token
// rotates, so the file is re-read every minute.
func (k *kube) auth() (http.Header, error) {
	h := http.Header{}
	tok := k.token
	if tok == "" && k.tokenFile != "" {
		k.mu.Lock()
		if k.cached == "" || time.Since(k.cachedAt) > time.Minute {
			b, err := os.ReadFile(k.tokenFile)
			if err != nil {
				k.mu.Unlock()
				return nil, adapter.Unavailable(fmt.Errorf("agent-sandbox: read the service account token: %w", err))
			}
			k.cached, k.cachedAt = strings.TrimSpace(string(b)), time.Now()
		}
		tok = k.cached
		k.mu.Unlock()
	}
	if tok != "" {
		h.Set("Authorization", "Bearer "+tok)
	}
	return h, nil
}

// do sends a JSON request; contentType defaults to application/json.
func (k *kube) do(ctx context.Context, method, path, contentType string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
		if contentType == "" {
			contentType = "application/json"
		}
	}
	h, err := k.auth()
	if err != nil {
		return err
	}
	resp, err := k.c.Do(ctx, method, path, body, contentType, h)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out)
}

// Exec channels (k8s.io/apimachinery/pkg/util/remotecommand).
const (
	chStdin  = 0
	chStdout = 1
	chStderr = 2
	chStatus = 3
	chClose  = 255 // v5: close the stream whose number follows
)

// execStatus is the metav1.Status the API server sends on the status
// channel when the command ends.
type execStatus struct {
	Status  string `json:"status"`
	Message string `json:"message"`
	Reason  string `json:"reason"`
	Details struct {
		Causes []struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"causes"`
	} `json:"details"`
}

// exec runs argv in a pod's container, sends stdin and passes output to
// emit as it arrives. It returns the command's exit code. Without the v5
// protocol (Kubernetes before 1.30) stdin cannot be closed, so stdin needs v5.
func (k *kube) exec(ctx context.Context, ns, pod, container string, argv []string, stdin io.Reader, emit func(stream string, data []byte) error) (int, error) {
	q := url.Values{"stdout": {"true"}, "stderr": {"true"}}
	if container != "" {
		q.Set("container", container)
	}
	if stdin != nil {
		q.Set("stdin", "true")
	}
	for _, a := range argv {
		q.Add("command", a)
	}
	h, err := k.auth()
	if err != nil {
		return -1, err
	}
	key := make([]byte, 16)
	_, _ = rand.Read(key)
	wsKey := base64.StdEncoding.EncodeToString(key)
	h.Set("Connection", "Upgrade")
	h.Set("Upgrade", "websocket")
	h.Set("Sec-WebSocket-Version", "13")
	h.Set("Sec-WebSocket-Key", wsKey)
	h.Set("Sec-WebSocket-Protocol", "v5.channel.k8s.io, v4.channel.k8s.io")
	up := &httpx.Client{BaseURL: k.c.BaseURL, HTTP: k.ws, Provider: k.c.Provider}
	resp, err := up.Do(ctx, http.MethodGet, "/api/v1/namespaces/"+url.PathEscape(ns)+"/pods/"+url.PathEscape(pod)+"/exec?"+q.Encode(), nil, "", h)
	if err != nil {
		return -1, err
	}
	conn, ok := resp.Body.(io.ReadWriteCloser)
	if resp.StatusCode != http.StatusSwitchingProtocols || !ok {
		resp.Body.Close()
		return -1, adapter.Unavailable(fmt.Errorf("agent-sandbox: exec was not upgraded to a WebSocket (HTTP %d)", resp.StatusCode))
	}
	defer conn.Close()
	if resp.Header.Get("Sec-WebSocket-Accept") != acceptKey(wsKey) {
		return -1, adapter.Unavailable(errors.New("agent-sandbox: exec upgrade answered with a bad Sec-WebSocket-Accept"))
	}
	proto := resp.Header.Get("Sec-WebSocket-Protocol")
	if stdin != nil && proto != "v5.channel.k8s.io" {
		return -1, api.Errorf(api.CodeUnsupportedOperation, "agent-sandbox: stdin needs the v5.channel.k8s.io exec protocol (Kubernetes 1.30 or later); the API server offered %q", proto).With("operation", "exec.stdin")
	}
	// Cancelling the context closes the connection, which ends the read loop.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	ws := &wsConn{rw: conn, r: bufio.NewReaderSize(conn, 64<<10)}
	if stdin != nil {
		go func() {
			buf := make([]byte, 32<<10)
			for {
				n, err := stdin.Read(buf)
				if n > 0 {
					if ws.write(append([]byte{chStdin}, buf[:n]...)) != nil {
						return
					}
				}
				if err != nil {
					_ = ws.write([]byte{chClose, chStdin})
					return
				}
			}
		}()
	}
	for {
		msg, err := ws.read()
		if err != nil {
			if ctx.Err() != nil {
				return -1, ctx.Err()
			}
			return -1, adapter.Unavailable(fmt.Errorf("agent-sandbox: exec stream ended without a status: %w", err))
		}
		if len(msg) < 2 {
			continue // an empty frame opens a channel
		}
		switch msg[0] {
		case chStdout, chStderr:
			stream := "stdout"
			if msg[0] == chStderr {
				stream = "stderr"
			}
			if err := emit(stream, msg[1:]); err != nil {
				return -1, err
			}
		case chStatus:
			var st execStatus
			if err := json.Unmarshal(msg[1:], &st); err != nil {
				return -1, fmt.Errorf("agent-sandbox: decode exec status: %w", err)
			}
			return st.exitCode()
		}
	}
}

func (st execStatus) exitCode() (int, error) {
	if st.Status == "Success" {
		return 0, nil
	}
	if st.Reason == "NonZeroExitCode" {
		for _, c := range st.Details.Causes {
			if c.Reason == "ExitCode" {
				if n, err := strconv.Atoi(c.Message); err == nil {
					return n, nil
				}
			}
		}
	}
	// The runtime could not start the command: report it as a shell would.
	switch m := strings.ToLower(st.Message); {
	case strings.Contains(m, "executable file not found") || strings.Contains(m, "no such file or directory"):
		return 127, nil
	case strings.Contains(m, "permission denied"):
		return 126, nil
	}
	return -1, api.Errorf(api.CodeInvalidRequest, "agent-sandbox: exec failed: %s", st.Message)
}

func acceptKey(key string) string {
	h := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h[:])
}

// wsConn is the client side of RFC 6455 binary messaging: frames sent are
// masked, control frames are answered, fragments are joined.
type wsConn struct {
	rw io.ReadWriter
	r  *bufio.Reader
	mu sync.Mutex // one writer at a time
}

const maxMessage = 16 << 20

func (c *wsConn) write(payload []byte) error { return c.frame(0x2, payload) }

func (c *wsConn) frame(opcode byte, payload []byte) error {
	hdr := []byte{0x80 | opcode, 0}
	switch n := len(payload); {
	case n < 126:
		hdr[1] = byte(n)
	case n <= 0xFFFF:
		hdr[1] = 126
		hdr = binary.BigEndian.AppendUint16(hdr, uint16(n))
	default:
		hdr[1] = 127
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	hdr[1] |= 0x80 // clients mask every frame
	mask := make([]byte, 4)
	_, _ = rand.Read(mask)
	out := make([]byte, 0, len(hdr)+4+len(payload))
	out = append(append(out, hdr...), mask...)
	for i, b := range payload {
		out = append(out, b^mask[i%4])
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, err := c.rw.Write(out)
	return err
}

// read returns the next data message.
func (c *wsConn) read() ([]byte, error) {
	var msg []byte
	for {
		var h [2]byte
		if _, err := io.ReadFull(c.r, h[:]); err != nil {
			return nil, err
		}
		fin, opcode := h[0]&0x80 != 0, h[0]&0x0F
		n := uint64(h[1] & 0x7F)
		switch n {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(c.r, b[:]); err != nil {
				return nil, err
			}
			n = uint64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(c.r, b[:]); err != nil {
				return nil, err
			}
			n = binary.BigEndian.Uint64(b[:])
		}
		var mask []byte
		if h[1]&0x80 != 0 {
			mask = make([]byte, 4)
			if _, err := io.ReadFull(c.r, mask); err != nil {
				return nil, err
			}
		}
		if n > maxMessage || uint64(len(msg))+n > maxMessage {
			return nil, fmt.Errorf("websocket message over %d bytes", maxMessage)
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(c.r, p); err != nil {
			return nil, err
		}
		for i := range p {
			if mask != nil {
				p[i] ^= mask[i%4]
			}
		}
		switch opcode {
		case 0x8: // close
			return nil, io.EOF
		case 0x9: // ping
			if err := c.frame(0xA, p); err != nil {
				return nil, err
			}
			continue
		case 0xA: // pong
			continue
		}
		msg = append(msg, p...)
		if fin {
			return msg, nil
		}
	}
}
