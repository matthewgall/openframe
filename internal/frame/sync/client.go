package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/matthewgall/openframe/internal/protocol"
)

// Client is the frame's HTTP client for the management server's frame
// API (spec §9). All communication is initiated by the frame (spec
// §12); the server never connects out.
//
// The client is deliberately thin: retry, scheduling, verification and
// state transitions live in the engine so they can be tested against
// scripted servers.
type Client struct {
	base  string // e.g. https://frame.example.com
	token string // device credential (never logged)
	httpc *http.Client
}

// NewClient builds a client. token may be empty (unpaired frame);
// request methods then fail fast with ErrUnpaired without dialling.
func NewClient(baseURL, token string) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("sync: invalid server url %q", baseURL)
	}
	return &Client{
		base:  strings.TrimSuffix(u.String(), "/"),
		token: token,
		httpc: &http.Client{Timeout: 2 * time.Minute},
	}, nil
}

// ErrUnpaired is returned when the frame has no device credential.
var ErrUnpaired = errors.New("sync: frame is not paired (no device credential)")

// ErrServerDown is a connectivity-level failure (DNS, refused,
// timeout) as opposed to an HTTP error status. The engine maps it to
// backoff without counting it as a protocol error.
type ErrServerDown struct{ Cause error }

func (e *ErrServerDown) Error() string { return "server unreachable: " + e.Cause.Error() }
func (e *ErrServerDown) Unwrap() error { return e.Cause }

// HTTPError is a non-2xx response with the server's error code.
type HTTPError struct {
	Status int
	Code   string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("server returned %d (%s)", e.Status, e.Code)
}

// IsNotFound reports whether err is a 404 (e.g. device unknown or
// credential not yet issued server-side).
func IsNotFound(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status == http.StatusNotFound
}

// IsUnauthorized reports whether err is a 401 (bad/revoked credential).
func IsUnauthorized(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status == http.StatusUnauthorized
}

func (c *Client) requirePairing() error {
	if c.token == "" {
		return ErrUnpaired
	}
	return nil
}

func (c *Client) manifestPath(deviceID string) string {
	return c.base + protocol.APIPath + "/frames/" + deviceID + "/manifest"
}

// ReportHealth posts the frame's health report (M5, spec §13) and
// returns the server's remote-sync request sequence. A new sequence
// means an administrator triggered a synchronisation (spec §6).
func (c *Client) ReportHealth(ctx context.Context, deviceID string, payload []byte) (int64, error) {
	if err := c.requirePairing(); err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+protocol.APIPath+"/frames/"+deviceID+"/health",
		bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)

	resp, err := c.httpc.Do(req)
	if err != nil {
		return 0, &ErrServerDown{Cause: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return 0, c.httpError(resp)
	}
	var out struct {
		SyncRequestSeq int64 `json:"sync_request_seq"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, fmt.Errorf("decode health response: %w", err)
	}
	return out.SyncRequestSeq, nil
}

// ManifestResult is the outcome of a conditional manifest fetch.
type ManifestResult struct {
	// Modified is false on 304 (content unchanged since etag).
	Modified bool
	ETag     string
	Body     []byte
	Protocol string
	Revision int64
}

// GetManifest fetches the manifest, using etag ("" for the first
// fetch) for the conditional request.
func (c *Client) GetManifest(ctx context.Context, deviceID, etag string) (*ManifestResult, error) {
	if err := c.requirePairing(); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.manifestPath(deviceID), nil)
	if err != nil {
		return nil, err
	}
	c.authorize(req)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, &ErrServerDown{Cause: err}
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotModified:
		return &ManifestResult{Modified: false, ETag: etag}, nil
	default:
		return nil, c.httpError(resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var probe struct {
		Protocol string `json:"protocol"`
		Revision int64  `json:"revision"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return nil, fmt.Errorf("malformed manifest: %w", err)
	}
	return &ManifestResult{
		Modified: true,
		ETag:     resp.Header.Get("ETag"),
		Body:     body,
		Protocol: probe.Protocol,
		Revision: probe.Revision,
	}, nil
}

// OpenDownload streams a manifest resource into w, resuming from
// offset when > 0 (server single-range support). It returns the
// number of bytes written this call and whether the server answered
// with a partial (206) body.
//
// Size discipline: at most expectedSize-offset bytes are written; a
// server that overflows the declared size causes an error so the
// engine can discard the partial file (spec §9: "Enforce download
// size limits").
func (c *Client) OpenDownload(ctx context.Context, deviceID, resource string, offset int64, w io.Writer, expectedSize int64) (int64, bool, error) {
	if err := c.requirePairing(); err != nil {
		return 0, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.base+protocol.APIPath+"/frames/"+deviceID+"/photos/"+resource, nil)
	if err != nil {
		return 0, false, err
	}
	c.authorize(req)
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return 0, false, &ErrServerDown{Cause: err}
	}
	defer resp.Body.Close()

	var total int64
	partial := false
	switch resp.StatusCode {
	case http.StatusOK:
		total, _ = parseContentLength(resp.Header.Get("Content-Length"))
	case http.StatusPartialContent:
		partial = true
		total = parseContentRangeTotal(resp.Header.Get("Content-Range"))
		if total == -1 {
			total = expectedSize
		}
	default:
		return 0, false, c.httpError(resp)
	}
	if expectedSize > 0 && total > expectedSize {
		return 0, partial, fmt.Errorf("server declared %d bytes, manifest says %d", total, expectedSize)
	}
	// Limit the copy to the remaining manifest-declared size.
	lim := expectedSize
	if offset > 0 {
		lim -= offset
	}
	if lim < 0 {
		lim = 0
	}
	// io.CopyN bounds the write to the remaining manifest-declared
	// size (there is no io.LimitWriter in the standard library).
	// CopyN returns nil only after copying exactly lim bytes; a short
	// body surfaces as io.ErrUnexpectedEOF and the engine retries.
	n, err := io.CopyN(w, resp.Body, lim)
	if err != nil {
		return n, partial, err
	}
	return n, partial, nil
}

// PostChanges uploads queued local changes.
func (c *Client) PostChanges(ctx context.Context, deviceID string, ops []ChangeOp) (*ChangeResult, error) {
	if err := c.requirePairing(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(map[string]any{"ops": ops})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base+protocol.APIPath+"/frames/"+deviceID+"/changes",
		strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	c.authorize(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, &ErrServerDown{Cause: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, c.httpError(resp)
	}
	var out ChangeResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("malformed changes response: %w", err)
	}
	return &out, nil
}

func (c *Client) authorize(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.token)
}

func (c *Client) httpError(resp *http.Response) error {
	var pe struct {
		Code string `json:"code"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if json.Unmarshal(body, &pe) == nil && pe.Code != "" {
		return &HTTPError{Status: resp.StatusCode, Code: pe.Code}
	}
	return &HTTPError{Status: resp.StatusCode, Code: http.StatusText(resp.StatusCode)}
}

func parseContentLength(s string) (int64, error) {
	var n int64
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

// parseContentRangeTotal extracts the total from "bytes a-b/total";
// returns -1 when unparseable (an "*" total means unknown).
func parseContentRangeTotal(cr string) int64 {
	slash := strings.LastIndex(cr, "/")
	if slash < 0 {
		return -1
	}
	t := strings.TrimSpace(cr[slash+1:])
	if t == "*" {
		return -1
	}
	var n int64
	if _, err := fmt.Sscanf(t, "%d", &n); err != nil {
		return -1
	}
	return n
}
