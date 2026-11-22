package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// TOTP parameters (RFC 6238; Google Authenticator compatible):
// HMAC-SHA1, 6 digits, 30s period, ±1 step verification window.
const (
	TOTPPeriod = 30 * time.Second
	TOTPDigits = 6
	TOTPWindow = 1 // ±1 step: a clock skew up to 30s still verifies
)

// GenerateTOTPSecret returns a fresh 20-byte base32 secret (no
// padding) for the otpauth URI.
func GenerateTOTPSecret() (string, error) {
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf), nil
}

// TOTPCodeAt computes the 6-digit code for a secret at a given instant
// (the pure algorithm; exposed for tests and the confirm flow).
func TOTPCodeAt(secret, now string, t time.Time) (int, error) {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return 0, fmt.Errorf("totp: bad secret: %w", err)
	}
	// (TOTPPeriod is a time.Duration — nanoseconds — so divide by its
	// second value, not the raw constant.)
	counter := uint64(t.Unix() / int64(TOTPPeriod/time.Second))
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := (uint32(sum[off])&0x7f)<<24 |
		uint32(sum[off+1])<<16 |
		uint32(sum[off+2])<<8 |
		uint32(sum[off+3])
	code := v % 1_000_000
	return int(code), nil
}

// VerifyTOTP checks a 6-digit code against the secret with the ±1
// step window. now is injected for testability.
func VerifyTOTP(secret, code string, now time.Time) bool {
	code = strings.TrimSpace(code)
	if len(code) != TOTPDigits {
		return false
	}
	n := 0
	for _, r := range code {
		if r < '0' || r > '9' {
			return false
		}
		n = n*10 + int(r-'0')
	}
	for step := -TOTPWindow; step <= TOTPWindow; step++ {
		want, err := TOTPCodeAt(secret, "", now.Add(time.Duration(step)*TOTPPeriod))
		if err != nil {
			return false
		}
		if want == n {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------
// Recovery codes.

// RecoveryCodeCount is issued per enablement: 10 single-use codes.
const RecoveryCodeCount = 10

// recoveryAlphabet excludes 0/O/1/I (the confusable characters).
const recoveryAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// GenerateRecoveryCode returns one code in the "ABCDE-FGHIJ" shape.
func GenerateRecoveryCode() (string, error) {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	out := make([]byte, 0, 11)
	for i, c := range b {
		if i == 5 {
			out = append(out, '-')
		}
		out = append(out, recoveryAlphabet[int(c)%len(recoveryAlphabet)])
	}
	return string(out), nil
}

// GenerateRecoveryCodes returns RecoveryCodeCount fresh codes.
func GenerateRecoveryCodes() ([]string, error) {
	codes := make([]string, 0, RecoveryCodeCount)
	for i := 0; i < RecoveryCodeCount; i++ {
		c, err := GenerateRecoveryCode()
		if err != nil {
			return nil, err
		}
		codes = append(codes, c)
	}
	return codes, nil
}

// HashRecoveryCode is the at-rest form (codes are never stored in
// clear; redemption compares hashes).
func HashRecoveryCode(code string) string {
	sum := sha256.Sum256([]byte(strings.ToUpper(strings.TrimSpace(code))))
	return hex.EncodeToString(sum[:])
}

// IsRecoveryCodeShape reports whether the string could be a recovery
// code (11 chars, "XXXXX-XXXXX" over the alphabet) — the login/MFA
// step uses this to route a submission to the right verifier (6-digit
// codes are TOTP, 11-char codes are recovery).
func IsRecoveryCodeShape(s string) bool {
	s = strings.ToUpper(strings.TrimSpace(s))
	if len(s) != 11 || s[5] != '-' {
		return false
	}
	for i, r := range s {
		if i == 5 {
			continue
		}
		if !strings.ContainsRune(recoveryAlphabet, r) {
			return false
		}
	}
	return true
}
