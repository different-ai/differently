package northflank

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

// A minimal RFC 6455 client: Northflank runs commands only over a
// WebSocket, and the standard library has no WebSocket client. net/http
// performs the HTTP/1.1 upgrade and hands back the connection; this file
// frames JSON text messages over it.

const (
	opText   = 1
	opBinary = 2
	opClose  = 8
	opPing   = 9
	opPong   = 10

	maxMessage = 16 << 20
	wsGUID     = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
)

type wsConn struct {
	rw   io.ReadWriteCloser
	r    *bufio.Reader
	wmu  sync.Mutex
	done chan struct{}
	once sync.Once
}

// dialWS upgrades a GET to rawURL (ws:// or wss://) into a WebSocket.
func dialWS(ctx context.Context, hc *http.Client, rawURL string) (*wsConn, error) {
	u := strings.Replace(strings.Replace(rawURL, "wss://", "https://", 1), "ws://", "http://", 1)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
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
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, &upgradeError{status: resp.StatusCode, header: resp.Header, body: raw}
	}
	sum := sha1.Sum([]byte(key + wsGUID))
	if resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) {
		resp.Body.Close()
		return nil, errors.New("websocket: bad Sec-WebSocket-Accept")
	}
	rw, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		resp.Body.Close()
		return nil, errors.New("websocket: the connection cannot be upgraded")
	}
	c := &wsConn{rw: rw, r: bufio.NewReader(rw), done: make(chan struct{})}
	go func() {
		// Cancelling the context unblocks a pending read.
		select {
		case <-ctx.Done():
			rw.Close()
		case <-c.done:
		}
	}()
	return c, nil
}

// upgradeError is a refused upgrade, kept so the caller can classify it.
type upgradeError struct {
	status int
	header http.Header
	body   []byte
}

func (e *upgradeError) Error() string { return fmt.Sprintf("websocket upgrade refused: %d", e.status) }

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
	hdr = append(hdr, mask...)
	body := make([]byte, len(payload))
	for i, b := range payload {
		body[i] = b ^ mask[i%4]
	}
	_, err := c.rw.Write(append(hdr, body...))
	return err
}

func (c *wsConn) writeText(b []byte) error { return c.write(opText, b) }

// read returns the next text or binary message, answering pings and
// joining fragments. A close frame ends the stream with io.EOF.
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
		var mask []byte
		if h[1]&0x80 != 0 {
			mask = make([]byte, 4)
			if _, err := io.ReadFull(c.r, mask); err != nil {
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
		for i := range payload {
			if mask != nil {
				payload[i] ^= mask[i%4]
			}
		}
		switch op {
		case opClose:
			c.write(opClose, nil)
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

func (c *wsConn) close() {
	c.once.Do(func() {
		c.write(opClose, []byte{0x03, 0xe8}) // 1000: normal closure
		c.rw.Close()
		close(c.done)
	})
}
