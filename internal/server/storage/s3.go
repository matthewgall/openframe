package storage

// S3Storage implements Storage on an S3-compatible object store
// (AWS S3, Cloudflare R2, MinIO, ...) using the official AWS SDK for
// Go v2 (github.com/aws/aws-sdk-go-v2/service/s3). The SDK is pure Go
// (no cgo), so the cross-compile matrix is unaffected.
//
// Semantics match LocalFS exactly (same interface contract):
//   - content addressing: an existing key is never rewritten (Put skips
//     when present); originals are hash-verified against their key;
//   - OpenAt maps to a ranged GET; an offset at/past the end yields a
//     reader that returns immediate EOF, as a local seek past EOF does;
//   - 404 maps to a wrapped ErrNotFound.
//
// The AWS configuration is hermetic: static credentials and an
// explicit region only — no env-var, shared-credentials-file, or IMDS
// discovery. The server talks to exactly the store named in
// OpenFrame's config and nothing else.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/matthewgall/openframe/internal/config"
)

// maxPutBytes bounds the (buffered) upload size. API-layer uploads are
// capped by server.max_upload_bytes (default 50 MiB); this is the
// storage-layer backstop.
const maxPutBytes = 512 * 1024 * 1024

// S3Storage is an S3-compatible Storage backend.
type S3Storage struct {
	client *s3.Client
	bucket string
	prefix string

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
	return newS3(cfg, httpClient, nil)
}

// newS3 is NewS3 with an optional aws.Config mutator (used by tests to
// shorten retry delays; production passes nil).
func newS3(cfg config.S3Config, httpClient *http.Client, mutate func(*aws.Config)) (*S3Storage, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("storage: s3 bucket is required")
	}
	// An empty endpoint means AWS: the SDK derives the regional
	// endpoint from the region. A provided endpoint (R2, MinIO, ...)
	// must be an absolute http(s) URL.
	if cfg.Endpoint != "" {
		u, err := url.Parse(cfg.Endpoint)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("storage: s3 endpoint %q must be an absolute http(s) URL", cfg.Endpoint)
		}
	}

	awsCfg := aws.Config{
		Region:      cfg.Region,
		RetryMode:   aws.RetryModeStandard,
		Credentials: credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
	}
	if httpClient != nil {
		awsCfg.HTTPClient = httpClient
	}
	if mutate != nil {
		mutate(&awsCfg)
	}

	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.PathStyle
	})

	return &S3Storage{
		client: client,
		bucket: cfg.Bucket,
		prefix: strings.Trim(cfg.Prefix, "/"),
		clock:  time.Now,
	}, nil
}

// fullKey applies the optional namespace prefix.
func (s *S3Storage) fullKey(key Key) string {
	if s.prefix == "" {
		return string(key)
	}
	return s.prefix + "/" + string(key)
}

// listPrefix is the prefix filter for List operations (the namespace,
// including its trailing slash; empty lists the whole bucket).
func (s *S3Storage) listPrefix() *string {
	if s.prefix == "" {
		return nil
	}
	return aws.String(s.prefix + "/")
}

// s3ErrClass maps an SDK error to the storage contract:
// notFound for 404s, pastEnd for a 416 on a ranged GET, otherwise the
// error is returned unchanged.
func s3ErrClass(err error) (errOut error, notFound, pastEnd bool) {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return err, false, false
	}
	switch apiErr.ErrorCode() {
	case "NoSuchKey", "NotFound":
		return nil, true, false
	case "InvalidRange", "RequestedRangeNotSatisfiable":
		// AWS returns InvalidRange; non-conformant stores may surface
		// the HTTP status phrase instead. Both mean "offset past end".
		return nil, false, true
	}
	return err, false, false
}

// Put stores data under key (see the Storage contract).
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
	if _, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.fullKey(key)),
		Body:   bytes.NewReader(buf),
	}); err != nil {
		return fmt.Errorf("storage: s3 put %s: %w", key, err)
	}
	s.usageMu.Lock()
	s.usageValid = false
	s.usageMu.Unlock()
	return nil
}

// OpenAt returns a reader starting at offset (see the Storage
// contract). The read is a ranged GET; an offset at or past the object
// end yields a reader that returns immediate EOF (S3 answers 416,
// which maps to the same behaviour as a local seek past EOF).
func (s *S3Storage) OpenAt(ctx context.Context, key Key, offset int64) (io.ReadCloser, error) {
	in := &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.fullKey(key)),
	}
	if offset > 0 {
		in.Range = aws.String(fmt.Sprintf("bytes=%d-", offset))
	}
	out, err := s.client.GetObject(ctx, in)
	if err != nil {
		wrapped, notFound, pastEnd := s3ErrClass(err)
		switch {
		case notFound:
			return nil, fmt.Errorf("storage: object %s: %w", key, ErrNotFound)
		case pastEnd:
			return io.NopCloser(bytes.NewReader(nil)), nil
		default:
			return nil, fmt.Errorf("storage: s3 get %s: %w", key, wrapped)
		}
	}
	return out.Body, nil
}

// Open reads the whole object from its start.
func (s *S3Storage) Open(ctx context.Context, key Key) (io.ReadCloser, error) {
	return s.OpenAt(ctx, key, 0)
}

// Size returns the object size.
func (s *S3Storage) Size(ctx context.Context, key Key) (int64, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.fullKey(key)),
	})
	if err != nil {
		wrapped, notFound, _ := s3ErrClass(err)
		if notFound {
			return 0, fmt.Errorf("storage: object %s: %w", key, ErrNotFound)
		}
		return 0, fmt.Errorf("storage: s3 head %s: %w", key, wrapped)
	}
	return aws.ToInt64(out.ContentLength), nil
}

// Exists reports whether the object exists.
func (s *S3Storage) Exists(ctx context.Context, key Key) (bool, error) {
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.fullKey(key)),
	})
	if err == nil {
		return true, nil
	}
	_, notFound, _ := s3ErrClass(err)
	if notFound {
		return false, nil
	}
	return false, fmt.Errorf("storage: s3 head %s: %w", key, err)
}

// TotalUsage sums the sizes of all objects (30s cached, same as the
// local backend).
func (s *S3Storage) TotalUsage(ctx context.Context) (int64, error) {
	now := s.clock()
	s.usageMu.Lock()
	if s.usageValid && now.Sub(s.usageAt) < usageCacheTTL {
		defer s.usageMu.Unlock()
		return s.usageBytes, nil
	}
	s.usageMu.Unlock()

	p := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: s.listPrefix(),
	})
	var total int64
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return 0, fmt.Errorf("storage: s3 list: %w", err)
		}
		for _, obj := range page.Contents {
			total += aws.ToInt64(obj.Size)
		}
	}
	s.usageMu.Lock()
	s.usageBytes, s.usageAt, s.usageValid = total, now, true
	s.usageMu.Unlock()
	return total, nil
}

// Ping performs the cheapest authenticated operation (a one-key
// ListObjectsV2) to verify endpoint, credentials, bucket and network
// reachability. Used by `openframe doctor`.
func (s *S3Storage) Ping(ctx context.Context) error {
	if _, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:  aws.String(s.bucket),
		Prefix:  s.listPrefix(),
		MaxKeys: aws.Int32(1),
	}); err != nil {
		return err
	}
	return nil
}
