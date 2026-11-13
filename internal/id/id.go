// Package id generates RFC 4122 version 4 UUIDs from crypto/rand.
//
// We use UUIDs for device identities and pairing challenges so that
// identities are unguessable and do not embed sequence numbers that a
// watcher could correlate.
package id

import (
	"crypto/rand"
	"fmt"
)

// NewUUID returns a random (version 4) UUID string.
func NewUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate uuid: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// IsValidUUID reports whether s is a canonical 36-character lowercase
// UUID string (version 4 preferred but any version accepted; device
// identities are always v4).
func IsValidUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
				return false
			}
		}
	}
	return true
}
