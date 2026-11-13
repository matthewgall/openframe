package updater

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
)

// TrustedKeysFile is the deployment-provided file of pinned public
// keys (spec §14 "Pin trusted verification keys independently of the
// release server"; ADR-0007). It is written by the deployment package
// and never downloaded from the release server.
//
// Format:
//
//	{
//	  "keys": {
//	    "key-1": { "key_id": "key-1", "ed25519": "<base64 32 bytes>",
//	                "note": "initial release key" }
//	  }
//	}
type TrustedKeys struct {
	Keys map[string]TrustedKey `json:"keys"`
}

// TrustedKey is one pinned release-signing public key.
type TrustedKey struct {
	KeyID string `json:"key_id"`
	// Ed25519 is the raw 32-byte public key, base64.
	Ed25519 string `json:"ed25519"`
	// Note is a free-form label for operators (not signed, not
	// authoritative).
	Note string `json:"note,omitempty"`
}

// ErrNoTrustedKeys means the trust store is empty or unreadable.
var ErrNoTrustedKeys = errors.New("updater: no trusted keys")

// LoadTrustedKeys reads and validates a trusted-keys file.
func LoadTrustedKeys(path string) (*TrustedKeys, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("updater: read trust file: %w", err)
	}
	var tk TrustedKeys
	if err := json.Unmarshal(b, &tk); err != nil {
		return nil, fmt.Errorf("updater: parse trust file: %w", err)
	}
	if len(tk.Keys) == 0 {
		return nil, ErrNoTrustedKeys
	}
	for id, k := range tk.Keys {
		if id != k.KeyID {
			return nil, fmt.Errorf("updater: trust key id mismatch (%q vs %q)", id, k.KeyID)
		}
		if _, err := DecodePublicKey(k.Ed25519); err != nil {
			return nil, fmt.Errorf("updater: trust key %q: %w", id, err)
		}
	}
	return &tk, nil
}

// DecodePublicKey decodes a base64 raw 32-byte Ed25519 public key.
func DecodePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("key not base64: %w", err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("key must be %d bytes, got %d", ed25519.PublicKeySize, len(b))
	}
	return ed25519.PublicKey(b), nil
}

// Public returns the raw public key for keyID, or an error if it is
// not pinned.
func (t *TrustedKeys) Public(keyID string) (ed25519.PublicKey, error) {
	k, ok := t.Keys[keyID]
	if !ok {
		return nil, fmt.Errorf("updater: signer key %q is not pinned in the trust store", keyID)
	}
	return DecodePublicKey(k.Ed25519)
}

// KeyIDs returns the pinned key ids, sorted (for deterministic output).
func (t *TrustedKeys) KeyIDs() []string {
	ids := make([]string, 0, len(t.Keys))
	for id := range t.Keys {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// AddTrusted merges another set of keys (used when a signed rotation
// event is applied). Existing keys are never removed by rotation;
// retirement is a deployment action that rewrites the trust file.
func (t *TrustedKeys) AddTrusted(other *TrustedKeys) {
	if t.Keys == nil {
		t.Keys = map[string]TrustedKey{}
	}
	for id, k := range other.Keys {
		if _, exists := t.Keys[id]; !exists {
			t.Keys[id] = k
		}
	}
}

// Encode renders the trust store as canonical JSON (for writing the
// trust file after a rotation, in tests, etc.).
func (t *TrustedKeys) Encode() ([]byte, error) {
	return json.MarshalIndent(t, "", "  ")
}
