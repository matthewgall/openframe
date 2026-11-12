// Package storage defines the server-side object storage boundary.
//
// Two backends are specified (local filesystem, S3-compatible). M2
// ships the local filesystem backend; the S3 backend arrives later
// without API changes (spec §5: "Do not require S3 for initial
// deployment").
//
// Objects are immutable and content-addressed: a key is the SHA-256 of
// the object bytes (plus a variant suffix for derivatives). Write
// semantics are "create if absent" — an existing key always refers to
// the same bytes, so concurrent duplicate uploads cannot corrupt
// anything and "replacing" an object is impossible by design.
package storage

import (
	"context"
	"fmt"
	"io"
)

// Key identifies an object. For originals the key is the bare sha256
// hex; for derivatives it is "<original sha256>.<variant>" (e.g.
// "<hash>.display").
type Key string

// Original is the key suffix for an original object (no suffix).
const Original = ""

// Variant names for derived objects.
const (
	VariantDisplay   = "display"   // long edge 1920, aspect preserved
	VariantThumb     = "thumb"     // 384x384 center crop
	VariantPortrait  = "portrait"  // 1080x1620 center crop (portrait frames)
	VariantLandscape = "landscape" // 1920x1200 center crop (landscape frames)
)

// AllVariants lists every derivative variant (order is generation order).
var AllVariants = []string{VariantDisplay, VariantThumb, VariantPortrait, VariantLandscape}

// Storage is the backend abstraction.
type Storage interface {
	// Open returns a reader for key. NotFound errors wrap ErrNotFound.
	Open(ctx context.Context, key Key) (io.ReadCloser, error)

	// Size reports the object size in bytes.
	Size(ctx context.Context, key Key) (int64, error)

	// Exists reports whether the object is present.
	Exists(ctx context.Context, key Key) (bool, error)

	// Put stores data under key. If the key already exists, the existing
	// object is verified to be byte-identical (content addressing) and
	// the write is skipped; a mismatch is an error.
	Put(ctx context.Context, key Key, data io.Reader) error

	// TotalUsage reports bytes used by all objects (bounded scan;
	// backends may cache with a short TTL).
	TotalUsage(ctx context.Context) (int64, error)
}

// ErrNotFound is wrapped when an object does not exist.
var ErrNotFound = fmt.Errorf("storage: object not found")
