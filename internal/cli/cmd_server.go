package cli

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/matthewgall/openframe/internal/db"
	"github.com/matthewgall/openframe/internal/id"
	"github.com/matthewgall/openframe/internal/logging"
	"github.com/matthewgall/openframe/internal/server"
	"github.com/matthewgall/openframe/internal/version"
	migrations "github.com/matthewgall/openframe/migrations"
)

func runServer(args []string) int {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	var cf commonFlags
	cf.register(fs)
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}

	cfg, log, err := loadEnv(&cf)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		return ExitError
	}
	log = logging.NewComponent(log, "server")
	log.Info("starting openframe server",
		"version", version.Version, "commit", version.Commit)

	// T-09: TLS is a deployment requirement (reverse proxy). The
	// server itself listens plain on its bind address; when the
	// public base URL is unset or not HTTPS, say so loudly at every
	// start so a plain-HTTP deployment cannot be accidental.
	switch {
	case cfg.Server.PublicBaseURL == "":
		log.Warn("server.public_base_url is not configured — set it to the HTTPS URL frames will use, behind a TLS-terminating reverse proxy (threat model T-09)")
	case !strings.HasPrefix(cfg.Server.PublicBaseURL, "https://"):
		log.Warn("public base URL is not HTTPS — serve the server through a TLS-terminating reverse proxy before exposing it (threat model T-09)")
	}

	ctx, stop := signalContext()
	defer stop()

	// Database.
	serverDir, _, _ := cfg.Paths()
	dbPath := filepath.Join(serverDir, "server.db")
	dbh, err := db.Open(ctx, dbPath, false)
	if err != nil {
		log.Error("failed to open server database", "path", dbPath, "error", err)
		return ExitError
	}
	defer dbh.Close()

	if applied, err := db.MigrateFromFS(ctx, dbh, migrations.FS(), "server"); err != nil {
		log.Error("migrations failed", "error", err)
		return ExitError
	} else if len(applied) > 0 {
		log.Info("database migrations applied", "versions", applied)
	}

	// Instance identity (used by future pairing and backup tooling).
	if err := ensureInstanceID(ctx, dbh); err != nil {
		log.Warn("could not set instance id", "error", err)
	}

	srv, err := server.New(cfg, log, dbh)
	if err != nil {
		log.Error("failed to build server", "error", err)
		return ExitError
	}

	if err := srv.Run(ctx); err != nil {
		log.Error("server failed", "error", err)
		return ExitError
	}
	return ExitOK
}

// ensureInstanceID stores a stable server instance UUID in meta.
func ensureInstanceID(ctx context.Context, dbh *sql.DB) error {
	var existing string
	err := dbh.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'instance_id'").Scan(&existing)
	if err == nil {
		return nil
	}
	if err != sql.ErrNoRows {
		return err
	}
	uid, err := id.NewUUID()
	if err != nil {
		return err
	}
	_, err = dbh.ExecContext(ctx,
		"INSERT INTO meta (key, value) VALUES ('instance_id', ?)", uid)
	return err
}
