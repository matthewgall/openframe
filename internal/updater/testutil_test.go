package updater

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
)

// writeFile writes b to path with perm (test helper).
func writeFile(path string, b []byte, perm os.FileMode) error {
	return os.WriteFile(path, b, perm)
}

// genKey returns a fresh Ed25519 key pair (for forged-signature tests).
func genKey() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

// keysB64 returns the base64 encoding of key-b's public key (for
// rotation tests).
func (tt *testTrust) keysB64() string {
	return base64.StdEncoding.EncodeToString(tt.keyB.Public().(ed25519.PublicKey))
}
