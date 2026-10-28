package secrets

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSealOpenBindsName(t *testing.T) {
	box, err := New(make([]byte, KeySize))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := box.Seal("daytona", "dtn_secret")
	if err != nil || strings.Contains(string(sealed), "dtn_secret") {
		t.Fatalf("seal: %v", err)
	}
	if got, err := box.Open("daytona", sealed); err != nil || got != "dtn_secret" {
		t.Fatalf("open: %q %v", got, err)
	}
	if _, err := box.Open("freestyle", sealed); err == nil {
		t.Fatal("a value sealed for one name opened under another")
	}
	other, _ := New([]byte(strings.Repeat("k", KeySize)))
	if _, err := other.Open("daytona", sealed); err == nil {
		t.Fatal("a value opened with the wrong key")
	}
	again, _ := box.Seal("daytona", "dtn_secret")
	if string(again) == string(sealed) {
		t.Fatal("sealing reused a nonce")
	}
}

func TestLoad(t *testing.T) {
	if box, err := Load("DIFFERENTLY_TEST_NO_KEY", ""); box != nil || err != nil {
		t.Fatalf("an unset key should give no box: %v %v", box, err)
	}
	t.Setenv("DIFFERENTLY_TEST_KEY", NewKey())
	if box, err := Load("DIFFERENTLY_TEST_KEY", ""); box == nil || err != nil {
		t.Fatalf("env key: %v", err)
	}
	t.Setenv("DIFFERENTLY_TEST_KEY", base64.StdEncoding.EncodeToString([]byte("short")))
	if _, err := Load("DIFFERENTLY_TEST_KEY", ""); err == nil {
		t.Fatal("a short key was accepted")
	}
	file := filepath.Join(t.TempDir(), "key")
	_ = os.WriteFile(file, []byte(NewKey()+"\n"), 0o600)
	if box, err := Load("", file); box == nil || err != nil {
		t.Fatalf("file key: %v", err)
	}
	var none *Box
	if _, err := none.Seal("x", "y"); err != ErrNoKey {
		t.Fatalf("no key: %v", err)
	}
}
