package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/matthewgall/openframe/internal/frame"
	"github.com/matthewgall/openframe/internal/version"
)

// pair output for both human and JSON consumers.
type pairStatus struct {
	DeviceID  string   `json:"device_id"`
	State     string   `json:"state"` // unconfigured | pending
	ServerURL string   `json:"server_url,omitempty"`
	FrameVer  string   `json:"frame_version"`
	NextSteps []string `json:"next_steps"`
}

func runPair(args []string) int {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	var cf commonFlags
	cf.register(fs)
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}

	cfg, _, cfgErr := configTolerant(&cf)
	if cfgErr != nil {
		fmt.Fprintln(os.Stderr, "warning: using defaults:", cfgErr)
	}

	ctx, cancel := signalContext()
	defer cancel()

	// Pairing initialises local device identity (stable across restarts
	// and re-pairing). The M5 pairing protocol (challenge -> admin
	// approval -> one-time credential claim) runs inside `openframe
	// frame`; this command reports the current state and next steps.
	dbh, deviceID, err := initFrameState(ctx, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed to initialise frame state:", err)
		return ExitError
	}
	defer dbh.Close()

	state := "unconfigured"
	var serverURL string
	if cfg.Frame.ServerURL != "" {
		serverURL = cfg.Frame.ServerURL
		state = "unpaired"
		if cfg.Frame.DeviceToken != "" {
			state = "paired"
		}
	}
	if state == "unpaired" && deviceID != "" {
		// A credential claimed through the pairing flow is stored in a
		// 0600 file (not in the config); identity-bound.
		_, frameDir, _ := cfg.Paths()
		if cred, cerr := frame.LoadCredential(filepath.Join(frameDir, frame.CredentialFileName)); cerr == nil && cred != nil && cred.DeviceID == deviceID {
			state = "paired"
		}
	}

	next := []string{}
	switch state {
	case "unconfigured":
		next = append(next,
			"set frame.server_url (or OPENFRAME_FRAME_SERVER_URL) to the management server",
		)
	case "unpaired":
		next = append(next,
			"start `openframe frame` — the pairing screen shows an 8-character code",
			"in the admin web UI (Frames → pending requests), enter that code and approve",
			"the frame claims its credential automatically and starts syncing",
			"(manual alternative: register this device id in the admin UI, issue a",
			" credential there, and set frame.device_token / OPENFRAME_FRAME_DEVICE_TOKEN)",
		)
	}
	if state != "" {
		next = append(next, "the frame identity above is stable across restarts and re-pairing")
	}

	out := pairStatus{
		DeviceID:  deviceID,
		State:     state,
		ServerURL: serverURL,
		FrameVer:  version.Version,
		NextSteps: next,
	}

	if cf.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			return ExitError
		}
		return ExitOK
	}

	fmt.Println("OpenFrame frame pairing")
	fmt.Println("-----------------------")
	fmt.Printf("  device id:  %s\n", out.DeviceID)
	fmt.Printf("  state:      %s\n", out.State)
	if out.ServerURL != "" {
		fmt.Printf("  server:     %s\n", out.ServerURL)
	} else {
		fmt.Println("  server:     (not configured — set frame.server_url)")
	}
	fmt.Println()
	for _, s := range out.NextSteps {
		fmt.Println("  next:", s)
	}
	return ExitOK
}
