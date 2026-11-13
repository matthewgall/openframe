package updater

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Update request markers (spec §14 Update scheduling: frames can
// request an update; the privileged updater honours the request at
// its next run). The frame is unprivileged and never installs; it can
// only *ask*. The marker file lives in the updates state dir (which
// the frame can write and the updater can read, both unprivileged as
// far as the filesystem is concerned — only the updater binary runs
// as root, and it alone touches slots and the service).
const (
	// RequestFileName is the update-request marker.
	RequestFileName = "update-request.json"
	// RequestMaxAge bounds how long an unprocessed request stays
	// valid (a request left for weeks is stale).
	RequestMaxAge = 7 * 24 * time.Hour
)

// UpdateRequest is the marker content.
type UpdateRequest struct {
	RequestedAt time.Time `json:"requested_at"`
	// By identifies the requester ("frame-ui", "cli", "admin").
	By string `json:"by"`
}

// RequestUpdate writes the marker (frame side, unprivileged).
func RequestUpdate(stateDir, by string) error {
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(UpdateRequest{RequestedAt: time.Now().UTC(), By: by})
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(stateDir, RequestFileName), b, 0o644)
}

// PendingRequest reads and validates the marker; nil when absent or
// stale.
func PendingRequest(stateDir string, now time.Time) *UpdateRequest {
	b, err := os.ReadFile(filepath.Join(stateDir, RequestFileName))
	if err != nil {
		return nil
	}
	var r UpdateRequest
	if err := json.Unmarshal(b, &r); err != nil {
		return nil
	}
	if now.Sub(r.RequestedAt) > RequestMaxAge {
		return nil
	}
	return &r
}

// ClearRequest removes the marker (updater side, after consuming it).
func ClearRequest(stateDir string) {
	_ = os.Remove(filepath.Join(stateDir, RequestFileName))
}

// ErrNoRequest is returned when there is no pending request.
var ErrNoRequest = errors.New("updater: no pending update request")

// writeFileAtomic writes b to path via temp+fsync+rename.
func writeFileAtomic(path string, b []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name) // no-op after rename
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return fsyncDir(dir)
}

// String renders a short summary for CLI output.
func (r UpdateRequest) String() string {
	return fmt.Sprintf("%s by %s at %s", r.RequestedAt.UTC().Format(time.RFC3339), r.By, r.RequestedAt.Local().Format(time.RFC3339))
}
