//go:build experimental_adapters

package groundbox

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// Groundbox streams a command's output, and takes its stdin, over a
// WebSocket: GET /sandboxes/{id}/exec/stream. The client sends "start", then
// optional "stdin" and "stdin_end"; the server sends "stdout" and "stderr"
// chunks, then one "exit" or "error" (with one of the API's error codes). A
// command started with resumable: true runs on when the connection drops:
// the server names it in "started", numbers its output with seq, and a new
// connection sends "attach" to read on from the last seq. Closing the socket
// with 1000 kills the command.
//
// The standard library has no WebSocket client, so this file holds a minimal
// RFC 6455 one: net/http performs the HTTP/1.1 upgrade and hands back the
// connection, and JSON text messages are framed over it.

const (
	opText  = 1
	opClose = 8
	opPing  = 9
	opPong  = 10

	maxMessage = 16 << 20
	wsGUID     = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	// stdinChunk keeps each stdin message well under the server's frame limit.
	stdinChunk = 256 << 10
	// reattaches is how many dropped connections one resumable command
	// survives.
	reattaches = 5
)

type wsConn struct {
	rw   io.ReadWriteCloser
	r    *bufio.Reader
	wmu  sync.Mutex
	done chan struct{}
	once sync.Once
}

// upgradeError is a refused upgrade.
type upgradeError struct{ status int }

func (e *upgradeError) Error() string { return fmt.Sprintf("websocket upgrade refused: %d", e.status) }

// dialWS upgrades a GET to rawURL (http:// or https://) into a WebSocket.
// Cancelling ctx closes the connection with 1000, which ends the command.
func dialWS(ctx context.Context, hc *http.Client, rawURL, token string) (*wsConn, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, 16)
	rand.Read(nonce)
	key := base64.StdEncoding.EncodeToString(nonce)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", key)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		resp.Body.Close()
		return nil, &upgradeError{status: resp.StatusCode}
	}
	sum := sha1.Sum([]byte(key + wsGUID))
	rw, ok := resp.Body.(io.ReadWriteCloser)
	if !ok || resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) {
		resp.Body.Close()
		return nil, errors.New("websocket: the server did not complete the upgrade")
	}
	c := &wsConn{rw: rw, r: bufio.NewReader(rw), done: make(chan struct{})}
	go func() {
		select {
		case <-ctx.Done():
			c.close()
		case <-c.done:
		}
	}()
	return c, nil
}

// write sends one masked frame, as clients must.
func (c *wsConn) write(op byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	hdr := []byte{0x80 | op}
	switch n := len(payload); {
	case n < 126:
		hdr = append(hdr, 0x80|byte(n))
	case n <= 0xffff:
		hdr = append(hdr, 0x80|126)
		hdr = binary.BigEndian.AppendUint16(hdr, uint16(n))
	default:
		hdr = append(hdr, 0x80|127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	mask := make([]byte, 4)
	rand.Read(mask)
	frame := append(hdr, mask...)
	for i, b := range payload {
		frame = append(frame, b^mask[i%4])
	}
	_, err := c.rw.Write(frame)
	return err
}

func (c *wsConn) writeJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.write(opText, b)
}

// read returns the next message, answering pings and joining fragments. A
// close frame ends the stream with io.EOF.
func (c *wsConn) read() ([]byte, error) {
	var msg []byte
	for {
		var h [2]byte
		if _, err := io.ReadFull(c.r, h[:]); err != nil {
			return nil, err
		}
		fin, op := h[0]&0x80 != 0, h[0]&0x0f
		n := uint64(h[1] & 0x7f)
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
		var mask [4]byte
		masked := h[1]&0x80 != 0
		if masked {
			if _, err := io.ReadFull(c.r, mask[:]); err != nil {
				return nil, err
			}
		}
		if n > maxMessage || uint64(len(msg))+n > maxMessage {
			return nil, errors.New("websocket: message too large")
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(c.r, payload); err != nil {
			return nil, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
		}
		switch op {
		case opClose:
			return nil, io.EOF
		case opPing:
			if err := c.write(opPong, payload); err != nil {
				return nil, err
			}
			continue
		case opPong:
			continue
		}
		msg = append(msg, payload...)
		if fin {
			return msg, nil
		}
	}
}

// close ends the connection with 1000 (normal closure).
func (c *wsConn) close() {
	c.once.Do(func() {
		c.write(opClose, []byte{0x03, 0xe8})
		c.rw.Close()
		close(c.done)
	})
}

// drop ends the connection without a close frame, so a resumable command is
// not killed by it.
func (c *wsConn) drop() {
	c.once.Do(func() {
		c.rw.Close()
		close(c.done)
	})
}

type streamMessage struct {
	Type     string `json:"type"`
	Data     string `json:"data"`
	Seq      *int64 `json:"seq"`
	ExecID   string `json:"exec_id"`
	ExitCode int    `json:"exit_code"`
	TimedOut bool   `json:"timed_out"`
	Message  string `json:"message"`
	Code     string `json:"code"`
}

func (a *Adapter) dialExec(ctx context.Context, ref string) (*wsConn, error) {
	conn, err := dialWS(ctx, a.ws, a.opts.APIURL+a.path(ref)+"/exec/stream", a.opts.APIKey)
	if err == nil {
		return conn, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var refused *upgradeError
	if errors.As(err, &refused) {
		// The upgrade passes the same checks as any request: read the
		// sandbox to learn why it was refused (unknown, rejected key).
		if _, gerr := a.get(ctx, ref); gerr != nil {
			return nil, gerr
		}
	}
	return nil, adapter.Unavailable(fmt.Errorf("groundbox: exec stream: %w", err))
}

// streamError is the error a stream ended with, classed by its code as the
// same failure would be in an HTTP answer.
func streamError(ref string, m streamMessage) error {
	msg := "groundbox: " + strings.TrimSpace(m.Message)
	switch m.Code {
	case "conflict": // the sandbox is paused, stopped or over
		return api.Errorf(api.CodeInvalidState, "%s", msg)
	case "not_found":
		return api.Errorf(api.CodeNotFound, "%s", msg)
	case "bad_request":
		return api.Errorf(api.CodeInvalidRequest, "%s", msg)
	case "concurrency_limit", "rate_limited", "no_capacity":
		return adapter.InsufficientCapacity(msg)
	case "":
		return adapter.SandboxFailed(fmt.Errorf("groundbox: exec failed in sandbox %s: %s", ref, strings.TrimSpace(m.Message)))
	}
	return api.Errorf(api.CodeProviderUnavailable, "%s (%s)", msg, m.Code)
}

// ExecStream runs a command and relays its output as it is produced. The
// command is resumable: a dropped connection is picked up where it left
// off. Stdin sent on a connection that dropped may not have arrived.
func (a *Adapter) ExecStream(ctx context.Context, ref string, req api.ExecRequest, emit func(api.ExecEvent) error) (int, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // closes the socket with 1000: a command still running is killed
	conn, err := a.dialExec(ctx, ref)
	if err != nil {
		return -1, err
	}
	start := execBody(req, maxStreamSeconds)
	start["type"], start["stdin"], start["resumable"] = "start", req.Stdin != "", true
	if err := conn.writeJSON(start); err != nil {
		return -1, adapter.Unavailable(fmt.Errorf("groundbox: exec stream: %w", err))
	}
	if req.Stdin != "" {
		// Sent while the output is read below, so neither side waits on the
		// other. As base64, so that bytes that are not text arrive as written.
		// On the connection the command started on: a re-attach replaces conn,
		// and stdin does not follow it.
		go func(conn *wsConn) {
			for in := []byte(req.Stdin); len(in) > 0; {
				n := min(len(in), stdinChunk)
				if conn.writeJSON(map[string]string{"type": "stdin", "data_base64": base64.StdEncoding.EncodeToString(in[:n])}) != nil {
					return
				}
				in = in[n:]
			}
			conn.writeJSON(map[string]string{"type": "stdin_end"})
		}(conn)
	}
	var execID string
	var seq int64
	for dropped := 0; ; {
		raw, err := conn.read()
		if err != nil {
			if ctx.Err() != nil {
				return -1, ctx.Err()
			}
			if execID == "" || dropped == reattaches {
				return -1, adapter.Unavailable(errors.New("groundbox: exec stream ended without an exit code"))
			}
			// The command runs on in the sandbox: read on from the last
			// output seen.
			dropped++
			conn.drop()
			select {
			case <-ctx.Done():
				return -1, ctx.Err()
			case <-time.After(time.Duration(dropped) * a.opts.PollInterval):
			}
			if conn, err = a.dialExec(ctx, ref); err != nil {
				return -1, err
			}
			if err := conn.writeJSON(map[string]any{"type": "attach", "exec_id": execID, "seq": seq}); err != nil {
				return -1, adapter.Unavailable(fmt.Errorf("groundbox: exec stream: %w", err))
			}
			continue
		}
		var m streamMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return -1, adapter.Unavailable(fmt.Errorf("groundbox: unreadable exec message: %w", err))
		}
		if m.Seq != nil {
			seq = *m.Seq
		}
		switch m.Type {
		case "started":
			execID = m.ExecID
		case "stdout", "stderr":
			if err := emit(api.ExecEvent{Stream: m.Type, Data: m.Data}); err != nil {
				return -1, err
			}
		case "exit":
			return exitCode(m.ExitCode, m.TimedOut), nil
		case "error":
			return -1, streamError(ref, m)
		}
	}
}
