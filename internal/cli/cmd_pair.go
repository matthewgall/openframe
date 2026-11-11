package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/openframe/openframe/internal/version"
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

	// Pairing initialises local device identity. The networked pairing
	// protocol (challenge -> admin approval -> credentials) lands in
	// milestone 5; until then the frame is in the "pending" state with a
	// stable local identity.
	dbh, deviceID, err := initFrameState(ctx, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed to initialise frame state:", err)
		return ExitError
	}
	defer dbh.Close()

	state := "pending"
	var serverURL string
	if cfg.Frame.ServerURL != "" {
		serverURL = cfg.Frame.ServerURL
	}

	out := pairStatus{
		DeviceID:  deviceID,
		State:     state,
		ServerURL: serverURL,
		FrameVer:  version.Version,
		NextSteps: []string{
			"pairing protocol (challenge, admin approval, device credentials) arrives in milestone 5",
			"the frame identity above is stable and will be used by the pairing flow",
		},
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
