package storage

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// Official AWS SigV4 test-suite vectors (the "get-vanilla" family from
// aws-sig-v4-test-suite, service name "service"):
//
//	https://docs.aws.amazon.com/IAM/latest/UserGuide/create-signed-request.html
//
// They pin the full canonicalisation pipeline (request layout, header
// sorting, query sorting, HMAC chain) against known-good signatures.
var vectorCreds = sigV4Creds{
	AccessKeyID:     "AKIDEXAMPLE",
	SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
	Region:          "us-east-1",
	Service:         "service",
}

var vectorTime = time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)

func signatureOf(t *testing.T, auth string) string {
	t.Helper()
	i := strings.LastIndex(auth, "Signature=")
	if i < 0 {
		t.Fatalf("no Signature in %q", auth)
	}
	return auth[i+len("Signature="):]
}

func TestSigV4GetVanilla(t *testing.T) {
	auth, query := vectorCreds.sign(sigV4Request{
		Method: "GET",
		Path:   "/",
		Headers: map[string]string{
			"Host":       "example.amazonaws.com",
			"X-Amz-Date": "20150830T123600Z",
		},
		PayloadHash: emptySHA256,
		Timestamp:   vectorTime,
	})
	if query != "" {
		t.Errorf("query = %q, want empty", query)
	}
	want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
	if auth != want {
		t.Errorf("auth = %q\nwant %q", auth, want)
	}
}

func TestSigV4GetVanillaQueryOrder(t *testing.T) {
	// Unsorted input; the canonical query must sort by name.
	// The expected signature is cross-verified against an independent
	// SigV4 implementation (separate language, separate HMAC library)
	// over the same canonical request as the official test-suite
	// "get-vanilla-query-order" case.
	auth, query := vectorCreds.sign(sigV4Request{
		Method: "GET",
		Path:   "/",
		Query: url.Values{
			"Param2": {"value2"},
			"Param1": {"value1"},
		},
		Headers: map[string]string{
			"Host":       "example.amazonaws.com",
			"X-Amz-Date": "20150830T123600Z",
		},
		PayloadHash: emptySHA256,
		Timestamp:   vectorTime,
	})
	if query != "Param1=value1&Param2=value2" {
		t.Errorf("query = %q, want Param1=value1&Param2=value2", query)
	}
	if got := signatureOf(t, auth); got != "b97d918cfa904a5beff61c982a1b6f458b799221646efd99d3219ec94cdf2500" {
		t.Errorf("signature = %s\nwant b97d918cfa904a5beff61c982a1b6f458b799221646efd99d3219ec94cdf2500", got)
	}
}

// A signature must change when any signed input changes (guards
// against accidentally dropping a component from the canonical
// request).
func TestSigV4SignatureSensitivity(t *testing.T) {
	base := sigV4Request{
		Method: "PUT",
		Path:   "/bucket/abc.display",
		Headers: map[string]string{
			"Host":                 "example.amazonaws.com",
			"X-Amz-Date":           "20150830T123600Z",
			"X-Amz-Content-Sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
		PayloadHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Timestamp:   vectorTime,
	}
	baseAuth, _ := vectorCreds.sign(base)

	mut := func(name string, mutate func(*sigV4Request)) {
		t.Run(name, func(t *testing.T) {
			m := base
			mutate(&m)
			got, _ := vectorCreds.sign(m)
			if got == baseAuth {
				t.Errorf("signature unchanged after mutating %s", name)
			}
		})
	}
	mut("method", func(m *sigV4Request) { m.Method = "GET" })
	mut("path", func(m *sigV4Request) { m.Path = "/bucket/abc.thumb" })
	mut("query", func(m *sigV4Request) { m.Query = url.Values{"prefix": {"a/"}} })
	mut("header-value", func(m *sigV4Request) { m.Headers["X-Amz-Date"] = "20150830T123601Z" })
	mut("payload", func(m *sigV4Request) {
		m.PayloadHash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	})
	mut("timestamp", func(m *sigV4Request) { m.Timestamp = vectorTime.Add(time.Hour) })

	// Credential/region changes also change the signature.
	c2 := vectorCreds
	c2.Region = "eu-west-2"
	if got, _ := c2.sign(base); got == baseAuth {
		t.Error("signature unchanged after region change")
	}
	c3 := vectorCreds
	c3.SecretAccessKey = "OTHERSECRET"
	if got, _ := c3.sign(base); got == baseAuth {
		t.Error("signature unchanged after secret change")
	}
}

func TestRFC3986Encode(t *testing.T) {
	cases := []struct {
		in        string
		keepSlash bool
		want      string
	}{
		{"abc-123_.~", false, "abc-123_.~"}, // unreserved: untouched
		{"a b", false, "a%20b"},
		{"a/b", false, "a%2Fb"},
		{"a/b", true, "a/b"},
		{"héllo", false, "h%C3%A9llo"},
		{"$&+ =", false, "%24%26%2B%20%3D"},
	}
	for _, c := range cases {
		if got := rfc3986Encode(c.in, c.keepSlash); got != c.want {
			t.Errorf("rfc3986Encode(%q, keepSlash=%v) = %q, want %q", c.in, c.keepSlash, got, c.want)
		}
	}
}

func TestCanonicalURISegments(t *testing.T) {
	if got := canonicalURI("/bucket/abc def.display"); got != "/bucket/abc%20def.display" {
		t.Errorf("canonicalURI = %q", got)
	}
	if got := canonicalURI("/"); got != "/" {
		t.Errorf("root = %q", got)
	}
}

func TestNormalizeHeaderValue(t *testing.T) {
	if got := normalizeHeaderValue("  a   b \t c "); got != "a b c" {
		t.Errorf("normalize = %q", got)
	}
}
