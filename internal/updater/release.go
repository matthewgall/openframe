// Package updater implements the secure automatic software updater
// (milestone 6, spec §14, ADR-0007).
//
// It is deliberately independent of the frame's display and sync code:
// the only direction of dependency is frame → updater (to express
// update requests and to read update state), never the reverse. The
// privileged parts (slot activation, service restart) live behind a
// ServiceManager so the entire update/rollback state machine is
// testable without root, systemd or real hardware.
//
// Threat model, trust rules and the power-failure matrix are documented
// in docs/updater/trust-model.md; every rule has a named test.

package updater

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Release channels, newest-first by rollout order (a development frame
// receives a release before a production one).
const (
	ChannelDevelopment = "development"
	ChannelBeta        = "beta"
	ChannelStable      = "stable"
)

// ValidChannel reports whether c is a known release channel.
func ValidChannel(c string) bool {
	switch c {
	case ChannelDevelopment, ChannelBeta, ChannelStable:
		return true
	}
	return false
}

// Release is one signed software release. It is *data only*: the
// updater never interprets a field as a command or a URL to fetch and
// execute (spec §14 "Do not execute arbitrary commands supplied by
// release metadata"). The artifact is downloaded from the fixed
// release-server location for this release, never from a field here.
type Release struct {
	// Version is the semantic application version (e.g. "1.4.0").
	Version string `json:"version"`
	// Channel is stable | beta | development.
	Channel string `json:"channel"`
	// OS and Arch identify the target platform (linux/arm64, ...).
	OS   string `json:"os"`
	Arch string `json:"arch"`

	// ArtifactSHA256 and ArtifactSize identify and bound the binary.
	ArtifactSHA256 string `json:"artifact_sha256"`
	ArtifactSize   int64  `json:"artifact_size"`

	// MinProtocolVersion is the oldest manifest protocol this binary
	// speaks; a frame that only understands an older protocol must
	// refuse it (and vice versa). MinSchemaVersion is the oldest frame
	// database schema version this binary can safely start with — the
	// anti-migration-rollback bound (spec §14 "Database compatibility").
	MinProtocolVersion int `json:"min_protocol_version"`
	MinSchemaVersion   int `json:"min_schema_version"`

	// ReleasedAt is when the release was cut. NotBefore is an
	// optional lower time bound (metadata must not be honoured before
	// it, e.g. to delay a staged rollout). ExpiresAt is a hard upper
	// bound: after it the release is rejected even if never installed.
	ReleasedAt time.Time `json:"released_at"`
	NotBefore  time.Time `json:"not_before"`
	ExpiresAt  time.Time `json:"expires_at"`

	// RolloutPercent is 0-100. A frame is in the rollout when its
	// stable device-id hash bucket is below this percentage (see
	// rolloutEligible). RolloutDevices is an optional explicit
	// allow-list of device ids that are always in the rollout (used
	// for canary/development frames).
	RolloutPercent int      `json:"rollout_percent"`
	RolloutDevices []string `json:"rollout_devices,omitempty"`

	// IsRollback marks a release that is *intentionally* older than a
	// version some frames have already run, signed as a safe rollback
	// target. Anti-rollback still applies to it: it may not go below
	// the schema floor (see verify.go).
	IsRollback bool `json:"is_rollback"`

	// NewKeys, when non-empty, is a signed key-rotation event: each
	// entry is a new public key to add to the device's pinned trust
	// store, authorised by the key that signed this release. See
	// trust.go.
	NewKeys []NewKey `json:"new_keys,omitempty"`

	// SignerKeyID names the pinned public key that produced Signature.
	SignerKeyID string `json:"signer_key_id"`
	// Signature is an Ed25519 signature over SignedPayload().
	Signature string `json:"signature"`
}

// NewKey is one entry of a signed key-rotation event.
type NewKey struct {
	KeyID string `json:"key_id"`
	// Ed25519 is the new public key, base64 (32 raw bytes).
	Ed25519 string `json:"ed25519"`
}

// signedPayload is the canonical byte form that is signed and
// verified. It is Release minus the signature itself, with a
// deterministic field order (struct declaration order) so that the
// signer (release tooling/CI) and the verifier (frame) always agree on
// the exact bytes without a shared library at signing time.
//
// Note the ordering guarantees:
//   - fields are marshalled in declaration order (Go json.Marshal);
//   - times are RFC3339 UTC;
//   - RolloutDevices is sorted by the normaliser before marshalling.
type signedPayload struct {
	Version            string   `json:"version"`
	Channel            string   `json:"channel"`
	OS                 string   `json:"os"`
	Arch               string   `json:"arch"`
	ArtifactSHA256     string   `json:"artifact_sha256"`
	ArtifactSize       int64    `json:"artifact_size"`
	MinProtocolVersion int      `json:"min_protocol_version"`
	MinSchemaVersion   int      `json:"min_schema_version"`
	ReleasedAt         string   `json:"released_at"`
	NotBefore          string   `json:"not_before"`
	ExpiresAt          string   `json:"expires_at"`
	RolloutPercent     int      `json:"rollout_percent"`
	RolloutDevices     []string `json:"rollout_devices"`
	IsRollback         bool     `json:"is_rollback"`
	NewKeys            []NewKey `json:"new_keys"`
	SignerKeyID        string   `json:"signer_key_id"`
}

// SignedPayload returns the canonical bytes that Signature covers.
func (r Release) SignedPayload() ([]byte, error) {
	p := signedPayload{
		Version:            r.Version,
		Channel:            r.Channel,
		OS:                 r.OS,
		Arch:               r.Arch,
		ArtifactSHA256:     r.ArtifactSHA256,
		ArtifactSize:       r.ArtifactSize,
		MinProtocolVersion: r.MinProtocolVersion,
		MinSchemaVersion:   r.MinSchemaVersion,
		ReleasedAt:         r.ReleasedAt.UTC().Format(time.RFC3339),
		NotBefore:          r.NotBefore.UTC().Format(time.RFC3339),
		ExpiresAt:          r.ExpiresAt.UTC().Format(time.RFC3339),
		RolloutPercent:     r.RolloutPercent,
		RolloutDevices:     append([]string(nil), r.RolloutDevices...),
		IsRollback:         r.IsRollback,
		NewKeys:            append([]NewKey(nil), r.NewKeys...),
		SignerKeyID:        r.SignerKeyID,
	}
	// Deterministic ordering of list fields.
	sort.Strings(p.RolloutDevices)
	sort.Slice(p.NewKeys, func(i, j int) bool { return p.NewKeys[i].KeyID < p.NewKeys[j].KeyID })
	return json.Marshal(p)
}

// ValidateBasic checks the non-cryptographic invariants of a release
// (shape, not trust). Cryptographic and anti-rollback checks live in
// Verifier. It is used by the server when accepting a signed release
// and by the frame before paying the cost of verification.
func (r Release) ValidateBasic() error {
	if r.Version == "" {
		return errors.New("release: empty version")
	}
	if !ValidChannel(r.Channel) {
		return fmt.Errorf("release: unknown channel %q", r.Channel)
	}
	if r.OS == "" || r.Arch == "" {
		return errors.New("release: missing os/arch")
	}
	if len(r.ArtifactSHA256) != 64 {
		return errors.New("release: artifact_sha256 must be 64 hex chars")
	}
	if _, err := hex.DecodeString(r.ArtifactSHA256); err != nil {
		return fmt.Errorf("release: artifact_sha256 not hex: %w", err)
	}
	if r.ArtifactSize <= 0 {
		return errors.New("release: artifact_size must be positive")
	}
	if r.MinProtocolVersion < 1 {
		return errors.New("release: min_protocol_version must be >= 1")
	}
	if r.MinSchemaVersion < 1 {
		return errors.New("release: min_schema_version must be >= 1")
	}
	if r.ExpiresAt.IsZero() || r.ExpiresAt.Before(r.ReleasedAt) {
		return errors.New("release: expires_at must be after released_at")
	}
	if !r.NotBefore.IsZero() && r.NotBefore.Before(r.ReleasedAt) {
		return errors.New("release: not_before must not precede released_at")
	}
	if r.RolloutPercent < 0 || r.RolloutPercent > 100 {
		return errors.New("release: rollout_percent must be 0-100")
	}
	if r.SignerKeyID == "" {
		return errors.New("release: missing signer_key_id")
	}
	if r.Signature == "" {
		return errors.New("release: missing signature")
	}
	if _, err := base64.StdEncoding.DecodeString(r.Signature); err != nil {
		return fmt.Errorf("release: signature not base64: %w", err)
	}
	return nil
}

// SignRelease signs rel with priv (keyID names it) and returns a copy
// with Signature and SignerKeyID populated. Used by release tooling
// and by tests; the private key is never stored on the frame.
func SignRelease(rel Release, keyID string, priv ed25519.PrivateKey) (Release, error) {
	payload, err := rel.SignedPayload()
	if err != nil {
		return Release{}, fmt.Errorf("release: canonicalise: %w", err)
	}
	rel.SignerKeyID = keyID
	// Re-canonicalise with the key id set (it is part of the payload).
	payload, err = rel.SignedPayload()
	if err != nil {
		return Release{}, fmt.Errorf("release: canonicalise: %w", err)
	}
	rel.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, payload))
	return rel, nil
}

// VerifySignature checks the Ed25519 signature on rel against pub.
func VerifySignature(rel Release, pub ed25519.PublicKey) error {
	sig, err := base64.StdEncoding.DecodeString(rel.Signature)
	if err != nil {
		return fmt.Errorf("release: signature not base64: %w", err)
	}
	payload, err := rel.SignedPayload()
	if err != nil {
		return fmt.Errorf("release: canonicalise: %w", err)
	}
	if !ed25519.Verify(pub, payload, sig) {
		return errors.New("release: signature verification failed")
	}
	return nil
}

// ArtifactSHA256Hex returns the hex SHA-256 of data, for comparing a
// downloaded artifact against the release.
func ArtifactSHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ParseVersion compares dotted numeric versions. It is intentionally
// small (the release tooling pins "MAJOR.MINOR.PATCH"); pre-release
// suffixes are not part of the anti-rollback ordering.
type Version struct {
	Major, Minor, Patch int
	Raw                 string
}

// ParseVersion parses "a.b.c" (a trailing -suffix is ignored for
// ordering but preserved in Raw).
func ParseVersion(s string) (Version, error) {
	v := Version{Raw: s}
	base := s
	if i := strings.IndexByte(s, '-'); i >= 0 {
		base = s[:i]
	}
	parts := strings.Split(base, ".")
	if len(parts) != 3 {
		return v, fmt.Errorf("release: version %q not MAJOR.MINOR.PATCH", s)
	}
	for _, p := range parts {
		if p == "" {
			return v, fmt.Errorf("release: version %q has empty component", s)
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return v, fmt.Errorf("release: version %q not numeric", s)
			}
		}
	}
	v.Major, v.Minor, v.Patch = atoi(parts[0]), atoi(parts[1]), atoi(parts[2])
	return v, nil
}

func (v Version) Less(o Version) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	return v.Patch < o.Patch
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}
