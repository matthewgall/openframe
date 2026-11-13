package api_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/matthewgall/openframe/internal/updater"
)

// m6Signer holds a signing key pair for release tests.
type m6Signer struct {
	id   string
	priv ed25519.PrivateKey
}

func newM6Signer(t *testing.T, id string) *m6Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &m6Signer{id: id, priv: priv}
}

// m6Artifact is the canonical fake binary for these tests.
var m6Artifact = []byte("fake-binary")

// m6Release builds a signed release whose digest/size match
// m6Artifact.
func (s *m6Signer) release(t *testing.T, version, channel, osName, arch string, mut func(*updater.Release)) updater.Release {
	t.Helper()
	now := time.Now().UTC()
	rel := updater.Release{
		Version:            version,
		Channel:            channel,
		OS:                 osName,
		Arch:               arch,
		ArtifactSHA256:     updater.ArtifactSHA256Hex(m6Artifact),
		ArtifactSize:       int64(len(m6Artifact)),
		MinProtocolVersion: 1,
		MinSchemaVersion:   3,
		ReleasedAt:         now.Add(-time.Hour),
		ExpiresAt:          now.Add(90 * 24 * time.Hour),
		RolloutPercent:     100,
	}
	if mut != nil {
		mut(&rel)
	}
	signed, err := updater.SignRelease(rel, s.id, s.priv)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

// m6Upload posts a multipart release upload.
func (c *client) m6Upload(t *testing.T, rel updater.Release, artifact []byte) int {
	t.Helper()
	meta, err := json.Marshal(rel)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("metadata", string(meta)); err != nil {
		t.Fatal(err)
	}
	af, err := mw.CreateFormFile("artifact", "openframe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := af.Write(artifact); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, c.base+"/api/v1/releases", &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-CSRF-Token", c.csrf)
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)
	return resp.StatusCode
}

func latestURL(channel, osName, arch string) string {
	return fmt.Sprintf("/api/v1/releases/%s/latest.json?os=%s&arch=%s", channel, osName, arch)
}

// m6Latest fetches latest.json; returns (status, raw body).
func (c *client) m6Latest(t *testing.T, channel, osName, arch string) (int, []byte) {
	t.Helper()
	resp, data := c.do(http.MethodGet, latestURL(channel, osName, arch), nil, false)
	return resp.StatusCode, data
}

// 1. The full lifecycle: upload (staged, hidden) → approve (served,
// latest) → newer approve (latest moves) → pause (latest falls back)
// → signed rollback approve (latest flips to the older build).
func TestM6ReleaseLifecycle(t *testing.T) {
	ts, dbh := newTestServer(t)
	seedUser(t, dbh, "admin@m6.test", "admin", "m6pass-12345")
	c := newClient(t, ts.URL, "admin@m6.test", "m6pass-12345")
	signer := newM6Signer(t, "key-m6")
	artifact := m6Artifact

	// Nothing published yet.
	if code, _ := c.m6Latest(t, "stable", "linux", "arm64"); code != http.StatusNotFound {
		t.Fatalf("pre-upload latest = %d, want 404", code)
	}

	// Upload 1.1.0 → staged, not served.
	rel11 := signer.release(t, "1.1.0", "stable", "linux", "arm64", nil)
	meta11, _ := json.Marshal(rel11)
	if code := c.m6Upload(t, rel11, artifact); code != http.StatusCreated {
		t.Fatalf("upload 1.1.0 = %d, want 201", code)
	}
	if code, _ := c.m6Latest(t, "stable", "linux", "arm64"); code != http.StatusNotFound {
		t.Fatalf("staged release must not be served: latest = %d", code)
	}

	// Approve → served, and latest.json is the EXACT signed bytes
	// inside an envelope.
	q := "?channel=stable&os=linux&arch=arm64"
	var relOut struct {
		Status string `json:"status"`
	}
	if code := c.json(http.MethodPost, "/api/v1/releases/1.1.0/approve"+q, nil, &relOut); code != http.StatusOK {
		t.Fatalf("approve 1.1.0 = %d", code)
	}
	if relOut.Status != "approved" {
		t.Fatalf("status = %q", relOut.Status)
	}
	code, body := c.m6Latest(t, "stable", "linux", "arm64")
	if code != http.StatusOK {
		t.Fatalf("latest after approve = %d (body %s)", code, body)
	}
	var env struct {
		Release json.RawMessage `json:"release"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("envelope decode: %v", err)
	}
	if !bytes.Equal(env.Release, meta11) {
		t.Fatal("latest.json must serve the signed metadata verbatim")
	}
	// The frame-side verifier must accept it.
	var served updater.Release
	if err := json.Unmarshal(env.Release, &served); err != nil {
		t.Fatal(err)
	}
	pub := signer.priv.Public().(ed25519.PublicKey)
	if err := updater.VerifySignature(served, pub); err != nil {
		t.Fatalf("served metadata failed verification: %v", err)
	}

	// Upload + approve 1.2.0 → latest moves.
	rel12 := signer.release(t, "1.2.0", "stable", "linux", "arm64", nil)
	if code := c.m6Upload(t, rel12, artifact); code != http.StatusCreated {
		t.Fatalf("upload 1.2.0 = %d", code)
	}
	if code := c.json(http.MethodPost, "/api/v1/releases/1.2.0/approve"+q, nil, nil); code != http.StatusOK {
		t.Fatalf("approve 1.2.0 = %d", code)
	}
	_, body = c.m6Latest(t, "stable", "linux", "arm64")
	json.Unmarshal(body, &env)
	var v12 updater.Release
	json.Unmarshal(env.Release, &v12)
	if v12.Version != "1.2.0" {
		t.Fatalf("latest = %s, want 1.2.0", v12.Version)
	}

	// Pause 1.2.0 → latest falls back to the next approved (1.1.0).
	if code := c.json(http.MethodPost, "/api/v1/releases/1.2.0/pause"+q, nil, nil); code != http.StatusOK {
		t.Fatalf("pause 1.2.0 = %d", code)
	}
	_, body = c.m6Latest(t, "stable", "linux", "arm64")
	json.Unmarshal(body, &env)
	var vBack updater.Release
	json.Unmarshal(env.Release, &vBack)
	if vBack.Version != "1.1.0" {
		t.Fatalf("latest after pause = %s, want 1.1.0", vBack.Version)
	}

	// A signed rollback release (older version) flips latest when
	// approved — the operator's explicit move backwards.
	relRB := signer.release(t, "1.0.5", "stable", "linux", "arm64", func(r *updater.Release) {
		r.IsRollback = true
	})
	if code := c.m6Upload(t, relRB, artifact); code != http.StatusCreated {
		t.Fatalf("upload rollback = %d", code)
	}
	if code := c.json(http.MethodPost, "/api/v1/releases/1.0.5/approve"+q, nil, nil); code != http.StatusOK {
		t.Fatalf("approve rollback = %d", code)
	}
	_, body = c.m6Latest(t, "stable", "linux", "arm64")
	json.Unmarshal(body, &env)
	var vRB updater.Release
	json.Unmarshal(env.Release, &vRB)
	if vRB.Version != "1.0.5" || !vRB.IsRollback {
		t.Fatalf("latest = %s rollback=%v, want 1.0.5/true", vRB.Version, vRB.IsRollback)
	}
}

// 2. Platform and channel isolation: each (channel, os, arch) line is
// independent.
func TestM6ReleaseIsolation(t *testing.T) {
	ts, dbh := newTestServer(t)
	seedUser(t, dbh, "admin@m6b.test", "admin", "m6pass-12345")
	c := newClient(t, ts.URL, "admin@m6b.test", "m6pass-12345")
	signer := newM6Signer(t, "key-m6b")
	artifact := m6Artifact

	// arm64 stable at 1.1.0, amd64 stable at 1.2.0, beta stable-arm64
	// at 0.9.0.
	rA := signer.release(t, "1.1.0", "stable", "linux", "arm64", nil)
	rB := signer.release(t, "1.2.0", "stable", "linux", "amd64", nil)
	rC := signer.release(t, "0.9.0", "beta", "linux", "arm64", nil)
	for _, r := range []updater.Release{rA, rB, rC} {
		if code := c.m6Upload(t, r, artifact); code != http.StatusCreated {
			t.Fatalf("upload %s/%s = %d", r.Channel, r.Version, code)
		}
	}
	approve := func(version, channel, osName, arch string) {
		c.t.Helper()
		q := fmt.Sprintf("?channel=%s&os=%s&arch=%s", channel, osName, arch)
		if code := c.json(http.MethodPost, "/api/v1/releases/"+version+"/approve"+q, nil, nil); code != http.StatusOK {
			t.Fatalf("approve %s = %d", version, code)
		}
	}
	approve("1.1.0", "stable", "linux", "arm64")
	approve("1.2.0", "stable", "linux", "amd64")
	approve("0.9.0", "beta", "linux", "arm64")

	want := map[string]string{
		"stable/arm64": "1.1.0",
		"stable/amd64": "1.2.0",
		"beta/arm64":   "0.9.0",
	}
	for key, version := range want {
		parts := strings.SplitN(key, "/", 2)
		code, body := c.m6Latest(t, parts[0], "linux", parts[1])
		if code != http.StatusOK {
			t.Fatalf("latest %s = %d", key, code)
		}
		var env struct {
			Release json.RawMessage `json:"release"`
		}
		json.Unmarshal(body, &env)
		var r updater.Release
		json.Unmarshal(env.Release, &r)
		if r.Version != version {
			t.Fatalf("latest %s = %s, want %s", key, r.Version, version)
		}
	}
	// An unapproved platform line stays 404.
	if code, _ := c.m6Latest(t, "stable", "linux", "386"); code != http.StatusNotFound {
		t.Fatalf("unpublished platform = %d, want 404", code)
	}
}

// 3. Upload validation: the server refuses artifacts that do not match
// the signed digest/size, and malformed metadata.
func TestM6UploadRejections(t *testing.T) {
	ts, dbh := newTestServer(t)
	seedUser(t, dbh, "admin@m6c.test", "admin", "m6pass-12345")
	c := newClient(t, ts.URL, "admin@m6c.test", "m6pass-12345")
	signer := newM6Signer(t, "key-m6c")

	// Artifact mismatch: signed digest for m6Artifact, upload B.
	rel := signer.release(t, "1.1.0", "stable", "linux", "arm64", nil)
	if code := c.m6Upload(t, rel, []byte("different-bytes")); code != http.StatusBadRequest {
		t.Fatalf("mismatched artifact = %d, want 400", code)
	}
	// Declared size does not match the artifact (digest is correct
	// for the bytes; the signed size is wrong).
	artifact11 := []byte("fake-binarie")
	raw := signer.release(t, "1.1.1", "stable", "linux", "arm64", nil)
	raw.ArtifactSHA256 = updater.ArtifactSHA256Hex(artifact11)
	raw.ArtifactSize = int64(len(artifact11)) - 1
	signed2, err := updater.SignRelease(raw, signer.id, signer.priv)
	if err != nil {
		t.Fatal(err)
	}
	if code := c.m6Upload(t, signed2, artifact11); code != http.StatusBadRequest {
		t.Fatalf("size mismatch = %d, want 400", code)
	}
	// Malformed metadata (not JSON).
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("metadata", "m.json")
	fw.Write([]byte("{not-json"))
	af, _ := mw.CreateFormFile("artifact", "a")
	af.Write([]byte("x"))
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, c.base+"/api/v1/releases", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-CSRF-Token", c.csrf)
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed metadata = %d, want 400", resp.StatusCode)
	}
	// Unsigned metadata (missing signature) is refused.
	bad := rel
	bad.Signature = ""
	bad.Version = "1.1.2"
	if code := c.m6Upload(t, bad, m6Artifact); code != http.StatusBadRequest {
		t.Fatalf("unsigned metadata = %d, want 400", code)
	}
}

// 4. Staged-rollout expansion: the admin's rollout change is served in
// the (unsigned) serving block and applied by the frame client, while
// the signed document stays byte-identical.
func TestM6RolloutServing(t *testing.T) {
	ts, dbh := newTestServer(t)
	seedUser(t, dbh, "admin@m6d.test", "admin", "m6pass-12345")
	c := newClient(t, ts.URL, "admin@m6d.test", "m6pass-12345")
	signer := newM6Signer(t, "key-m6d")
	artifact := m6Artifact

	rel := signer.release(t, "1.1.0", "stable", "linux", "arm64", func(r *updater.Release) {
		r.RolloutPercent = 10
	})
	meta, _ := json.Marshal(rel)
	if code := c.m6Upload(t, rel, artifact); code != http.StatusCreated {
		t.Fatalf("upload = %d", code)
	}
	q := "?channel=stable&os=linux&arch=arm64"
	if code := c.json(http.MethodPost, "/api/v1/releases/1.1.0/approve"+q, nil, nil); code != http.StatusOK {
		t.Fatalf("approve = %d", code)
	}
	// Expand rollout to 50% + allow-list one device.
	if code := c.json(http.MethodPut, "/api/v1/releases/1.1.0/rollout"+q,
		map[string]any{"percent": 50, "devices": []string{"canary-1"}}, nil); code != http.StatusOK {
		t.Fatalf("set rollout = %d", code)
	}
	code, body := c.m6Latest(t, "stable", "linux", "arm64")
	if code != http.StatusOK {
		t.Fatalf("latest = %d", code)
	}
	var env struct {
		Release json.RawMessage `json:"release"`
		Serving struct {
			RolloutPercent int      `json:"rollout_percent"`
			RolloutDevices []string `json:"rollout_devices"`
		} `json:"serving"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(env.Release, meta) {
		t.Fatal("rollout change must not alter the signed document")
	}
	if env.Serving.RolloutPercent != 50 || len(env.Serving.RolloutDevices) != 1 {
		t.Fatalf("serving block = %+v", env.Serving)
	}
	// The frame client gets the SIGNED document verbatim (signature
	// intact) plus the serving override applied to eligibility.
	src := updater.NewHTTPReleaseSource(ts.URL)
	src.TargetOS, src.TargetArch = "linux", "arm64"
	fr, err := src.FetchReleaseEx(t.Context(), "stable")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if err := updater.VerifySignature(fr.Release, signer.priv.Public().(ed25519.PublicKey)); err != nil {
		t.Fatalf("client release failed verification: %v", err)
	}
	if fr.Release.RolloutPercent != 10 {
		t.Fatalf("signed rollout modified in transit: %d, want 10", fr.Release.RolloutPercent)
	}
	if fr.Serving == nil || fr.Serving.RolloutPercent != 50 {
		t.Fatalf("serving override = %+v, want 50%%", fr.Serving)
	}
	// Eligibility uses the override: the allow-listed canary is in
	// even though the signed percent is 10.
	if !updater.RolloutEligible(fr.Release, 50, fr.Serving.RolloutDevices, "canary-1") {
		t.Fatal("allow-listed canary must be eligible via the override")
	}
}

// 5. Delete rules: an approved release must be paused first; deleting
// the latest recomputes the pointer.
func TestM6DeleteRules(t *testing.T) {
	ts, dbh := newTestServer(t)
	seedUser(t, dbh, "admin@m6e.test", "admin", "m6pass-12345")
	c := newClient(t, ts.URL, "admin@m6e.test", "m6pass-12345")
	signer := newM6Signer(t, "key-m6e")
	artifact := m6Artifact
	q := "?channel=stable&os=linux&arch=arm64"

	r1 := signer.release(t, "1.0.0", "stable", "linux", "arm64", nil)
	r2 := signer.release(t, "1.1.0", "stable", "linux", "arm64", nil)
	c.m6Upload(t, r1, artifact)
	c.m6Upload(t, r2, artifact)
	c.json(http.MethodPost, "/api/v1/releases/1.0.0/approve"+q, nil, nil)
	c.json(http.MethodPost, "/api/v1/releases/1.1.0/approve"+q, nil, nil)

	// Approved (latest) cannot be deleted directly.
	if code := c.json(http.MethodDelete, "/api/v1/releases/1.1.0"+q, nil, nil); code != http.StatusConflict {
		t.Fatalf("delete approved = %d, want 409", code)
	}
	// Pause, then delete → latest falls back to 1.0.0.
	if code := c.json(http.MethodPost, "/api/v1/releases/1.1.0/pause"+q, nil, nil); code != http.StatusOK {
		t.Fatalf("pause = %d", code)
	}
	if code := c.json(http.MethodDelete, "/api/v1/releases/1.1.0"+q, nil, nil); code != http.StatusNoContent {
		t.Fatalf("delete paused = %d, want 204", code)
	}
	code, body := c.m6Latest(t, "stable", "linux", "arm64")
	if code != http.StatusOK {
		t.Fatalf("latest = %d", code)
	}
	var env struct {
		Release json.RawMessage `json:"release"`
	}
	json.Unmarshal(body, &env)
	var r updater.Release
	json.Unmarshal(env.Release, &r)
	if r.Version != "1.0.0" {
		t.Fatalf("latest after delete = %s, want 1.0.0", r.Version)
	}
	// Staged releases can be deleted directly.
	r3 := signer.release(t, "2.0.0", "stable", "linux", "arm64", nil)
	c.m6Upload(t, r3, artifact)
	if code := c.json(http.MethodDelete, "/api/v1/releases/2.0.0"+q, nil, nil); code != http.StatusNoContent {
		t.Fatalf("delete staged = %d, want 204", code)
	}
}

// 6. The artifact endpoint serves the exact bytes, supports Range
// requests (resumable downloads), and 404s for unknown versions.
func TestM6ArtifactServing(t *testing.T) {
	ts, dbh := newTestServer(t)
	seedUser(t, dbh, "admin@m6f.test", "admin", "m6pass-12345")
	c := newClient(t, ts.URL, "admin@m6f.test", "m6pass-12345")
	signer := newM6Signer(t, "key-m6f")
	artifact := bytes.Repeat([]byte("openframe-artifact-bytes-"), 100) // ~2.9KB
	q := "?channel=stable&os=linux&arch=arm64"

	rel := signer.release(t, "1.1.0", "stable", "linux", "arm64", nil)
	rel.ArtifactSHA256 = updater.ArtifactSHA256Hex(artifact)
	rel.ArtifactSize = int64(len(artifact))
	signed, err := updater.SignRelease(rel, signer.id, signer.priv)
	if err != nil {
		t.Fatal(err)
	}
	if code := c.m6Upload(t, signed, artifact); code != http.StatusCreated {
		t.Fatalf("upload = %d", code)
	}
	c.json(http.MethodPost, "/api/v1/releases/1.1.0/approve"+q, nil, nil)

	// Full fetch.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/releases/1.1.0/artifact?os=linux&arch=arm64", nil)
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, artifact) {
		t.Fatalf("full artifact: %d, %d bytes", resp.StatusCode, len(body))
	}
	// Range fetch (resume semantics).
	req2, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/releases/1.1.0/artifact?os=linux&arch=arm64", nil)
	req2.Header.Set("Range", fmt.Sprintf("bytes=%d-", len(artifact)/2))
	resp2, err := c.http.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	part, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusPartialContent {
		t.Fatalf("range status = %d, want 206", resp2.StatusCode)
	}
	if !bytes.Equal(part, artifact[len(artifact)/2:]) {
		t.Fatalf("range body = %d bytes, want %d", len(part), len(artifact)-len(artifact)/2)
	}
	// Unknown version → 404.
	req3, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/releases/9.9.9/artifact?os=linux&arch=arm64", nil)
	resp3, err := c.http.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown artifact = %d, want 404", resp3.StatusCode)
	}
}
