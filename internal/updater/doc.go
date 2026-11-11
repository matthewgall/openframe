// Package updater will host the secure automatic update coordinator
// (milestone 6).
//
// Planned boundaries (documented now so M6 does not reshape the rest of
// the tree; see ADR-0007):
//
//   - The updater runs as a separate binary (cmd/openframe-updater),
//     root-owned, with narrowly scoped privileges. The frame application
//     runs unprivileged and only requests updates; it never executes
//     binaries or rewrites its own slot.
//   - A/B slot deployment under /opt/openframe/slots/{a,b} with an
//     atomic "current" symlink and a recoverable transaction file under
//     /var/lib/openframe/updates/state.json.
//   - Ed25519-signed release metadata with pinned trusted keys,
//     anti-rollback versioning, channel metadata (stable/beta/dev) and
//     staged rollout.
//   - Automatic rollback to the last known-good slot, including across
//     power failure at every stage.
//
// This package exists from M1 to fix the dependency direction:
// internal/frame may depend on internal/updater (to express update
// requests and state), but the updater never depends on frame display
// or sync code.
package updater
