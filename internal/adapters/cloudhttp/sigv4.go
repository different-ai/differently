package cloudhttp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// AWSCredential is an AWS access key. SessionToken is set for temporary
// credentials (they expire; the pool needs a new value when they do).
type AWSCredential struct {
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
	SessionToken    string `json:"sessionToken,omitempty"`
}

// ParseAWS reads an AWSCredential from the pool's credential string.
func ParseAWS(raw string) (AWSCredential, error) {
	var c AWSCredential
	if err := json.Unmarshal([]byte(raw), &c); err != nil || c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return c, errors.New(`the credential must be JSON: {"accessKeyId", "secretAccessKey", optional "sessionToken"}`)
	}
	return c, nil
}

// SigV4 authorizes requests with AWS Signature Version 4 for one region and
// service (the signing name, for example "bedrock-agentcore").
func SigV4(c AWSCredential, region, service string) func(*http.Request, []byte) error {
	return func(r *http.Request, body []byte) error {
		signV4(r, body, c, region, service, time.Now())
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
