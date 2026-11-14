package storage

// S3Storage implements Storage on an S3-compatible object store
// (AWS S3, Cloudflare R2, MinIO, ...). Requests are signed with SigV4
// (sigv4.go); no cloud SDK is used (ADR-0004, ADR-0012).
//
// Semantics match LocalFS exactly (same interface contract):
//   - content addressing: an existing key is never rewritten (Put skips
//     when present); originals are hash-verified against their key;
//   - OpenAt maps to a Range GET; an offset at/past the end yields a
//     reader that returns immediate EOF, as a local seek past EOF does;
//   - 404 maps to a wrapped ErrNotFound.
//
// All operations are idempotent (content addressing makes PUT
// idempotent too), so transient failures (network errors, 500/502/
// 503/504) are retried with a small backoff.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/matthewgall/openframe/internal/config"
)

// maxPutBytes bounds the (buffered) upload size. API-layer uploads are
// capped by server.max_upload_bytes (default 50 MiB); this is the
// storage-layer backstop.
const maxPutBytes = 512 * 1024 * 1024

// listPageCap bounds TotalUsage's pagination (1000 pages x 1000 keys =
// 1 million objects).
const listPageCap = 1000

// S3Storage is an S3-compatible Storage backend.
type S3Storage struct {
	client    *http.Client
	endpoint  *url.URL // service root (no bucket)
	creds     sigV4Creds
	bucket    string
	prefix    string
	pathStyle bool

	// backoff between attempts: len(backoff)+1 total attempts.
	backoff []time.Duration
	// clock is injectable for deterministic tests.
	clock func() time.Time

	// TotalUsage cache (30s TTL, parity with LocalFS).
	usageMu    sync.Mutex
	usageBytes int64
	usageAt    time.Time
	usageValid bool
}

// NewS3 builds an S3 backend from validated config.
func NewS3(cfg config.S3Config, httpClient *http.Client) (*S3Storage, error) {
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = "https://s3." + cfg.Region + ".amazonaws.com"
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("storage: s3 endpoint %q must be an absolute http(s) URL", endpoint)
	}
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("storage: s3 bucket is required")
	}
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &S3Storage{
		client:    httpClient,
		endpoint:  u,
		creds:     sigV4Creds{AccessKeyID: cfg.AccessKeyID, SecretAccessKey: cfg.SecretAccessKey, Region: cfg.Region},
		bucket:    cfg.Bucket,
		prefix:    strings.Trim(cfg.Prefix, "/"),
		pathStyle: cfg.PathStyle,
		backoff:   []time.Duration{250 * time.Millisecond, time.Second},
		clock:     time.Now,
	}, nil
}

// fullKey applies the optional namespace prefix.
func (s *S3Storage) fullKey(key Key) string {
	if s.prefix == "" {
		return string(key)
	}
	return s.prefix + "/" + string(key)
}

// buildRequest returns the request URL (absolute) for an object
// operation, choosing path-style vs virtual-host addressing.
// listObjects is true for ListObjectsV2 (query goes on the bucket).
func (s *S3Storage) buildRequest(key string, listObjects bool) (*url.URL, error) {
	u := *s.endpoint
	u.User = nil
	seg := func(parts ...string) string {
		out := ""
		for i, p := range parts {
			if i > 0 {
				out += "/"
			}
			out += rfc3986Encode(p, false)
		}
		return out
	}
	keyParts := strings.Split(key, "/")
	if s.pathStyle {
		if listObjects {
			u.Path = "/" + seg(s.bucket)
		} else {
			u.Path = "/" + seg(append([]string{s.bucket}, keyParts...)...)
		}
	} else {
		u.Host = s.bucket + "." + u.Host
		if key != "" {
			u.Path = "/" + seg(keyParts...)
		} else {
			u.Path = "/"
		}
	}
	return &u, nil
}

// do performs the (signed) request with bounded retries and returns the
// response for the caller to map. The body must be nil or already
// buffered (retries resend identical bytes).
func (s *S3Storage) do(ctx context.Context, method, key string, query url.Values, body []byte, payloadHash string, extraHeaders map[string]string) (*http.Response, error) {
	target, err := s.buildRequest(key, query != nil && query.Get("list-type") != "")
	if err != nil {
		return nil, err
	}
	attempts := len(s.backoff) + 1
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(s.backoff[attempt-1]):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		resp, opErr := s.attempt(ctx, method, target, query, body, payloadHash, extraHeaders)
		if opErr == nil && !isRetryableStatus(resp.StatusCode) {
			return resp, nil
		}
		if resp != nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			lastErr = fmt.Errorf("storage: s3 %s: HTTP %d", method, resp.StatusCode)
		} else {
			lastErr = opErr
		}
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	return nil, lastErr
}

func (s *S3Storage) attempt(ctx context.Context, method string, target *url.URL, query url.Values, body []byte, payloadHash string, extraHeaders map[string]string) (*http.Response, error) {
	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	} else {
		reqBody = http.NoBody
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), reqBody)
	if err != nil {
		return nil, err
	}
	now := s.clock().UTC()
	req.Header.Set("X-Amz-Date", now.Format("20060102T150405Z"))
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	req.Host = target.Host

	headers := map[string]string{
		"host":                 target.Host,
		"x-amz-date":           req.Header.Get("X-Amz-Date"),
		"x-amz-content-sha256": payloadHash,
	}
	for k, v := range extraHeaders {
		headers[strings.ToLower(k)] = v
	}
	auth, encodedQuery := s.creds.sign(sigV4Request{
		Method:      method,
		Path:        target.Path,
		Query:       query,
		Headers:     headers,
		PayloadHash: payloadHash,
		Timestamp:   now,
	})
	req.Header.Set("Authorization", auth)
	if encodedQuery != "" {
		req.URL.RawQuery = encodedQuery
	}
	return s.client.Do(req)
}

// isRetryableStatus reports whether a response status warrants a retry
// (safe here because every operation is idempotent).
func isRetryableStatus(code int) bool {
	return code == http.StatusBadGateway || code == http.StatusServiceUnavailable ||
		code == http.StatusGatewayTimeout || code == http.StatusInternalServerError
}

// Open implements Storage.
func (s *S3Storage) Open(ctx context.Context, key Key) (io.ReadCloser, error) {
	return s.OpenAt(ctx, key, 0)
}

// OpenAt implements Storage via a Range GET.
func (s *S3Storage) OpenAt(ctx context.Context, key Key, offset int64) (io.ReadCloser, error) {
	if offset < 0 {
		return nil, fmt.Errorf("storage: s3 %s: negative offset", key)
	}
	extra := map[string]string{}
	if offset > 0 {
		extra["Range"] = "bytes=" + strconv.FormatInt(offset, 10) + "-"
	}
	resp, err := s.do(ctx, http.MethodGet, s.fullKey(key), nil, nil, emptySHA256, extra)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
	case http.StatusNotFound:
		resp.Body.Close()
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	case http.StatusRequestedRangeNotSatisfiable:
		// Offset at/past the end: mirror a local seek past EOF — a
		// reader that returns immediate EOF.
		resp.Body.Close()
		return io.NopCloser(strings.NewReader("")), nil
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, s3Error(http.MethodGet, resp.StatusCode, body)
	}
	// A 200 (Range ignored by the server) must be corrected by
	// skipping to the requested offset.
	if resp.StatusCode == http.StatusOK && offset > 0 {
		if _, err := io.CopyN(io.Discard, resp.Body, offset); err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("storage: s3 %s: skip to offset: %w", key, err)
		}
	}
	return resp.Body, nil
}

// Size implements Storage via HEAD.
func (s *S3Storage) Size(ctx context.Context, key Key) (int64, error) {
	resp, err := s.do(ctx, http.MethodHead, s.fullKey(key), nil, nil, emptySHA256, nil)
	if err != nil {
		return 0, err
	}
	code := resp.StatusCode
	size := resp.ContentLength
	resp.Body.Close()
	switch code {
	case http.StatusOK:
		return size, nil
	case http.StatusNotFound:
		return 0, fmt.Errorf("%w: %s", ErrNotFound, key)
	default:
		return 0, s3Error(http.MethodHead, code, nil)
	}
}

// Exists implements Storage via HEAD.
func (s *S3Storage) Exists(ctx context.Context, key Key) (bool, error) {
	resp, err := s.do(ctx, http.MethodHead, s.fullKey(key), nil, nil, emptySHA256, nil)
	if err != nil {
		return false, err
	}
	code := resp.StatusCode
	resp.Body.Close()
	switch code {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, s3Error(http.MethodHead, code, nil)
	}
}

// Put implements Storage. The body is buffered (needed for the payload
// hash and idempotent retries); the size is capped at maxPutBytes.
func (s *S3Storage) Put(ctx context.Context, key Key, data io.Reader) error {
	buf, err := io.ReadAll(io.LimitReader(data, maxPutBytes+1))
	if err != nil {
		return fmt.Errorf("storage: s3 read %s: %w", key, err)
	}
	if len(buf) > maxPutBytes {
		return fmt.Errorf("storage: s3 %s: object exceeds %d bytes", key, maxPutBytes)
	}
	sum := sha256.Sum256(buf)
	incoming := hex.EncodeToString(sum[:])
	// Originals use bare-hash keys: the key IS the content hash, so the
	// written bytes must hash back to it (LocalFS parity).
	if strings.IndexRune(string(key), '.') < 0 && incoming != string(key) {
		return fmt.Errorf("storage: content hash mismatch for %s (got %s, want %s)", key, incoming, key)
	}
	// Existing object: keep it (LocalFS parity — an existing
	// content-addressed object is never rewritten).
	exists, err := s.Exists(ctx, key)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	resp, err := s.do(ctx, http.MethodPut, s.fullKey(key), nil, buf, incoming, nil)
	if err != nil {
		return err
	}
	code := resp.StatusCode
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if code != http.StatusOK {
		return s3Error(http.MethodPut, code, body)
	}
	s.usageMu.Lock()
	s.usageValid = false
	s.usageMu.Unlock()
	return nil
}

// TotalUsage implements Storage via ListObjectsV2 pagination (30s
// cached, LocalFS parity).
func (s *S3Storage) TotalUsage(ctx context.Context) (int64, error) {
	s.usageMu.Lock()
	if s.usageValid && s.clock().Sub(s.usageAt) < usageCacheTTL {
		v := s.usageBytes
		s.usageMu.Unlock()
		return v, nil
	}
	s.usageMu.Unlock()

	var total int64
	token := ""
	for page := 0; page < listPageCap; page++ {
		q := url.Values{}
		q.Set("list-type", "2")
		q.Set("max-keys", "1000")
		if s.prefix != "" {
			q.Set("prefix", s.prefix+"/")
		}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := s.do(ctx, http.MethodGet, "", q, nil, emptySHA256, nil)
		if err != nil {
			return 0, err
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if rerr != nil {
			return 0, rerr
		}
		if resp.StatusCode != http.StatusOK {
			return 0, s3Error(http.MethodGet, resp.StatusCode, body)
		}
		var lr listBucketResult
		if err := xml.Unmarshal(body, &lr); err != nil {
			return 0, fmt.Errorf("storage: s3 list: bad XML: %w", err)
		}
		for _, o := range lr.Contents {
			total += o.Size
		}
		if !lr.IsTruncated {
			break
		}
		token = lr.NextContinuationToken
	}

	s.usageMu.Lock()
	s.usageBytes = total
	s.usageAt = s.clock()
	s.usageValid = true
	s.usageMu.Unlock()
	return total, nil
}

// Ping performs the cheapest authenticated operation (a one-key
// ListObjectsV2) to verify endpoint, credentials, bucket and network
// reachability. Used by `openframe doctor`.
func (s *S3Storage) Ping(ctx context.Context) error {
	q := url.Values{}
	q.Set("list-type", "2")
	q.Set("max-keys", "1")
	if s.prefix != "" {
		q.Set("prefix", s.prefix+"/")
	}
	resp, err := s.do(ctx, http.MethodGet, "", q, nil, emptySHA256, nil)
	if err != nil {
		return err
	}
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if rerr != nil {
		return rerr
	}
	if resp.StatusCode != http.StatusOK {
		return s3Error(http.MethodGet, resp.StatusCode, body)
	}
	return nil
}

type listBucketResult struct {
	XMLName               xml.Name     `xml:"ListBucketResult"`
	IsTruncated           bool         `xml:"IsTruncated"`
	NextContinuationToken string       `xml:"NextContinuationToken"`
	Contents              []listObject `xml:"Contents"`
}

type listObject struct {
	Key  string `xml:"Key"`
	Size int64  `xml:"Size"`
}

func s3Error(method string, code int, body []byte) error {
	msg := http.StatusText(code)
	// S3 error bodies carry a <Code> worth surfacing (NoSuchBucket,
	// AccessDenied, ...).
	if i := strings.Index(string(body), "<Code>"); i >= 0 {
		if j := strings.Index(string(body[i:]), "</Code>"); j >= 0 {
			msg = string(body[i+len("<Code>"):i+j]) + " (" + msg + ")"
		}
	}
	return fmt.Errorf("storage: s3 %s failed: %s", method, msg)
}
