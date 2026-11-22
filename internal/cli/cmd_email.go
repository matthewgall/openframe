package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/matthewgall/openframe/internal/server/email"
)

// runEmail is the operator's mail test: `openframe email test` sends a
// one-line message to the configured recipient so the SMTP setup can
// be verified without restarting anything.
func runEmail(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: openframe email test [--config PATH]")
		return ExitUsage
	}
	sub := args[0]
	if sub != "test" {
		fmt.Fprintf(os.Stderr, "unknown email command %q (only: test)\n", sub)
		return ExitUsage
	}

	var cf commonFlags
	fs := flag.NewFlagSet("email test", flag.ContinueOnError)
	cf.register(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return ExitUsage
	}
	cfg, log, err := loadEnv(&cf)
	if err != nil {
		fmt.Fprintln(os.Stderr, "openframe:", err)
		return ExitError
	}

	sender := email.New(log, cfg.Server.Email, cfg.Server.AdminEmail)
	if !sender.Enabled() {
		fmt.Fprintln(os.Stderr, "email is disabled (server.email.enabled is false); configure it first — run `openframe doctor` to see the current state")
		return ExitError
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	body := "This is a test message from `openframe email test`. " +
		"If you can read this, outbound mail is configured correctly."
	if err := sender.Send(ctx, "OpenFrame: outbound mail test", body); err != nil {
		fmt.Fprintln(os.Stderr, "send failed:", err)
		return 1
	}
	fmt.Printf("test message sent to %s via %s:%d\n", cfg.Server.AdminEmail, cfg.Server.Email.Host, cfg.Server.Email.Port)
	return ExitOK
}
