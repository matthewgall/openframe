package frame

import (
	"context"
	"time"

	"github.com/matthewgall/openframe/internal/frame/pairing"
)

// pairingComponent implements the frame half of the pairing protocol
// (M5, spec §12):
//
//  1. unpaired frame with a server configured → request a challenge,
//     publish the code on the display;
//  2. poll the challenge status every 30s;
//  3. on approval, claim the one-time credential, persist it, install
//     it in the sync engine and clear the pairing screen.
//
// The component exits (harmlessly) once the frame is paired. A frame
// with no server configured runs display-only forever (no pairing
// possible); a manually-provisioned frame (config token) is paired
// from the start.
func (f *Frame) pairingComponent(ctx context.Context) error {
	if f.engine.Paired() {
		<-ctx.Done()
		return nil
	}
	serverURL := f.effectiveConfig().Frame.ServerURL
	if serverURL == "" {
		// No server: nothing to pair with. Keep the manual hint on
		// screen (the UI shows it when no pairing code is set).
		<-ctx.Done()
		return nil
	}

	st := &pairingState{}
	// First tick immediately: the code should appear on screen as
	// soon as the frame has booted, not after a 30 s wait.
	if err := f.pairingTick(ctx, serverURL, st); err != nil {
		f.log.Debug("pairing tick failed", "error", err)
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if f.engine.Paired() {
				return nil
			}
			// Terminal: the server knows this device and no challenge
			// is left to resolve — only a manual credential can help.
			if st.terminal {
				return nil
			}
			if err := f.pairingTick(ctx, serverURL, st); err != nil {
				f.log.Debug("pairing tick failed", "error", err)
			}
		}
	}
}

// pairingState is the unpaired loop's state across ticks.
type pairingState struct {
	challengeID string
	registered  bool // the server already knows this device
	terminal    bool // nothing left to do short of a manual credential
}

// pairingTick is one round of the unpaired loop (extracted for tests).
func (f *Frame) pairingTick(ctx context.Context, serverURL string, st *pairingState) error {
	if st.challengeID == "" {
		name := f.deviceNameForPairing(ctx)
		ch, err := pairing.RequestChallenge(ctx, serverURL, f.identity, name)
		if err != nil {
			// The server knows this device (409). If it points at a
			// resolvable challenge (approved, one-time credential not
			// yet claimed) adopt it; otherwise self-pairing is
			// impossible — an administrator must issue a credential.
			if ar, ok := err.(*pairing.AlreadyRegistered); ok {
				st.registered = true
				if ar.ChallengeID != "" {
					st.challengeID = ar.ChallengeID
					f.log.Info("resuming previous pairing challenge", "challenge", ar.ChallengeID)
					return nil
				}
				f.log.Warn("device is already registered and no pairing challenge can be resumed; " +
					"an administrator must issue a credential for it")
			}
			st.terminal = st.registered
			return err
		}
		st.challengeID = ch.ID
		if f.ui != nil {
			f.ui.eng.SetPairing(ch.Code)
		}
		f.log.Info("pairing challenge requested",
			"challenge", ch.ID, "ttl_s", ch.TTLSeconds)
		return nil
	}

	stt, err := pairing.PollStatus(ctx, serverURL, st.challengeID)
	if err != nil {
		return err
	}
	switch stt.State {
	case pairing.StateApproved:
		if stt.Credential == "" {
			// Claimed by a previous instance (frame restarted after
			// approval, before claiming): the credential file should
			// exist; if not, the admin must re-pair.
			if cred, err := LoadCredential(f.credPath); err == nil && cred != nil {
				return f.completePairing(ctx, cred.Token)
			}
			f.log.Warn("challenge approved but credential already claimed elsewhere; " +
				"the credential file is missing — ask an administrator to re-pair")
			if f.ui != nil {
				f.ui.eng.SetPairing("")
			}
			st.terminal = st.registered
			return nil
		}
		if err := SaveCredential(f.credPath, &Credential{
			DeviceID:  f.identity,
			Token:     stt.Credential,
			PairedVia: "pairing",
		}); err != nil {
			return err
		}
		return f.completePairing(ctx, stt.Credential)
	case pairing.StateRejected, pairing.StateExpired:
		if f.ui != nil {
			f.ui.eng.SetPairing("")
		}
		f.log.Info("pairing challenge closed", "state", stt.State)
		st.challengeID = ""
		// A challenge the frame created itself can be re-created next
		// tick (fresh code on screen). A resumed challenge is terminal:
		// the device is registered, so creation will 409 forever.
		if st.registered {
			st.terminal = true
		}
	}
	return nil
}

// completePairing installs the credential and flips the frame to its
// paired state (UI + sync engine).
func (f *Frame) completePairing(ctx context.Context, token string) error {
	if err := f.engine.SetCredential(token); err != nil {
		return err
	}
	if f.ui != nil {
		f.ui.eng.SetPairing("")
	}
	f.log.Info("paired", "device_id", f.identity)
	// Immediate first sync (don't wait for the manifest interval).
	f.engine.Check(ctx)
	return nil
}

// deviceNameForPairing is the display name sent with the challenge:
// the operator-configured frame name, else a generic default.
func (f *Frame) deviceNameForPairing(ctx context.Context) string {
	if n, ok, _ := getState(ctx, f.db, StateKeyDeviceName); ok && n != "" {
		return n
	}
	if n := f.effectiveConfig().Frame.Name; n != "" {
		return n
	}
	return "Frame"
}
