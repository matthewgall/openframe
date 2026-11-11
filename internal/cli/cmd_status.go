package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/openframe/openframe/internal/version"
)

type statusReport struct {
	Version string     `json:"version"`
	Frame   frameState `json:"frame"`
	Age     string     `json:"last_heartbeat_age,omitempty"`
}

func runStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	var cf commonFlags
	cf.register(fs)
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}

	cfg, _, _ := configTolerant(&cf)
	ctx, cancel := signalContext()
	defer cancel()

	st := readFrameState(ctx, cfg)
	rep := statusReport{Version: version.Version, Frame: st}
	if st.LastHeartbeat != nil {
		rep.Age = time.Since(*st.LastHeartbeat).Truncate(time.Second).String()
	}

	if cf.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			return ExitError
		}
		return ExitOK
	}

	fmt.Println("OpenFrame status")
	fmt.Println("================")
	fmt.Printf("  version:  %s\n", rep.Version)
	if !st.Available {
		fmt.Println("  frame:    (no local frame state found)")
		fmt.Println()
		fmt.Println("  Run `openframe frame` to start the frame application,")
		fmt.Println("  or `openframe doctor` for a full diagnosis.")
		return ExitOK
	}
	if st.DeviceID != "" {
		fmt.Printf("  device:   %s\n", st.DeviceID)
	}
	state := "stopped"
	if st.Running {
		state = "running"
	} else if st.LastHeartbeat != nil {
		state = "last ran " + rep.Age + " ago"
	}
	fmt.Printf("  state:    %s\n", state)
	if st.LastHeartbeat != nil {
		fmt.Printf("  last heartbeat: %s\n", st.LastHeartbeat.UTC().Format(time.RFC3339))
	}
	if st.ServerConfigured {
		fmt.Println("  server:   configured")
	} else {
		fmt.Println("  server:   (not configured)")
	}
	if st.LastSyncError != "" {
		fmt.Printf("  last sync error: %s\n", st.LastSyncError)
	}
	return ExitOK
}
