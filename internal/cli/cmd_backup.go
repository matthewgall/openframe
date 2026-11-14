package cli

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/matthewgall/openframe/internal/backup"
	"github.com/matthewgall/openframe/internal/config"
	"github.com/matthewgall/openframe/internal/version"
)

// runBackup implements `openframe backup <server|frame|verify>` —
// M7 backup and restore. A backup is a self-verifying directory
// (manifest + sha256 per file + integrity-checked DB snapshot) that
// can be taken while the service runs (VACUUM INTO is an online
// consistent snapshot).
func runBackup(args []string) int {
	if len(args) == 0 {
		backupUsage()
		return ExitUsage
	}
	switch args[0] {
	case "server":
		return backupServer(args[1:])
	case "frame":
		return backupFrame(args[1:])
	case "verify":
		return backupVerify(args[1:])
	case "help", "-h", "--help":
		backupUsage()
		return ExitOK
	}
	fmt.Fprintf(os.Stderr, "unknown backup subcommand %q\n\n", args[0])
	backupUsage()
	return ExitUsage
}

func backupUsage() {
	fmt.Fprint(os.Stderr, `openframe backup — back up and verify OpenFrame data (M7)

Usage:
  openframe backup server --out DIR   snapshot server.db + photos (online-safe)
  openframe backup frame  --out DIR   snapshot frame.db + credential (+ update state if readable)
  openframe backup verify --dir DIR   re-hash every file + integrity-check the DB

Default --out is <data_dir>/backups/<kind>-<UTC timestamp>. The
restore counterpart is: openframe restore <server|frame> --from DIR
`)
}

func defaultBackupDir(dataDir, kind string) string {
	stamp := time.Now().UTC().Format("20060102-150405")
	return filepath.Join(dataDir, "backups", kind+"-"+stamp)
}

func backupServer(args []string) int {
	cf := &commonFlags{}
	fs := flag.NewFlagSet("backup server", flag.ExitOnError)
	cf.register(fs)
	out := fs.String("out", "", "output directory (default: <data_dir>/backups/server-<stamp>)")
	fs.Parse(args)

	cfg, err := config.Load(cf.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return ExitError
	}
	if *out == "" {
		*out = defaultBackupDir(cfg.DataDir, "server")
	}
	serverDir, _, _ := cfg.Paths()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	note := ""
	if cfg.Server.PhotoStorage.Backend == "s3" {
		note = fmt.Sprintf("photo objects live in S3 bucket %q and are NOT included in this backup (the bucket provides durability; see docs/deployment/production.md)", cfg.Server.PhotoStorage.S3.Bucket)
	}
	res, err := backup.BackupServer(ctx, *out, backup.ServerPaths{
		Kind:        backup.KindServer,
		DataDir:     cfg.DataDir,
		DBPath:      filepath.Join(serverDir, "server.db"),
		PhotoDir:    cfg.PhotoDir(),
		Version:     version.Version,
		StorageNote: note,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "backup: %v\n", err)
		return ExitError
	}
	fmt.Printf("server backup complete: %s\n", res.Dir)
	fmt.Printf("  files: %d (%s)\n", res.Files, humanBytes(res.Bytes))
	if note != "" {
		fmt.Printf("  note: %s\n", note)
	}
	fmt.Printf("  verify with: openframe backup verify --dir %s\n", res.Dir)
	return ExitOK
}

func backupFrame(args []string) int {
	cf := &commonFlags{}
	fs := flag.NewFlagSet("backup frame", flag.ExitOnError)
	cf.register(fs)
	out := fs.String("out", "", "output directory (default: <data_dir>/backups/frame-<stamp>)")
	fs.Parse(args)

	cfg, err := config.Load(cf.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return ExitError
	}
	if *out == "" {
		*out = defaultBackupDir(cfg.DataDir, "frame")
	}
	_, frameDir, updatesDir := cfg.Paths()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	res, err := backup.BackupFrame(ctx, *out, backup.FramePaths{
		Kind:        backup.KindFrame,
		DataDir:     cfg.DataDir,
		DBPath:      filepath.Join(frameDir, "frame.db"),
		Credential:  filepath.Join(frameDir, "credential"),
		UpdateState: filepath.Join(updatesDir, "state.json"),
		Version:     version.Version,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "backup: %v\n", err)
		return ExitError
	}
	fmt.Printf("frame backup complete: %s\n", res.Dir)
	fmt.Printf("  files: %d (%s)\n", res.Files, humanBytes(res.Bytes))
	for _, s := range res.Info.Skipped {
		fmt.Printf("  skipped: %s\n", s)
	}
	fmt.Printf("  verify with: openframe backup verify --dir %s\n", res.Dir)
	return ExitOK
}

func backupVerify(args []string) int {
	cf := &commonFlags{}
	fs := flag.NewFlagSet("backup verify", flag.ExitOnError)
	cf.register(fs)
	dir := fs.String("dir", "", "backup directory to verify")
	fs.Parse(args)
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "--dir is required")
		return ExitUsage
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	info, err := backup.Verify(ctx, *dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "verify FAILED: %v\n", err)
		return ExitError
	}
	fmt.Printf("backup OK: %s (%s, created %s, %d files, %s)\n",
		*dir, info.Kind, info.CreatedAt, info.Files, humanBytes(info.Bytes))
	return ExitOK
}

// runRestore implements `openframe restore <server|frame> --from DIR`.
// Restore verifies the backup first (hashes + integrity), preserves
// any existing live data beside the target (.pre-restore-<stamp>),
// then moves the backup contents into place.
func runRestore(args []string) int {
	if len(args) == 0 {
		restoreUsage()
		return ExitUsage
	}
	switch args[0] {
	case "server":
		return restoreOne(args[1:], backup.KindServer)
	case "frame":
		return restoreOne(args[1:], backup.KindFrame)
	case "help", "-h", "--help":
		restoreUsage()
		return ExitOK
	}
	fmt.Fprintf(os.Stderr, "unknown restore subcommand %q\n\n", args[0])
	restoreUsage()
	return ExitUsage
}

func restoreUsage() {
	fmt.Fprint(os.Stderr, `openframe restore — restore OpenFrame data from a backup (M7)

Usage:
  openframe restore server --from DIR [--yes]
  openframe restore frame  --from DIR [--yes]

The backup is verified (file hashes + DB integrity) before anything
is touched. Stop the service being restored first (systemctl stop
openframe-server / openframe-frame): a running process keeps its old
handles and SQLite data can be left in an inconsistent mix. Existing
live data is preserved as <name>.pre-restore-<stamp>, never deleted.
`)
}

func restoreOne(args []string, kind string) int {
	cf := &commonFlags{}
	fs := flag.NewFlagSet("restore "+kind, flag.ExitOnError)
	cf.register(fs)
	from := fs.String("from", "", "backup directory")
	yes := fs.Bool("yes", false, "do not prompt for confirmation")
	fs.Parse(args)
	if *from == "" {
		fmt.Fprintln(os.Stderr, "--from is required")
		return ExitUsage
	}

	cfg, err := config.Load(cf.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return ExitError
	}

	info, _, err := backup.LoadInfo(*from)
	if err != nil {
		fmt.Fprintf(os.Stderr, "restore: %v\n", err)
		return ExitError
	}
	if info.Kind != kind {
		fmt.Fprintf(os.Stderr, "restore: backup is kind %q but `restore %s` was requested\n", info.Kind, kind)
		return ExitError
	}

	// Targets: manifest top-level name -> live path.
	targets := map[string]string{}
	if kind == backup.KindServer {
		serverDir, _, _ := cfg.Paths()
		targets["server.db"] = filepath.Join(serverDir, "server.db")
		targets["photos"] = cfg.PhotoDir()
	} else {
		_, frameDir, updatesDir := cfg.Paths()
		targets["frame.db"] = filepath.Join(frameDir, "frame.db")
		targets["credential"] = filepath.Join(frameDir, "credential")
		targets["update-state.json"] = filepath.Join(updatesDir, "state.json")
	}

	// Live-data warning + confirmation.
	var live []string
	for name, path := range targets {
		if _, err := os.Stat(path); err == nil {
			live = append(live, fmt.Sprintf("  %s -> %s", name, path))
		}
	}
	if len(live) > 0 {
		fmt.Println("This will REPLACE the following live data (each file is preserved as <name>.pre-restore-<stamp>):")
		for _, l := range live {
			fmt.Println(l)
		}
		fmt.Println("STOP THE SERVICE BEING RESTORED FIRST.")
		if !*yes {
			fmt.Print("Continue? Type 'restore' to confirm: ")
			reader := bufio.NewReader(os.Stdin)
			confirm, _ := reader.ReadString('\n')
			if strings.TrimSpace(confirm) != "restore" {
				fmt.Println("aborted")
				return ExitError
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	moves, err := backup.Restore(ctx, *from, targets)
	if err != nil {
		fmt.Fprintf(os.Stderr, "restore: %v\n", err)
		return ExitError
	}
	for _, mv := range moves {
		if mv.Preserved != "" {
			fmt.Printf("preserved: %s\n", mv.Preserved)
		}
	}
	fmt.Printf("restore complete: %d file(s) from %s\n", len(moves), *from)
	fmt.Println("start the service and check: openframe doctor")
	return ExitOK
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
