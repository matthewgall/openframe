package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/matthewgall/openframe/internal/config"
	"github.com/matthewgall/openframe/internal/db"
	"github.com/matthewgall/openframe/internal/updater"
	"github.com/matthewgall/openframe/internal/version"
	migs "github.com/matthewgall/openframe/migrations"
)

// runUpdate implements `openframe update <status|check|request|reset|set-channel>`
// — the UNPRIVILEGED frame side of milestone 6. It can read update
// state, ask the release server what is available (and verify it
// against the pinned trust store), and *request* an update. It never
// touches the slots, the current symlink or the frame service: those
// are owned by the root openframe-updater (spec §14: the frame and the
// updater are separate components).
func runUpdate(args []string) int {
	if len(args) == 0 {
		updateUsage()
		return ExitUsage
	}
	switch args[0] {
	case "status":
		return updateStatus(args[1:])
	case "check":
		return updateCheck(args[1:])
	case "request":
		return updateRequest(args[1:])
	case "reset":
		return updateReset(args[1:])
	case "set-channel":
		return updateSetChannel(args[1:])
	case "help", "-h", "--help":
		updateUsage()
		return ExitOK
	}
	fmt.Fprintf(os.Stderr, "unknown update subcommand %q\n\n", args[0])
	updateUsage()
	return ExitUsage
}

func updateUsage() {
	fmt.Fprint(os.Stderr, `openframe update — frame-side update operations (unprivileged)

Usage:
  openframe update status        show update state (phase, slots, versions, floor)
  openframe update check         check the release server for a verified update
  openframe update request       request an update (honoured by the updater at its next run)
  openframe update reset         clear the pending request marker and (if writable) the update state
  openframe update set-channel C set this frame's release channel (C: stable|beta|development)

set-channel stores the channel as a LOCAL override in frame.db (it wins
over the admin UI's per-frame channel, matching the normal settings
precedence). The frame applies it at its next config recompute (restart
or server config push); the updater reads the persisted value on its
next run. Use: openframe update set-channel ""
to clear the local override and fall back to the admin UI value / stable.

The privileged openframe-updater performs installs and rollbacks; these
commands only read state, verify releases and express intent.
`)
}

// updatePaths resolves the updater-relevant paths from configuration.
type updatePaths struct {
	stateDir string
	trust    string
	dbPath   string
}

func updatePathsFromConfig(cfg *config.Config) updatePaths {
	_, frameDir, updatesDir := cfg.Paths()
	p := updatePaths{
		stateDir: updatesDir,
		dbPath:   filepath.Join(frameDir, "frame.db"),
	}
	// Trust file: production layout first, then the dev location
	// inside the state dir.
	for _, c := range []string{
		"/opt/openframe/trust/trusted-keys.json",
		filepath.Join(updatesDir, "trusted-keys.json"),
	} {
		if _, err := os.Stat(c); err == nil {
			p.trust = c
			break
		}
	}
	return p
}

func updateStatus(args []string) int {
	cf := &commonFlags{}
	fs := flag.NewFlagSet("update status", flag.ExitOnError)
	cf.register(fs)
	fs.Parse(args)

	cfg, err := config.Load(cf.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return ExitError
	}
	p := updatePathsFromConfig(cfg)
	layout := updater.NewLayout("/opt/openframe", "openframe")
	state, err := updater.LoadState(p.stateDir, layout)
	if err != nil {
		// A missing state file is normal before the updater has run
		// (and universal on dev machines without /opt/openframe): a
		// status command must degrade gracefully, not fail.
		if _, serr := os.Stat(filepath.Join(p.stateDir, updater.StateFileName)); os.IsNotExist(serr) {
			fmt.Printf("phase:            (none — the updater has not run on this machine)\n")
			fmt.Printf("active slot:      (no slot layout at %s)\n", "/opt/openframe")
			fmt.Printf("active version:   %s (this build)\n", version.Version)
			return ExitOK
		}
		fmt.Fprintf(os.Stderr, "state: %v\n", err)
		return ExitError
	}
	printUpdateState(state, layout, p.stateDir)
	return ExitOK
}

func printUpdateState(state *updater.State, layout *updater.Layout, stateDir string) {
	active, err := layout.ActiveSlot()
	activeStr := "(unknown)"
	if err == nil {
		activeStr = active
	}
	fmt.Printf("phase:            %s\n", state.Phase)
	fmt.Printf("active slot:      %s\n", activeStr)
	fmt.Printf("active version:   %s\n", orNone(state.ActiveVersion))
	fmt.Printf("last good:        %s (slot %s)\n", orNone(state.LastGoodVersion), state.LastGoodSlot)
	if state.CandidateSlot != "" {
		fmt.Printf("candidate:        %s (slot %s)\n", orNone(state.TargetVersion), state.CandidateSlot)
	}
	for ch, v := range state.MaxAccepted {
		fmt.Printf("floor %-12s %s\n", ch+":", v)
	}
	if state.LastFailed.Version != "" {
		fmt.Printf("last failed:      %s (%s, %s)\n", state.LastFailed.Version, state.LastFailed.Channel, state.LastFailed.At.Format(time.RFC3339))
	}
	if state.LastError != "" {
		fmt.Printf("last error:       %s\n", state.LastError)
	}
	if req := updater.PendingRequest(stateDir, time.Now()); req != nil {
		fmt.Printf("request pending:  %s\n", req.By)
	}
	fmt.Printf("health status:    %s\n", state.UpdateStatusForHealth())
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func updateCheck(args []string) int {
	cf := &commonFlags{}
	fs := flag.NewFlagSet("update check", flag.ExitOnError)
	cf.register(fs)
	channel := fs.String("channel", "", "override channel (default from config)")
	server := fs.String("server", "", "override release server URL (default from config)")
	fs.Parse(args)

	cfg, err := config.Load(cf.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return ExitError
	}
	p := updatePathsFromConfig(cfg)
	if p.trust == "" {
		fmt.Fprintln(os.Stderr, "no trusted-keys.json found (checked /opt/openframe/trust and the updates state dir); a release check requires pinned keys")
		return ExitError
	}
	ch := cfg.Frame.Update.Channel
	if *channel != "" {
		ch = *channel
	}
	if !updater.ValidChannel(ch) {
		fmt.Fprintf(os.Stderr, "unknown channel %q\n", ch)
		return ExitError
	}
	srv := cfg.Frame.ServerURL
	if *server != "" {
		srv = *server
	}
	if srv == "" {
		fmt.Fprintln(os.Stderr, "no server_url configured; nothing to check")
		return ExitError
	}

	// Device id (read-only; empty when the frame has no identity yet).
	deviceID := readDeviceID(p.dbPath)

	// Schema version: live DB when readable, else as-built.
	schemaVersion := 0
	if dbh := openFrameDBRO(p.dbPath); dbh != nil {
		defer dbh.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if v, err := db.SchemaVersion(ctx, dbh); err == nil {
			schemaVersion = v
		}
	}
	if schemaVersion == 0 {
		if m, err := db.LoadMigrations(migs.FS(), "frame"); err == nil && len(m) > 0 {
			schemaVersion = m[len(m)-1].Version
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := updater.Check(ctx, updater.CheckOptions{
		ServerURL:     srv,
		Channel:       ch,
		TrustPath:     p.trust,
		StateDir:      p.stateDir,
		OS:            "linux",
		Arch:          hostArch(),
		MaxProtocol:   1,
		SchemaVersion: schemaVersion,
		DeviceID:      deviceID,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "check: %v\n", err)
		return ExitError
	}
	if cf.json {
		return emitJSON(res)
	}
	fmt.Printf("channel:        %s\n", ch)
	fmt.Printf("active version: %s\n", orNone(res.ActiveVersion))
	if res.Release != nil {
		r := res.Release
		fmt.Printf("latest release: %s (%s, %s, %s, rollout %d%%)\n",
			r.Version, r.Channel, r.OS, r.Arch, r.RolloutPercent)
	}
	switch res.Action {
	case "available":
		fmt.Println("result:         update available")
	case "already-current":
		fmt.Println("result:         already up to date")
	case "rejected":
		fmt.Printf("result:         rejected: %s\n", res.ErrText)
	case "nothing":
		if res.ErrText != "" {
			fmt.Printf("result:         none: %s\n", res.ErrText)
		} else {
			fmt.Println("result:         none (device not in rollout or no release)")
		}
	}
	if len(res.RotationPending) > 0 {
		fmt.Printf("key rotation:   %v (persisted by the updater on next run)\n", res.RotationPending)
	}
	return ExitOK
}

func hostArch() string {
	// Frames are deployed per-architecture; report the runtime arch.
	return runtime.GOARCH
}

func readDeviceID(dbPath string) string {
	dbh := openFrameDBRO(dbPath)
	if dbh == nil {
		return ""
	}
	defer dbh.Close()
	var v string
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := dbh.QueryRowContext(ctx, "SELECT id FROM identity LIMIT 1").Scan(&v); err != nil {
		return ""
	}
	return v
}

func openFrameDBRO(path string) *sql.DB {
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dbh, err := db.Open(ctx, path, true)
	if err != nil {
		return nil
	}
	return dbh
}

func updateRequest(args []string) int {
	cf := &commonFlags{}
	fs := flag.NewFlagSet("update request", flag.ExitOnError)
	cf.register(fs)
	by := fs.String("by", "cli", "requester label (cli | frame-ui | admin)")
	fs.Parse(args)

	cfg, err := config.Load(cf.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return ExitError
	}
	p := updatePathsFromConfig(cfg)
	if err := updater.RequestUpdate(p.stateDir, *by); err != nil {
		fmt.Fprintf(os.Stderr, "request: %v\n", err)
		return ExitError
	}
	fmt.Println("update requested; the updater will honour it at its next run (outside-window runs are forced by the request, the policy still applies)")
	return ExitOK
}

func updateReset(args []string) int {
	cf := &commonFlags{}
	fs := flag.NewFlagSet("update reset", flag.ExitOnError)
	cf.register(fs)
	state := fs.Bool("state", false, "also reset the update state (requires write access to the state dir)")
	fs.Parse(args)

	cfg, err := config.Load(cf.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return ExitError
	}
	p := updatePathsFromConfig(cfg)
	updater.ClearRequest(p.stateDir)
	fmt.Println("cleared pending update request")
	if *state {
		if err := os.Remove(filepath.Join(p.stateDir, updater.StateFileName)); err != nil {
			if os.IsPermission(err) {
				fmt.Fprintln(os.Stderr, "state reset: permission denied (the state dir is owned by the updater); the updater's --status will still work")
				return ExitError
			}
			if !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "state reset: %v\n", err)
				return ExitError
			}
		}
		fmt.Println("reset update state (the updater will re-derive it from the layout on its next run)")
	}
	return ExitOK
}

// updateSetChannel writes the local update-channel override into the
// frame DB (M6, spec §14: per-frame channel settings live in the
// frame DB, set via the admin UI or the CLI). It writes BOTH the
// local settings row (the durable override, applied by the frame on
// recompute) and the effective-channel state row (what the root
// updater reads immediately, without waiting for the frame).
func updateSetChannel(args []string) int {
	cf := &commonFlags{}
	fs := flag.NewFlagSet("update set-channel", flag.ExitOnError)
	cf.register(fs)
	fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: openframe update set-channel <stable|beta|development|empty-to-clear>")
		return ExitUsage
	}
	channel := fs.Arg(0)
	if channel != "" && !updater.ValidChannel(channel) {
		fmt.Fprintf(os.Stderr, "unknown channel %q (want stable, beta or development)\n", channel)
		return ExitUsage
	}

	cfg, err := config.Load(cf.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return ExitError
	}
	p := updatePathsFromConfig(cfg)
	if _, err := os.Stat(p.dbPath); err != nil {
		fmt.Fprintf(os.Stderr, "frame database %s not found; run the frame once first\n", p.dbPath)
		return ExitError
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	dbh, err := db.Open(ctx, p.dbPath, false)
	if err != nil {
		fmt.Fprintf(os.Stderr, "frame db: %v\n", err)
		return ExitError
	}
	defer dbh.Close()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if channel == "" {
		if _, err := dbh.ExecContext(ctx, "DELETE FROM frame_settings WHERE key = 'update_channel'"); err != nil {
			fmt.Fprintf(os.Stderr, "settings: %v\n", err)
			return ExitError
		}
		fmt.Println("cleared the local update-channel override; the channel now falls back to the admin UI value, then stable")
		return ExitOK
	}
	if _, err := dbh.ExecContext(ctx,
		`INSERT INTO frame_settings (key, value, updated_at) VALUES ('update_channel', ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		channel, now); err != nil {
		fmt.Fprintf(os.Stderr, "settings: %v\n", err)
		return ExitError
	}
	// Publish the effective value now: the updater reads this row.
	// (A later server-config push or frame restart may raise/lower it
	// through the normal precedence if the local override is cleared.)
	if _, err := dbh.ExecContext(ctx,
		`INSERT INTO agent_state (key, value, updated_at) VALUES ('update_channel', ?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		channel, now); err != nil {
		fmt.Fprintf(os.Stderr, "state: %v\n", err)
		return ExitError
	}
	fmt.Printf("update channel set to %s (local override; the updater uses it from its next run)\n", channel)
	return ExitOK
}

// emitJSON writes v as indented JSON and returns the exit code.
func emitJSON(v any) int {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "json: %v\n", err)
		return ExitError
	}
	fmt.Println(string(b))
	return ExitOK
}
