package frame

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"
)

// Credential is the frame's device credential. It is persisted in a
// dedicated file (NOT frame.db) with 0600 permissions, written
// atomically (temp + rename) so a power loss cannot corrupt it.
//
// Trust model (see docs/security/threat-model.md, T-02): the frame has
// no secure element; the file is protected by the frame user's
// filesystem permissions. A stolen SD card does leak the credential —
// the mitigations are that the token is per-device scoped (it grants
// only that frame's own resources), it is hash-stored and
// constant-time-verified server-side, and it can be revoked/rotated
// immediately from the admin UI.
type Credential struct {
	DeviceID  string `json:"device_id"`
	Token     string `json:"token"`
	IssuedAt  string `json:"issued_at"`
	PairedVia string `json:"paired_via"` // "pairing" | "manual"
}

// CredentialFileName lives in the frame state directory.
const CredentialFileName = "credential"

const credentialFileName = CredentialFileName

// LoadCredential reads the persisted credential; nil (no error) when
// the file does not exist. A corrupt file is an error: silently
// dropping a credential would strand a paired frame.
func LoadCredential(path string) (*Credential, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("credential: read: %w", err)
	}
	var c Credential
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("credential: corrupt file: %w", err)
	}
	if c.Token == "" || c.DeviceID == "" {
		return nil, errors.New("credential: file is missing required fields")
	}
	return &c, nil
}

// SaveCredential persists the credential with 0600 permissions.
func SaveCredential(path string, c *Credential) error {
	if c == nil || c.Token == "" || c.DeviceID == "" {
		return errors.New("credential: refusing to store an incomplete credential")
	}
	if c.IssuedAt == "" {
		c.IssuedAt = time.Now().UTC().Format(time.RFC3339)
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("credential: write: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("credential: rename: %w", err)
	}
	return nil
}
