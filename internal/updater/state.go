package updater

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Update phases. The state file is the single source of truth the
// updater reconciles after any interruption (spec §14: "Power failure
// before activation", "Power failure after activation", "Interrupted
// rollback"). Phases that have *committed* nothing new recover to
// idle; phases after activation recover by checking the running slot
// against last-good (see Reconcile in updater.go).
const (
	PhaseIdle        = "idle"
	PhaseChecking    = "checking"
	PhaseDownloading = "downloading"
	PhaseVerifying   = "verifying"
	PhaseActivating  = "activating"
	PhaseValidating  = "validating"
	PhaseCommitting  = "committing"
	PhaseRollback    = "rollback"
	PhaseFailed      = "failed"
)

// MaxBootAttempts caps how many times the reconciler will attempt to
// roll back an uncommitted candidate at boot before declaring the
// update failed and leaving the frame in its current (safe) state for
// operator intervention. Prevents an infinite boot/rollback flap when
// something other than the slot is wrong.
const MaxBootAttempts = 3

// FailureCooldown is how long a release that failed validation (or
// integrity) is skipped before it may be retried. A failed candidate
// must not be re-attempted on every timer tick — each attempt costs a
// stop/swap/start/validate cycle of the running frame. Operators can
// force a retry; a re-released build with a new version number is not
// affected by the cooldown.
const FailureCooldown = 24 * time.Hour

// FailedTarget records the most recent release that failed validation
// or integrity, so the same target is skipped during FailureCooldown.
type FailedTarget struct {
	Version string    `json:"version"`
	Channel string    `json:"channel"`
	At      time.Time `json:"at"`
}

// State is the persisted update state, written atomically to
// <state-dir>/state.json. It is also what the frame reads (unprivileged)
// to report update_status in its health payload.
type State struct {
	// Phase is the current phase (see constants).
	Phase string `json:"phase"`

	// ActiveSlot/ActiveVersion describe the currently active binary
	// (what `current` points at and the version it is believed to be).
	ActiveSlot    string `json:"active_slot"`
	ActiveVersion string `json:"active_version,omitempty"`

	// LastGoodSlot/LastGoodVersion is the last *committed, healthy*
	// slot — the rollback target. Never modified until a commit.
	LastGoodSlot    string `json:"last_good_slot"`
	LastGoodVersion string `json:"last_good_version,omitempty"`

	// Candidate fields are set when an update is in flight.
	CandidateSlot string `json:"candidate_slot,omitempty"`
	TargetVersion string `json:"target_version,omitempty"`
	TargetSHA256  string `json:"target_sha256,omitempty"`
	Channel       string `json:"channel,omitempty"`

	// MaxAccepted[channel] is the highest version ever *committed* on
	// that channel — the anti-rollback floor the verifier checks.
	MaxAccepted map[string]string `json:"max_accepted,omitempty"`

	// BootAttempts counts consecutive boot-reconcile rollback attempts
	// against an uncommitted candidate; reset on commit or idle.
	BootAttempts int `json:"boot_attempts,omitempty"`

	LastError    string    `json:"last_error,omitempty"`
	LastUpdateAt time.Time `json:"last_update_at,omitempty"`
	// LastFailed is the most recent failed release target (see
	// FailedTarget / FailureCooldown).
	LastFailed FailedTarget `json:"last_failed,omitempty"`
	UpdatedAt  time.Time    `json:"updated_at"`
}

// StateFileName is the state file name in the updates directory.
const StateFileName = "state.json"

// DefaultState returns an idle state describing a fresh layout where
// slot a holds the initial binary.
func DefaultState(layout *Layout) (*State, error) {
	slot, err := layout.ActiveSlot()
	if err != nil {
		return nil, err
	}
	return &State{
		Phase:        PhaseIdle,
		ActiveSlot:   slot,
		LastGoodSlot: slot,
		MaxAccepted:  map[string]string{},
		UpdatedAt:    time.Now().UTC(),
	}, nil
}

// LoadState reads the state file; a missing file yields an initial
// idle state derived from the layout.
func LoadState(dir string, layout *Layout) (*State, error) {
	path := filepath.Join(dir, StateFileName)
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return DefaultState(layout)
	}
	if err != nil {
		return nil, fmt.Errorf("updater: read state: %w", err)
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("updater: parse state: %w", err)
	}
	if s.Phase == "" {
		s.Phase = PhaseIdle
	}
	if s.MaxAccepted == nil {
		s.MaxAccepted = map[string]string{}
	}
	return &s, nil
}

// Save writes the state atomically (temp + fsync + rename) so a power
// failure yields either the old or the new complete file.
func (s *State) Save(dir string) error {
	s.UpdatedAt = time.Now().UTC()
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, StateFileName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, StateFileName+".tmp-*")
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
	if err := tmp.Chmod(0o644); err != nil {
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

// InFlight reports whether the state describes an update that has
// crossed the activation point (needs boot reconciliation), versus an
// early phase that is safe to abandon to idle.
func (s *State) InFlight() bool {
	switch s.Phase {
	case PhaseActivating, PhaseValidating, PhaseCommitting, PhaseRollback:
		return true
	}
	return false
}

// HasUncommittedCandidate reports whether the active slot is a
// candidate that has not been committed (the "power failure after
// activation" condition).
func (s *State) HasUncommittedCandidate() bool {
	return s.CandidateSlot != "" && s.CandidateSlot == s.ActiveSlot &&
		s.CandidateSlot != s.LastGoodSlot
}

// MarkError records an error and keeps the phase (caller decides the
// terminal phase).
func (s *State) MarkError(err error) {
	if err != nil {
		s.LastError = err.Error()
	}
}

// UpdateStatusForHealth maps the state to the frame health
// update_status value (spec §14: "The admin dashboard must show which
// frames are updating, which succeeded, and which need attention").
func (s *State) UpdateStatusForHealth() string {
	switch s.Phase {
	case PhaseIdle:
		if s.LastError != "" && s.LastUpdateAt != (time.Time{}) {
			return "update_failed"
		}
		return "none"
	case PhaseChecking, PhaseDownloading, PhaseVerifying, PhaseActivating, PhaseValidating, PhaseCommitting:
		return "updating"
	case PhaseRollback:
		return "rollback"
	case PhaseFailed:
		return "update_failed"
	}
	return "unknown"
}

// ErrStateCorrupt is returned when the state file is present but
// unusable; the caller must treat the layout conservatively.
var ErrStateCorrupt = errors.New("updater: state file corrupt")
