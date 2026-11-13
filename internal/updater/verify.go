package updater

import (
	"errors"
	"fmt"
	"runtime"
	"time"
)

// Verifier rejection errors. Every trust rule in
// docs/updater/trust-model.md maps to exactly one of these, and each
// has a named test (spec §20 "Updaters" matrix).
var (
	// ErrBadSignature: the Ed25519 signature does not verify against
	// the pinned key for signer_key_id.
	ErrBadSignature = errors.New("updater: invalid signature")
	// ErrUnknownSigner: signer_key_id is not in the device's pinned
	// trust store. Trust roots are device-pinned, never downloaded.
	ErrUnknownSigner = errors.New("updater: signer key is not pinned")
	// ErrExpired: now is past the metadata's expires_at.
	ErrExpired = errors.New("updater: release metadata expired")
	// ErrNotBefore: now is before the metadata's not_before bound.
	ErrNotBefore = errors.New("updater: release metadata not valid yet")
	// ErrPlatformMismatch: the release targets a different os/arch.
	ErrPlatformMismatch = errors.New("updater: release platform mismatch")
	// ErrProtocolIncompat: the release requires a manifest protocol
	// newer than this build understands.
	ErrProtocolIncompat = errors.New("updater: protocol version incompatible")
	// ErrSchemaIncompat: the release requires a frame DB schema newer
	// than the one in place; installing it could make rollback
	// unsafe, so the update is refused (spec §14 Database
	// compatibility: "Reject an update if safe rollback cannot be
	// guaranteed").
	ErrSchemaIncompat = errors.New("updater: schema version incompatible; safe rollback cannot be guaranteed")
	// ErrDowngrade: the release is not newer than the best version
	// this channel has already accepted (anti-rollback). A signed
	// rollback release (IsRollback) is exempt — it is the *authorised*
	// way to move backwards.
	ErrDowngrade = errors.New("updater: downgrade rejected (anti-rollback)")
	// ErrInvalidNewKey: a signed rotation event carries a malformed
	// public key; the whole release is rejected.
	ErrInvalidNewKey = errors.New("updater: rotation event carries an invalid key")
	// ErrArtifactHash / ErrArtifactSize: the downloaded bytes do not
	// match the signed digest/size.
	ErrArtifactHash = errors.New("updater: artifact sha256 mismatch")
	ErrArtifactSize = errors.New("updater: artifact size mismatch")
)

// Verifier verifies releases against a pinned trust store.
type Verifier struct {
	trust *TrustedKeys
}

// NewVerifier builds a Verifier over a loaded trust store.
func NewVerifier(trust *TrustedKeys) *Verifier {
	return &Verifier{trust: trust}
}

// VerifyOptions carries the device facts a verification decision
// depends on. Now is injectable for tests.
type VerifyOptions struct {
	Now  time.Time // zero means time.Now().UTC()
	OS   string    // empty means runtime.GOOS
	Arch string    // empty means runtime.GOARCH
	// MaxProtocol is the newest manifest protocol this build speaks.
	MaxProtocol int
	// SchemaVersion is the frame DB schema version currently in place.
	SchemaVersion int
	// MaxAccepted[channel] is the highest version this channel has
	// ever committed (anti-rollback floor). Empty = first release.
	MaxAccepted map[string]string
}

// VerifyResult is the outcome of a fully verified release.
type VerifyResult struct {
	Release Release
	// NewKeys, when non-nil, is a verified rotation event: the
	// coordinator persists these keys to the trust store (see
	// trust-model.md, rotation).
	NewKeys *TrustedKeys
}

// Verify runs the complete trust chain over rel. It returns one of
// the named errors on rejection, or a VerifyResult on success.
//
// Order matters and is part of the model: identity (who signed this)
// before policy (may we take it). Compatibility checks only run on
// correctly-signed metadata, so an attacker cannot probe the device
// through rejection differences on forged releases.
func (v *Verifier) Verify(rel Release, opts VerifyOptions) (*VerifyResult, error) {
	if err := rel.ValidateBasic(); err != nil {
		return nil, err
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	}
	if opts.OS == "" {
		opts.OS = runtime.GOOS
	}
	if opts.Arch == "" {
		opts.Arch = runtime.GOARCH
	}

	// 1. Identity: pinned signer + valid signature.
	pub, err := v.trust.Public(rel.SignerKeyID)
	if err != nil {
		return nil, ErrUnknownSigner
	}
	if err := VerifySignature(rel, pub); err != nil {
		return nil, ErrBadSignature
	}

	// A signed rotation event is checked here (it is part of the
	// signed metadata, so a valid signature authorises it).
	var newKeys *TrustedKeys
	if len(rel.NewKeys) > 0 {
		newKeys = &TrustedKeys{Keys: map[string]TrustedKey{}}
		for _, nk := range rel.NewKeys {
			_, err := DecodePublicKey(nk.Ed25519)
			if err != nil || nk.KeyID == "" {
				return nil, fmt.Errorf("%w: %s: %v", ErrInvalidNewKey, nk.KeyID, err)
			}
			newKeys.Keys[nk.KeyID] = TrustedKey{KeyID: nk.KeyID, Ed25519: nk.Ed25519}
		}
	}

	// 2. Time bounds.
	if !opts.Now.Before(rel.ExpiresAt) {
		return nil, ErrExpired
	}
	if !rel.NotBefore.IsZero() && opts.Now.Before(rel.NotBefore) {
		return nil, ErrNotBefore
	}

	// 3. Platform.
	if rel.OS != opts.OS || rel.Arch != opts.Arch {
		return nil, ErrPlatformMismatch
	}

	// 4. Compatibility bounds.
	if rel.MinProtocolVersion > opts.MaxProtocol {
		return nil, ErrProtocolIncompat
	}
	if rel.MinSchemaVersion > opts.SchemaVersion {
		return nil, ErrSchemaIncompat
	}

	// 5. Anti-rollback (version ordering per channel).
	relVer, err := ParseVersion(rel.Version)
	if err != nil {
		return nil, err
	}
	if !rel.IsRollback {
		if floor, ok := opts.MaxAccepted[rel.Channel]; ok && floor != "" {
			floorVer, err := ParseVersion(floor)
			if err != nil {
				return nil, fmt.Errorf("updater: stored floor %q: %w", floor, err)
			}
			if !floorVer.Less(relVer) { // relVer <= floor
				return nil, ErrDowngrade
			}
		}
	}

	return &VerifyResult{Release: rel, NewKeys: newKeys}, nil
}

// VerifyArtifact checks a downloaded artifact against the signed
// digest and size (spec §14 step 6).
func VerifyArtifact(data []byte, rel Release) error {
	if int64(len(data)) != rel.ArtifactSize {
		return fmt.Errorf("%w: got %d, want %d", ErrArtifactSize, len(data), rel.ArtifactSize)
	}
	if got := ArtifactSHA256Hex(data); got != rel.ArtifactSHA256 {
		return fmt.Errorf("%w: got %s", ErrArtifactHash, got[:16])
	}
	return nil
}
