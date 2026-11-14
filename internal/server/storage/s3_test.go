package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awretry "github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/matthewgall/openframe/internal/config"
)

// fakeS3 is an in-memory S3-compatible server for testing S3Storage.
// It validates the SigV4 shape of EVERY request (Authorization layout,
// X-Amz-Date, and — for PUT — that X-Amz-Content-Sha256 matches the
// actual body hash), so a signing regression fails every test, not
// just the vector tests.
type fakeS3 struct {
	t *testing.T
	// mu guards objs; failMu guards the counters only — never held at the
	// same time as mu (fail() can be called while mu is held).
	mu       sync.Mutex
	failMu   sync.Mutex
	objs     map[string][]byte
	bucket   string
	pageSize int // forced List page size (exercises pagination)
	fail503  int // remaining 503s to return before serving

	requests     int
	putRequests  int
	authFailures int
	virtual      int // object ops received via virtual-host addressing
}

var authRe = regexp.MustCompile(`^AWS4-HMAC-SHA256 Credential=AKID/(\d{8})/(\w[\w-]*)/s3/aws4_request, SignedHeaders=[a-z0-9;-]+, Signature=[0-9a-f]{64}$`)

func newFakeS3(t *testing.T, bucket string, pageSize int) (*fakeS3, *httptest.Server) {
	t.Helper()
	f := &fakeS3{t: t, objs: map[string][]byte{}, bucket: bucket, pageSize: pageSize}
	ts := httptest.NewServer(http.HandlerFunc(f.ServeHTTP))
	t.Cleanup(ts.Close)
	return f, ts
}

// newFakeS3TLS is newFakeS3 over TLS — needed when the client dials an
// https:// endpoint (AWS-shaped) while actually hitting the test server.
func newFakeS3TLS(t *testing.T, bucket string, pageSize int) (*fakeS3, *httptest.Server) {
	t.Helper()
	f := &fakeS3{t: t, objs: map[string][]byte{}, bucket: bucket, pageSize: pageSize}
	ts := httptest.NewTLSServer(http.HandlerFunc(f.ServeHTTP))
	t.Cleanup(ts.Close)
	return f, ts
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests++
	if f.fail503 > 0 {
		f.fail503--
		f.mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, "<Error><Code>SlowDown</Code></Error>")
		return
	}
	f.mu.Unlock()

	// --- SigV4 shape validation (failure = test failure) ---
	fail := func(format string, args ...any) {
		f.failMu.Lock()
		f.authFailures++
		f.failMu.Unlock()
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, "<Error><Code>SignatureDoesNotMatch</Code><Message>%v</Message></Error>", fmt.Sprintf(format, args...))
		f.t.Errorf("request failed SigV4 validation: %v", fmt.Sprintf(format, args...))
	}
	auth := r.Header.Get("Authorization")
	if m := authRe.FindStringSubmatch(auth); m == nil {
		fail("bad Authorization header: %q", auth)
		return
	}
	if d := r.Header.Get("X-Amz-Date"); !regexp.MustCompile(`^\d{8}T\d{6}Z$`).MatchString(d) {
		fail("bad X-Amz-Date: %q", d)
		return
	}
	// host is a required signed header for S3 requests.
	if !strings.Contains(auth, "SignedHeaders=") || !strings.Contains(auth, "host") {
		fail("SignedHeaders missing host")
		return
	}

	// --- Routing (path-style or virtual-host) ---
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	var keyPath string
	if len(parts) > 0 && parts[0] == f.bucket {
		keyPath = strings.Join(parts[1:], "/") // path-style: bucket in path
	} else if strings.HasPrefix(r.Host, f.bucket+".") {
		keyPath = strings.Join(parts, "/") // virtual-host: bucket in Host
		f.mu.Lock()
		f.virtual++
		f.mu.Unlock()
	} else {
		fail("addressing: path %q host %q — bucket %q in neither", r.URL.Path, r.Host, f.bucket)
		return
	}
	isList := r.URL.Query().Get("list-type") == "2"

	f.mu.Lock()
	defer f.mu.Unlock()

	switch {
	case isList:
		f.serveList(w, r)
	case r.Method == http.MethodPut:
		f.putRequests++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			fail("read body: %v", err)
			return
		}
		sum := sha256.Sum256(body)
		gotHash := r.Header.Get("X-Amz-Content-Sha256")
		wantHash := hex.EncodeToString(sum[:])
		// Over TLS, S3 clients may sign with UNSIGNED-PAYLOAD (integrity
		// comes from TLS); plain-HTTP uploads carry the real body hash.
		if gotHash != wantHash && gotHash != "UNSIGNED-PAYLOAD" {
			fail("payload hash %s does not match body hash %s (or UNSIGNED-PAYLOAD)", gotHash, wantHash)
			return
		}
		f.objs[keyPath] = body
		w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
	case r.Method == http.MethodGet:
		key := keyPath
		obj, ok := f.objs[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, "<Error><Code>NoSuchKey</Code></Error>")
			return
		}
		if rng := r.Header.Get("Range"); rng != "" {
			// Range: bytes=N-
			var off int64
			if _, err := fmt.Sscanf(rng, "bytes=%d-", &off); err != nil {
				fail("bad Range %q", rng)
				return
			}
			if off >= int64(len(obj)) {
				// S3 answers a non-satisfiable range with 416 + error XML.
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				fmt.Fprint(w, `<Error><Code>InvalidRange</Code><Message>Requested range not satisfiable</Message></Error>`)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", off, int64(len(obj))-1, len(obj)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(obj[off:])
			return
		}
		w.Write(obj)
	case r.Method == http.MethodHead:
		key := keyPath
		obj, ok := f.objs[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(obj)))
	default:
		fail("unexpected method %s", r.Method)
	}
}

func (f *fakeS3) serveList(w http.ResponseWriter, r *http.Request) {
	// Caller's prefix filter (the backend sends the namespace prefix).
	prefix := r.URL.Query().Get("prefix")
	keys := make([]string, 0, len(f.objs))
	for k := range f.objs {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	tokenOffset := 0
	if tok := r.URL.Query().Get("continuation-token"); tok != "" {
		fmt.Sscanf(tok, "tok-%d", &tokenOffset)
	}
	end := tokenOffset + f.pageSize
	if end > len(keys) {
		end = len(keys)
	}
	truncated := end < len(keys)
	page := keys[tokenOffset:end]

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	b.WriteString(`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
	fmt.Fprintf(&b, `<Name>%s</Name><Prefix>%s</Prefix><KeyCount>%d</KeyCount><MaxKeys>%d</MaxKeys><IsTruncated>%v</IsTruncated>`,
		f.bucket, prefix, len(page), f.pageSize, truncated)
	if truncated {
		fmt.Fprintf(&b, `<NextContinuationToken>tok-%d</NextContinuationToken>`, end)
	}
	for _, k := range page {
		fmt.Fprintf(&b, `<Contents><Key>%s</Key><LastModified>2026-01-01T00:00:00Z</LastModified><ETag>"x"</ETag><Size>%d</Size><StorageClass>STANDARD</StorageClass></Contents>`, k, len(f.objs[k]))
	}
	b.WriteString(`</ListBucketResult>`)
	w.Write([]byte(b.String()))
}

// newTestS3 wires an S3Storage against a fake (path-style, like R2).
func newTestS3(t *testing.T, mut func(*config.S3Config)) (*S3Storage, *fakeS3) {
	t.Helper()
	cfg := config.S3Config{
		Endpoint:        "", // set after server exists
		Region:          "auto",
		Bucket:          "test-bucket",
		AccessKeyID:     "AKID",
		SecretAccessKey: "SECRET",
		PathStyle:       true,
	}
	if mut != nil {
		mut(&cfg)
	}
	f, ts := newFakeS3(t, cfg.Bucket, 2)
	cfg.Endpoint = ts.URL
	s, err := newS3(cfg, ts.Client(), func(c *aws.Config) {
		c.Retryer = func() aws.Retryer {
			return fastRetryer{inner: awretry.NewStandard(func(o *awretry.StandardOptions) { o.MaxAttempts = 3 }), delay: 2 * time.Millisecond}
		}
	})
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	return s, f
}

// fastRetryer wraps the SDK's standard retryer with a fixed short
// delay so retry tests run in milliseconds.
type fastRetryer struct {
	inner aws.Retryer
	delay time.Duration
}

func (f fastRetryer) IsErrorRetryable(err error) bool { return f.inner.IsErrorRetryable(err) }
func (f fastRetryer) MaxAttempts() int                { return f.inner.MaxAttempts() }
func (f fastRetryer) RetryDelay(int, error) (time.Duration, error) {
	return f.delay, nil
}
func (f fastRetryer) GetRetryToken(ctx context.Context, opErr error) (func(error) error, error) {
	return f.inner.GetRetryToken(ctx, opErr)
}
func (f fastRetryer) GetInitialToken() func(error) error { return f.inner.GetInitialToken() }

// hashKey returns a key that IS the sha256 of data (content-addressed).
func hashKey(t *testing.T, data []byte) Key {
	t.Helper()
	sum := sha256.Sum256(data)
	return Key(hex.EncodeToString(sum[:]))
}

func TestS3PutGetRoundTrip(t *testing.T) {
	s, f := newTestS3(t, nil)
	ctx := context.Background()

	data := []byte("original photo bytes")
	key := hashKey(t, data)
	if err := s.Put(ctx, key, bytes.NewReader(data)); err != nil {
		t.Fatalf("Put original: %v", err)
	}
	deriv := []byte("display derivative bytes")
	dkey := Key(string(key) + "." + VariantDisplay)
	if err := s.Put(ctx, dkey, bytes.NewReader(deriv)); err != nil {
		t.Fatalf("Put derivative: %v", err)
	}

	got, err := io.ReadAll(mustOpen(t, s, ctx, key))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("Open original = %q, %v; want %q", got, err, data)
	}
	got, err = io.ReadAll(mustOpen(t, s, ctx, dkey))
	if err != nil || !bytes.Equal(got, deriv) {
		t.Fatalf("Open derivative = %q, %v", got, err)
	}

	size, err := s.Size(ctx, key)
	if err != nil || size != int64(len(data)) {
		t.Fatalf("Size = %d, %v; want %d", size, err, len(data))
	}
	if ok, _ := s.Exists(ctx, dkey); !ok {
		t.Fatal("Exists(derivative) = false, want true")
	}
	total, err := s.TotalUsage(ctx)
	if err != nil || total != int64(len(data)+len(deriv)) {
		t.Fatalf("TotalUsage = %d, %v; want %d", total, err, len(data)+len(deriv))
	}
	f.mu.Lock()
	requests := f.requests
	f.mu.Unlock()
	f.failMu.Lock()
	authFailures := f.authFailures
	f.failMu.Unlock()
	if requests == 0 || authFailures != 0 {
		t.Fatalf("requests=%d authFailures=%d", requests, authFailures)
	}
}

func mustOpen(t *testing.T, s *S3Storage, ctx context.Context, key Key) io.ReadCloser {
	t.Helper()
	rc, err := s.Open(ctx, key)
	if err != nil {
		t.Fatalf("Open(%s): %v", key, err)
	}
	return rc
}

func TestS3PutHashMismatch(t *testing.T) {
	s, f := newTestS3(t, nil)
	ctx := context.Background()
	data := []byte("some bytes")
	// A bare-hash key whose value does not match the content hash.
	err := s.Put(ctx, "deadbeef", bytes.NewReader(data))
	if err == nil || !strings.Contains(err.Error(), "content hash mismatch") {
		t.Fatalf("Put mismatched original: err = %v, want content hash mismatch", err)
	}
	if len(f.objs) != 0 {
		t.Fatalf("object stored despite hash mismatch: %v", f.objs)
	}
}

func TestS3PutDedupSkips(t *testing.T) {
	s, f := newTestS3(t, nil)
	ctx := context.Background()
	data := []byte("same content twice")
	key := hashKey(t, data)
	for i := 0; i < 2; i++ {
		if err := s.Put(ctx, key, bytes.NewReader(data)); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	f.mu.Lock()
	puts := f.putRequests
	f.mu.Unlock()
	if puts != 1 {
		t.Fatalf("PUT requests = %d, want 1 (second Put must skip)", puts)
	}
}

func TestS3PutExistingDerivativeKept(t *testing.T) {
	s, f := newTestS3(t, nil)
	ctx := context.Background()
	data := []byte("original")
	key := hashKey(t, data)
	_ = s.Put(ctx, key, bytes.NewReader(data))

	dkey := Key(string(key) + "." + VariantDisplay)
	first := []byte("first derivative")
	_ = s.Put(ctx, dkey, bytes.NewReader(first))
	f.mu.Lock()
	putsBefore := f.putRequests
	f.mu.Unlock()

	// A re-encode (different bytes, same key) must NOT replace the
	// stored object (LocalFS parity: existing objects are kept).
	if err := s.Put(ctx, dkey, bytes.NewReader([]byte("second derivative"))); err != nil {
		t.Fatalf("Put existing derivative: %v", err)
	}
	f.mu.Lock()
	puts := f.putRequests
	stored := f.objs[string(dkey)]
	f.mu.Unlock()
	if puts != putsBefore {
		t.Fatalf("PUT requests grew to %d, want %d (skip expected)", puts, putsBefore)
	}
	if !bytes.Equal(stored, first) {
		t.Fatalf("stored derivative = %q, want %q (existing object must be kept)", stored, first)
	}
}

func TestS3OpenAt(t *testing.T) {
	s, _ := newTestS3(t, nil)
	ctx := context.Background()
	data := []byte("0123456789")
	key := hashKey(t, data)
	_ = s.Put(ctx, key, bytes.NewReader(data))

	// Offset mid-object.
	rc, err := s.OpenAt(ctx, key, 4)
	if err != nil {
		t.Fatalf("OpenAt(4): %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if string(got) != "456789" {
		t.Fatalf("OpenAt(4) = %q, want 456789", got)
	}
	// Offset 0 == Open.
	rc, _ = s.OpenAt(ctx, key, 0)
	got, _ = io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) {
		t.Fatalf("OpenAt(0) = %q", got)
	}
	// Past the end: immediate EOF (LocalFS seek-past-EOF parity).
	rc, err = s.OpenAt(ctx, key, 999)
	if err != nil {
		t.Fatalf("OpenAt(999): %v", err)
	}
	got, err = io.ReadAll(rc)
	rc.Close()
	if len(got) != 0 || err != nil {
		t.Fatalf("OpenAt(999) = %q, %v; want empty, nil", got, err)
	}
	// Missing object.
	if _, err := s.OpenAt(ctx, "nope", 0); err == nil || !errorsIsNotFound(err) {
		t.Fatalf("OpenAt(missing) err = %v, want ErrNotFound", err)
	}
}

func TestS3SizeExistsNotFound(t *testing.T) {
	s, _ := newTestS3(t, nil)
	ctx := context.Background()
	if _, err := s.Size(ctx, "absent"); err == nil || !errorsIsNotFound(err) {
		t.Fatalf("Size(absent) = %v, want ErrNotFound", err)
	}
	if ok, err := s.Exists(ctx, "absent"); err != nil || ok {
		t.Fatalf("Exists(absent) = %v, %v; want false, nil", ok, err)
	}
}

func TestS3RetriesOn503(t *testing.T) {
	s, f := newTestS3(t, nil)
	ctx := context.Background()
	data := []byte("retry me")
	key := hashKey(t, data)
	_ = s.Put(ctx, key, bytes.NewReader(data))

	// Two 503s then success: the operation must succeed.
	f.fail503 = 2
	rc, err := s.Open(ctx, key)
	if err != nil {
		t.Fatalf("Open after 503s: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) {
		t.Fatalf("content = %q", got)
	}

	// More 503s than attempts: must fail with the 503.
	f.fail503 = 99
	if _, err := s.Size(ctx, key); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("Size with persistent 503 = %v, want 503 error", err)
	}
	f.fail503 = 0 // stop failing before the 404 check

	// 404 must NOT be retried (single server hit).
	f.mu.Lock()
	before := f.requests
	f.mu.Unlock()
	if _, err := s.Size(ctx, "absent"); err == nil || !errorsIsNotFound(err) {
		t.Fatalf("Size(absent) = %v", err)
	}
	f.mu.Lock()
	hits := f.requests - before
	f.mu.Unlock()
	if hits != 1 {
		t.Fatalf("404 produced %d server hits, want 1 (no retry on 404)", hits)
	}
}

func TestS3TotalUsagePagination(t *testing.T) {
	s, f := newTestS3(t, nil) // fake pages at 2 objects
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		data := []byte(fmt.Sprintf("object %d payload", i))
		key := hashKey(t, data)
		if err := s.Put(ctx, key, bytes.NewReader(data)); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
	}
	total, err := s.TotalUsage(ctx)
	if err != nil {
		t.Fatalf("TotalUsage: %v", err)
	}
	want := int64(0)
	for i := 0; i < 5; i++ {
		want += int64(len(fmt.Sprintf("object %d payload", i)))
	}
	if total != want {
		t.Fatalf("TotalUsage = %d, want %d", total, want)
	}
	f.mu.Lock()
	reqs := f.requests
	f.mu.Unlock()
	// 5 Puts + 5 HEADs (dedup checks) + 3 List pages (2+2+1).
	if reqs != 13 {
		t.Fatalf("requests = %d, want 13 (5 put + 5 head + 3 list pages)", reqs)
	}
	// Cached: a second call within the TTL adds no requests.
	if _, err := s.TotalUsage(ctx); err != nil {
		t.Fatalf("TotalUsage(2nd): %v", err)
	}
	f.mu.Lock()
	reqs2 := f.requests
	f.mu.Unlock()
	if reqs2 != reqs {
		t.Fatalf("cached TotalUsage added %d requests, want 0", reqs2-reqs)
	}
}

func TestS3Prefix(t *testing.T) {
	s, f := newTestS3(t, func(c *config.S3Config) { c.Prefix = "openframe/prod" })
	ctx := context.Background()
	data := []byte("namespaced")
	key := hashKey(t, data)
	if err := s.Put(ctx, key, bytes.NewReader(data)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	f.mu.Lock()
	_, ok := f.objs["openframe/prod/"+string(key)]
	f.mu.Unlock()
	if !ok {
		t.Fatalf("object not stored under prefix: keys = %v", keysOf(f))
	}
	total, err := s.TotalUsage(ctx)
	if err != nil || total != int64(len(data)) {
		t.Fatalf("TotalUsage = %d, %v; want %d", total, err, len(data))
	}
}

func keysOf(f *fakeS3) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.objs))
	for k := range f.objs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestS3Validation checks NewS3's config validation.
func TestS3Validation(t *testing.T) {
	if _, err := NewS3(config.S3Config{Endpoint: "https://x", Region: "auto", AccessKeyID: "a", SecretAccessKey: "s"}, nil); err == nil {
		t.Error("missing bucket: expected error")
	}
	if _, err := NewS3(config.S3Config{Endpoint: "not a url", Region: "auto", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s"}, nil); err == nil {
		t.Error("bad endpoint: expected error")
	}
	// Empty endpoint is valid (AWS regional endpoint, derived by the SDK
	// from the region).
	if _, err := NewS3(config.S3Config{Region: "eu-central-1", Bucket: "b", AccessKeyID: "a", SecretAccessKey: "s"}, nil); err != nil {
		t.Fatalf("empty endpoint should be valid (AWS): %v", err)
	}
}

// hostRewritingRT dials a fixed host while preserving the request's
// intended Host header. This lets virtual-host addressing be tested
// against an httptest server, whose bucket subdomain would not resolve.
type hostRewritingRT struct {
	to string
	rt http.RoundTripper // must trust the test server's certificate
}

func (h hostRewritingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	if clone.Host == "" {
		clone.Host = clone.URL.Host // pin the intended virtual-host header
	}
	clone.URL.Host = h.to
	return h.rt.RoundTrip(clone)
}

// TestS3VirtualHostAddressing verifies that with an AWS endpoint and
// path_style: false the SDK addresses objects via the Host header
// (bucket.<endpoint-host>) with the bucket OUT of the path — the AWS
// S3 style. (For non-AWS endpoints the SDK always uses path-style,
// which is what R2/MinIO want — covered by every other test here.)
//
// The client is pointed at an AWS-shaped endpoint but a host-rewriting
// transport dials the TLS test server instead (the bucket subdomain of
// an httptest host would not resolve).
func TestS3VirtualHostAddressing(t *testing.T) {
	cfg := config.S3Config{
		Region: "us-east-1", Bucket: "test-bucket",
		AccessKeyID: "AKID", SecretAccessKey: "SECRET", PathStyle: false,
	}
	f, ts := newFakeS3TLS(t, cfg.Bucket, 100)
	cfg.Endpoint = "https://s3.us-east-1.amazonaws.com"
	host := strings.TrimPrefix(ts.URL, "https://")
	base, ok := ts.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("unexpected test transport %T", ts.Client().Transport)
	}
	hc := &http.Client{Transport: hostRewritingRT{to: host, rt: base.Clone()}}
	s, err := newS3(cfg, hc, func(c *aws.Config) {
		c.Retryer = func() aws.Retryer {
			return fastRetryer{inner: awretry.NewStandard(func(o *awretry.StandardOptions) { o.MaxAttempts = 3 }), delay: time.Millisecond}
		}
	})
	if err != nil {
		t.Fatalf("newS3: %v", err)
	}
	ctx := context.Background()

	data := []byte("virtual host bytes")
	key := hashKey(t, data)
	if err := s.Put(ctx, key, bytes.NewReader(data)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := io.ReadAll(mustOpen(t, s, ctx, key))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("Open = %q, %v", got, err)
	}

	f.mu.Lock()
	virtual, total := f.virtual, f.requests
	f.mu.Unlock()
	if virtual == 0 {
		t.Fatalf("no virtual-host requests received (requests=%d); SDK used path-style", total)
	}
}

func errorsIsNotFound(err error) bool {
	return err != nil && (err == ErrNotFound || strings.Contains(err.Error(), "object not found"))
}
