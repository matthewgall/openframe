package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
)

// Fixed test vectors for the RFC 6238 test secret
// "12345678901234567890" (base32 GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ),
// cross-verified against pquerna/otp and an independent Python
// stdlib implementation (HMAC-SHA1, 6 digits, 30s):
//
//	T=59          (counter 1)         -> 287082
//	T=1111111109  (counter 37037036)  -> 081804
//	T=1111111120  (counter 37037037)  -> 050471
func TestTOTPRFCVectors(t *testing.T) {
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	if !VerifyTOTP(secret, "287082", time.Unix(59, 0)) {
		t.Error("vector T=59 (287082) must verify")
	}
	if !VerifyTOTP(secret, "081804", time.Unix(1111111109, 0)) {
		t.Error("vector T=1111111109 (081804) must verify")
	}
	if !VerifyTOTP(secret, "050471", time.Unix(1111111120, 0)) {
		t.Error("vector T=1111111120 (050471) must verify")
	}
	// The code from the previous 30s window must verify via the -1
	// step, the one two windows back must not (library-generated:
	// the reference implementation, independently of ours).
	t0 := time.Unix(1111111120, 0)
	prev, err := totp.GenerateCode(secret, t0.Add(-TOTPPeriod))
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyTOTP(secret, prev, t0) {
		t.Errorf("previous-window code %q must verify via -1 step", prev)
	}
	old, err := totp.GenerateCode(secret, t0.Add(-2*TOTPPeriod))
	if err != nil {
		t.Fatal(err)
	}
	if VerifyTOTP(secret, old, t0) {
		t.Errorf("two-windows-back code %q must NOT verify", old)
	}
}

func TestVerifyTOTPWithLibraryCodes(t *testing.T) {
	secret, err := GenerateTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	// The reference library (pquerna/otp) must agree with our
	// implementation at now, now±30s.
	for _, offset := range []time.Duration{0, -TOTPPeriod, TOTPPeriod} {
		code, err := totp.GenerateCode(secret, now.Add(offset))
		if err != nil {
			t.Fatal(err)
		}
		if !VerifyTOTP(secret, code, now) {
			t.Errorf("library code %q (offset %v) must verify", code, offset)
		}
	}
	// Garbage must never verify.
	for _, bad := range []string{"", "12345", "1234567", "abcdef", "12 3456"} {
		if VerifyTOTP(secret, bad, now) {
			t.Errorf("garbage %q must not verify", bad)
		}
	}
}

func TestRecoveryCodes(t *testing.T) {
	codes, err := GenerateRecoveryCodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != RecoveryCodeCount {
		t.Fatalf("want %d codes, got %d", RecoveryCodeCount, len(codes))
	}
	seen := map[string]bool{}
	for _, c := range codes {
		if seen[c] {
			t.Fatalf("duplicate recovery code %q", c)
		}
		seen[c] = true
		if !IsRecoveryCodeShape(c) {
			t.Errorf("generated code %q fails its own shape check", c)
		}
	}
	// Shape check: the alphabet excludes confusables.
	for _, r := range "0O1I" {
		if strings.ContainsRune(recoveryAlphabet, r) {
			t.Errorf("alphabet must not contain confusable %c", r)
		}
	}
	if IsRecoveryCodeShape("123456") || IsRecoveryCodeShape("12345-67890") ||
		IsRecoveryCodeShape("AAAAA_BBBBB") || IsRecoveryCodeShape("O0O0O-O0O0O") {
		t.Error("shape check accepts non-alphabet or malformed codes")
	}
	// A, B, E are IN the alphabet: this is a well-formed code.
	if !IsRecoveryCodeShape("AAAAA-BBBBB") {
		t.Error("well-formed code must pass the shape check")
	}
	// Hashing is stable and case-insensitive on input.
	h1 := HashRecoveryCode("ABCDE-FGHIJ")
	h2 := HashRecoveryCode("abcde-fghij")
	if h1 != HashRecoveryCode("ABCDE-FGHIJ") || h1 != h2 {
		t.Error("recovery hash must be stable and case-insensitive")
	}
	if h1 == HashRecoveryCode("ABCDE-FGHIK") {
		t.Error("different codes must hash differently")
	}
}
