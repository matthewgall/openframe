package updater

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// Coordinator runs the update process of spec §14 against a Layout,
// a State, and a Service. Every step that mutates the world is
// preceded by a state save, so Reconcile can resume or roll back
// after any interruption.
type Coordinator struct {
	Layout *Layout
	// StateDir holds state.json and incoming artifacts. The frame
	// reads state.json here (read-only, unprivileged).
	StateDir string
	// TrustPath is the pinned trust store (device-pinned, never
	// fetched). Rotations are appended here after verification.
	TrustPath string
	// Service controls the frame application.
	Service Service
	// FetchRelease retrieves the latest signed metadata for a
	// channel from the release server.
	FetchRelease func(ctx context.Context, channel string) (Release, error)
	// DownloadArtifact retrieves the release artifact bytes.
	DownloadArtifact func(ctx context.Context, rel Release) (io.ReadCloser, error)
	// FetchReleaseEx, when set, provides the signed release plus the
	// server's unsigned rollout override (applied to eligibility only).
	FetchReleaseEx func(ctx context.Context, channel string) (FetchedRelease, error)
	// Now is the clock (injectable for window/time-bound tests).
	Now func() time.Time
	// SchemaVersion is the frame DB schema version in place.
	SchemaVersion int
	// MaxProtocol is the newest protocol this build speaks.
	MaxProtocol int
	// DeviceID is used for staged-rollout membership.
	DeviceID string
	// OS/Arch is the target platform (empty: the platform this binary
	// runs on; set explicitly in tests and for cross-arch checks).
	OS   string
	Arch string
	// ValidateTimeout bounds post-activation readiness (default
	// 5m; production frame cold-boot + first sync headroom).
	ValidateTimeout time.Duration
	// SpaceCheck is the disk-space predicate (default CheckDiskSpace;
	// injectable for tests).
	SpaceCheck func(dir string, required int64) error
	// Log receives structured progress (default: discard).
	Log *slog.Logger
}

// Outcome of a coordination run.
type Result struct {
	// Action: "updated" | "already-current" | "nothing" | "rejected"
	// | "failed" | "rollback" | "reconciled" | "skipped-window" |
	// "manual-check-only".
	Action     string
	Release    *Release
	NewSlot    string
	NewVersion string
	OldVersion string
	Err        error
}

// Configure defaults (clock, log, timeout) on the coordinator.
func (c *Coordinator) Configure() *Coordinator {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Log == nil {
		c.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if c.ValidateTimeout <= 0 {
		c.ValidateTimeout = 5 * time.Minute
	}
	if c.SpaceCheck == nil {
		c.SpaceCheck = CheckDiskSpace
	}
	if c.FetchReleaseEx != nil && c.FetchRelease == nil {
		ex := c.FetchReleaseEx
		c.FetchRelease = func(ctx context.Context, channel string) (Release, error) {
			fr, err := ex(ctx, channel)
			if err != nil {
				return Release{}, err
			}
			return fr.Release, nil
		}
	}
	if c.FetchReleaseEx == nil && c.FetchRelease != nil {
		f := c.FetchRelease
		c.FetchReleaseEx = func(ctx context.Context, channel string) (FetchedRelease, error) {
			rel, err := f(ctx, channel)
			if err != nil {
				return FetchedRelease{}, err
			}
			return FetchedRelease{Release: rel}, nil
		}
	}
	return c
}

func (c *Coordinator) logf(phase string, format string, args ...any) {
	c.Log.Info("updater", "phase", phase, "msg", fmt.Sprintf(format, args...))
}

// loadTrust reads the pinned trust store.
func (c *Coordinator) loadTrust() (*TrustedKeys, error) {
	return LoadTrustedKeys(c.TrustPath)
}

// loadState reads (or initialises) the state.
func (c *Coordinator) loadState() (*State, error) {
	return LoadState(c.StateDir, c.Layout)
}

// Run performs one full coordination cycle: reconcile any
// interrupted work first, then (if allowed by policy and window)
// check for, verify, install and validate a release — rolling back
// automatically on failure.
//
// A failed run never leaves the frame without a working binary: the
// worst accepted end states are "old binary active" or "rollback
// complete". The frame's sync/slideshow is a separate process and is
// unaffected by updater failures (spec §14: "An unavailable update
// server must not affect slideshow or gallery functionality").
func (c *Coordinator) Run(ctx context.Context, channel, policy string, window *Window, force bool) (*Result, error) {
	c.Configure()
	trust, err := c.loadTrust()
	if err != nil {
		return nil, fmt.Errorf("updater: trust store: %w", err)
	}
	state, err := c.loadState()
	if err != nil {
		return nil, fmt.Errorf("updater: state: %w", err)
	}

	// 0. Reconcile anything a previous run (or power loss) left in
	// flight. This is also the "frame cannot start after update"
	// detector at boot: the boot timer runs the updater before the
	// frame, and an unhealthy uncommitted candidate is rolled back.
	if state.InFlight() || state.Phase == PhaseFailed || state.CandidateSlot != "" {
		rr, err := c.reconcile(ctx, trust, state)
		if err != nil {
			// Unrecoverable state/layout problem. The frame binary is
			// whatever `current` points at — unchanged by us.
			return &Result{Action: ResultFailed, Err: err, OldVersion: state.ActiveVersion}, nil
		}
		if rr != nil {
			if rr.Action == ResultUpdated {
				// This cycle finished an interrupted update; done.
				return rr, nil
			}
			if rr.Err != nil || rr.Action != ResultReconciled {
				// A rollback was reported or failed; no further action
				// this cycle (state records what happened).
				return rr, nil
			}
		}
		if reloaded, err := c.loadState(); err == nil {
			state = reloaded
		}
		if state.Phase == PhaseFailed {
			// Terminal until operator intervention (reset the update
			// state, fix the cause, then the next run proceeds).
			return &Result{Action: ResultReconciled, Err: fmt.Errorf("update failed; operator intervention required: %s", orDash(state.LastError))}, nil
		}
		// Clean reconciliation: the device may still have a pending
		// update, so keep going.
		c.logf(state.Phase, "reconciled after interruption")
	}

	// A pending update request (frame UI, CLI or admin) is honoured
	// as a forced run: the maintenance window is skipped, but the
	// policy, verification and rollback machinery still apply. The
	// marker is consumed once, whether or not an update is actually
	// installed (there may be nothing to install).
	if req := PendingRequest(c.StateDir, c.Now()); req != nil {
		c.logf(PhaseIdle, "honouring update request (%s by %s)", req.RequestedAt.UTC().Format(time.RFC3339), req.By)
		ClearRequest(c.StateDir)
		force = true
	}

	// Manual policy: report what would happen, install nothing.
	if policy == PolicyManual && !force {
		res, err := c.check(ctx, trust, state, channel)
		if err != nil {
			return res, err
		}
		res.Action = ResultManualCheckOnly
		return res, nil
	}

	// Download-only policy: verify and stage the artifact into the
	// inactive slot, but never activate it (spec §14: policy
	// automatic | download_only | manual). Activation happens when an
	// operator switches the frame to automatic or forces an update.
	if policy == PolicyDownloadOnly && !force {
		res, err := c.stage(ctx, trust, state, channel)
		if err != nil {
			return res, err
		}
		return res, nil
	}

	// Maintenance window gate (skippable with force, e.g. `--force`).
	if !force && window != nil && !window.Contains(c.Now()) {
		if err := c.setPhase(state, PhaseIdle); err != nil {
			return nil, err
		}
		return &Result{Action: ResultSkippedWindow, OldVersion: state.ActiveVersion}, nil
	}

	return c.attempt(ctx, trust, state, channel, force)
}

// check fetches and verifies the latest release without installing.
// Used by manual policy and `openframe update check`.
func (c *Coordinator) check(ctx context.Context, trust *TrustedKeys, state *State, channel string) (*Result, error) {
	fr, err := c.FetchReleaseEx(ctx, channel)
	if err != nil {
		return &Result{Action: ResultNothing, Err: err, OldVersion: state.ActiveVersion}, nil
	}
	res, err := c.verify(ctx, trust, state, channel, fr.Release)
	if err != nil {
		return &Result{Action: ResultRejected, Release: &fr.Release, Err: err, OldVersion: state.ActiveVersion}, nil
	}
	if !rolloutEligible(res.Release, c.DeviceID) {
		return &Result{Action: ResultNothing, Release: &res.Release, OldVersion: state.ActiveVersion}, nil
	}
	if res.Release.Version == state.ActiveVersion {
		return &Result{Action: ResultAlreadyCurrent, Release: &res.Release, OldVersion: state.ActiveVersion}, nil
	}
	return &Result{Action: ResultAvailable, Release: &res.Release, OldVersion: state.ActiveVersion}, nil
}

// stage downloads and installs a verified release into the inactive
// slot without activating it (download_only policy).
func (c *Coordinator) stage(ctx context.Context, trust *TrustedKeys, state *State, channel string) (*Result, error) {
	old := state.ActiveVersion
	res, err := c.check(ctx, trust, state, channel)
	if err != nil {
		return res, err
	}
	if res.Action != ResultAvailable {
		return res, nil
	}
	rel := res.Release
	if err := c.SpaceCheck(c.Layout.SlotsDir, ArtifactSpaceRequirement(rel.ArtifactSize)); err != nil {
		return &Result{Action: ResultRejected, Release: rel, Err: err, OldVersion: old}, nil
	}
	candidate, err := c.Layout.InactiveSlot()
	if err != nil {
		return nil, err
	}
	if err := c.setPhaseWith(state, PhaseDownloading, *rel, channel); err != nil {
		return nil, err
	}
	body, err := c.DownloadArtifact(ctx, *rel)
	if err != nil {
		_ = c.failUpdate(state, fmt.Errorf("download: %w", err))
		return &Result{Action: ResultFailed, Release: rel, Err: err, OldVersion: old}, nil
	}
	incoming := filepath.Join(c.StateDir, "incoming.bin")
	f, err := os.Create(incoming)
	if err != nil {
		body.Close()
		_ = c.failUpdate(state, err)
		return &Result{Action: ResultFailed, Release: rel, Err: err, OldVersion: old}, nil
	}
	_, copyErr := io.Copy(f, body)
	body.Close()
	if cerr := f.Close(); copyErr == nil {
		copyErr = cerr
	}
	if copyErr != nil {
		os.Remove(incoming)
		_ = c.failUpdate(state, copyErr)
		return &Result{Action: ResultFailed, Release: rel, Err: copyErr, OldVersion: old}, nil
	}
	if err := c.setPhase(state, PhaseVerifying); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(incoming)
	if err == nil {
		err = VerifyArtifact(data, *rel)
	}
	if err != nil {
		os.Remove(incoming)
		_ = c.failUpdate(state, err)
		return &Result{Action: ResultFailed, Release: rel, Err: err, OldVersion: old}, nil
	}
	if err := c.Layout.InstallBinary(candidate, bytesReader(data), rel.ArtifactSize); err != nil {
		_ = c.failUpdate(state, err)
		return &Result{Action: ResultFailed, Release: rel, Err: err, OldVersion: old}, nil
	}
	os.Remove(incoming)
	state.CandidateSlot = candidate
	if err := state.Save(c.StateDir); err != nil {
		return nil, err
	}
	if err := c.setPhase(state, PhaseIdle); err != nil {
		return nil, err
	}
	c.logf(PhaseIdle, "staged %s in slot %s (not activated)", rel.Version, candidate)
	return &Result{Action: ResultStaged, Release: rel, NewSlot: candidate, NewVersion: rel.Version, OldVersion: old}, nil
}

// verify runs the verifier and applies a verified key rotation.
func (c *Coordinator) verify(ctx context.Context, trust *TrustedKeys, state *State, channel string, rel Release) (*VerifyResult, error) {
	v := NewVerifier(trust)
	opts := VerifyOptions{
		Now:           c.Now().UTC(),
		OS:            c.OS,
		Arch:          c.Arch,
		MaxProtocol:   c.MaxProtocol,
		SchemaVersion: c.SchemaVersion,
		MaxAccepted:   state.MaxAccepted,
	}
	res, err := v.Verify(rel, opts)
	if err != nil {
		return nil, err
	}
	if res.NewKeys != nil {
		if err := c.applyRotation(trust, res.NewKeys); err != nil {
			return nil, err
		}
	}
	return res, nil
}

// applyRotation persists verified new keys into the trust file
// (device-pinned rotation, trust-model.md §Rotation).
func (c *Coordinator) applyRotation(current, add *TrustedKeys) error {
	merged := &TrustedKeys{Keys: map[string]TrustedKey{}}
	for k, v := range current.Keys {
		merged.Keys[k] = v
	}
	merged.AddTrusted(add)
	b, err := merged.Encode()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.TrustPath), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(c.TrustPath), "trusted-keys.tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, c.TrustPath); err != nil {
		return err
	}
	return fsyncDir(filepath.Dir(c.TrustPath))
}

// attempt runs the install pipeline: fetch → verify → eligibility →
// space → download → verify artifact → install slot → activate →
// validate → commit | rollback.
func (c *Coordinator) attempt(ctx context.Context, trust *TrustedKeys, state *State, channel string, force bool) (*Result, error) {
	old := state.ActiveVersion
	c.logf(PhaseChecking, "checking %s channel (active %s)", channel, orDash(old))

	fr, err := c.FetchReleaseEx(ctx, channel)
	if err != nil {
		// Release server unavailable: not an error for the frame —
		// report and keep the old binary.
		c.logf(PhaseChecking, "release server unavailable: %v", err)
		return &Result{Action: ResultNothing, Err: err, OldVersion: old}, nil
	}
	rel := fr.Release
	serving := fr.Serving

	// Already current: the offered release is what we run. Checked
	// before verification so "same version" can never be confused with
	// a downgrade (the anti-rollback floor equals the active version
	// after a commit).
	if rel.Version == old && !rel.IsRollback {
		return &Result{Action: ResultAlreadyCurrent, Release: &rel, OldVersion: old}, nil
	}

	// Failure cooldown: a release that just failed validation is not
	// retried on this tick (each attempt costs the running frame a
	// stop/swap/start/validate cycle). Force overrides.
	if !force && state.LastFailed.Version != "" &&
		state.LastFailed.Version == rel.Version &&
		state.LastFailed.Channel == rel.Channel &&
		c.Now().Sub(state.LastFailed.At) < FailureCooldown {
		c.logf(PhaseChecking, "release %s failed at %s; in cooldown, skipping", rel.Version, state.LastFailed.At.UTC().Format(time.RFC3339))
		return &Result{Action: ResultNothing, Release: &rel,
			Err:        fmt.Errorf("release %s previously failed; in %s cooldown", rel.Version, FailureCooldown),
			OldVersion: old}, nil
	}

	res, err := c.verify(ctx, trust, state, channel, rel)
	if err != nil {
		c.logf(PhaseChecking, "release %s rejected: %v", rel.Version, err)
		_ = c.setPhase(state, PhaseIdle)
		state.MarkError(err)
		_ = state.Save(c.StateDir)
		return &Result{Action: ResultRejected, Release: &rel, Err: err, OldVersion: old}, nil
	}
	rel = res.Release
	percent, devices := rel.RolloutPercent, rel.RolloutDevices
	if serving != nil {
		percent, devices = serving.RolloutPercent, serving.RolloutDevices
	}
	if !rolloutEligibleWith(rel, percent, devices, c.DeviceID) {
		c.logf(PhaseChecking, "release %s not in rollout for this device", rel.Version)
		return &Result{Action: ResultNothing, Release: &rel, OldVersion: old}, nil
	}

	// Disk space, before spending bandwidth.
	if err := c.SpaceCheck(c.Layout.SlotsDir, ArtifactSpaceRequirement(rel.ArtifactSize)); err != nil {
		c.logf(PhaseChecking, "space check failed: %v", err)
		_ = c.setPhase(state, PhaseIdle)
		state.MarkError(err)
		_ = state.Save(c.StateDir)
		return &Result{Action: ResultRejected, Release: &rel, Err: err, OldVersion: old}, nil
	}

	// Download to the state dir, then verify against the signed
	// digest (spec §14 step 6: "Verify artifact integrity").
	if err := c.setPhaseWith(state, PhaseDownloading, rel, channel); err != nil {
		return nil, err
	}
	body, err := c.DownloadArtifact(ctx, rel)
	if err != nil {
		c.logf(PhaseDownloading, "download failed: %v", err)
		_ = c.failUpdate(state, fmt.Errorf("download: %w", err))
		return &Result{Action: ResultFailed, Release: &rel, Err: err, OldVersion: old}, nil
	}
	incoming := filepath.Join(c.StateDir, "incoming.bin")
	f, err := os.Create(incoming)
	if err != nil {
		body.Close()
		_ = c.failUpdate(state, err)
		return &Result{Action: ResultFailed, Release: &rel, Err: err, OldVersion: old}, nil
	}
	_, copyErr := io.Copy(f, body)
	body.Close()
	if cerr := f.Close(); copyErr == nil {
		copyErr = cerr
	}
	if copyErr != nil {
		os.Remove(incoming)
		c.logf(PhaseDownloading, "interrupted/corrupt download: %v", copyErr)
		_ = c.failUpdate(state, fmt.Errorf("download: %w", copyErr))
		return &Result{Action: ResultFailed, Release: &rel, Err: copyErr, OldVersion: old}, nil
	}

	if err := c.setPhase(state, PhaseVerifying); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(incoming)
	if err == nil {
		err = VerifyArtifact(data, rel)
	}
	if err != nil {
		os.Remove(incoming)
		c.logf(PhaseVerifying, "artifact integrity failed: %v", err)
		_ = c.failUpdate(state, err)
		return &Result{Action: ResultFailed, Release: &rel, Err: err, OldVersion: old}, nil
	}

	// Install into the inactive slot (never the running one).
	candidate, err := c.Layout.InactiveSlot()
	if err != nil {
		return nil, err
	}
	if err := c.setPhaseWithCandidate(state, PhaseVerifying, rel, channel, candidate); err != nil {
		return nil, err
	}
	if err := c.Layout.InstallBinary(candidate, bytesReader(data), rel.ArtifactSize); err != nil {
		c.logf(PhaseVerifying, "slot install failed: %v", err)
		_ = c.failUpdate(state, err)
		return &Result{Action: ResultFailed, Release: &rel, Err: err, OldVersion: old}, nil
	}
	os.Remove(incoming)

	// Stop, swap, start, validate.
	if err := c.setPhase(state, PhaseActivating); err != nil {
		return nil, err
	}
	if err := c.Service.Stop(ctx); err != nil {
		c.logf(PhaseActivating, "stop failed: %v", err)
		_ = c.failUpdate(state, fmt.Errorf("stop: %w", err))
		return &Result{Action: ResultFailed, Release: &rel, Err: err, OldVersion: old}, nil
	}
	if err := c.Layout.Activate(candidate); err != nil {
		c.logf(PhaseActivating, "activation failed: %v; restoring service", err)
		_ = c.Service.Start(ctx)
		_ = c.failUpdate(state, fmt.Errorf("activate: %w", err))
		return &Result{Action: ResultFailed, Release: &rel, Err: err, OldVersion: old}, nil
	}
	if err := c.setPhase(state, PhaseValidating); err != nil {
		return nil, err
	}
	if err := c.Service.Start(ctx); err != nil {
		c.logf(PhaseValidating, "start failed: %v; rolling back", err)
		rbErr := c.rollback(ctx, state, &rel, err)
		return &Result{Action: ResultRollback, Release: &rel, Err: errors.Join(err, rbErr), OldVersion: old}, nil
	}
	if err := c.Service.WaitReady(ctx, c.ValidateTimeout); err != nil {
		c.logf(PhaseValidating, "candidate not healthy: %v; rolling back", err)
		rbErr := c.rollback(ctx, state, &rel, err)
		return &Result{Action: ResultRollback, Release: &rel, Err: errors.Join(err, rbErr), OldVersion: old}, nil
	}

	// Commit: the candidate is now last-good (raises the
	// anti-rollback floor via commit()).
	if err := c.setPhase(state, PhaseCommitting); err != nil {
		return nil, err
	}
	if err := c.commit(state, &rel); err != nil {
		return nil, err
	}
	c.logf(PhaseIdle, "updated %s -> %s (slot %s)", orDash(old), rel.Version, candidate)
	return &Result{Action: ResultUpdated, Release: &rel, NewSlot: candidate, NewVersion: rel.Version, OldVersion: old}, nil
}

// rollback restores the last-good slot and validates it. The rollback
// path uses only local operations (no network), satisfying "Offline
// rollback: rollback must not depend on the update server".
func (c *Coordinator) rollback(ctx context.Context, state *State, rel *Release, cause error) error {
	if err := c.setPhase(state, PhaseRollback); err != nil {
		return err
	}
	state.MarkError(cause)
	if rel.Version != "" {
		// Record the failed target: the same release is skipped for
		// FailureCooldown (unless forced), so a bad build cannot flap
		// the frame on every timer tick.
		state.LastFailed = FailedTarget{Version: rel.Version, Channel: rel.Channel, At: c.Now().UTC()}
	}
	target := state.LastGoodSlot
	c.logf(PhaseRollback, "rolling back %s -> %s (slot %s)", rel.Version, orDash(state.LastGoodVersion), target)
	if err := c.Service.Stop(ctx); err != nil {
		c.logf(PhaseRollback, "stop during rollback: %v", err)
	}
	if err := c.Layout.Activate(target); err != nil {
		// The layout itself is broken; record and let reconciliation
		// (with boot attempts) handle repeated failures.
		if err2 := c.Service.Start(ctx); err2 != nil {
			c.logf(PhaseRollback, "could not restart service after failed rollback: %v", err2)
		}
		state.BootAttempts++
		if state.BootAttempts >= MaxBootAttempts {
			state.Phase = PhaseFailed
		}
		state.MarkError(err)
		_ = state.Save(c.StateDir)
		return fmt.Errorf("rollback activation: %w", err)
	}
	if err := c.Service.Start(ctx); err != nil {
		state.BootAttempts++
		if state.BootAttempts >= MaxBootAttempts {
			state.Phase = PhaseFailed
		}
		state.MarkError(err)
		_ = state.Save(c.StateDir)
		return fmt.Errorf("rollback start: %w", err)
	}
	if err := c.Service.WaitReady(ctx, c.ValidateTimeout); err != nil {
		state.BootAttempts++
		if state.BootAttempts >= MaxBootAttempts {
			state.Phase = PhaseFailed
		}
		state.MarkError(err)
		_ = state.Save(c.StateDir)
		return fmt.Errorf("rollback not healthy: %w", err)
	}
	// Back on the last-good binary. The failure stays recorded
	// (LastError + LastUpdateAt) so the frame's health reports
	// update_failed and the admin dashboard can show the rollback.
	state.ActiveSlot = target
	state.ActiveVersion = state.LastGoodVersion
	state.CandidateSlot = ""
	state.TargetVersion = ""
	state.TargetSHA256 = ""
	state.Channel = ""
	state.LastUpdateAt = c.Now().UTC()
	if err := state.Save(c.StateDir); err != nil {
		return err
	}
	if err := c.setPhase(state, PhaseIdle); err != nil {
		return err
	}
	c.logf(PhaseIdle, "rollback complete; on %s (slot %s)", orDash(state.ActiveVersion), target)
	return nil
}

// reconcile resumes or unwinds an interrupted run. Called at the top
// of Run (which happens at boot via the boot timer and on every
// scheduled tick). It is candidate-centric: the presence of an
// uncommitted candidate (state.CandidateSlot set) is what makes a run
// resumable, regardless of the nominal phase. A candidate survives a
// commit or rollback only by being cleared, so CandidateSlot!=""
// always means "work in progress that must be finished or unwound".
func (c *Coordinator) reconcile(ctx context.Context, trust *TrustedKeys, state *State) (*Result, error) {
	rel := Release{Version: state.TargetVersion, Channel: state.Channel}

	// 1. Interrupted rollback: finish it (offline, idempotent).
	if state.Phase == PhaseRollback {
		if err := c.rollback(ctx, state, &rel, fmt.Errorf("completing interrupted rollback")); err != nil {
			return &Result{Action: ResultRollback, Err: err}, nil
		}
		return &Result{Action: ResultRollback}, nil
	}

	// 2. Terminal failure: leave it for the operator. A *new* release
	// is still tried by attempt() below in the same Run.
	if state.Phase == PhaseFailed {
		return &Result{Action: ResultReconciled}, nil
	}

	// 3. A candidate exists. Verify the artifact on disk before doing
	// anything with it — we only ever act on bytes we can prove are the
	// signed artifact.
	if state.CandidateSlot != "" {
		if state.CandidateSlot == state.LastGoodSlot {
			// Already identical to last-good; clear and move on.
			if err := c.clearCandidate(state); err != nil {
				return nil, err
			}
			return &Result{Action: ResultReconciled}, nil
		}
		intact := false
		if state.TargetSHA256 != "" {
			if sha, err := SHA256File(c.Layout.BinaryPath(state.CandidateSlot)); err == nil && sha == state.TargetSHA256 {
				intact = true
			}
		}
		if !intact {
			// Candidate missing or corrupted: abandon safely to idle.
			c.logf(state.Phase, "candidate %s not verifiable; abandoning", state.CandidateSlot)
			os.Remove(filepath.Join(c.StateDir, "incoming.bin"))
			if err := c.clearCandidate(state); err != nil {
				return nil, err
			}
			return &Result{Action: ResultReconciled}, nil
		}
		// Post- or pre-swap: which slot is active decides.
		active, err := c.Layout.ActiveSlot()
		if err != nil {
			return nil, fmt.Errorf("updater: reconcile: %w", err)
		}
		if active == state.CandidateSlot {
			c.logf(state.Phase, "candidate %s already active; validating", state.CandidateSlot)
			return c.validateOrRollback(ctx, state, &rel)
		}
		c.logf(state.Phase, "resuming verified candidate %s", state.CandidateSlot)
		return c.finishFromActivation(ctx, state, &rel)
	}

	// 4. Early phase, no candidate (partial download / pre-install
	// crash): nothing was committed; abandon to idle.
	switch state.Phase {
	case PhaseChecking, PhaseDownloading, PhaseVerifying, PhaseActivating, PhaseValidating, PhaseCommitting:
		os.Remove(filepath.Join(c.StateDir, "incoming.bin"))
		if err := c.clearCandidate(state); err != nil {
			return nil, err
		}
		return &Result{Action: ResultReconciled}, nil
	}
	return nil, nil
}

// clearCandidate resets candidate fields and returns to idle.
func (c *Coordinator) clearCandidate(state *State) error {
	state.CandidateSlot, state.TargetVersion, state.TargetSHA256, state.Channel = "", "", "", ""
	if err := state.Save(c.StateDir); err != nil {
		return err
	}
	return c.setPhase(state, PhaseIdle)
}

// finishFromActivation continues a verified-but-not-yet-activated
// candidate: stop, swap, start, validate, commit | rollback. The stop
// is mandatory even when the service is up: after the swap the new
// binary must be the one that runs, not the pre-swap process still in
// memory.
func (c *Coordinator) finishFromActivation(ctx context.Context, state *State, rel *Release) (*Result, error) {
	if err := c.Service.Stop(ctx); err != nil {
		c.logf(PhaseActivating, "stop before resume: %v", err)
	}
	if err := c.Layout.Activate(state.CandidateSlot); err != nil {
		_ = c.failUpdate(state, fmt.Errorf("reconcile activate: %w", err))
		_ = c.Service.Start(ctx) // restore whatever was running
		return &Result{Action: ResultFailed, Release: rel, Err: err}, nil
	}
	if err := c.setPhase(state, PhaseValidating); err != nil {
		return nil, err
	}
	if err := c.Service.Start(ctx); err != nil {
		rbErr := c.rollback(ctx, state, rel, err)
		return &Result{Action: ResultRollback, Release: rel, Err: errors.Join(err, rbErr)}, nil
	}
	if err := c.Service.WaitReady(ctx, c.ValidateTimeout); err != nil {
		rbErr := c.rollback(ctx, state, rel, err)
		return &Result{Action: ResultRollback, Release: rel, Err: errors.Join(err, rbErr)}, nil
	}
	if err := c.commit(state, rel); err != nil {
		return nil, err
	}
	return &Result{Action: ResultUpdated, Release: rel, NewSlot: state.LastGoodSlot, NewVersion: rel.Version}, nil
}

// validateOrRollback is the post-activation decision after a power
// loss: healthy candidate commits, anything else rolls back.
func (c *Coordinator) validateOrRollback(ctx context.Context, state *State, rel *Release) (*Result, error) {
	ok, _ := c.Service.Healthy(ctx)
	if ok {
		if err := c.setPhase(state, PhaseCommitting); err != nil {
			return nil, err
		}
		if err := c.commit(state, rel); err != nil {
			return nil, err
		}
		return &Result{Action: ResultReconciled}, nil
	}
	// Not healthy: roll back (offline). The rollback itself is the
	// outcome of this run — report it and let the failure cooldown
	// keep the bad release away until the next eligible cycle.
	cause := fmt.Errorf("candidate %s not healthy after restart", rel.Version)
	_ = c.Service.Stop(ctx)
	if err := c.rollback(ctx, state, rel, cause); err != nil {
		return &Result{Action: ResultRollback, Err: err}, nil
	}
	return &Result{Action: ResultRollback, OldVersion: state.ActiveVersion}, nil
}

// commit records the candidate as last-good and raises the
// anti-rollback floor.
func (c *Coordinator) commit(state *State, rel *Release) error {
	state.ActiveSlot = state.CandidateSlot
	state.ActiveVersion = rel.Version
	state.LastGoodSlot = state.CandidateSlot
	state.LastGoodVersion = rel.Version
	state.CandidateSlot, state.TargetVersion, state.TargetSHA256, state.Channel = "", "", "", ""
	state.BootAttempts = 0
	state.LastError = ""
	state.LastFailed = FailedTarget{}
	state.LastUpdateAt = c.Now().UTC()
	if ch := rel.Channel; ch != "" {
		if cur, ok := state.MaxAccepted[ch]; !ok || isNewer(rel.Version, cur) {
			state.MaxAccepted[ch] = rel.Version
		}
	}
	if err := state.Save(c.StateDir); err != nil {
		return err
	}
	return c.setPhase(state, PhaseIdle)
}

// failUpdate marks a pre-activation failure: the old binary is still
// active and healthy, so we return to idle with the error recorded
// (surfaced as update_failed in the frame's health payload).
func (c *Coordinator) failUpdate(state *State, err error) error {
	state.CandidateSlot, state.TargetVersion, state.TargetSHA256, state.Channel = "", "", "", ""
	state.MarkError(err)
	if err := state.Save(c.StateDir); err != nil {
		return err
	}
	return c.setPhase(state, PhaseIdle)
}

// setPhase transitions the phase, saving state (durable before the
// next world mutation).
func (c *Coordinator) setPhase(state *State, phase string) error {
	state.Phase = phase
	return state.Save(c.StateDir)
}

func (c *Coordinator) setPhaseWith(state *State, phase string, rel Release, channel string) error {
	state.Channel = channel
	state.TargetVersion = rel.Version
	state.TargetSHA256 = rel.ArtifactSHA256
	return c.setPhase(state, phase)
}

func (c *Coordinator) setPhaseWithCandidate(state *State, phase string, rel Release, channel, candidate string) error {
	state.CandidateSlot = candidate
	return c.setPhaseWith(state, phase, rel, channel)
}

func isNewer(a, b string) bool {
	va, err := ParseVersion(a)
	if err != nil {
		return false
	}
	vb, err := ParseVersion(b)
	if err != nil {
		return true
	}
	return vb.Less(va)
}

func orDash(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func bytesReader(b []byte) *sliceReader { return &sliceReader{b: b} }

type sliceReader struct {
	b   []byte
	off int
}

func (r *sliceReader) Read(p []byte) (int, error) {
	if r.off >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.off:])
	r.off += n
	return n, nil
}

// --- Result actions ---

const (
	ResultUpdated         = "updated"
	ResultAlreadyCurrent  = "already-current"
	ResultAvailable       = "available"
	ResultNothing         = "nothing"
	ResultRejected        = "rejected"
	ResultFailed          = "failed"
	ResultRollback        = "rollback"
	ResultReconciled      = "reconciled"
	ResultSkippedWindow   = "skipped-window"
	ResultManualCheckOnly = "manual-check-only"
	ResultStaged          = "staged"
)

// --- Update policy + maintenance window (spec §14 Update scheduling) ---

// Update policies.
const (
	PolicyAutomatic    = "automatic"     // check + install in window
	PolicyDownloadOnly = "download_only" // check + verify + stage, no activation
	PolicyManual       = "manual"        // report only; operator triggers with force
)

// Window is a daily maintenance window (HH:MM–HH:MM in a timezone).
type Window struct {
	Start string // "02:00"
	End   string // "05:00"
	TZ    string // IANA zone; "UTC" if empty
}

// Contains reports whether t falls inside the window. Windows that
// cross midnight (e.g. 23:00–01:00) are supported.
func (w *Window) Contains(t time.Time) bool {
	if w == nil {
		return true
	}
	loc, err := time.LoadLocation(w.TZ)
	if err != nil {
		loc = time.UTC
	}
	t = t.In(loc)
	start, ok1 := parseHM(w.Start)
	end, ok2 := parseHM(w.End)
	if !ok1 || !ok2 {
		return true // malformed window: fail open (operator sees config)
	}
	day := int(t.Hour()*60 + t.Minute())
	if start <= end {
		return day >= start && day < end
	}
	return day >= start || day < end
}

func parseHM(s string) (int, bool) {
	if len(s) != 5 || s[2] != ':' {
		return 0, false
	}
	h, ok1 := twoDigits(s[0:2])
	m, ok2 := twoDigits(s[3:5])
	if !ok1 || !ok2 || h > 23 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

func twoDigits(s string) (int, bool) {
	if len(s) != 2 {
		return 0, false
	}
	d1, d0 := s[0]-'0', s[1]-'0'
	if d1 > 9 || d0 > 9 {
		return 0, false
	}
	return int(d1)*10 + int(d0), true
}

// Ed25519 key helpers used by release tooling and the CLI.

// GenerateSigningKey returns a fresh Ed25519 key pair as base64 raw
// bytes (private: 64 bytes, public: 32 bytes), for the `openframe
// release-keygen` command.
func GenerateSigningKey() (privB64, pubB64 string, err error) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(priv), base64.StdEncoding.EncodeToString(pub), nil
}
