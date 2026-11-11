// Package migrations embeds all SQL migrations into the binary.
//
// Layout:
//
//	migrations/server/NNNN_name.sql  server-side schema
//	migrations/frame/NNNN_name.sql   frame-side schema
//
// Versions are monotonically increasing per subset and must never be
// edited after release; checksums are enforced at migration time
// (internal/db). Rollback strategy is forward-fix only: OpenFrame
// migrations are written to be backward-compatible (additive columns,
// soft deletes) so an older binary can still read a newer database. See
// docs/architecture/overview.md "Database compatibility" and ADR-0008.
package migrations

import (
	"embed"
	"io/fs"
)

//go:embed server frame
var all embed.FS

// FS exposes the embedded migration tree to internal/db.
func FS() fs.FS {
	sub, err := fs.Sub(all, ".")
	if err != nil {
		panic("migrations: embedded layout changed: " + err.Error())
	}
	return sub
}
