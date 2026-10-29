package cloudhttp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
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

	"github.com/different-ai/differently/pkg/adapter"
)

// TokenSource returns a bearer token, refreshing it before it expires.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// refreshMargin renews a token this long before it expires.
const refreshMargin = 2 * time.Minute

// cached memoizes a fetched token until shortly before its expiry.
type cached struct {
	fetch func(ctx context.Context) (token string, ttl time.Duration, err error)

	mu  sync.Mutex
	tok string
	exp time.Time
}

func (c *cached) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.tok != "" && now.Add(refreshMargin).Before(c.exp) {
		return c.tok, nil
	}
	tok, ttl, err := c.fetch(ctx)
	if err != nil {
		return "", err
	}
	c.tok, c.exp = tok, now.Add(ttl)
	return tok, nil
}

// EntraCredential is a Microsoft Entra ID app registration (service
// principal). Set ClientSecret, or FederatedTokenFile for workload identity
// federation (AKS workload identity mounts that file into the pod).
type EntraCredential struct {
	TenantID           string `json:"tenantId"`
	ClientID           string `json:"clientId"`
	ClientSecret       string `json:"clientSecret,omitempty"`
	FederatedTokenFile string `json:"federatedTokenFile,omitempty"`
	// AuthorityHost defaults to https://login.microsoftonline.com.
	AuthorityHost string `json:"authorityHost,omitempty"`
}

// ParseEntra reads an EntraCredential from the pool's credential string.
func ParseEntra(raw string) (EntraCredential, error) {
	var c EntraCredential
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return c, errors.New(`the credential must be JSON: {"tenantId", "clientId", "clientSecret"} or {"tenantId", "clientId", "federatedTokenFile"}`)
	}
	switch {
	case c.TenantID == "" || c.ClientID == "":
		return c, errors.New("the credential needs tenantId and clientId")
	case c.ClientSecret == "" && c.FederatedTokenFile == "":
		return c, errors.New("the credential needs clientSecret or federatedTokenFile")
	}
	if c.AuthorityHost == "" {
		c.AuthorityHost = "https://login.microsoftonline.com"
	}
	return c, nil
}

// Entra returns a token source using the OAuth 2.0 client-credentials grant
// for scope (for example "https://dynamicsessions.io/.default").
func Entra(c EntraCredential, scope string, hc *http.Client) TokenSource {
	tokenURL := strings.TrimRight(c.AuthorityHost, "/") + "/" + url.PathEscape(c.TenantID) + "/oauth2/v2.0/token"
	return &cached{fetch: func(ctx context.Context) (string, time.Duration, error) {
		form := url.Values{"grant_type": {"client_credentials"}, "client_id": {c.ClientID}, "scope": {scope}}
		if c.FederatedTokenFile != "" {
			// Re-read every time: the kubelet rotates the projected token.
			b, err := os.ReadFile(c.FederatedTokenFile)
			if err != nil {
				return "", 0, adapter.Unavailable(fmt.Errorf("entra id: read the federated token file: %w", err))
			}
			form.Set("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer")
			form.Set("client_assertion", strings.TrimSpace(string(b)))
		} else {
			form.Set("client_secret", c.ClientSecret)
		}
		return postToken(ctx, hc, "entra id", tokenURL, form)
	}}
}

// GoogleServiceAccount is the JSON key file Google issues for a service
// account. Only the fields the JWT bearer grant needs are read.
type GoogleServiceAccount struct {
	Type         string `json:"type"`
	ClientEmail  string `json:"client_email"`
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
	TokenURI     string `json:"token_uri"`
}

// ParseGoogle reads a service-account key file from the credential string.
func ParseGoogle(raw string) (GoogleServiceAccount, *rsa.PrivateKey, error) {
	var sa GoogleServiceAccount
	if err := json.Unmarshal([]byte(raw), &sa); err != nil || sa.Type != "service_account" {
		return sa, nil, errors.New("the credential must be a Google service-account JSON key (type service_account)")
	}
	if sa.ClientEmail == "" || sa.PrivateKey == "" {
		return sa, nil, errors.New("the service-account key needs client_email and private_key")
	}
	block, _ := pem.Decode([]byte(sa.PrivateKey))
	if block == nil {
		return sa, nil, errors.New("the service-account private_key is not PEM")
	}
	var key *rsa.PrivateKey
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		key, _ = k.(*rsa.PrivateKey)
	} else if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		key = k
	}
	if key == nil {
		return sa, nil, errors.New("the service-account private_key is not an RSA key")
	}
	if sa.TokenURI == "" {
		sa.TokenURI = "https://oauth2.googleapis.com/token"
	}
	return sa, key, nil
}

// Google returns a token source using the OAuth 2.0 JWT bearer grant
// (RFC 7523) with a service-account key, for scope (usually
// "https://www.googleapis.com/auth/cloud-platform").
func Google(sa GoogleServiceAccount, key *rsa.PrivateKey, scope string, hc *http.Client) TokenSource {
	return &cached{fetch: func(ctx context.Context) (string, time.Duration, error) {
		now := time.Now()
		assertion, err := signJWT(key, sa.PrivateKeyID, map[string]any{
			"iss": sa.ClientEmail, "scope": scope, "aud": sa.TokenURI,
			"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
		})
		if err != nil {
			return "", 0, err
		}
		form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {assertion}}
		return postToken(ctx, hc, "google oauth", sa.TokenURI, form)
	}}
}

func signJWT(key *rsa.PrivateKey, kid string, claims map[string]any) (string, error) {
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	if kid != "" {
		header["kid"] = kid
	}
	h, _ := json.Marshal(header)
	c, _ := json.Marshal(claims)
	enc := base64.RawURLEncoding
	unsigned := enc.EncodeToString(h) + "." + enc.EncodeToString(c)
	sum := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + enc.EncodeToString(sig), nil
}

// postToken exchanges a form for an OAuth 2.0 access token. Any failure is
// provider_unavailable: without a token the pool cannot accept work.
func postToken(ctx context.Context, hc *http.Client, who, tokenURL string, form url.Values) (string, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", 0, adapter.Unavailable(fmt.Errorf("%s: token request failed: %w", who, err))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var body struct {
		AccessToken string          `json:"access_token"`
		ExpiresIn   json.RawMessage `json:"expires_in"`
		Error       string          `json:"error"`
	}
	_ = json.Unmarshal(raw, &body)
	if resp.StatusCode != http.StatusOK || body.AccessToken == "" {
		reason := body.Error
		if reason == "" {
			reason = http.StatusText(resp.StatusCode)
		}
		// error_description can echo request details; keep only the OAuth code.
		return "", 0, adapter.Unavailable(fmt.Errorf("%s: credentials were rejected (%s); check the pool's credential reference", who, reason))
	}
	secs, err := strconv.Atoi(strings.Trim(string(body.ExpiresIn), `"`)) // some issuers send a string
	if err != nil || secs <= 0 {
		secs = 3600
	}
	return body.AccessToken, time.Duration(secs) * time.Second, nil
}

// Bearer authorizes requests with a token from ts.
func Bearer(ts TokenSource) func(*http.Request, []byte) error {
	return func(r *http.Request, _ []byte) error {
		tok, err := ts.Token(r.Context())
		if err != nil {
			return err
		}
		r.Header.Set("Authorization", "Bearer "+tok)
		return nil
	}
}
