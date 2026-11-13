// Package releases implements server-side release hosting for the
// secure updater (M6, spec §14, ADR-0007).
//
// The server stores the exact signed metadata bytes uploaded by the
// release tooling and serves them verbatim (latest.json); it never
// re-serialises, so the frame's signature check covers exactly what
// was signed. Serving is gated by explicit admin actions:
// upload → staged (hidden), approve → served + becomes latest for its
// (channel, os, arch), pause → suspended (latest recomputed).
package releases

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/matthewgall/openframe/internal/updater"
)

var (
	ErrNotFound  = errors.New("releases: not found")
	ErrDuplicate = errors.New("releases: already exists")
)

// Statuses.
const (
	StatusStaged   = "staged"
	StatusApproved = "approved"
	StatusPaused   = "paused"
)

// MaxArtifactBytes bounds an uploaded artifact (binaries are tens of
// MB; the cap rejects accidents, not legitimate builds).
const MaxArtifactBytes = int64(500 * 1024 * 1024)

// Release is the admin-visible record. Metadata holds the exact
// signed JSON bytes (served verbatim by latest.json).
type Release struct {
	Version            string    `json:"version"`
	Channel            string    `json:"channel"`
	OS                 string    `json:"os"`
	Arch               string    `json:"arch"`
	ArtifactSHA256     string    `json:"artifact_sha256"`
	ArtifactSize       int64     `json:"artifact_size"`
	Metadata           []byte    `json:"-"`
	Status             string    `json:"status"`
	IsLatest           bool      `json:"is_latest"`
	RolloutPercent     int       `json:"rollout_percent"`
	RolloutDevices     []string  `json:"rollout_devices"`
	ReleasedAt         time.Time `json:"released_at"`
	NotBefore          string    `json:"not_before,omitempty"`
	ExpiresAt          time.Time `json:"expires_at"`
	MinProtocolVersion int       `json:"min_protocol_version"`
	MinSchemaVersion   int       `json:"min_schema_version"`
	IsRollback         bool      `json:"is_rollback"`
	SignerKeyID        string    `json:"signer_key_id"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
	// HasArtifact reports whether the artifact file is on disk.
	HasArtifact bool `json:"has_artifact"`
}

// Store persists releases; artifacts live as flat files under
// ArtifactDir (managed by the server process, not the photo storage
// layout).
type Store struct {
	dbh         *sql.DB
	ArtifactDir string
}

func NewStore(dbh *sql.DB, artifactDir string) *Store {
	return &Store{dbh: dbh, ArtifactDir: artifactDir}
}

// ArtifactPath is the on-disk path of a release artifact.
func (s *Store) ArtifactPath(version, osName, arch string) string {
	name := fmt.Sprintf("%s-%s-%s.bin", version, osName, arch)
	return filepath.Join(s.ArtifactDir, name)
}

// Upload stores a signed release + artifact in staged state. The
// metadata must be a well-formed signed release (shape checked via
// updater.Release.ValidateBasic — the server trusts no signature; it
// only refuses malformed documents so the frame is never fed garbage).
func (s *Store) Upload(ctx context.Context, metadataJSON []byte, artifact []byte) (*Release, error) {
	var meta updater.Release
	if err := json.Unmarshal(metadataJSON, &meta); err != nil {
		return nil, fmt.Errorf("releases: metadata not JSON: %w", err)
	}
	if err := meta.ValidateBasic(); err != nil {
		return nil, fmt.Errorf("releases: %w", err)
	}
	if int64(len(artifact)) != meta.ArtifactSize {
		return nil, fmt.Errorf("releases: artifact size %d != declared %d", len(artifact), meta.ArtifactSize)
	}
	if updater.ArtifactSHA256Hex(artifact) != meta.ArtifactSHA256 {
		return nil, errors.New("releases: artifact sha256 does not match signed metadata")
	}
	// Store the canonical signed document exactly as sent.
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.dbh.ExecContext(ctx, `
		INSERT INTO releases (version, channel, os, arch, artifact_sha256, artifact_size,
		                      metadata, status, is_latest, rollout_percent, rollout_devices,
		                      released_at, not_before, expires_at, min_protocol_version,
		                      min_schema_version, is_rollback, signer_key_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		meta.Version, meta.Channel, meta.OS, meta.Arch, meta.ArtifactSHA256, meta.ArtifactSize,
		string(metadataJSON), StatusStaged, meta.RolloutPercent, mustJSON(meta.RolloutDevices),
		meta.ReleasedAt.UTC().Format(time.RFC3339), meta.NotBefore.UTC().Format(time.RFC3339),
		meta.ExpiresAt.UTC().Format(time.RFC3339), meta.MinProtocolVersion, meta.MinSchemaVersion,
		boolInt(meta.IsRollback), meta.SignerKeyID, now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrDuplicate
		}
		return nil, err
	}
	if err := os.MkdirAll(s.ArtifactDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(s.ArtifactPath(meta.Version, meta.OS, meta.Arch), artifact, 0o644); err != nil {
		return nil, err
	}
	return s.Get(ctx, meta.Version, meta.Channel, meta.OS, meta.Arch)
}

// Get fetches one release (metadata bytes included).
func (s *Store) Get(ctx context.Context, version, channel, osName, arch string) (*Release, error) {
	var (
		r      Release
		latest int
		meta   string
		devs   string
		relTS  string
		expTS  string
		creTS  string
		updTTS string
	)
	err := s.dbh.QueryRowContext(ctx, `
		SELECT version, channel, os, arch, artifact_sha256, artifact_size, metadata,
		       status, is_latest, rollout_percent, rollout_devices, released_at, not_before,
		       expires_at, min_protocol_version, min_schema_version, is_rollback,
		       signer_key_id, created_at, updated_at
		FROM releases WHERE version = ? AND channel = ? AND os = ? AND arch = ?`,
		version, channel, osName, arch).
		Scan(&r.Version, &r.Channel, &r.OS, &r.Arch, &r.ArtifactSHA256, &r.ArtifactSize, &meta,
			&r.Status, &latest, &r.RolloutPercent, &devs, &relTS, &r.NotBefore,
			&expTS, &r.MinProtocolVersion, &r.MinSchemaVersion, &r.IsRollback,
			&r.SignerKeyID, &creTS, &updTTS)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.IsLatest = latest == 1
	r.Metadata = []byte(meta)
	r.RolloutDevices, _ = unmarshalStringSlice(devs)
	r.ReleasedAt = parseTS(relTS)
	r.ExpiresAt = parseTS(expTS)
	r.CreatedAt = parseTS(creTS)
	r.UpdatedAt = parseTS(updTTS)
	if _, err := os.Stat(s.ArtifactPath(r.Version, r.OS, r.Arch)); err == nil {
		r.HasArtifact = true
	}
	return &r, nil
}

// List returns all releases, newest first.
func (s *Store) List(ctx context.Context) ([]Release, error) {
	rows, err := s.dbh.QueryContext(ctx, `
		SELECT version, channel, os, arch, artifact_sha256, artifact_size, metadata,
		       status, is_latest, rollout_percent, rollout_devices, released_at, not_before,
		       expires_at, min_protocol_version, min_schema_version, is_rollback,
		       signer_key_id, created_at, updated_at
		FROM releases ORDER BY created_at DESC, version DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Release
	for rows.Next() {
		var (
			r                          Release
			latest                     int
			meta                       string
			devs                       string
			relTS, expTS, creTS, updTS string
		)
		if err := rows.Scan(&r.Version, &r.Channel, &r.OS, &r.Arch, &r.ArtifactSHA256, &r.ArtifactSize, &meta,
			&r.Status, &latest, &r.RolloutPercent, &devs, &relTS, &r.NotBefore,
			&expTS, &r.MinProtocolVersion, &r.MinSchemaVersion, &r.IsRollback,
			&r.SignerKeyID, &creTS, &updTS); err != nil {
			return nil, err
		}
		r.IsLatest = latest == 1
		r.Metadata = nil // list view omits the (large) metadata body
		r.RolloutDevices, _ = unmarshalStringSlice(devs)
		r.ReleasedAt = parseTS(relTS)
		r.ExpiresAt = parseTS(expTS)
		r.CreatedAt = parseTS(creTS)
		r.UpdatedAt = parseTS(updTS)
		if _, err := os.Stat(s.ArtifactPath(r.Version, r.OS, r.Arch)); err == nil {
			r.HasArtifact = true
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Approve sets status=approved and makes the release the latest for
// its (channel, os, arch) — clearing the pointer from siblings.
// Approving a signed rollback release is how an operator points the
// channel back at an older, safe build.
func (s *Store) Approve(ctx context.Context, version, channel, osName, arch string) (*Release, error) {
	tx, err := s.dbh.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx,
		`UPDATE releases SET is_latest = 0 WHERE channel = ? AND os = ? AND arch = ?`,
		channel, osName, arch); err != nil {
		return nil, err
	}
	res2, err := tx.ExecContext(ctx,
		`UPDATE releases SET status = ?, is_latest = 1, updated_at = ?
		 WHERE version = ? AND channel = ? AND os = ? AND arch = ?`,
		StatusApproved, now, version, channel, osName, arch)
	if err != nil {
		return nil, err
	}
	if n, _ := res2.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.Get(ctx, version, channel, osName, arch)
}

// Pause suspends serving. If the paused release was latest, the
// pointer moves to the highest-version approved sibling (or none).
func (s *Store) Pause(ctx context.Context, version, channel, osName, arch string) (*Release, error) {
	tx, err := s.dbh.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := tx.ExecContext(ctx,
		`UPDATE releases SET status = ?, is_latest = 0, updated_at = ?
		 WHERE version = ? AND channel = ? AND os = ? AND arch = ?`,
		StatusPaused, now, version, channel, osName, arch)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	if err := s.recomputeLatestTx(ctx, tx, channel, osName, arch); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.Get(ctx, version, channel, osName, arch)
}

// Delete removes a release and its artifact; the latest pointer is
// recomputed when needed. Approved-and-latest releases must be paused
// first (an operator should make that decision explicitly).
func (s *Store) Delete(ctx context.Context, version, channel, osName, arch string) error {
	tx, err := s.dbh.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	err = tx.QueryRowContext(ctx,
		`SELECT status FROM releases WHERE version = ? AND channel = ? AND os = ? AND arch = ?`,
		version, channel, osName, arch).Scan(&status)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if status == StatusApproved {
		return errors.New("releases: pause an approved release before deleting it")
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM releases WHERE version = ? AND channel = ? AND os = ? AND arch = ?`,
		version, channel, osName, arch); err != nil {
		return err
	}
	if err := s.recomputeLatestTx(ctx, tx, channel, osName, arch); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_ = os.Remove(s.ArtifactPath(version, osName, arch))
	return nil
}

// SetRollout updates the staged-rollout fields of a release. The
// stored metadata is the signed document of record and is NOT
// rewritten (changing rollout is an operator action on the *serving*
// copy, mirrored from the signed document at upload; frames verify
// the served metadata against the pinned key).
func (s *Store) SetRollout(ctx context.Context, version, channel, osName, arch string, percent int, devices []string) (*Release, error) {
	if percent < 0 || percent > 100 {
		return nil, errors.New("releases: rollout_percent must be 0-100")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.dbh.ExecContext(ctx,
		`UPDATE releases SET rollout_percent = ?, rollout_devices = ?, updated_at = ?
		 WHERE version = ? AND channel = ? AND os = ? AND arch = ?`,
		percent, mustJSON(devices), now, version, channel, osName, arch)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrNotFound
	}
	return s.Get(ctx, version, channel, osName, arch)
}

// Latest returns the serving release for a platform, or ErrNotFound.
// Expired releases (expires_at in the past) are not served even if
// they are the pointer target — the pointer is recomputed on the fly.
func (s *Store) Latest(ctx context.Context, channel, osName, arch string, now time.Time) (*Release, error) {
	var (
		r                          Release
		latest                     int
		meta                       string
		devs                       string
		relTS, expTS, creTS, updTS string
	)
	err := s.dbh.QueryRowContext(ctx, `
		SELECT version, channel, os, arch, artifact_sha256, artifact_size, metadata,
		       status, is_latest, rollout_percent, rollout_devices, released_at, not_before,
		       expires_at, min_protocol_version, min_schema_version, is_rollback,
		       signer_key_id, created_at, updated_at
		FROM releases
		WHERE channel = ? AND os = ? AND arch = ? AND is_latest = 1 AND status = 'approved'
		LIMIT 1`, channel, osName, arch).
		Scan(&r.Version, &r.Channel, &r.OS, &r.Arch, &r.ArtifactSHA256, &r.ArtifactSize, &meta,
			&r.Status, &latest, &r.RolloutPercent, &devs, &relTS, &r.NotBefore,
			&expTS, &r.MinProtocolVersion, &r.MinSchemaVersion, &r.IsRollback,
			&r.SignerKeyID, &creTS, &updTS)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.IsLatest = true
	r.Metadata = []byte(meta)
	r.RolloutDevices, _ = unmarshalStringSlice(devs)
	r.ReleasedAt = parseTS(relTS)
	r.ExpiresAt = parseTS(expTS)
	r.CreatedAt = parseTS(creTS)
	r.UpdatedAt = parseTS(updTS)
	if !r.ExpiresAt.After(now) {
		// Expired pointer target: fall back to the next approved.
		if next, err := s.nextApproved(ctx, channel, osName, arch, r.Version, now); err == nil {
			return next, nil
		}
		return nil, ErrNotFound
	}
	return &r, nil
}

// nextApproved finds the highest-version approved, non-expired release
// for a platform, excluding excludeVersion. Version ordering is numeric
// (semver), so the comparison happens in Go: TEXT ordering would put
// 1.10.0 before 1.9.0.
func (s *Store) nextApproved(ctx context.Context, channel, osName, arch, exclude string, now time.Time) (*Release, error) {
	var best *Release
	for _, r := range approvedList(ctx, s.dbh, channel, osName, arch) {
		if r.Version == exclude {
			continue
		}
		if !r.ExpiresAt.After(now) {
			continue
		}
		if best == nil {
			best = r
			continue
		}
		vb, err := updater.ParseVersion(best.Version)
		if err != nil || !vb.Less(mustParse(r.Version)) {
			best = r
		}
	}
	if best == nil {
		return nil, ErrNotFound
	}
	return best, nil
}

func mustParse(s string) updater.Version {
	v, err := updater.ParseVersion(s)
	if err != nil {
		return updater.Version{}
	}
	return v
}

// approvedList loads all approved releases for a platform (small set;
// ordered numerically in Go).
func approvedList(ctx context.Context, dbh *sql.DB, channel, osName, arch string) []*Release {
	rows, err := dbh.QueryContext(ctx, `
		SELECT version, channel, os, arch, artifact_sha256, artifact_size, metadata,
		       status, is_latest, rollout_percent, rollout_devices, released_at, not_before,
		       expires_at, min_protocol_version, min_schema_version, is_rollback,
		       signer_key_id, created_at, updated_at
		FROM releases
		WHERE channel = ? AND os = ? AND arch = ? AND status = 'approved'`, channel, osName, arch)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*Release
	for rows.Next() {
		var (
			r                                      Release
			latest                                 int
			meta, devs, relTS, expTS, creTS, updTS string
		)
		if err := rows.Scan(&r.Version, &r.Channel, &r.OS, &r.Arch, &r.ArtifactSHA256, &r.ArtifactSize, &meta,
			&r.Status, &latest, &r.RolloutPercent, &devs, &relTS, &r.NotBefore,
			&expTS, &r.MinProtocolVersion, &r.MinSchemaVersion, &r.IsRollback,
			&r.SignerKeyID, &creTS, &updTS); err != nil {
			continue
		}
		r.Metadata = []byte(meta)
		r.RolloutDevices, _ = unmarshalStringSlice(devs)
		r.ReleasedAt = parseTS(relTS)
		r.ExpiresAt = parseTS(expTS)
		r.CreatedAt = parseTS(creTS)
		r.UpdatedAt = parseTS(updTS)
		out = append(out, &r)
	}
	return out
}

// recomputeLatestTx points is_latest at the highest-version approved
// release for the platform (used after pause/delete). Numeric semver
// ordering (in Go — see nextApproved).
func (s *Store) recomputeLatestTx(ctx context.Context, tx *sql.Tx, channel, osName, arch string) error {
	var next string
	var bestVer updater.Version
	found := false
	rows, err := tx.QueryContext(ctx,
		`SELECT version FROM releases
		 WHERE channel = ? AND os = ? AND arch = ? AND status = 'approved'`, channel, osName, arch)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return err
		}
		pv, err := updater.ParseVersion(v)
		if err != nil {
			continue
		}
		if !found || bestVer.Less(pv) {
			bestVer, next, found = pv, v, true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !found {
		return nil
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE releases SET is_latest = 1 WHERE version = ? AND channel = ? AND os = ? AND arch = ?`,
		next, channel, osName, arch)
	return err
}

// Artifact opens the artifact file for serving (Range-capable via
// http.ServeContent).
func (s *Store) Artifact(version, osName, arch string) (*os.File, int64, error) {
	p := s.ArtifactPath(version, osName, arch)
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, ErrNotFound
		}
		return nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, st.Size(), nil
}

// --- helpers ---

func parseTS(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func unmarshalStringSlice(s string) ([]string, error) {
	if s == "" || s == "null" {
		return []string{}, nil
	}
	var out []string
	err := json.Unmarshal([]byte(s), &out)
	return out, err
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
