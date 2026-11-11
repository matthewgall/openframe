// Package cli implements the openframe command-line interface.
//
// The dispatcher is hand-rolled on net/flag rather than using a
// framework (ADR-0004): OpenFrame has six flat commands, and a
// framework would add a dependency with no functional benefit.
package cli

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/openframe/openframe/internal/config"
	"github.com/openframe/openframe/internal/logging"
)

// Exit codes.
const (
	ExitOK    = 0
	ExitError = 1 // runtime failure (also doctor "fail" checks)
	ExitUsage = 2 // bad command-line usage
)

type command struct {
	name  string
	short string
	run   func(args []string) int
}

var commands = []command{
	{"server", "run the management server", runServer},
	{"frame", "run the frame application (headless in milestone 1)", runFrame},
	{"pair", "show or initialise frame pairing state", runPair},
	{"doctor", "diagnose the local system and OpenFrame state", runDoctor},
	{"status", "show local OpenFrame status", runStatus},
	{"version", "print version information", runVersion},
}

// Execute is the CLI entry point.
func Execute(args []string) int {
	if len(args) == 0 {
		usage()
		return ExitUsage
	}
	name := args[0]
	switch name {
	case "help", "-h", "--help":
		usage()
		return ExitOK
	}
	for _, c := range commands {
		if c.name == name {
			return c.run(args[1:])
		}
	}
	fmt.Fprintf(os.Stderr, "unknown command %q\n\n", name)
	usage()
	return ExitUsage
}

func usage() {
	fmt.Fprint(os.Stderr, `openframe — self-hosted digital photo frame platform

Usage:
  openframe <command> [flags]

Commands:
`)
	for _, c := range commands {
		fmt.Fprintf(os.Stderr, "  %-9s %s\n", c.name, c.short)
	}
	fmt.Fprint(os.Stderr, `
Common flags:
  --config PATH     configuration file (YAML); defaults + environment apply
  --log-level LVL   debug | info | warn | error

Configuration environment variables use the OPENFRAME_ prefix and mirror
the YAML path, e.g. OPENFRAME_SERVER_LISTEN, OPENFRAME_FRAME_SERVER_URL.
See docs/development.md.
`)
}

// commonFlags are shared by the long-running commands.
type commonFlags struct {
	configPath string
	logLevel   string
	json       bool
}

func (c *commonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.configPath, "config", "", "path to YAML configuration file")
	fs.StringVar(&c.logLevel, "log-level", "", "override log level (debug|info|warn|error)")
	fs.BoolVar(&c.json, "json", false, "JSON output (where applicable)")
}

// loadEnv loads config and builds the root logger. Commands that need to
// work even when the configuration is invalid (doctor, status) fall back
// to defaults via configTolerant below.
func loadEnv(cf *commonFlags) (*config.Config, *slog.Logger, error) {
	cfg, err := config.Load(cf.configPath)
	if err != nil {
		return nil, nil, err
	}
	if cf.logLevel != "" {
		cfg.Log.Level = cf.logLevel
	}
	log, err := logging.New(logging.Options{
		Level:  cfg.Log.Level,
		Format: cfg.Log.Format,
		Output: cfg.Log.Output,
	})
	if err != nil {
		return nil, nil, err
	}
	return cfg, log, nil
}

// configTolerant loads configuration but degrades to defaults when the
// file is missing or invalid, returning the underlying error so callers
// can report it as a warning (doctor/status must work on a broken box).
func configTolerant(cf *commonFlags) (*config.Config, *slog.Logger, error) {
	cfg, err := config.Load(cf.configPath)
	if err != nil {
		cfg = config.Default()
		if cf.logLevel != "" {
			cfg.Log.Level = cf.logLevel
		}
		log, lerr := logging.New(logging.Options{
			Level: cfg.Log.Level, Format: cfg.Log.Format, Output: cfg.Log.Output,
		})
		if lerr != nil {
			log = slog.Default()
		}
		return cfg, log, err
	}
	if cf.logLevel != "" {
		cfg.Log.Level = cf.logLevel
	}
	log, lerr := logging.New(logging.Options{
		Level:  cfg.Log.Level,
		Format: cfg.Log.Format,
		Output: cfg.Log.Output,
	})
	if lerr != nil {
		return cfg, slog.Default(), err
	}
	return cfg, log, err
}

// signalContext returns a context cancelled by SIGINT/SIGTERM.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}
