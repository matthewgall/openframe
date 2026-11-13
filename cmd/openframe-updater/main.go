// openframe-updater is the privileged A/B update component
// (milestone 6, spec §14, ADR-0007).
//
// It is a separate executable from the frame on purpose: it runs as
// root (or with narrow capabilities) and is the only component that
// writes the binary slots, swaps the current symlink or restarts the
// frame service. The frame application runs unprivileged and never
// updates itself.
//
// Operation modes:
//
//	--once           run one coordination cycle and exit (systemd
//	                 oneshot/timer; exit 0 = ok/skipped, 1 = failure)
//	(no --once)      run forever, one cycle per --check-interval
//	--status         print the update state and exit
//
// The boot timer (see docs/hardware/deployment) runs --once at boot
// before the frame service, which is what makes "frame cannot start
// after update" recoverable: an unhealthy uncommitted candidate is
// detected and rolled back before the frame starts.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/matthewgall/openframe/internal/db"
	"github.com/matthewgall/openframe/internal/logging"
	"github.com/matthewgall/openframe/internal/updater"
	"github.com/matthewgall/openframe/internal/version"
	migs "github.com/matthewgall/openframe/migrations"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("openframe-updater", flag.ContinueOnError)
	var (
		root            = fs.String("root", "/opt/openframe", "install root (slots + current symlink)")
		stateDir        = fs.String("state-dir", "/var/lib/openframe/updates", "update state directory (state.json)")
		trustPath       = fs.String("trust", "", "trusted keys file (default: <root>/trust/trusted-keys.json)")
		server          = fs.String("server", "", "release server base URL (required to update)")
		channel         = fs.String("channel", "", "release channel: stable | beta | development (default: the channel persisted in the frame DB — set via the admin UI or `openframe update set-channel` — else stable)")
		policy          = fs.String("policy", "automatic", "update policy: automatic | download_only | manual")
		windowStart     = fs.String("window-start", "02:00", "maintenance window start (HH:MM)")
		windowEnd       = fs.String("window-end", "05:00", "maintenance window end (HH:MM)")
		windowTZ        = fs.String("window-tz", "UTC", "maintenance window IANA timezone")
		checkInterval   = fs.Duration("check-interval", 6*time.Hour, "interval between check cycles (daemon mode)")
		validateTimeout = fs.Duration("validate-timeout", 5*time.Minute, "post-activation readiness timeout")
		service         = fs.String("service", "systemd", "service manager: systemd | process")
		unit            = fs.String("unit", "openframe-frame", "systemd unit for the frame")
		healthURL       = fs.String("health-url", "", "frame health URL for readiness (systemd mode)")
		binaryPath      = fs.String("binary", "", "frame binary to run (process mode only)")
		deviceID        = fs.String("device-id", "", "frame device id (staged rollout membership)")
		frameDB         = fs.String("frame-db", "", "frame database path (read schema version; default: as-built)")
		maxProtocol     = fs.Int("max-protocol", 1, "newest manifest protocol this build speaks")
		once            = fs.Bool("once", false, "run one coordination cycle and exit")
		force           = fs.Bool("force", false, "ignore the maintenance window and failure cooldown")
		status          = fs.Bool("status", false, "print update state and exit")
		logLevel        = fs.String("log-level", "info", "log level")
		showVersion     = fs.Bool("version", false, "print version and exit")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Println(version.Current().String())
		return 0
	}
	channelFlagged := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "channel" {
			channelFlagged = true
		}
	})
	if channelFlagged && !updater.ValidChannel(*channel) {
		fmt.Fprintf(os.Stderr, "unknown channel %q\n", *channel)
		return 2
	}
	switch *policy {
	case updater.PolicyAutomatic, updater.PolicyDownloadOnly, updater.PolicyManual:
	default:
		fmt.Fprintf(os.Stderr, "unknown policy %q\n", *policy)
		return 2
	}

	log, err := logging.New(logging.Options{Level: *logLevel, Format: "json", Output: "stderr"})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	layout := updater.NewLayout(*root, "openframe")
	if *trustPath == "" {
		*trustPath = layout.TrustPath()
	}

	if *status {
		return printStatus(layout, *stateDir)
	}

	// Schema version: read the live frame DB when given (exact),
	// otherwise fall back to the as-built migration count.
	schemaVersion, err := resolveSchemaVersion(*frameDB)
	if err != nil {
		log.Warn("schema version fallback to as-built", "error", err)
		schemaVersion, _ = asBuiltSchemaVersion()
	}

	channelName := resolveChannel(channelFlagged, *channel, *frameDB, log)

	var svc updater.Service
	switch *service {
	case "systemd":
		svc = updater.NewSystemdService(*unit, *healthURL)
	case "process":
		if *binaryPath == "" {
			fmt.Fprintln(os.Stderr, "--binary is required with --service process")
			return 2
		}
		svc = updater.NewProcessService(*binaryPath)
	default:
		fmt.Fprintf(os.Stderr, "unknown service manager %q\n", *service)
		return 2
	}

	src := updater.NewHTTPReleaseSource(*server)
	coord := &updater.Coordinator{
		Layout:           layout,
		StateDir:         *stateDir,
		TrustPath:        *trustPath,
		Service:          svc,
		FetchReleaseEx:   src.FetchReleaseEx,
		DownloadArtifact: src.DownloadArtifact,
		OS:               runtime.GOOS,
		Arch:             runtime.GOARCH,
		SchemaVersion:    schemaVersion,
		MaxProtocol:      *maxProtocol,
		DeviceID:         *deviceID,
		ValidateTimeout:  *validateTimeout,
		Log:              log,
	}

	window := &updater.Window{Start: *windowStart, End: *windowEnd, TZ: *windowTZ}

	if *once {
		return runCycle(context.Background(), coord, channelName, *policy, window, *force, log)
	}

	// Daemon mode: immediate first cycle, then per interval, until
	// SIGTERM/SIGINT.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	runCycle(ctx, coord, channelName, *policy, window, *force, log)
	ticker := time.NewTicker(*checkInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("shutting down")
			return 0
		case <-ticker.C:
			if err := ctx.Err(); err != nil {
				return 0
			}
			runCycle(ctx, coord, channelName, *policy, window, *force, log)
		}
	}
}

// runCycle executes one coordination cycle and maps the outcome to an
// exit code for --once mode: 0 for anything the frame can keep doing
// (updated, current, skipped, rejected-with-reason, cooldown), 1 for
// coordination failures (corrupt state, broken layout).
func runCycle(ctx context.Context, coord *updater.Coordinator, channel, policy string, window *updater.Window, force bool, log *slog.Logger) int {
	res, err := coord.Run(ctx, channel, policy, window, force)
	if err != nil {
		log.Error("update cycle failed", "error", err)
		return 1
	}
	log.Info("update cycle", "action", res.Action,
		"old", res.OldVersion, "new", res.NewVersion, "release_err", errText(res.Err))
	if res.Action == "failed" {
		return 1
	}
	return 0
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func printStatus(layout *updater.Layout, stateDir string) int {
	state, err := updater.LoadState(stateDir, layout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "state: %v\n", err)
		return 1
	}
	active, err := layout.ActiveSlot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "layout: %v\n", err)
		return 1
	}
	fmt.Printf("phase:            %s\n", state.Phase)
	fmt.Printf("active slot:      %s\n", active)
	fmt.Printf("active version:   %s\n", orEmpty(state.ActiveVersion))
	fmt.Printf("last good:        %s (slot %s)\n", orEmpty(state.LastGoodVersion), state.LastGoodSlot)
	if state.CandidateSlot != "" {
		fmt.Printf("candidate:        %s (slot %s)\n", state.TargetVersion, state.CandidateSlot)
	}
	if len(state.MaxAccepted) > 0 {
		for ch, v := range state.MaxAccepted {
			fmt.Printf("floor %-12s %s\n", ch+":", v)
		}
	}
	if state.LastFailed.Version != "" {
		fmt.Printf("last failed:      %s (%s, %s)\n", state.LastFailed.Version, state.LastFailed.Channel, state.LastFailed.At.Format(time.RFC3339))
	}
	if state.LastError != "" {
		fmt.Printf("last error:       %s\n", state.LastError)
	}
	fmt.Printf("health status:    %s\n", state.UpdateStatusForHealth())
	return 0
}

func orEmpty(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// resolveChannel determines the effective release channel (M6, spec
// §14): an explicit --channel flag wins, then the channel the frame
// persisted in its own DB (the admin UI's per-frame setting arrives
// via the manifest; the frame stores the resolved value in
// frame.db), then stable. The updater only reads the frame DB.
func resolveChannel(flagged bool, flagVal, frameDB string, log *slog.Logger) string {
	if flagged {
		return flagVal
	}
	ch := updater.ChannelStable
	if frameDB == "" {
		return ch
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dbh, err := db.Open(ctx, frameDB, true)
	if err != nil {
		log.Warn("frame db channel read failed", "error", err)
		return ch
	}
	defer dbh.Close()
	var v string
	if err := dbh.QueryRowContext(ctx,
		"SELECT value FROM agent_state WHERE key = ?", "update_channel").Scan(&v); err == nil && updater.ValidChannel(v) {
		ch = v
	}
	return ch
}

// resolveSchemaVersion reads the live frame DB schema when a path is
// given; otherwise the as-built count.
func resolveSchemaVersion(frameDB string) (int, error) {
	if frameDB == "" {
		return asBuiltSchemaVersion()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dbh, err := db.Open(ctx, frameDB, true)
	if err != nil {
		return 0, err
	}
	defer dbh.Close()
	return db.SchemaVersion(ctx, dbh)
}

func asBuiltSchemaVersion() (int, error) {
	m, err := db.LoadMigrations(migs.FS(), "frame")
	if err != nil {
		return 0, err
	}
	if len(m) == 0 {
		return 0, fmt.Errorf("no frame migrations embedded")
	}
	return m[len(m)-1].Version, nil
}
