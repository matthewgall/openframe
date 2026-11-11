package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/openframe/openframe/internal/db"
	"github.com/openframe/openframe/internal/frame"
	"github.com/openframe/openframe/internal/frame/display"
	"github.com/openframe/openframe/internal/logging"
	"github.com/openframe/openframe/internal/version"
	migrations "github.com/openframe/openframe/migrations"
)

func runFrame(args []string) int {
	fs := flag.NewFlagSet("frame", flag.ContinueOnError)
	var cf commonFlags
	cf.register(fs)
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}

	cfg, rootLog, err := loadEnv(&cf)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		return ExitError
	}
	log := logging.NewComponent(rootLog, "frame")
	log.Info("starting openframe frame",
		"version", version.Version, "commit", version.Commit)

	ctx, stop := signalContext()
	defer stop()

	_, frameDir, _ := cfg.Paths()
	dbPath := filepath.Join(frameDir, "frame.db")
	dbh, err := db.Open(ctx, dbPath, false)
	if err != nil {
		log.Error("failed to open frame database", "path", dbPath, "error", err)
		return ExitError
	}
	defer dbh.Close()

	if applied, err := db.MigrateFromFS(ctx, dbh, migrations.FS(), "frame"); err != nil {
		log.Error("migrations failed", "error", err)
		return ExitError
	} else if len(applied) > 0 {
		log.Info("database migrations applied", "versions", applied)
	}

	// The display driver attaches its own component to the bare root
	// logger (nested component attributes would duplicate JSON keys).
	drv, err := display.New(ctx, display.Options{
		Name:   cfg.Frame.Display.Driver,
		Width:  cfg.Frame.Display.Width,
		Height: cfg.Frame.Display.Height,
		Log:    rootLog,
	})
	if err != nil {
		log.Error("failed to initialise display driver", "driver", cfg.Frame.Display.Driver, "error", err)
		return ExitError
	}
	defer drv.Close()

	f, err := frame.New(cfg, rootLog, dbh, drv)
	if err != nil {
		log.Error("failed to build frame", "error", err)
		return ExitError
	}
	if err := f.Run(ctx); err != nil {
		log.Error("frame failed", "error", err)
		return ExitError
	}
	return ExitOK
}
