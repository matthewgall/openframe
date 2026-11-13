package updater

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"
)

// testTrust holds two signing key pairs and a trust store pinning
// both, for the verification tests.
type testTrust struct {
	trust  *TrustedKeys
	keyA   ed25519.PrivateKey
	keyB   ed25519.PrivateKey
	keyAID string
	keyBID string
}

func newTestTrust(t *testing.T) *testTrust {
	t.Helper()
	mk := func(id string) (ed25519.PrivateKey, TrustedKey) {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return priv, TrustedKey{KeyID: id, Ed25519: base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))}
	}
	a, aKey := mk("key-a")
	b, bKey := mk("key-b")
	return &testTrust{
		trust:  &TrustedKeys{Keys: map[string]TrustedKey{"key-a": aKey, "key-b": bKey}},
		keyA:   a,
		keyB:   b,
		keyAID: "key-a",
		keyBID: "key-b",
	}
}

// sampleRelease returns a signed, currently-valid release for
// linux/arm64, version 1.5.0, full rollout.
func (tt *testTrust) sampleRelease(t *testing.T) Release {
	t.Helper()
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	rel := Release{
		Version:            "1.5.0",
		Channel:            ChannelStable,
		OS:                 "linux",
		Arch:               "arm64",
		ArtifactSHA256:     strings.Repeat("ab", 32),
		ArtifactSize:       1000,
		MinProtocolVersion: 1,
		MinSchemaVersion:   3,
		ReleasedAt:         now.Add(-time.Hour),
		ExpiresAt:          now.Add(24 * time.Hour),
		RolloutPercent:     100,
	}
	signed, err := SignRelease(rel, tt.keyAID, tt.keyA)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func verifyOpts(now time.Time) VerifyOptions {
	return VerifyOptions{
		Now:           now,
		OS:            "linux",
		Arch:          "arm64",
		MaxProtocol:   1,
		SchemaVersion: 3,
		MaxAccepted:   map[string]string{},
	}
}

func TestSignAndVerify(t *testing.T) {
	tt := newTestTrust(t)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	rel := tt.sampleRelease(t)
	v := NewVerifier(tt.trust)
	res, err := v.Verify(rel, verifyOpts(now))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if res.Release.Version != "1.5.0" || res.NewKeys != nil {
		t.Fatalf("result = %+v", res)
	}
}

// TestVerifyTamperedField signs a release then corrupts each signed
// field individually: every one must fail signature verification.
func TestVerifyTamperedField(t *testing.T) {
	tt := newTestTrust(t)
	now := verifyOpts(time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)).Now
	base := tt.sampleRelease(t)
	v := NewVerifier(tt.trust)

	mutate := func(name string, f func(*Release)) {
		t.Run(name, func(t *testing.T) {
			rel := base
			f(&rel)
			if _, err := v.Verify(rel, verifyOpts(now)); !errors.Is(err, ErrBadSignature) {
				t.Fatalf("tampered %s: got %v, want ErrBadSignature", name, err)
			}
		})
	}

	mutate("version", func(r *Release) { r.Version = "1.5.1" })
	mutate("channel", func(r *Release) { r.Channel = ChannelBeta })
	mutate("os", func(r *Release) { r.OS = "darwin" })
	mutate("arch", func(r *Release) { r.Arch = "amd64" })
	mutate("sha256", func(r *Release) { r.ArtifactSHA256 = strings.Repeat("cd", 32) })
	mutate("size", func(r *Release) { r.ArtifactSize = 999 })
	mutate("min_protocol", func(r *Release) { r.MinProtocolVersion = 2 })
	mutate("min_schema", func(r *Release) { r.MinSchemaVersion = 4 })
	mutate("released_at", func(r *Release) { r.ReleasedAt = r.ReleasedAt.Add(time.Minute) })
	mutate("expires_at", func(r *Release) { r.ExpiresAt = r.ExpiresAt.Add(time.Minute) })
	mutate("rollout_percent", func(r *Release) { r.RolloutPercent = 50 })
	mutate("is_rollback", func(r *Release) { r.IsRollback = true })
	mutate("signer_key_id", func(r *Release) { r.SignerKeyID = tt.keyBID })
}

func TestVerifyUnknownSigner(t *testing.T) {
	tt := newTestTrust(t)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	rel := tt.sampleRelease(t)
	// Trust store without the signing key.
	lone := &TrustedKeys{Keys: map[string]TrustedKey{}}
	for _, k := range tt.trust.Keys {
		if k.KeyID != rel.SignerKeyID {
			lone.Keys[k.KeyID] = k
		}
	}
	if _, err := NewVerifier(lone).Verify(rel, verifyOpts(now)); !errors.Is(err, ErrUnknownSigner) {
		t.Fatalf("got %v, want ErrUnknownSigner", err)
	}
}

func TestVerifyTimeBounds(t *testing.T) {
	tt := newTestTrust(t)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	v := NewVerifier(tt.trust)

	// Times are part of the signed payload, so the release must be
	// signed *with* the time bound under test.
	signedWith := func(mut func(*Release)) Release {
		rel := Release{
			Version:            "1.5.0",
			Channel:            ChannelStable,
			OS:                 "linux",
			Arch:               "arm64",
			ArtifactSHA256:     strings.Repeat("ab", 32),
			ArtifactSize:       1000,
			MinProtocolVersion: 1,
			MinSchemaVersion:   3,
			ReleasedAt:         now.Add(-time.Hour),
			ExpiresAt:          now.Add(24 * time.Hour),
			RolloutPercent:     100,
		}
		mut(&rel)
		s, err := SignRelease(rel, tt.keyAID, tt.keyA)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	t.Run("expired", func(t *testing.T) {
		rel := signedWith(func(r *Release) { r.ExpiresAt = now.Add(-time.Minute) })
		if _, err := v.Verify(rel, verifyOpts(now)); !errors.Is(err, ErrExpired) {
			t.Fatalf("got %v, want ErrExpired", err)
		}
	})
	t.Run("not_before", func(t *testing.T) {
		rel := signedWith(func(r *Release) {
			r.NotBefore = now.Add(time.Hour)
			r.ExpiresAt = now.Add(24 * time.Hour)
		})
		if _, err := v.Verify(rel, verifyOpts(now)); !errors.Is(err, ErrNotBefore) {
			t.Fatalf("got %v, want ErrNotBefore", err)
		}
	})
}

func TestVerifyPlatformMismatch(t *testing.T) {
	tt := newTestTrust(t)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	rel := tt.sampleRelease(t)
	v := NewVerifier(tt.trust)
	opts := verifyOpts(now)
	opts.Arch = "amd64"
	if _, err := v.Verify(rel, opts); !errors.Is(err, ErrPlatformMismatch) {
		t.Fatalf("got %v, want ErrPlatformMismatch", err)
	}
}

func TestVerifyCompatibilityBounds(t *testing.T) {
	tt := newTestTrust(t)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	v := NewVerifier(tt.trust)

	t.Run("protocol", func(t *testing.T) {
		rel := tt.sampleRelease(t)
		opts := verifyOpts(now)
		opts.MaxProtocol = 0 // frame speaks nothing the release requires
		if _, err := v.Verify(rel, opts); !errors.Is(err, ErrProtocolIncompat) {
			t.Fatalf("got %v, want ErrProtocolIncompat", err)
		}
	})
	t.Run("schema", func(t *testing.T) {
		rel := tt.sampleRelease(t)
		opts := verifyOpts(now)
		opts.SchemaVersion = 2 // frame DB older than the release requires
		if _, err := v.Verify(rel, opts); !errors.Is(err, ErrSchemaIncompat) {
			t.Fatalf("got %v, want ErrSchemaIncompat", err)
		}
	})
}

func TestVerifyAntiRollback(t *testing.T) {
	tt := newTestTrust(t)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	v := NewVerifier(tt.trust)
	opts := verifyOpts(now)
	opts.MaxAccepted[ChannelStable] = "1.5.0"

	mkVer := func(ver string, rollback bool) Release {
		rel := tt.sampleRelease(t)
		rel.Version = ver
		rel.IsRollback = rollback
		signed, err := SignRelease(rel, tt.keyAID, tt.keyA)
		if err != nil {
			t.Fatal(err)
		}
		return signed
	}

	if _, err := v.Verify(mkVer("1.4.9", false), opts); !errors.Is(err, ErrDowngrade) {
		t.Fatalf("older release: got %v, want ErrDowngrade", err)
	}
	if _, err := v.Verify(mkVer("1.5.0", false), opts); !errors.Is(err, ErrDowngrade) {
		t.Fatalf("same version: got %v, want ErrDowngrade (must be strictly newer)", err)
	}
	if _, err := v.Verify(mkVer("1.5.1", false), opts); err != nil {
		t.Fatalf("newer release: got %v, want ok", err)
	}
	// A signed rollback release may move backwards — it is the
	// authorised direction.
	if _, err := v.Verify(mkVer("1.4.9", true), opts); err != nil {
		t.Fatalf("signed rollback: got %v, want ok", err)
	}
	// The floor is per-channel: the same version on beta is fresh.
	opts.MaxAccepted[ChannelBeta] = ""
	rel := mkVer("1.5.0", false)
	rel.Channel = ChannelBeta
	signed, err := SignRelease(rel, tt.keyAID, tt.keyA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(signed, opts); err != nil {
		t.Fatalf("beta first release: got %v, want ok", err)
	}
}

func TestVerifyKeyRotation(t *testing.T) {
	tt := newTestTrust(t)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	v := NewVerifier(tt.trust)

	// key-b signs a rotation event adding key-c.
	_, keyC, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rel := tt.sampleRelease(t)
	rel.NewKeys = []NewKey{{KeyID: "key-c", Ed25519: base64.StdEncoding.EncodeToString(keyC.Public().(ed25519.PublicKey))}}
	signed, err := SignRelease(rel, tt.keyBID, tt.keyB)
	if err != nil {
		t.Fatal(err)
	}
	res, err := v.Verify(signed, verifyOpts(now))
	if err != nil {
		t.Fatalf("rotation: %v", err)
	}
	if res.NewKeys == nil || len(res.NewKeys.Keys) != 1 {
		t.Fatalf("NewKeys = %+v", res.NewKeys)
	}
	if _, err := res.NewKeys.Public("key-c"); err != nil {
		t.Fatalf("rotated key not usable: %v", err)
	}

	// A forged rotation (signed by key-a but *claiming* to add a key
	// is fine — the signature authorises it; what must fail is a
	// malformed key in the event).
	bad := tt.sampleRelease(t)
	bad.NewKeys = []NewKey{{KeyID: "key-x", Ed25519: "not-base64!!"}}
	signed, err = SignRelease(bad, tt.keyAID, tt.keyA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(signed, verifyOpts(now)); !errors.Is(err, ErrInvalidNewKey) {
		t.Fatalf("malformed rotation: got %v, want ErrInvalidNewKey", err)
	}
}

func TestVerifyArtifact(t *testing.T) {
	tt := newTestTrust(t)
	rel := tt.sampleRelease(t)
	// Build an artifact matching the signed digest.
	data := make([]byte, rel.ArtifactSize)
	for i := range data {
		data[i] = byte(i % 7)
	}
	rel.ArtifactSHA256 = ArtifactSHA256Hex(data)
	signed, err := SignRelease(rel, tt.keyAID, tt.keyA)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyArtifact(data, signed); err != nil {
		t.Fatalf("good artifact: %v", err)
	}
	if err := VerifyArtifact(data[:len(data)-1], signed); !errors.Is(err, ErrArtifactSize) {
		t.Fatalf("short artifact: got %v, want ErrArtifactSize", err)
	}
	corrupt := append([]byte(nil), data...)
	corrupt[10] ^= 0xff
	if err := VerifyArtifact(corrupt, signed); !errors.Is(err, ErrArtifactHash) {
		t.Fatalf("corrupt artifact: got %v, want ErrArtifactHash", err)
	}
}

func TestRolloutEligible(t *testing.T) {
	// Allow-list wins regardless of percentage.
	if !rolloutEligible(Release{RolloutDevices: []string{"dev-1"}}, "dev-1") {
		t.Fatal("allow-listed device must be eligible")
	}
	if rolloutEligible(Release{RolloutDevices: []string{"dev-1"}}, "dev-2") {
		t.Fatal("non-listed device with 0% must not be eligible")
	}
	if !rolloutEligible(Release{RolloutPercent: 100}, "anyone") {
		t.Fatal("100% must include everyone")
	}
	if rolloutEligible(Release{RolloutPercent: 0}, "anyone") {
		t.Fatal("0% must include no one")
	}

	// Stability + nested cohorts: with 1000 fake devices, the 10%
	// cohort must be a strict subset of the 50% cohort, and each
	// device's membership must be deterministic.
	const n = 1000
	var in10, in50 int
	for i := 0; i < n; i++ {
		dev := "device-" + string(rune('a'+i/26)) + string(rune('a'+i%26)) + string(rune('0'+i%10)) + string(rune('0'+(i/10)%10))
		a := rolloutEligible(Release{RolloutPercent: 10}, dev)
		b := rolloutEligible(Release{RolloutPercent: 10}, dev)
		if a != b {
			t.Fatalf("membership not stable for %s", dev)
		}
		if a {
			in10++
			if !rolloutEligible(Release{RolloutPercent: 50}, dev) {
				t.Fatalf("10%% cohort not nested in 50%% cohort: %s", dev)
			}
		}
		if rolloutEligible(Release{RolloutPercent: 50}, dev) {
			in50++
		}
	}
	if in10 < 85 || in10 > 115 {
		t.Fatalf("10%% cohort size %d implausibly off 100", in10)
	}
	if in50 < 470 || in50 > 530 {
		t.Fatalf("50%% cohort size %d implausibly off 500", in50)
	}
}

func TestVersionParsing(t *testing.T) {
	cases := []struct {
		a, b string
		less bool // a < b ?
	}{
		{"1.2.3", "1.2.4", true},
		{"1.2.3", "1.3.0", true},
		{"1.2.3", "2.0.0", true},
		{"1.2.3", "1.2.3", false},
		{"1.10.0", "1.9.0", false},
	}
	for _, c := range cases {
		va, err := ParseVersion(c.a)
		if err != nil {
			t.Fatal(err)
		}
		vb, err := ParseVersion(c.b)
		if err != nil {
			t.Fatal(err)
		}
		if got := va.Less(vb); got != c.less {
			t.Errorf("%s < %s = %v, want %v", c.a, c.b, got, c.less)
		}
	}
	if _, err := ParseVersion("1.2"); err == nil {
		t.Error("1.2 must be rejected")
	}
	if _, err := ParseVersion("x.y.z"); err == nil {
		t.Error("x.y.z must be rejected")
	}
}

// TestSignedPayloadDeterministic pins the canonicalisation: equal
// logical content (different slice orderings, equal instants in
// different zones) must produce byte-identical payloads.
func TestSignedPayloadDeterministic(t *testing.T) {
	mk := func() Release {
		rel := Release{
			Version:            "1.0.0",
			Channel:            ChannelStable,
			OS:                 "linux",
			Arch:               "arm64",
			ArtifactSHA256:     strings.Repeat("00", 32),
			ArtifactSize:       10,
			MinProtocolVersion: 1,
			MinSchemaVersion:   3,
			ReleasedAt:         time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			ExpiresAt:          time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
			RolloutDevices:     []string{"b", "a", "c"},
			NewKeys: []NewKey{
				{KeyID: "k2", Ed25519: "AAAA"},
				{KeyID: "k1", Ed25519: "BBBB"},
			},
			SignerKeyID: "key-a",
		}
		return rel
	}
	p1, err := mk().SignedPayload()
	if err != nil {
		t.Fatal(err)
	}
	r2 := mk()
	r2.ReleasedAt = time.Date(2026, 1, 1, 1, 0, 0, 0, time.FixedZone("+01:00", 3600)) // same instant
	r2.RolloutDevices = []string{"c", "b", "a"}
	r2.NewKeys = []NewKey{{KeyID: "k1", Ed25519: "BBBB"}, {KeyID: "k2", Ed25519: "AAAA"}}
	p2, err := r2.SignedPayload()
	if err != nil {
		t.Fatal(err)
	}
	if string(p1) != string(p2) {
		t.Fatalf("payloads differ:\n%s\n%s", p1, p2)
	}
}

func TestLoadTrustedKeys(t *testing.T) {
	tt := newTestTrust(t)
	b, err := tt.trust.Encode()
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/trusted-keys.json"
	if err := writeFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadTrustedKeys(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.Keys) != 2 {
		t.Fatalf("keys = %d", len(loaded.Keys))
	}

	// Empty store is rejected.
	empty := t.TempDir() + "/empty.json"
	writeFile(empty, []byte(`{"keys":{}}`), 0o600)
	if _, err := LoadTrustedKeys(empty); !errors.Is(err, ErrNoTrustedKeys) {
		t.Fatalf("empty: got %v, want ErrNoTrustedKeys", err)
	}
	// Key id mismatch is rejected.
	bad := t.TempDir() + "/bad.json"
	writeFile(bad, []byte(`{"keys":{"a":{"key_id":"b","ed25519":"`+tt.trust.Keys["key-a"].Ed25519+`"}}}`), 0o600)
	if _, err := LoadTrustedKeys(bad); err == nil {
		t.Fatal("id mismatch must be rejected")
	}
	// Bad key bytes are rejected.
	bad2 := t.TempDir() + "/bad2.json"
	writeFile(bad2, []byte(`{"keys":{"a":{"key_id":"a","ed25519":"aGk="}}}`), 0o600)
	if _, err := LoadTrustedKeys(bad2); err == nil {
		t.Fatal("short key must be rejected")
	}
}
