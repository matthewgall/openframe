package cli

import (
	"fmt"

	"github.com/matthewgall/openframe/internal/config"
)

// emailCheck reports the outbound-mail configuration (doctor is
// read-only: it reports, it never sends — use `openframe email test`
// for a live delivery check).
func emailCheck(cfg *config.Config, add func(string, checkStatus, string)) {
	e := cfg.Server.Email
	if !e.Enabled {
		add("email", checkWarn, "disabled — first-run credentials are logged to the journal instead of being emailed")
		return
	}
	detail := fmt.Sprintf("smtp %s:%d", e.Host, e.Port)
	if e.Username != "" {
		detail += " (auth: " + e.Username + ")"
	}
	detail += " -> " + cfg.Server.AdminEmail
	if e.From != "" {
		detail += " from " + e.From
	}
	add("email", checkOK, detail)
}
