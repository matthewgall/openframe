package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
)

func newLocalFS(t *testing.T) *LocalFS {
	t.Helper()
	fs, err := NewLocalFS(t.TempDir())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	return fs
}

func TestPutOpenExistsRoundTrip(t *testing.T) {
	fs := newLocalFS(t)
	ctx := context.Background()

	data := []byte("hello openframe storage")
	hash := sha256.Sum256(data)
	key := Key(hex.EncodeToString(hash[:]))

	if exists, _ := fs.Exists(ctx, key); exists {
		t.Fatal("object exists before put")
	}
	if err := fs.Put(ctx, key, bytes.NewReader(data)); err != nil {
		t.Fatalf("put: %v", err)
	}
	if exists, _ := fs.Exists(ctx, key); !exists {
		t.Fatal("object missing after put")
	}
	rc, err := fs.Open(ctx, key)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, data) {
		t.Fatal("round-trip mismatch")
	}
	size, err := fs.Size(ctx, key)
	if err != nil || size != int64(len(data)) {
		t.Fatalf("size: %d %v", size, err)
	}
}

func TestPutContentAddressedMismatch(t *testing.T) {
	fs := newLocalFS(t)
	ctx := context.Background()

	data := []byte("content A")
	hash := sha256.Sum256(data)
	key := Key(hex.EncodeToString(hash[:]))
	if err := fs.Put(ctx, key, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	// Writing different bytes under the same hash key must fail.
	if err := fs.Put(ctx, key, bytes.NewReader([]byte("content B"))); err == nil {
		t.Fatal("content-addressing violation accepted")
	}
	// The original object must be intact.
	rc, _ := fs.Open(ctx, key)
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, data) {
		t.Fatal("original object corrupted")
	}
	// Re-writing identical bytes is a no-op success.
	if err := fs.Put(ctx, key, bytes.NewReader(data)); err != nil {
		t.Fatalf("identical re-put: %v", err)
	}
}

func TestPutRejectsBadKeys(t *testing.T) {
	fs := newLocalFS(t)
	ctx := context.Background()
	badKeys := []Key{
		"../../etc/passwd",
		"absolute/path",
		"UPPERCASE",
		"short",
		"not-hex-hex-hex-hex-hex-hex-hex-hex-hex-hex-hex-hex",
		Key(""),
	}
	for _, k := range badKeys {
		if err := fs.Put(ctx, k, strings.NewReader("x")); err == nil {
			t.Fatalf("accepted bad key %q", k)
		}
	}
	// Derivative keys (hash.variant) are accepted.
	data := []byte("derivative bytes")
	hash := sha256.Sum256(data)
	dk := Key(hex.EncodeToString(hash[:]) + "." + VariantThumb)
	if err := fs.Put(ctx, dk, bytes.NewReader(data)); err != nil {
		t.Fatalf("rejected valid derivative key: %v", err)
	}
}

func TestOpenNotFound(t *testing.T) {
	fs := newLocalFS(t)
	ctx := context.Background()
	key := Key(strings.Repeat("ab", 32))
	if _, err := fs.Open(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got: %v", err)
	}
	if exists, _ := fs.Exists(ctx, key); exists {
		t.Fatal("exists reported true for missing object")
	}
}

func TestTotalUsage(t *testing.T) {
	fs := newLocalFS(t)
	ctx := context.Background()

	d1 := []byte("1234567890")
	d2 := []byte("abcdefghij")
	for _, d := range [][]byte{d1, d2} {
		h := sha256.Sum256(d)
		if err := fs.Put(ctx, Key(hex.EncodeToString(h[:])), bytes.NewReader(d)); err != nil {
			t.Fatal(err)
		}
	}
	usage, err := fs.TotalUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if usage != int64(len(d1)+len(d2)) {
		t.Fatalf("usage: %d want %d", usage, len(d1)+len(d2))
	}
}
