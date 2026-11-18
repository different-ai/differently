//go:build experimental_adapters

package cloudhttp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/different-ai/differently/pkg/adapter"
)

// AWSCredential says where a pool's AWS keys come from. It is either a
// static access key (AccessKeyID and SecretAccessKey, with SessionToken for
// temporary keys that the operator renews), or a Source that Differently
// refreshes by itself:
//
//   - "web-identity": IAM Roles for Service Accounts on EKS. The projected
//     service-account token is exchanged for role credentials with STS
//     AssumeRoleWithWebIdentity.
//   - "pod-identity": EKS Pod Identity (and ECS task roles): the container
//     credentials endpoint the platform injects.
//   - "instance-role": the EC2 instance profile, read from the instance
//     metadata service (IMDSv2).
//
// A source's fields default to the environment variables AWS sets for it,
// so {"source": "web-identity"} is enough on a pod that has them.
type AWSCredential struct {
	AccessKeyID     string `json:"accessKeyId,omitempty"`
	SecretAccessKey string `json:"secretAccessKey,omitempty"`
	SessionToken    string `json:"sessionToken,omitempty"`

	Source string `json:"source,omitempty"`

	// web-identity: default AWS_ROLE_ARN, AWS_WEB_IDENTITY_TOKEN_FILE and
	// AWS_ROLE_SESSION_NAME. STSEndpoint defaults to the regional endpoint.
	RoleARN              string `json:"roleArn,omitempty"`
	WebIdentityTokenFile string `json:"webIdentityTokenFile,omitempty"`
	RoleSessionName      string `json:"roleSessionName,omitempty"`
	STSEndpoint          string `json:"stsEndpoint,omitempty"`

	// pod-identity: default AWS_CONTAINER_CREDENTIALS_FULL_URI and
	// AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE.
	ContainerCredentialsURI         string `json:"containerCredentialsUri,omitempty"`
	ContainerAuthorizationTokenFile string `json:"containerAuthorizationTokenFile,omitempty"`

	// instance-role: default http://169.254.169.254 (or
	// AWS_EC2_METADATA_SERVICE_ENDPOINT).
	IMDSEndpoint string `json:"imdsEndpoint,omitempty"`
}

// AWS credential sources.
const (
	AWSSourceWebIdentity  = "web-identity"
	AWSSourcePodIdentity  = "pod-identity"
	AWSSourceInstanceRole = "instance-role"
)

const awsCredentialHelp = `the credential must be JSON: {"accessKeyId", "secretAccessKey", optional "sessionToken"}, or {"source": "web-identity" | "pod-identity" | "instance-role"}`

// ParseAWS reads an AWSCredential from the pool's credential string and
// fills a source's fields from the environment AWS sets for it.
func ParseAWS(raw string) (AWSCredential, error) {
	var c AWSCredential
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return c, errors.New(awsCredentialHelp)
	}
	static := c.AccessKeyID != "" || c.SecretAccessKey != ""
	switch c.Source {
	case "":
		if c.AccessKeyID == "" || c.SecretAccessKey == "" {
			return c, errors.New(awsCredentialHelp)
		}
		return c, nil
	case AWSSourceWebIdentity:
		c.RoleARN = orEnv(c.RoleARN, "AWS_ROLE_ARN")
		c.WebIdentityTokenFile = orEnv(c.WebIdentityTokenFile, "AWS_WEB_IDENTITY_TOKEN_FILE")
		c.RoleSessionName = orEnv(c.RoleSessionName, "AWS_ROLE_SESSION_NAME")
		if c.RoleSessionName == "" {
			c.RoleSessionName = "differently"
		}
		if c.RoleARN == "" || c.WebIdentityTokenFile == "" {
			return c, errors.New(`the web-identity source needs roleArn and webIdentityTokenFile, or AWS_ROLE_ARN and AWS_WEB_IDENTITY_TOKEN_FILE in the server's environment (EKS sets them on a pod whose service account is annotated with the role)`)
		}
	case AWSSourcePodIdentity:
		c.ContainerCredentialsURI = orEnv(c.ContainerCredentialsURI, "AWS_CONTAINER_CREDENTIALS_FULL_URI")
		c.ContainerAuthorizationTokenFile = orEnv(c.ContainerAuthorizationTokenFile, "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE")
		if c.ContainerCredentialsURI == "" {
			return c, errors.New(`the pod-identity source needs containerCredentialsUri, or AWS_CONTAINER_CREDENTIALS_FULL_URI in the server's environment (EKS Pod Identity sets it on a pod with an association)`)
		}
		if err := checkContainerURI(c.ContainerCredentialsURI); err != nil {
			return c, err
		}
	case AWSSourceInstanceRole:
		c.IMDSEndpoint = strings.TrimRight(orEnv(c.IMDSEndpoint, "AWS_EC2_METADATA_SERVICE_ENDPOINT"), "/")
		if c.IMDSEndpoint == "" {
			c.IMDSEndpoint = "http://169.254.169.254"
		}
	default:
		return c, errors.New(awsCredentialHelp)
	}
	if static {
		return c, errors.New("the credential has both an access key and a source; keep one")
	}
	return c, nil
}

func orEnv(v, name string) string {
	if v != "" {
		return v
	}
	return os.Getenv(name)
}

// checkContainerURI applies the AWS SDKs' rule for the container
// credentials endpoint: HTTPS anywhere, plain HTTP only to loopback or the
// ECS and EKS agent addresses. The authorization token is sent to it.
func checkContainerURI(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("containerCredentialsUri is not a URL")
	}
	if u.Scheme == "https" {
		return nil
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); u.Scheme == "http" && (host == "localhost" || ip != nil && (ip.IsLoopback() ||
		ip.Equal(net.ParseIP("169.254.170.2")) || ip.Equal(net.ParseIP("169.254.170.23")) || ip.Equal(net.ParseIP("fd00:ec2::23")))) {
		return nil
	}
	return errors.New("containerCredentialsUri must be https, or http to loopback or the ECS/EKS agent address (169.254.170.2, 169.254.170.23, fd00:ec2::23)")
}

// awsRefreshMargin renews temporary keys this long before they expire.
const awsRefreshMargin = 5 * time.Minute

// AWSKeys returns a function that yields the current access key for c:
// the static key, or temporary keys fetched from c.Source and renewed
// before they expire. Failures are provider_unavailable and never include
// key material or tokens.
func AWSKeys(c AWSCredential, region string, hc *http.Client) func(context.Context) (AWSCredential, error) {
	if c.Source == "" {
		return func(context.Context) (AWSCredential, error) { return c, nil }
	}
	var (
		mu   sync.Mutex
		keys AWSCredential
		exp  time.Time
	)
	return func(ctx context.Context) (AWSCredential, error) {
		mu.Lock()
		defer mu.Unlock()
		now := time.Now()
		if keys.AccessKeyID != "" && now.Add(awsRefreshMargin).Before(exp) {
			return keys, nil
		}
		k, e, err := fetchAWSKeys(ctx, c, region, hc)
		if err != nil {
			if keys.AccessKeyID != "" && now.Before(exp) {
				return keys, nil // still valid: keep working through a brief outage
			}
			return AWSCredential{}, adapter.Unavailable(fmt.Errorf("aws %s credentials: %w", c.Source, err))
		}
		if k.AccessKeyID == "" || k.SecretAccessKey == "" {
			return AWSCredential{}, adapter.Unavailable(fmt.Errorf("aws %s credentials: the response had no access key", c.Source))
		}
		if e.IsZero() {
			e = now.Add(time.Hour)
		}
		keys, exp = k, e
		return keys, nil
	}
}

func fetchAWSKeys(ctx context.Context, c AWSCredential, region string, hc *http.Client) (AWSCredential, time.Time, error) {
	switch c.Source {
	case AWSSourceWebIdentity:
		return webIdentityKeys(ctx, c, region, hc)
	case AWSSourcePodIdentity:
		return containerKeys(ctx, c, hc)
	}
	return instanceRoleKeys(ctx, c, hc)
}

// awsGet sends one small request and returns the body of a 200 response.
// Error text names the status only: bodies can echo tokens.
func awsGet(ctx context.Context, hc *http.Client, method, rawURL string, header map[string]string, body io.Reader) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, 0, errors.New("bad endpoint URL")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // drop the URL: it is noise, and STS URLs are long
		}
		return nil, 0, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	return raw, resp.StatusCode, nil
}

// webIdentityKeys calls STS AssumeRoleWithWebIdentity (an unsigned call)
// with the projected service-account token, re-read on every refresh
// because the kubelet rotates it.
func webIdentityKeys(ctx context.Context, c AWSCredential, region string, hc *http.Client) (AWSCredential, time.Time, error) {
	tok, err := os.ReadFile(c.WebIdentityTokenFile)
	if err != nil {
		return AWSCredential{}, time.Time{}, fmt.Errorf("read the web identity token file: %w", err)
	}
	endpoint := c.STSEndpoint
	if endpoint == "" {
		endpoint = "https://sts." + region + ".amazonaws.com"
	}
	form := url.Values{"Action": {"AssumeRoleWithWebIdentity"}, "Version": {"2011-06-15"},
		"RoleArn": {c.RoleARN}, "RoleSessionName": {c.RoleSessionName}, "WebIdentityToken": {strings.TrimSpace(string(tok))}}
	raw, status, err := awsGet(ctx, hc, http.MethodPost, strings.TrimRight(endpoint, "/")+"/",
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, strings.NewReader(form.Encode()))
	if err != nil {
		return AWSCredential{}, time.Time{}, err
	}
	if status != http.StatusOK {
		var e struct {
			Code string `xml:"Error>Code"`
		}
		_ = xml.Unmarshal(raw, &e)
		if e.Code == "" {
			e.Code = http.StatusText(status)
		}
		// The message can quote the token's claims; keep only STS's code.
		return AWSCredential{}, time.Time{}, fmt.Errorf("STS refused to assume the role (%s); check the role's trust policy and the service account", e.Code)
	}
	var out struct {
		Creds struct {
			AccessKeyID     string    `xml:"AccessKeyId"`
			SecretAccessKey string    `xml:"SecretAccessKey"`
			SessionToken    string    `xml:"SessionToken"`
			Expiration      time.Time `xml:"Expiration"`
		} `xml:"AssumeRoleWithWebIdentityResult>Credentials"`
	}
	if err := xml.Unmarshal(raw, &out); err != nil {
		return AWSCredential{}, time.Time{}, errors.New("unreadable STS response")
	}
	return AWSCredential{AccessKeyID: out.Creds.AccessKeyID, SecretAccessKey: out.Creds.SecretAccessKey, SessionToken: out.Creds.SessionToken}, out.Creds.Expiration, nil
}

// roleKeys is the JSON the container credentials endpoint and the instance
// metadata service both return.
type roleKeys struct {
	AccessKeyID     string    `json:"AccessKeyId"`
	SecretAccessKey string    `json:"SecretAccessKey"`
	Token           string    `json:"Token"`
	Expiration      time.Time `json:"Expiration"`
}

func (k roleKeys) credential() (AWSCredential, time.Time) {
	return AWSCredential{AccessKeyID: k.AccessKeyID, SecretAccessKey: k.SecretAccessKey, SessionToken: k.Token}, k.Expiration
}

// containerKeys reads the container credentials endpoint. EKS Pod Identity
// authorizes it with a token file the kubelet rotates.
func containerKeys(ctx context.Context, c AWSCredential, hc *http.Client) (AWSCredential, time.Time, error) {
	header := map[string]string{}
	if c.ContainerAuthorizationTokenFile != "" {
		tok, err := os.ReadFile(c.ContainerAuthorizationTokenFile)
		if err != nil {
			return AWSCredential{}, time.Time{}, fmt.Errorf("read the container authorization token file: %w", err)
		}
		header["Authorization"] = strings.TrimSpace(string(tok))
	}
	raw, status, err := awsGet(ctx, hc, http.MethodGet, c.ContainerCredentialsURI, header, nil)
	if err != nil {
		return AWSCredential{}, time.Time{}, err
	}
	if status != http.StatusOK {
		return AWSCredential{}, time.Time{}, fmt.Errorf("the container credentials endpoint answered %d; check the pod identity association", status)
	}
	var k roleKeys
	if err := json.Unmarshal(raw, &k); err != nil {
		return AWSCredential{}, time.Time{}, errors.New("unreadable container credentials response")
	}
	cred, exp := k.credential()
	return cred, exp, nil
}

// instanceRoleKeys reads the instance profile's keys with IMDSv2: a session
// token first, then the role name, then its credentials.
func instanceRoleKeys(ctx context.Context, c AWSCredential, hc *http.Client) (AWSCredential, time.Time, error) {
	fail := func(what string, status int) (AWSCredential, time.Time, error) {
		return AWSCredential{}, time.Time{}, fmt.Errorf("the instance metadata service answered %d for %s; the instance needs an instance profile, and a pod needs a metadata hop limit of 2", status, what)
	}
	tok, status, err := awsGet(ctx, hc, http.MethodPut, c.IMDSEndpoint+"/latest/api/token", map[string]string{"X-aws-ec2-metadata-token-ttl-seconds": "21600"}, nil)
	if err != nil {
		return AWSCredential{}, time.Time{}, err
	}
	if status != http.StatusOK {
		return fail("a session token", status)
	}
	header := map[string]string{"X-aws-ec2-metadata-token": strings.TrimSpace(string(tok))}
	base := c.IMDSEndpoint + "/latest/meta-data/iam/security-credentials/"
	names, status, err := awsGet(ctx, hc, http.MethodGet, base, header, nil)
	if err != nil {
		return AWSCredential{}, time.Time{}, err
	}
	role, _, _ := strings.Cut(strings.TrimSpace(string(names)), "\n")
	if status != http.StatusOK || role == "" {
		return fail("the role name", status)
	}
	raw, status, err := awsGet(ctx, hc, http.MethodGet, base+url.PathEscape(role), header, nil)
	if err != nil {
		return AWSCredential{}, time.Time{}, err
	}
	if status != http.StatusOK {
		return fail("the role's credentials", status)
	}
	var k roleKeys
	if err := json.Unmarshal(raw, &k); err != nil {
		return AWSCredential{}, time.Time{}, errors.New("unreadable instance metadata response")
	}
	cred, exp := k.credential()
	return cred, exp, nil
}

// SigV4 authorizes requests with AWS Signature Version 4 for one region and
// service (the signing name, for example "bedrock-agentcore"), with the
// static key or the refreshed keys of c's source.
func SigV4(c AWSCredential, region, service string) func(*http.Request, []byte) error {
	keys := AWSKeys(c, region, &http.Client{Timeout: 30 * time.Second})
	return func(r *http.Request, body []byte) error {
		k, err := keys(r.Context())
		if err != nil {
			return err
		}
		signV4(r, body, k, region, service, time.Now())
		return nil
	}
}

// signV4 follows "Create a signed AWS API request" in the IAM user guide:
// canonical request, string to sign, derived key, Authorization header.
func signV4(r *http.Request, body []byte, c AWSCredential, region, service string, now time.Time) {
	amzDate := now.UTC().Format("20060102T150405Z")
	day := amzDate[:8]
	r.Header.Set("X-Amz-Date", amzDate)
	if c.SessionToken != "" {
		r.Header.Set("X-Amz-Security-Token", c.SessionToken)
	}
	payload := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(payload[:])

	// Sign the host and every header already set, except ones proxies or
	// the transport may change.
	headers := map[string]string{"host": r.URL.Host}
	if r.Host != "" {
		headers["host"] = r.Host
	}
	for k, vs := range r.Header {
		lk := strings.ToLower(k)
		switch lk {
		case "authorization", "user-agent", "expect", "x-amzn-trace-id", "accept-encoding", "content-length":
			continue
		}
		vals := make([]string, len(vs))
		for i, v := range vs {
			vals[i] = strings.Join(strings.Fields(v), " ")
		}
		headers[lk] = strings.Join(vals, ",")
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + headers[k] + "\n")
	}
	signed := strings.Join(names, ";")

	canonical := strings.Join([]string{
		r.Method,
		canonicalPath(r.URL),
		canonicalQuery(r.URL.Query()),
		canonHeaders.String(),
		signed,
		payloadHash,
	}, "\n")
	scope := day + "/" + region + "/" + service + "/aws4_request"
	sum := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])

	key := hmacSHA256([]byte("AWS4"+c.SecretAccessKey), day)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, service)
	key = hmacSHA256(key, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(key, toSign))
	r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+c.AccessKeyID+"/"+scope+", SignedHeaders="+signed+", Signature="+sig)
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// canonicalPath encodes each segment of the already-escaped path again:
// services other than S3 sign the path URI-encoded twice.
func canonicalPath(u *url.URL) string {
	p := u.EscapedPath()
	if p == "" {
		return "/"
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = awsEscape(s)
	}
	return strings.Join(segs, "/")
}

// canonicalQuery sorts encoded parameters by name, then by value.
func canonicalQuery(q url.Values) string {
	type kv struct{ k, v string }
	pairs := make([]kv, 0, len(q))
	for k, vs := range q {
		for _, v := range vs {
			pairs = append(pairs, kv{awsEscape(k), awsEscape(v)})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}
		return pairs[i].v < pairs[j].v
	})
	out := make([]string, len(pairs))
	for i, p := range pairs {
		out[i] = p.k + "=" + p.v
	}
	return strings.Join(out, "&")
}

// awsEscape is RFC 3986 percent-encoding: everything but A-Z a-z 0-9 - _ . ~.
func awsEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteString("%" + strings.ToUpper(hex.EncodeToString([]byte{c})))
	}
	return b.String()
}
