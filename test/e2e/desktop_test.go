//go:build e2e

package e2e

import (
	"bufio"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/different-ai/differently/pkg/api"
)

// desktopImage is the template of a desktop serving VNC over a WebSocket on
// port 6080, such as test/e2e/desktop (DIFFERENTLY_E2E_DESKTOP_IMAGE): an
// image on container runtimes, a Fast Sandbox template ID built from it on
// microVM pools. Without it the desktop test is skipped.
var desktopImage = os.Getenv("DIFFERENTLY_E2E_DESKTOP_IMAGE")

// A full desktop streams from a sandbox through its signed port URL: a real
// VNC handshake over wss, one framebuffer update saved as a PNG
// (DIFFERENTLY_E2E_ARTIFACTS, default the test's temp dir), and, where the
// pool pauses, the same desktop session streams again after pause/resume.
func TestDesktopStreams(t *testing.T) {
	if desktopImage == "" {
		t.Skip("set DIFFERENTLY_E2E_DESKTOP_IMAGE to a desktop image serving VNC over a WebSocket on port 6080")
	}
	if !advertises(t, "ports.expose") {
		t.Skip("the pool does not expose ports")
	}
	c := tenant(t, "desktop", api.Quota{})
	boot := time.Now()
	sb := create(t, c, api.CreateSandboxRequest{Template: desktopImage, Profile: "medium"})
	sh(t, c, sb.ID, "/usr/local/bin/start-desktop")
	port, err := c.ExposePort(ctx(t), sb.ID, 6080)
	if err != nil {
		t.Fatal(err)
	}
	var first desktopFrame
	eventually(t, 2*time.Minute, "a framebuffer update over the port URL", func() bool {
		first, err = vncFrame(port.URL)
		return err == nil
	})
	t.Logf("first frame %dx%d from %q %s after create", first.img.Bounds().Dx(), first.img.Bounds().Dy(), first.name, time.Since(boot).Round(10*time.Millisecond))
	saveFrame(t, "desktop-first-frame.png", first.img)
	xvfb := strings.TrimSpace(sh(t, c, sb.ID, "pgrep -x Xvfb").Stdout)

	if !advertises(t, "lifecycle.pause-resume") {
		return
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
	if port, err = c.ExposePort(ctx(t), sb.ID, 6080); err != nil {
		t.Fatal(err)
	}
	var after desktopFrame
	eventually(t, time.Minute, "a framebuffer update after resume", func() bool {
		after, err = vncFrame(port.URL)
		return err == nil
	})
	t.Logf("pause %s; resume to first frame %s", paused.Round(10*time.Millisecond), time.Since(start).Round(10*time.Millisecond))
	saveFrame(t, "desktop-after-resume.png", after.img)
	if got := strings.TrimSpace(sh(t, c, sb.ID, "pgrep -x Xvfb").Stdout); got != xvfb {
		t.Fatalf("the X server was restarted across pause/resume: pid %q, now %q", xvfb, got)
	}
}

type desktopFrame struct {
	name string
	img  *image.RGBA
}

func saveFrame(t *testing.T, name string, img image.Image) {
	t.Helper()
	dir := env("DIFFERENTLY_E2E_ARTIFACTS", t.TempDir())
	_ = os.MkdirAll(dir, 0o755)
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
	t.Logf("saved %s", f.Name())
}

// vncFrame opens a WebSocket at the port URL's /vnc/e2e path, completes an
// RFB 3.8 handshake with no authentication and returns one full raw
// framebuffer update.
func vncFrame(portURL string) (desktopFrame, error) {
	u, err := url.Parse(strings.TrimRight(portURL, "/") + "/vnc/e2e")
	if err != nil {
		return desktopFrame{}, err
	}
	ws, err := dialWS(u)
	if err != nil {
		return desktopFrame{}, err
	}
	defer ws.Close()
	_ = ws.conn.SetDeadline(time.Now().Add(30 * time.Second))
	version := make([]byte, 12)
	if _, err := io.ReadFull(ws, version); err != nil {
		return desktopFrame{}, err
	}
	if string(version) != "RFB 003.008\n" {
		return desktopFrame{}, fmt.Errorf("server version %q", version)
	}
	ws.Write([]byte("RFB 003.008\n"))
	var n [1]byte
	if _, err := io.ReadFull(ws, n[:]); err != nil || n[0] == 0 {
		return desktopFrame{}, fmt.Errorf("no security types: %v", err)
	}
	types := make([]byte, n[0])
	io.ReadFull(ws, types)
	if !strings.ContainsRune(string(types), 1) {
		return desktopFrame{}, fmt.Errorf("security types %v offer no None", types)
	}
	ws.Write([]byte{1})
	var result uint32
	if err := binary.Read(ws, binary.BigEndian, &result); err != nil || result != 0 {
		return desktopFrame{}, fmt.Errorf("security result %d: %v", result, err)
	}
	ws.Write([]byte{1}) // ClientInit, shared
	var init struct {
		W, H uint16
		PF   [16]byte
		N    uint32
	}
	if err := binary.Read(ws, binary.BigEndian, &init); err != nil {
		return desktopFrame{}, err
	}
	name := make([]byte, init.N)
	io.ReadFull(ws, name)
	// SetPixelFormat: 32 bpp, depth 24, little endian, true colour, RGB at 16/8/0.
	ws.Write([]byte{0, 0, 0, 0, 32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0})
	ws.Write([]byte{2, 0, 0, 1, 0, 0, 0, 0}) // SetEncodings: raw
	req := []byte{3, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(req[6:], init.W)
	binary.BigEndian.PutUint16(req[8:], init.H)
	ws.Write(req)
	img := image.NewRGBA(image.Rect(0, 0, int(init.W), int(init.H)))
	for {
		var head [4]byte
		if _, err := io.ReadFull(ws, head[:]); err != nil {
			return desktopFrame{}, err
		}
		switch head[0] {
		case 0: // FramebufferUpdate
		case 2: // Bell
			continue
		default:
			return desktopFrame{}, fmt.Errorf("unexpected server message %d", head[0])
		}
		for i := 0; i < int(binary.BigEndian.Uint16(head[2:])); i++ {
			var r struct {
				X, Y, W, H uint16
				Enc        int32
			}
			if err := binary.Read(ws, binary.BigEndian, &r); err != nil {
				return desktopFrame{}, err
			}
			if r.Enc != 0 {
				return desktopFrame{}, fmt.Errorf("rectangle encoding %d, asked for raw", r.Enc)
			}
			px := make([]byte, int(r.W)*int(r.H)*4)
			if _, err := io.ReadFull(ws, px); err != nil {
				return desktopFrame{}, err
			}
			for j := 0; j < len(px); j += 4 {
				p := j / 4
				img.Set(int(r.X)+p%int(r.W), int(r.Y)+p/int(r.W), color.RGBA{px[j+2], px[j+1], px[j], 255})
			}
		}
		return desktopFrame{name: string(name), img: img}, nil
	}
}

// wsConn is a minimal client WebSocket: binary frames out (masked), message
// payloads in as a byte stream. The standard library has no WebSocket client.
type wsConn struct {
	conn net.Conn
	r    *bufio.Reader
	left int64
}

func dialWS(u *url.URL) (*wsConn, error) {
	host := u.Host
	var conn net.Conn
	var err error
	switch u.Scheme {
	case "https", "wss":
		if u.Port() == "" {
			host += ":443"
		}
		conn, err = tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", host, &tls.Config{ServerName: u.Hostname(), NextProtos: []string{"http/1.1"}})
	default:
		if u.Port() == "" {
			host += ":80"
		}
		conn, err = net.DialTimeout("tcp", host, 10*time.Second)
	}
	if err != nil {
		return nil, err
	}
	key := make([]byte, 16)
	rand.Read(key)
	origin := "https://" + u.Host
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Protocol: binary\r\nOrigin: %s\r\n\r\n",
		u.RequestURI(), u.Host, base64.StdEncoding.EncodeToString(key), origin)
	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return nil, fmt.Errorf("websocket upgrade: %s", resp.Status)
	}
	return &wsConn{conn: conn, r: r}, nil
}

func (w *wsConn) Read(p []byte) (int, error) {
	for w.left == 0 {
		var h [2]byte
		if _, err := io.ReadFull(w.r, h[:]); err != nil {
			return 0, err
		}
		n := int64(h[1] & 0x7f)
		switch n {
		case 126:
			var x uint16
			binary.Read(w.r, binary.BigEndian, &x)
			n = int64(x)
		case 127:
			binary.Read(w.r, binary.BigEndian, &n)
		}
		switch h[0] & 0x0f {
		case 8:
			return 0, io.EOF
		case 9, 10: // ping, pong: skip
			io.CopyN(io.Discard, w.r, n)
			continue
		}
		w.left = n
	}
	if int64(len(p)) > w.left {
		p = p[:w.left]
	}
	n, err := w.r.Read(p)
	w.left -= int64(n)
	return n, err
}

func (w *wsConn) Write(p []byte) (int, error) {
	frame := []byte{0x82}
	switch {
	case len(p) < 126:
		frame = append(frame, 0x80|byte(len(p)))
	case len(p) < 1<<16:
		frame = append(frame, 0x80|126, byte(len(p)>>8), byte(len(p)))
	default:
		frame = append(frame, 0x80|127)
		frame = binary.BigEndian.AppendUint64(frame, uint64(len(p)))
	}
	mask := make([]byte, 4)
	rand.Read(mask)
	frame = append(frame, mask...)
	for i, b := range p {
		frame = append(frame, b^mask[i%4])
	}
	return w.conn.Write(frame)
}

func (w *wsConn) Close() error { return w.conn.Close() }
