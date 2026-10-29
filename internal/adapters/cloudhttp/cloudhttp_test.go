package cloudhttp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"hash/crc32"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/different-ai/differently/pkg/adapter"
	"github.com/different-ai/differently/pkg/api"
)

// The worked example in the IAM user guide ("Examples of the complete
// Signature Version 4 signing process"): IAM ListUsers on 2015-08-30.
func TestSigV4DocumentedExample(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, "https://iam.amazonaws.com/?Action=ListUsers&Version=2010-05-08", nil)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	cred := AWSCredential{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}
	signV4(r, nil, cred, "us-east-1", "iam", time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC))
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/iam/aws4_request, SignedHeaders=content-type;host;x-amz-date, Signature=5d672d79c15b13162d9279b0855cfba6789a8edb4c82c400e06b5924a6f2b5d7"
	if got := r.Header.Get("Authorization"); got != want {
		t.Fatalf("authorization\n got %s\nwant %s", got, want)
	}
}

func TestSigV4SessionTokenAndPath(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "https://bedrock-agentcore.us-east-1.amazonaws.com/code-interpreters/aws.codeinterpreter.v1/tools/invoke", nil)
	signV4(r, []byte(`{}`), AWSCredential{AccessKeyID: "AK", SecretAccessKey: "s", SessionToken: "tok"}, "us-east-1", "bedrock-agentcore", time.Now())
	auth := r.Header.Get("Authorization")
	if r.Header.Get("X-Amz-Security-Token") != "tok" || !strings.Contains(auth, "x-amz-security-token") || !strings.Contains(auth, "/us-east-1/bedrock-agentcore/aws4_request") {
		t.Fatalf("signed %q", auth)
	}
	u, _ := http.NewRequest(http.MethodGet, "https://h/a%20b/c", nil)
	if got := canonicalPath(u.URL); got != "/a%2520b/c" {
		t.Fatalf("non-S3 paths are encoded twice: %s", got)
	}
}

func TestEntraClientSecretAndFederatedToken(t *testing.T) {
	var calls atomic.Int32
	var last map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/tenant-1/oauth2/v2.0/token" {
			t.Errorf("token path %s", r.URL.Path)
		}
		_ = r.ParseForm()
		last = map[string]string{}
		for k := range r.PostForm {
			last[k] = r.PostForm.Get(k)
		}
		if last["client_secret"] == "wrong" {
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client", "error_description": "AADSTS7000215: Invalid client secret wrong"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"token_type": "Bearer", "expires_in": 3599, "access_token": "tok-" + last["client_id"]})
	}))
	defer srv.Close()
	ctx := context.Background()

	cred, err := ParseEntra(`{"tenantId":"tenant-1","clientId":"app","clientSecret":"s3cret","authorityHost":"` + srv.URL + `"}`)
	if err != nil {
		t.Fatal(err)
	}
	ts := Entra(cred, "https://dynamicsessions.io/.default", srv.Client())
	for range 3 {
		if tok, err := ts.Token(ctx); err != nil || tok != "tok-app" {
			t.Fatalf("token %q %v", tok, err)
		}
	}
	if calls.Load() != 1 || last["scope"] != "https://dynamicsessions.io/.default" || last["grant_type"] != "client_credentials" {
		t.Fatalf("token cached across calls: %d calls, form %v", calls.Load(), last)
	}

	file := filepath.Join(t.TempDir(), "token")
	os.WriteFile(file, []byte("federated-jwt\n"), 0o600)
	fed, _ := ParseEntra(`{"tenantId":"tenant-1","clientId":"wi","federatedTokenFile":"` + file + `","authorityHost":"` + srv.URL + `"}`)
	if tok, err := Entra(fed, "s", srv.Client()).Token(ctx); err != nil || tok != "tok-wi" || last["client_assertion"] != "federated-jwt" || last["client_secret"] != "" {
		t.Fatalf("federated token %q %v %v", tok, err, last)
	}

	bad, _ := ParseEntra(`{"tenantId":"tenant-1","clientId":"app","clientSecret":"wrong","authorityHost":"` + srv.URL + `"}`)
	_, err = Entra(bad, "s", srv.Client()).Token(ctx)
	if !api.IsCode(err, api.CodeProviderUnavailable) || strings.Contains(err.Error(), "wrong") || !strings.Contains(err.Error(), "invalid_client") {
		t.Fatalf("rejected secret: %v", err)
	}
	for _, raw := range []string{`nope`, `{"tenantId":"t"}`, `{"tenantId":"t","clientId":"c"}`} {
		if _, err := ParseEntra(raw); err == nil {
			t.Fatalf("ParseEntra(%s) should fail", raw)
		}
	}
}

func TestGoogleServiceAccountJWT(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			t.Errorf("grant %q", r.PostForm.Get("grant_type"))
		}
		parts := strings.Split(r.PostForm.Get("assertion"), ".")
		sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
			t.Errorf("signature: %v", err)
		}
		var claims map[string]any
		b, _ := base64.RawURLEncoding.DecodeString(parts[1])
		json.Unmarshal(b, &claims)
		if claims["iss"] != "sa@p.iam.gserviceaccount.com" || claims["aud"] != "http://"+r.Host+"/token" || claims["scope"] != "https://www.googleapis.com/auth/cloud-platform" {
			t.Errorf("claims %v", claims)
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "ya29.x", "expires_in": 3599, "token_type": "Bearer"})
	}))
	defer srv.Close()
	keyJSON, _ := json.Marshal(map[string]string{"type": "service_account", "client_email": "sa@p.iam.gserviceaccount.com", "private_key_id": "k1", "private_key": pemKey, "token_uri": srv.URL + "/token"})
	sa, k, err := ParseGoogle(string(keyJSON))
	if err != nil {
		t.Fatal(err)
	}
	if tok, err := Google(sa, k, "https://www.googleapis.com/auth/cloud-platform", srv.Client()).Token(context.Background()); err != nil || tok != "ya29.x" {
		t.Fatalf("token %q %v", tok, err)
	}
	if _, _, err := ParseGoogle(`{"type":"authorized_user"}`); err == nil {
		t.Fatal("only service-account keys are accepted")
	}
}

func TestProviderErrorShapes(t *testing.T) {
	amz := http.Header{}
	amz.Set("X-Amzn-ErrorType", "ThrottlingException:http://internal.amazon.com/coral/com.amazonaws.bedrockagentcore/")
	cases := []struct {
		h          http.Header
		raw, code  string
		msgContain string
	}{
		{nil, `{"error":{"code":"SessionNotFound","message":"no session"}}`, "SessionNotFound", "no session"},
		{nil, `{"error":{"code":404,"message":"not here","status":"NOT_FOUND"}}`, "NOT_FOUND", "not here"},
		{amz, `{"message":"Rate exceeded"}`, "ThrottlingException", "Rate exceeded"},
		{nil, `{"__type":"com.amazonaws#ValidationException","message":"bad"}`, "ValidationException", "bad"},
		{nil, `{"error":{"status":409,"message":"name taken"}}`, "", "name taken"},
		{nil, `{"error":"plain"}`, "", "plain"},
		{nil, `<html>`, "", ""},
	}
	for _, c := range cases {
		h := c.h
		if h == nil {
			h = http.Header{}
		}
		code, msg := ProviderError(h, []byte(c.raw))
		if code != c.code || !strings.Contains(msg, c.msgContain) {
			t.Errorf("%s: got %q %q", c.raw, code, msg)
		}
	}
}

func TestClientAuthorizesAndClassifies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer t" {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/quota":
			w.WriteHeader(400)
			w.Write([]byte(`{"error":{"code":"QuotaExceeded","message":"too many"}}`))
		case "/missing":
			w.WriteHeader(404)
			w.Write([]byte(`{"error":{"code":"NotFound","message":"gone"}}`))
		default:
			w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer srv.Close()
	c := New("cloud", srv.URL, func(r *http.Request, _ []byte) error { r.Header.Set("Authorization", "Bearer t"); return nil })
	c.Classify = func(status int, code string) string {
		if code == "QuotaExceeded" {
			return adapter.CodeInsufficientCapacity
		}
		return ""
	}
	ctx := context.Background()
	var out struct{ OK bool }
	if err := c.JSON(ctx, http.MethodGet, "/x", nil, &out); err != nil || !out.OK {
		t.Fatalf("ok %v %v", out, err)
	}
	if err := c.JSON(ctx, http.MethodGet, "/quota", nil, nil); !api.IsCode(err, adapter.CodeInsufficientCapacity) {
		t.Fatalf("override: %v", err)
	}
	if err := c.JSON(ctx, http.MethodGet, "/missing", nil, nil); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("404: %v", err)
	}
	c.Authorize = func(r *http.Request, _ []byte) error {
		r.Header.Set("Authorization", "Bearer secret-value")
		return nil
	}
	err := c.JSON(ctx, http.MethodGet, "/x", nil, nil)
	if !api.IsCode(err, api.CodeProviderUnavailable) || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("401: %v", err)
	}
	c.Authorize = func(*http.Request, []byte) error { return adapter.Unavailable(context.DeadlineExceeded) }
	if err := c.JSON(ctx, http.MethodGet, "/x", nil, nil); !api.IsCode(err, api.CodeProviderUnavailable) {
		t.Fatalf("token failure: %v", err)
	}
}

func TestEventStreamRoundTrip(t *testing.T) {
	var stream []byte
	stream = append(stream, EncodeEvent(map[string]string{":message-type": "event", ":event-type": "result", ":content-type": "application/json"}, []byte(`{"a":1}`))...)
	// A message with a timestamp header (type 8) and an empty payload.
	ts := []byte{5, ':', 'd', 'a', 't', 'e', 8, 0, 0, 1, 2, 3, 4, 5, 6}
	msg := binary.BigEndian.AppendUint32(nil, uint32(16+len(ts)))
	msg = binary.BigEndian.AppendUint32(msg, uint32(len(ts)))
	msg = binary.BigEndian.AppendUint32(msg, crc32.ChecksumIEEE(msg))
	msg = append(msg, ts...)
	msg = binary.BigEndian.AppendUint32(msg, crc32.ChecksumIEEE(msg))
	stream = append(stream, msg...)

	var got []Event
	if err := ReadEvents(bytes.NewReader(stream), func(e Event) error { got = append(got, e); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Headers[":event-type"] != "result" || string(got[0].Payload) != `{"a":1}` || len(got[1].Payload) != 0 {
		t.Fatalf("events %+v", got)
	}
	corrupt := bytes.Clone(stream)
	corrupt[20] ^= 0xff
	if err := ReadEvents(bytes.NewReader(corrupt), func(Event) error { return nil }); err == nil {
		t.Fatal("a corrupted message must fail its checksum")
	}
	if err := ReadEvents(bytes.NewReader(stream[:len(stream)-3]), func(Event) error { return nil }); err == nil {
		t.Fatal("a truncated stream must fail")
	}
}
