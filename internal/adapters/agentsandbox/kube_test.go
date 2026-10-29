package agentsandbox

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
)

// wsServer is the server side of RFC 6455 for tests: it reads masked
// client frames and writes unmasked ones.
type wsServer struct {
	conn net.Conn
	r    *bufio.Reader
}

// upgrade answers a pods/exec upgrade with the first offered protocol that
// is in allow.
func upgrade(t *testing.T, w http.ResponseWriter, r *http.Request, allow ...string) (*wsServer, string) {
	t.Helper()
	proto := ""
	for _, p := range strings.Split(r.Header.Get("Sec-WebSocket-Protocol"), ",") {
		for _, a := range allow {
			if strings.TrimSpace(p) == a && proto == "" {
				proto = a
			}
		}
	}
	conn, rw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Fatal(err)
	}
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + acceptKey(r.Header.Get("Sec-WebSocket-Key")) + "\r\n" +
		"Sec-WebSocket-Protocol: " + proto + "\r\n\r\n"
	if _, err := rw.WriteString(resp); err != nil {
		t.Fatal(err)
	}
	if err := rw.Flush(); err != nil {
		t.Fatal(err)
	}
	return &wsServer{conn: conn, r: rw.Reader}, proto
}

func (s *wsServer) send(ch byte, data []byte) {
	p := append([]byte{ch}, data...)
	hdr := []byte{0x82, 0}
	switch n := len(p); {
	case n < 126:
		hdr[1] = byte(n)
	case n <= 0xFFFF:
		hdr[1] = 126
		hdr = binary.BigEndian.AppendUint16(hdr, uint16(n))
	default:
		hdr[1] = 127
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	_, _ = s.conn.Write(append(hdr, p...))
}

func (s *wsServer) status(v any) {
	b, _ := json.Marshal(v)
	s.send(chStatus, b)
}

// recv reads one client message, unmasking it.
func (s *wsServer) recv() ([]byte, error) {
	c := &wsConn{rw: s.conn, r: s.r}
	return c.read()
}

// stdin collects channel-0 data until the v5 close message for stdin.
func (s *wsServer) stdin() (string, error) {
	var b strings.Builder
	for {
		msg, err := s.recv()
		if err != nil {
			return b.String(), err
		}
		switch {
		case len(msg) == 2 && msg[0] == chClose && msg[1] == chStdin:
			return b.String(), nil
		case len(msg) > 0 && msg[0] == chStdin:
			b.Write(msg[1:])
		}
	}
}

func exitStatus(code string) map[string]any {
	return map[string]any{"status": "Failure", "reason": "NonZeroExitCode", "message": "command terminated with non-zero exit code",
		"details": map[string]any{"causes": []any{map[string]string{"reason": "ExitCode", "message": code}}}}
}

func TestWebSocketFraming(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	client := &wsConn{rw: a, r: bufio.NewReader(a)}
	server := &wsServer{conn: b, r: bufio.NewReader(b)}
	big := strings.Repeat("x", 70000) // a 64-bit length
	go func() {
		_ = client.write([]byte("small"))
		_ = client.write([]byte(strings.Repeat("y", 300))) // a 16-bit length
		_ = client.write([]byte(big))
	}()
	for _, want := range []string{"small", strings.Repeat("y", 300), big} {
		got, err := server.recv()
		if err != nil || string(got) != want {
			t.Fatalf("got %d bytes %v, want %d", len(got), err, len(want))
		}
	}
	// A ping is answered with a masked pong and skipped; fragments are joined.
	go func() {
		_, _ = b.Write([]byte{0x89, 2, 'h', 'i'})      // ping
		_, _ = b.Write([]byte{0x02, 3, 'a', 'b', 'c'}) // first fragment
		_, _ = b.Write([]byte{0x80, 3, 'd', 'e', 'f'}) // final continuation
		_, _ = b.Write([]byte{0x88, 0})                // close
	}()
	type result struct {
		msg string
		err error
	}
	got := make(chan result, 2)
	go func() {
		m, err := client.read()
		got <- result{string(m), err}
		_, err = client.read()
		got <- result{"", err}
	}()
	pong := make([]byte, 8) // header, mask, "hi"
	if _, err := io.ReadFull(server.r, pong); err != nil {
		t.Fatal(err)
	}
	if pong[0] != 0x8A || pong[1] != 0x80|2 || pong[6]^pong[2] != 'h' || pong[7]^pong[3] != 'i' {
		t.Fatalf("pong frame % x", pong)
	}
	if r := <-got; r.err != nil || r.msg != "abcdef" {
		t.Fatalf("fragments: %q %v", r.msg, r.err)
	}
	if r := <-got; r.err != io.EOF {
		t.Fatalf("close: %v", r.err)
	}
}
