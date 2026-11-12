// Package auth implements server authentication: Argon2id password
// hashing, database-backed sessions with revocation, CSRF token
// derivation, and login rate limiting (spec §5; T-05).
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters (OWASP recommendation for interactive logins).
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // 64 MiB
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// MinPasswordLength is enforced on set/change.
const MinPasswordLength = 10

var errInvalidPHC = errors.New("auth: invalid PHC password hash")

// HashPassword produces an Argon2id PHC string:
// $argon2id$v=19$m=65536,t=3,p=4$<salt-b64>$<hash-b64>
func HashPassword(password string) (string, error) {
	if len(password) < MinPasswordLength {
		return "", fmt.Errorf("password must be at least %d characters", MinPasswordLength)
	}
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	sum := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		argonMemory, argonTime, argonThreads,
		enc.EncodeToString(salt), enc.EncodeToString(sum)), nil
}

// VerifyPassword checks a password against a PHC hash. It parses the
// parameters from the stored string so future parameter upgrades are
// transparent.
func VerifyPassword(phc, password string) error {
	parts := strings.Split(phc, "$")
	// $, argon2id, v=19, m=..,t=..,p=.., salt, hash
	if len(parts) != 6 || parts[1] != "argon2id" {
		return errInvalidPHC
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != 19 {
		return errInvalidPHC
	}
	var mem, time_, threads int
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &time_, &threads); err != nil ||
		mem <= 0 || time_ <= 0 || threads <= 0 {
		return errInvalidPHC
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return errInvalidPHC
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return errInvalidPHC
	}
	got := argon2.IDKey([]byte(password), salt, uint32(time_), uint32(mem), uint8(threads), uint32(len(want)))
	if !constantTimeEqual(got, want) {
		return errors.New("auth: password mismatch")
	}
	return nil
}

func constantTimeEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// TemporaryPassword generates a one-time password for user recovery
// (admin-controlled, spec §5). Format: 24 base32 chars, e.g.
// "OFPD-K4XT-9M2Q-V7ZR".
func TemporaryPassword() (string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no 0/O/1/I
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, 0, 24)
	for _, b := range buf {
		out = append(out, alphabet[int(b)%len(alphabet)])
	}
	// Group for readability.
	var s strings.Builder
	for i, c := range out {
		if i > 0 && i%4 == 0 {
			s.WriteByte('-')
		}
		s.WriteByte(c)
	}
	return s.String(), nil
}

// NewToken returns a 32-byte random session token (hex, 64 chars).
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// TokenHash is the stored form of a session token.
func TokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// DeriveCSRFToken derives the per-session CSRF token (synchronizer
// pattern) from the session token. It is deterministic, so the server
// can recompute it at any time without a second stored secret; an
// attacker who steals the session cookie already has full session
// control, so this does not weaken the session.
func DeriveCSRFToken(sessionToken string) string {
	sum := sha256.Sum256([]byte("openframe-csrf:" + sessionToken))
	return hex.EncodeToString(sum[:])
}
