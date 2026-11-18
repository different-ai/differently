// Package install checks install.sh, the script a new user pipes into sh,
// against a release served from this machine: the archive layout and
// checksums.txt format are the ones .github/workflows/release.yml produces.
package install

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const version = "v9.9.9-test.1"

// release builds the archive for this machine with two stand-in binaries.
// The stand-in CLI knows the "run" command only when hasRun is set, as a
// release older than the command does not.
func release(t *testing.T, hasRun bool) (name string, archive []byte) {
	t.Helper()
	name = fmt.Sprintf("differently_%s_%s_%s", version, runtime.GOOS, runtime.GOARCH)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, bin := range []string{"differently", "differently-server"} {
		body := []byte("#!/bin/sh\necho " + bin + " " + version + "\n")
		if !hasRun {
			body = append(body, "[ \"$1\" = run ] && exit 1\nexit 0\n"...)
		}
		if err := tw.WriteHeader(&tar.Header{Name: name + "/" + bin, Mode: 0o755, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return name, buf.Bytes()
}

// serve publishes the archive and a checksums.txt holding sum for it.
func serve(t *testing.T, name string, archive []byte, sum string) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /"+version+"/"+name+".tar.gz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) })
	mux.HandleFunc("GET /"+version+"/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "%s  differently_%s_other_arch.tar.gz\n%s  %s.tar.gz\n", strings.Repeat("0", 64), version, sum, name)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func run(t *testing.T, url, dir string, env ...string) (string, error) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("install.sh covers Linux and macOS")
	}
	for _, tool := range []string{"sh", "tar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	cmd := exec.Command("sh", filepath.Join("..", "..", "install.sh"))
	cmd.Env = append(os.Environ(), "DIFFERENTLY_DOWNLOAD_URL="+url, "DIFFERENTLY_INSTALL_DIR="+dir)
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestInstallsBothBinariesFromACheckedArchive(t *testing.T) {
	name, archive := release(t, true)
	dir := filepath.Join(t.TempDir(), "bin")
	out, err := run(t, serve(t, name, archive, sum(archive)), dir, "DIFFERENTLY_VERSION="+version)
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	for _, bin := range []string{"differently", "differently-server"} {
		got, err := exec.Command(filepath.Join(dir, bin)).Output()
		if err != nil || strings.TrimSpace(string(got)) != bin+" "+version {
			t.Errorf("%s: output %q, err %v", bin, got, err)
		}
	}
	// The directory is new, so it cannot be on PATH: the script must say so
	// and give the next command.
	for _, want := range []string{"is not on your PATH", "differently-server --dev", "differently run -- echo hello"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// An older release has no "run": the next step shown must be one it has.
func TestNextStepMatchesTheInstalledRelease(t *testing.T) {
	name, archive := release(t, false)
	out, err := run(t, serve(t, name, archive, sum(archive)), t.TempDir(), "DIFFERENTLY_VERSION="+version)
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	if strings.Contains(out, "differently run") || !strings.Contains(out, "differently sandbox create --wait") {
		t.Errorf("the next step names a command this release lacks:\n%s", out)
	}
}

// A version given without the leading v is the same release.
func TestAcceptsAVersionWithoutThePrefix(t *testing.T) {
	name, archive := release(t, true)
	dir := t.TempDir()
	if out, err := run(t, serve(t, name, archive, sum(archive)), dir, "DIFFERENTLY_VERSION="+strings.TrimPrefix(version, "v")); err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "differently")); err != nil {
		t.Error(err)
	}
}

// An archive that does not match checksums.txt is never installed.
func TestRefusesAnArchiveThatFailsItsChecksum(t *testing.T) {
	name, archive := release(t, true)
	dir := t.TempDir()
	out, err := run(t, serve(t, name, archive, strings.Repeat("a", 64)), dir, "DIFFERENTLY_VERSION="+version)
	if err == nil || !strings.Contains(out, "checksum mismatch") {
		t.Fatalf("want a checksum failure, got err %v\n%s", err, out)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("%d files were installed from an unverified archive", len(entries))
	}
}

func TestRejectsAVersionThatIsNotARelease(t *testing.T) {
	for _, bad := range []string{"latest", "v1/../../x", "v1 2"} {
		out, err := run(t, "http://127.0.0.1:1", t.TempDir(), "DIFFERENTLY_VERSION="+bad)
		if err == nil || !strings.Contains(out, "is not a release version") {
			t.Errorf("version %q: err %v\n%s", bad, err, out)
		}
	}
}

// A missing archive is reported with its address, and nothing is installed.
func TestReportsAReleaseThatDoesNotExist(t *testing.T) {
	name, archive := release(t, true)
	dir := t.TempDir()
	out, err := run(t, serve(t, name, archive, sum(archive)), dir, "DIFFERENTLY_VERSION=v0.0.0")
	if err == nil || !strings.Contains(out, "could not download") {
		t.Fatalf("want a download failure, got err %v\n%s", err, out)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("%d files were installed", len(entries))
	}
}
