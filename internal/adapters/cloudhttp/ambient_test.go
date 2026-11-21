//go:build experimental_adapters

package cloudhttp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/different-ai/differently/pkg/api"
)

func tokenFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// STS AssumeRoleWithWebIdentity as documented: an unsigned form POST that
// answers XML, and an ErrorResponse on refusal.
func TestAWSWebIdentity(t *testing.T) {
	var calls atomic.Int32
	expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = r.ParseForm()
		f := r.PostForm
		if r.Header.Get("Authorization") != "" || f.Get("Action") != "AssumeRoleWithWebIdentity" || f.Get("Version") != "2011-06-15" ||
			f.Get("RoleArn") != "arn:aws:iam::123456789012:role/differently" || f.Get("RoleSessionName") != "differently" {
			t.Errorf("sts request %v", f)
		}
		if f.Get("WebIdentityToken") != "projected-jwt" {
			w.WriteHeader(400)
			w.Write([]byte(`<ErrorResponse><Error><Type>Sender</Type><Code>InvalidIdentityToken</Code><Message>bad token ` + f.Get("WebIdentityToken") + `</Message></Error></ErrorResponse>`))
			return
		}
		w.Write([]byte(`<AssumeRoleWithWebIdentityResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><AssumeRoleWithWebIdentityResult>
<Credentials><AccessKeyId>ASIAWEB</AccessKeyId><SecretAccessKey>web-secret</SecretAccessKey><SessionToken>web-token</SessionToken><Expiration>` + expires + `</Expiration></Credentials>
</AssumeRoleWithWebIdentityResult></AssumeRoleWithWebIdentityResponse>`))
	}))
	defer srv.Close()
	ctx := context.Background()

	// EKS sets these on a pod whose service account names the role.
	t.Setenv("AWS_ROLE_ARN", "arn:aws:iam::123456789012:role/differently")
	t.Setenv("AWS_WEB_IDENTITY_TOKEN_FILE", tokenFile(t, "projected-jwt\n"))
	t.Setenv("AWS_ROLE_SESSION_NAME", "")
	c, err := ParseAWS(`{"source":"web-identity","stsEndpoint":"` + srv.URL + `"}`)
	if err != nil {
		t.Fatal(err)
	}
	keys := AWSKeys(c, "us-east-1", srv.Client())
	for range 3 {
		k, err := keys(ctx)
		if err != nil || k.AccessKeyID != "ASIAWEB" || k.SecretAccessKey != "web-secret" || k.SessionToken != "web-token" {
			t.Fatalf("keys %+v %v", k, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("keys are cached until they near expiry: %d STS calls", calls.Load())
	}

	bad, err := ParseAWS(`{"source":"web-identity","roleArn":"arn:aws:iam::123456789012:role/differently","webIdentityTokenFile":"` + tokenFile(t, "stolen-jwt") + `","stsEndpoint":"` + srv.URL + `"}`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = AWSKeys(bad, "us-east-1", srv.Client())(ctx)
	if !api.IsCode(err, api.CodeProviderUnavailable) || !strings.Contains(err.Error(), "InvalidIdentityToken") || strings.Contains(err.Error(), "stolen-jwt") {
		t.Fatalf("a refused token is provider_unavailable, without the token: %v", err)
	}
}

// Keys within the refresh margin are renewed; a failed renewal keeps the
// keys that are still valid.
func TestAWSKeysRefresh(t *testing.T) {
	var calls atomic.Int32
	var down atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if down.Load() {
			w.WriteHeader(500)
			return
		}
		if r.Header.Get("Authorization") != "pod-token" {
			w.WriteHeader(403)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"AccessKeyId": "ASIAPOD" + string(rune('0'+n)), "SecretAccessKey": "s", "Token": "t",
			"Expiration": time.Now().Add(awsRefreshMargin - time.Second).UTC().Format(time.RFC3339)})
	}))
	defer srv.Close()
	ctx := context.Background()

	t.Setenv("AWS_CONTAINER_CREDENTIALS_FULL_URI", srv.URL+"/v1/credentials")
	t.Setenv("AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE", tokenFile(t, "pod-token"))
	c, err := ParseAWS(`{"source":"pod-identity"}`)
	if err != nil {
		t.Fatal(err)
	}
	keys := AWSKeys(c, "us-east-1", srv.Client())
	if k, err := keys(ctx); err != nil || k.AccessKeyID != "ASIAPOD1" || k.SessionToken != "t" {
		t.Fatalf("first keys %+v %v", k, err)
	}
	if k, err := keys(ctx); err != nil || k.AccessKeyID != "ASIAPOD2" {
		t.Fatalf("keys inside the refresh margin are renewed: %+v %v", k, err)
	}
	down.Store(true)
	if k, err := keys(ctx); err != nil || k.AccessKeyID != "ASIAPOD2" {
		t.Fatalf("a failed renewal keeps keys that are still valid: %+v %v", k, err)
	}

	// The authorizer signs with the refreshed keys and their session token.
	down.Store(false)
	r, _ := http.NewRequest(http.MethodGet, "https://bedrock-agentcore.us-east-1.amazonaws.com/x", nil)
	if err := SigV4(c, "us-east-1", "bedrock-agentcore")(r, nil); err != nil {
		t.Fatal(err)
	}
	if auth := r.Header.Get("Authorization"); !strings.Contains(auth, "Credential=ASIAPOD") || r.Header.Get("X-Amz-Security-Token") != "t" {
		t.Fatalf("signed %q", auth)
	}

	wrong, _ := ParseAWS(`{"source":"pod-identity","containerCredentialsUri":"` + srv.URL + `","containerAuthorizationTokenFile":"` + tokenFile(t, "other") + `"}`)
	if _, err := AWSKeys(wrong, "us-east-1", srv.Client())(ctx); !api.IsCode(err, api.CodeProviderUnavailable) || strings.Contains(err.Error(), "other") {
		t.Fatalf("a refused pod token: %v", err)
	}
}

// IMDSv2: a session token, the role name, then the role's credentials.
func TestAWSInstanceRole(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/latest/api/token":
			if r.Header.Get("X-aws-ec2-metadata-token-ttl-seconds") == "" {
				w.WriteHeader(400)
				return
			}
			w.Write([]byte("imds-session"))
		case r.Header.Get("X-aws-ec2-metadata-token") != "imds-session":
			w.WriteHeader(401)
		case r.URL.Path == "/latest/meta-data/iam/security-credentials/":
			w.Write([]byte("node-role\n"))
		case r.URL.Path == "/latest/meta-data/iam/security-credentials/node-role":
			json.NewEncoder(w).Encode(map[string]any{"Code": "Success", "AccessKeyId": "ASIANODE", "SecretAccessKey": "s", "Token": "t",
				"Expiration": time.Now().Add(6 * time.Hour).UTC().Format(time.RFC3339)})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c, err := ParseAWS(`{"source":"instance-role","imdsEndpoint":"` + srv.URL + `/"}`)
	if err != nil {
		t.Fatal(err)
	}
	if k, err := AWSKeys(c, "us-east-1", srv.Client())(context.Background()); err != nil || k.AccessKeyID != "ASIANODE" || k.SessionToken != "t" {
		t.Fatalf("keys %+v %v", k, err)
	}

	none := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			w.Write([]byte("imds-session"))
			return
		}
		w.WriteHeader(404) // no instance profile
	}))
	defer none.Close()
	c.IMDSEndpoint = none.URL
	if _, err := AWSKeys(c, "us-east-1", none.Client())(context.Background()); !api.IsCode(err, api.CodeProviderUnavailable) || !strings.Contains(err.Error(), "instance profile") {
		t.Fatalf("no instance profile: %v", err)
	}
}

func TestParseAWS(t *testing.T) {
	for _, name := range []string{"AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE", "AWS_EC2_METADATA_SERVICE_ENDPOINT"} {
		t.Setenv(name, "")
	}
	if c, err := ParseAWS(`{"accessKeyId":"AKIA","secretAccessKey":"s"}`); err != nil || c.Source != "" {
		t.Fatalf("static key: %+v %v", c, err)
	}
	if c, err := ParseAWS(`{"source":"instance-role"}`); err != nil || c.IMDSEndpoint != "http://169.254.169.254" {
		t.Fatalf("instance role defaults: %+v %v", c, err)
	}
	if c, err := ParseAWS(`{"source":"pod-identity","containerCredentialsUri":"http://169.254.170.23/v1/credentials"}`); err != nil || c.ContainerCredentialsURI == "" {
		t.Fatalf("the EKS agent address is allowed: %v", err)
	}
	for _, raw := range []string{
		`nope`, `{}`, `{"accessKeyId":"AKIA"}`,
		`{"source":"sso"}`,
		`{"source":"web-identity"}`, // no role or token file, here or in the environment
		`{"source":"pod-identity"}`, // no endpoint
		`{"source":"pod-identity","containerCredentialsUri":"http://credentials.example.com/"}`, // the token must not go there in clear text
		`{"source":"instance-role","accessKeyId":"AKIA","secretAccessKey":"s"}`,
	} {
		if _, err := ParseAWS(raw); err == nil {
			t.Errorf("ParseAWS(%s) should fail", raw)
		}
	}
}

// The metadata server route that Compute Engine and GKE Workload Identity
// both serve.
func TestGoogleMetadataServer(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Metadata-Flavor") != "Google" {
			w.WriteHeader(403)
			return
		}
		if r.URL.Path != "/computeMetadata/v1/instance/service-accounts/default/token" || r.URL.Query().Get("scopes") != "https://www.googleapis.com/auth/cloud-platform" {
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(map[string]any{"error": "not_found"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "ya29.meta", "expires_in": 3599, "token_type": "Bearer"})
	}))
	defer srv.Close()
	ctx := context.Background()
	const scope = "https://www.googleapis.com/auth/cloud-platform"

	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(srv.URL, "http://"))
	ts, err := GoogleTokens(`{"type":"metadata_server"}`, scope, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if tok, err := ts.Token(ctx); err != nil || tok != "ya29.meta" {
			t.Fatalf("token %q %v", tok, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("token cached: %d calls", calls.Load())
	}
	missing, _ := GoogleTokens(`{"type":"metadata_server","serviceAccount":"nobody@p.iam.gserviceaccount.com","metadataHost":"`+srv.URL+`"}`, scope, srv.Client())
	if _, err := missing.Token(ctx); !api.IsCode(err, api.CodeProviderUnavailable) || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("an unknown service account: %v", err)
	}
	for _, raw := range []string{`nope`, `{"type":"authorized_user"}`, `{"type":"external_account"}`,
		`{"type":"external_account","audience":"a","subject_token_type":"t","token_url":"u","credential_source":{"url":"http://169.254.169.254/"}}`} {
		if _, err := GoogleTokens(raw, scope, srv.Client()); err == nil {
			t.Errorf("GoogleTokens(%s) should fail", raw)
		}
	}
}

// Workload identity federation: the token exchange (RFC 8693) at Google's
// STS, then IAM Credentials generateAccessToken for the service account.
func TestGoogleWorkloadIdentityFederation(t *testing.T) {
	const scope = "https://www.googleapis.com/auth/cloud-platform"
	const audience = "//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/pool/providers/eks"
	impScopes := []string{scope} // what generateAccessToken must be asked for
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/token":
			_ = r.ParseForm()
			f := r.PostForm
			if f.Get("grant_type") != "urn:ietf:params:oauth:grant-type:token-exchange" || f.Get("audience") != audience ||
				f.Get("requested_token_type") != "urn:ietf:params:oauth:token-type:access_token" ||
				f.Get("subject_token_type") != "urn:ietf:params:oauth:token-type:jwt" || f.Get("scope") != scope {
				t.Errorf("exchange %v", f)
			}
			if f.Get("subject_token") != "k8s-jwt" {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "bad " + f.Get("subject_token")})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "federated", "expires_in": 3600, "token_type": "Bearer"})
		case "/v1/projects/-/serviceAccounts/sa@p.iam.gserviceaccount.com:generateAccessToken":
			var body struct {
				Scope []string `json:"scope"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if r.Header.Get("Authorization") != "Bearer federated" || !slices.Equal(body.Scope, impScopes) {
				w.WriteHeader(403)
				json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": 403, "status": "PERMISSION_DENIED", "message": "denied"}})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"accessToken": "ya29.sa", "expireTime": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	config := func(file string, impersonate bool, format string) string {
		c := map[string]any{"type": "external_account", "audience": audience, "subject_token_type": "urn:ietf:params:oauth:token-type:jwt",
			"token_url": srv.URL + "/v1/token", "credential_source": map[string]any{"file": file}}
		if impersonate {
			c["service_account_impersonation_url"] = srv.URL + "/v1/projects/-/serviceAccounts/sa@p.iam.gserviceaccount.com:generateAccessToken"
		}
		if format != "" {
			c["credential_source"] = map[string]any{"file": file, "format": map[string]string{"type": "json", "subject_token_field_name": format}}
		}
		b, _ := json.Marshal(c)
		return string(b)
	}

	direct, err := GoogleTokens(config(tokenFile(t, "k8s-jwt\n"), false, ""), scope, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if tok, err := direct.Token(ctx); err != nil || tok != "federated" {
		t.Fatalf("federated token %q %v", tok, err)
	}
	viaSA, _ := GoogleTokens(config(tokenFile(t, `{"id_token":"k8s-jwt"}`), true, "id_token"), scope, srv.Client())
	if tok, err := viaSA.Token(ctx); err != nil || tok != "ya29.sa" {
		t.Fatalf("impersonated token %q %v", tok, err)
	}
	// Several scopes, as GKE needs, reach generateAccessToken as a list.
	impScopes = []string{scope, "https://www.googleapis.com/auth/userinfo.email"}
	twoScopes, _ := GoogleTokens(config(tokenFile(t, "k8s-jwt"), true, ""), scope+" https://www.googleapis.com/auth/userinfo.email", srv.Client())
	if tok, err := twoScopes.Token(ctx); err != nil || tok != "ya29.sa" {
		t.Fatalf("impersonated token with two scopes %q %v", tok, err)
	}
	impScopes = []string{scope}
	refused, _ := GoogleTokens(config(tokenFile(t, "forged-jwt"), false, ""), scope, srv.Client())
	if _, err := refused.Token(ctx); !api.IsCode(err, api.CodeProviderUnavailable) || !strings.Contains(err.Error(), "invalid_grant") || strings.Contains(err.Error(), "forged-jwt") {
		t.Fatalf("a refused subject token: %v", err)
	}
	gone, _ := GoogleTokens(config(filepath.Join(t.TempDir(), "absent"), false, ""), scope, srv.Client())
	if _, err := gone.Token(ctx); !api.IsCode(err, api.CodeProviderUnavailable) {
		t.Fatalf("a missing token file: %v", err)
	}
}
