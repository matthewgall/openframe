package storage

// AWS Signature Version 4, implemented over crypto/hmac + crypto/sha256
// (no cloud SDK — ADR-0004 dependency policy, ADR-0012).
//
// Reference: https://docs.aws.amazon.com/general/latest/gr/sigv4_signing.html
//
// Canonicalisation gotchas implemented here (each is tested against the
// official AWS test vectors in sigv4_test.go):
//   - canonical URI: every path segment RFC3986-encoded, '/' preserved,
//     no path normalisation (no '.'/'..' collapsing);
//   - canonical query: parameters sorted by name then value, both sides
//     RFC3986-encoded, joined with '&'; empty set -> empty string (the
//     line is still present in the canonical request);
//   - canonical headers: lowercased names, values trimmed and internal
//     whitespace collapsed, sorted, each on its own line;
//   - payload hash is part of the signed headers (x-amz-content-sha256),
//     so it must be known before the request is sent (Put buffers the
//     body; empty-body requests use the empty-SHA256 constant).

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// sigV4ServiceS3 is the service name used in the S3 signing scope.
const sigV4ServiceS3 = "s3"

// emptySHA256 is the hex SHA-256 of the empty string — the payload hash
// for requests with no body (GET/HEAD/DELETE/List).
const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// sigV4Creds are the long-term credentials a request is signed with.
type sigV4Creds struct {
	AccessKeyID     string
	SecretAccessKey string
	Region          string
	// Service names the AWS service in the signing scope ("s3" for the
	// storage backend; the test vectors use the suite's "service").
	Service string
}

// sigV4Request describes the request to sign. Query values are the
// decoded parameter values (the signer encodes them). Headers must
// include Host, X-Amz-Date and X-Amz-Content-Sha256 for S3.
type sigV4Request struct {
	Method      string
	Path        string     // final path, e.g. "/bucket/abcd.display"
	Query       url.Values // decoded params; may be empty
	Headers     map[string]string
	PayloadHash string    // hex sha256 of the body (or emptySHA256)
	Timestamp   time.Time // UTC
}

// sign produces the Authorization header value and the encoded query
// string (without the leading '?') for the request.
func (c sigV4Creds) sign(r sigV4Request) (authHeader, encodedQuery string) {
	ts := r.Timestamp.UTC()
	amzDate := ts.Format("20060102T150405Z")
	dateStamp := ts.Format("20060102")
	service := c.Service
	if service == "" {
		service = sigV4ServiceS3
	}
	scope := dateStamp + "/" + c.Region + "/" + service + "/aws4_request"

	// Canonical query string.
	keys := make([]string, 0, len(r.Query))
	for k := range r.Query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var qParts []string
	for _, k := range keys {
		vals := append([]string(nil), r.Query[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			qParts = append(qParts, rfc3986Encode(k, true)+"="+rfc3986Encode(v, true))
		}
	}
	canonicalQuery := strings.Join(qParts, "&")

	// Canonical headers + signed headers list (lowercase-keyed map).
	lc := make(map[string]string, len(r.Headers))
	for n, v := range r.Headers {
		lc[strings.ToLower(n)] = v
	}
	names := make([]string, 0, len(lc))
	for n := range lc {
		names = append(names, n)
	}
	sort.Strings(names)
	var hParts strings.Builder
	for _, n := range names {
		hParts.WriteString(n)
		hParts.WriteByte(':')
		hParts.WriteString(normalizeHeaderValue(lc[n]))
		hParts.WriteByte('\n')
	}
	signedHeaders := strings.Join(names, ";")

	canonicalRequest := strings.Join([]string{
		r.Method,
		canonicalURI(r.Path),
		canonicalQuery,
		hParts.String(),
		signedHeaders,
		r.PayloadHash,
	}, "\n")

	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hexSHA256([]byte(canonicalRequest)),
	}, "\n")

	signingKey := hmacSHA256(hmacSHA256(hmacSHA256(hmacSHA256([]byte("AWS4"+c.SecretAccessKey), []byte(dateStamp)), []byte(c.Region)), []byte(service)), []byte("aws4_request"))
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	authHeader = fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.AccessKeyID, scope, signedHeaders, signature)
	encodedQuery = canonicalQuery
	return authHeader, encodedQuery
}

// normalizeHeaderValue trims surrounding whitespace and collapses
// interior runs of spaces/tabs to a single space (SigV4 spec).
func normalizeHeaderValue(v string) string {
	v = strings.TrimSpace(v)
	var b strings.Builder
	b.Grow(len(v))
	prevSpace := false
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c == ' ' || c == '\t' {
			if !prevSpace {
				b.WriteByte(' ')
			}
			prevSpace = true
		} else {
			b.WriteByte(c)
			prevSpace = false
		}
	}
	return b.String()
}

// canonicalURI RFC3986-encodes each segment of path, preserving '/'.
func canonicalURI(path string) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	segments := strings.Split(path, "/")
	for i, s := range segments {
		segments[i] = rfc3986Encode(s, false)
	}
	return strings.Join(segments, "/")
}

// rfc3986Encode percent-encodes every byte that is not an RFC3986
// unreserved character (A-Z a-z 0-9 - _ . ~), optionally keeping '/'.
func rfc3986Encode(s string, keepSlash bool) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'),
			c == '-' || c == '_' || c == '.' || c == '~':
			b.WriteByte(c)
		case c == '/' && keepSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func hexSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
