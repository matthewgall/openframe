package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"time"
)

// CheckOptions are the inputs for a read-only release check (used by
// `openframe update check`, which must work unprivileged: no slot
// writes, no service control, no trust-file modification — a verified
// rotation is reported, not persisted, when the trust file is not
// writable).
type CheckOptions struct {
	ServerURL string
	Channel   string
	TrustPath string
	// StateDir, when set and readable, contributes the anti-rollback
	// floor (MaxAccepted) from a previous run.
	StateDir string
	// OS/Arch target platform (empty: this build's platform).
	OS            string
	Arch          string
	MaxProtocol   int
	SchemaVersion int
	DeviceID      string
	// MaxAcceptedOverride, when non-nil, is used instead of (or in
	// addition to) the state file.
	MaxAcceptedOverride map[string]string
	// Now (zero: time.Now).
	Now time.Time
	// HTTP client timeout override (zero: default).
	HTTPTimeout time.Duration
}

// CheckResult is the outcome of a read-only check.
type CheckResult struct {
	// Action mirrors the Result actions that make sense read-only:
	// "available", "already-current", "nothing", "rejected".
	Action string `json:"action"`
	// ActiveVersion is what state says is active (may be "").
	ActiveVersion string `json:"active_version,omitempty"`
	// Release is the latest published release (always present when
	// fetched, even if rejected).
	Release *Release `json:"release,omitempty"`
	// Err explains a rejection or fetch failure.
	Err error `json:"-"`
	// ErrText renders Err for output.
	ErrText string `json:"error,omitempty"`
	// InRollout reports staged-rollout membership.
	InRollout bool `json:"in_rollout,omitempty"`
	// RotationPending names new keys a verified rotation would add.
	RotationPending []string `json:"rotation_pending,omitempty"`
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// Check fetches the latest release for the channel and runs the full
// verification chain without mutating anything (spec: the frame can
// ask "is there an update?" without any privileged component).
func Check(ctx context.Context, opts CheckOptions) (*CheckResult, error) {
	res := &CheckResult{Action: ResultNothing}
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	}
	if opts.OS == "" {
		opts.OS = runtime.GOOS
	}
	if opts.Arch == "" {
		opts.Arch = runtime.GOARCH
	}
	if opts.Channel == "" {
		opts.Channel = ChannelStable
	}

	trust, err := LoadTrustedKeys(opts.TrustPath)
	if err != nil {
		return nil, fmt.Errorf("check: trust store: %w", err)
	}

	src := NewHTTPReleaseSource(opts.ServerURL)
	if opts.HTTPTimeout > 0 {
		src.HTTP.Timeout = opts.HTTPTimeout
	}
	rel, err := src.FetchRelease(ctx, opts.Channel)
	if err != nil {
		res.Err = err
		res.ErrText = err.Error()
		return res, nil // fetch failure is a result, not a panic
	}
	res.Release = &rel

	// Anti-rollback floor from state (best effort).
	maxAccepted := map[string]string{}
	if opts.MaxAcceptedOverride != nil {
		for k, v := range opts.MaxAcceptedOverride {
			maxAccepted[k] = v
		}
	}
	if opts.StateDir != "" {
		if b, err := os.ReadFile(opts.StateDir + "/" + StateFileName); err == nil {
			var s State
			if jsonUnmarshal(b, &s) == nil && s.MaxAccepted != nil {
				for k, v := range s.MaxAccepted {
					if _, ok := maxAccepted[k]; !ok {
						maxAccepted[k] = v
					}
				}
				res.ActiveVersion = s.ActiveVersion
			}
		}
	}

	v := NewVerifier(trust)
	vres, err := v.Verify(rel, VerifyOptions{
		Now:           opts.Now,
		OS:            opts.OS,
		Arch:          opts.Arch,
		MaxProtocol:   opts.MaxProtocol,
		SchemaVersion: opts.SchemaVersion,
		MaxAccepted:   maxAccepted,
	})
	if err != nil {
		res.Action = ResultRejected
		res.Err = err
		res.ErrText = err.Error()
		return res, nil
	}
	if vres.NewKeys != nil {
		res.RotationPending = vres.NewKeys.KeyIDs()
	}
	res.InRollout = rolloutEligible(rel, opts.DeviceID)
	if !res.InRollout {
		res.Action = ResultNothing
		return res, nil
	}
	if rel.Version == res.ActiveVersion {
		res.Action = ResultAlreadyCurrent
		return res, nil
	}
	res.Action = ResultAvailable
	return res, nil
}
