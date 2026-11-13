package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/matthewgall/openframe/internal/db"
)

// These tests exercise the real command implementations (including real
// SQLite databases in temp dirs). `openframe server` and `openframe
// frame` long-running paths are covered by the package-level smoke
// tests and by internal/server and internal/frame tests; here we cover
// the dispatcher and the short commands.

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// runCmd executes a command, capturing stdout and exit code.
func runCmd(t *testing.T, args ...string) (string, int) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	code := Execute(args)
	w.Close()
	os.Stdout = old
	data, _ := io.ReadAll(r)
	return string(data), code
}

func TestDispatcherUnknownCommand(t *testing.T) {
	_, code := runCmd(t, "frobnicate")
	if code != ExitUsage {
		t.Errorf("code = %d, want %d", code, ExitUsage)
	}
}

func TestDispatcherNoArgs(t *testing.T) {
	_, code := runCmd(t)
	if code != ExitUsage {
		t.Errorf("code = %d, want %d", code, ExitUsage)
	}
}

func TestVersionCommand(t *testing.T) {
	out, code := runCmd(t, "version")
	if code != ExitOK {
		t.Errorf("code = %d", code)
	}
	if !strings.Contains(out, "openframe") {
		t.Errorf("output = %q", out)
	}

	jsonOut, code := runCmd(t, "version", "--json")
	if code != ExitOK {
		t.Errorf("json code = %d", code)
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(jsonOut), &m); err != nil {
		t.Errorf("version --json not JSON: %v (%s)", err, jsonOut)
	}
	if m["version"] == "" || m["os"] == "" {
		t.Errorf("version --json incomplete: %v", m)
	}
}

func TestPairInitialisesIdentity(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OPENFRAME_DATA_DIR", dir)

	out, code := runCmd(t, "pair")
	if code != ExitOK {
		t.Fatalf("code = %d, out=%q", code, out)
	}
	m := regexp.MustCompile(`device id:\s+(\S+)`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no device id in output: %q", out)
	}
	if !uuidRE.MatchString(m[1]) {
		t.Errorf("device id %q not a uuid", m[1])
	}

	// Second run must report the same identity.
	out2, code := runCmd(t, "pair")
	if code != ExitOK {
		t.Fatalf("second code = %d", code)
	}
	if !strings.Contains(out2, m[1]) {
		t.Errorf("identity changed between runs:\n%s\n%s", out, out2)
	}

	// JSON form.
	jout, code := runCmd(t, "pair", "--json")
	if code != ExitOK {
		t.Fatalf("json code = %d", code)
	}
	var p pairStatus
	if err := json.Unmarshal([]byte(jout), &p); err != nil {
		t.Fatalf("pair --json: %v (%s)", err, jout)
	}
	if p.DeviceID != m[1] {
		t.Errorf("json device id mismatch: %q vs %q", p.DeviceID, m[1])
	}
	// No server_url is configured in the temp env, so the frame is
	// unconfigured (M3 states: unconfigured | unpaired | paired).
	if p.State != "unconfigured" {
		t.Errorf("state = %q, want unconfigured", p.State)
	}
}

func TestBackupRestoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OPENFRAME_DATA_DIR", dir)

	// pair creates the frame DB.
	if _, code := runCmd(t, "pair"); code != ExitOK {
		t.Fatalf("pair code = %d", code)
	}
	out := filepath.Join(t.TempDir(), "frame-backup")
	outArg := "--out=" + out

	// Backup + verify.
	if outStr, code := runCmd(t, "backup", "frame", outArg); code != ExitOK {
		t.Fatalf("backup frame: %d, %q", code, outStr)
	}
	if outStr, code := runCmd(t, "backup", "verify", "--dir="+out); code != ExitOK {
		t.Fatalf("verify: %d, %q", code, outStr)
	}

	// Destroy the live db and restore from the backup.
	if err := os.Remove(filepath.Join(dir, "frame", "frame.db")); err != nil {
		t.Fatal(err)
	}
	if outStr, code := runCmd(t, "restore", "frame", "--from="+out, "--yes"); code != ExitOK {
		t.Fatalf("restore: %d, %q", code, outStr)
	}
	dbPath := filepath.Join(dir, "frame", "frame.db")
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("restored db missing: %v", err)
	}

	// Restore must be idempotent-safe: a second restore preserves the
	// first result as .pre-restore-* (no data loss, no crash).
	if outStr, code := runCmd(t, "restore", "frame", "--from="+out, "--yes"); code != ExitOK {
		t.Fatalf("second restore: %d, %q", code, outStr)
	}

	// Kind mismatch is rejected.
	if _, code := runCmd(t, "restore", "server", "--from="+out, "--yes"); code == ExitOK {
		t.Fatal("frame backup accepted by restore server")
	}
}

func TestUpdateSetChannel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OPENFRAME_DATA_DIR", dir)

	// pair creates the frame DB (identity + agent_state).
	if _, code := runCmd(t, "pair"); code != ExitOK {
		t.Fatalf("pair code = %d", code)
	}

	// Set the channel locally.
	out, code := runCmd(t, "update", "set-channel", "beta")
	if code != ExitOK {
		t.Fatalf("set-channel code = %d, out=%q", code, out)
	}
	if !strings.Contains(out, "beta") {
		t.Fatalf("output missing beta: %q", out)
	}

	// Both rows are persisted: the durable local override and the
	// state row the root updater reads (M6, spec §14).
	dbPath := filepath.Join(dir, "frame", "frame.db")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dbh, err := db.Open(ctx, dbPath, true)
	if err != nil {
		t.Fatal(err)
	}
	defer dbh.Close()
	var v1, v2 string
	if err := dbh.QueryRowContext(ctx,
		"SELECT value FROM frame_settings WHERE key = 'update_channel'").Scan(&v1); err != nil {
		t.Fatalf("frame_settings row: %v", err)
	}
	if err := dbh.QueryRowContext(ctx,
		"SELECT value FROM agent_state WHERE key = 'update_channel'").Scan(&v2); err != nil {
		t.Fatalf("agent_state row: %v", err)
	}
	if v1 != "beta" || v2 != "beta" {
		t.Fatalf("rows = %q/%q, want beta/beta", v1, v2)
	}

	// Clearing removes the override (the state row keeps its last
	// published value until the frame recomputes — acceptable: the
	// frame owns that row in normal operation).
	if out, code := runCmd(t, "update", "set-channel", ""); code != ExitOK {
		t.Fatalf("clear code = %d, out=%q", code, out)
	}
	if err := dbh.QueryRowContext(ctx,
		"SELECT value FROM frame_settings WHERE key = 'update_channel'").Scan(&v1); err != sql.ErrNoRows && err != nil {
		t.Fatalf("frame_settings row after clear: %v", err)
	}

	// Invalid channels are rejected at the CLI.
	if _, code := runCmd(t, "update", "set-channel", "nightly"); code == ExitOK {
		t.Fatal("invalid channel accepted")
	}
}

func TestStatusJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OPENFRAME_DATA_DIR", dir)

	// No frame state yet.
	out, code := runCmd(t, "status", "--json")
	if code != ExitOK {
		t.Fatalf("code = %d", code)
	}
	var s statusReport
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("status --json: %v (%s)", err, out)
	}
	if s.Frame.Available {
		t.Error("fresh data dir should have no frame state")
	}

	// After pair, identity appears.
	runCmd(t, "pair")
	out2, _ := runCmd(t, "status", "--json")
	var s2 statusReport
	json.Unmarshal([]byte(out2), &s2)
	if !s2.Frame.Available || s2.Frame.DeviceID == "" {
		t.Errorf("status after pair: %+v", s2)
	}
}

func TestDoctorJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OPENFRAME_DATA_DIR", dir)

	out, code := runCmd(t, "doctor", "--json")
	// doctor may exit 1 on warn? No — only fail checks set exit 1.
	if code != ExitOK && code != ExitError {
		t.Fatalf("unexpected code %d", code)
	}
	var rep doctorReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("doctor --json: %v (%s)", err, out)
	}
	if len(rep.Checks) < 10 {
		t.Errorf("only %d checks", len(rep.Checks))
	}
	names := map[string]bool{}
	for _, c := range rep.Checks {
		names[c.Name] = true
		if c.Status != checkOK && c.Status != checkWarn && c.Status != checkFail {
			t.Errorf("bad status %q on %s", c.Status, c.Name)
		}
	}
	for _, want := range []string{"os", "memory", "disk", "config", "server database", "frame database", "display", "touch", "pairing", "cache", "updater"} {
		if !names[want] {
			t.Errorf("missing check %q", want)
		}
	}
}

func TestDoctorDetectsCorruptDatabase(t *testing.T) {
	dir := t.TempDir()
	frameDB := filepath.Join(dir, "frame", "frame.db")
	os.MkdirAll(filepath.Dir(frameDB), 0o700)
	// Write garbage that is not a SQLite database.
	if err := os.WriteFile(frameDB, []byte("this is not a sqlite database at all, definitely not"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENFRAME_DATA_DIR", dir)

	out, _ := runCmd(t, "doctor", "--json")
	var rep doctorReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	for _, c := range rep.Checks {
		if c.Name == "frame database" && c.Status != checkFail {
			t.Errorf("corrupt frame db: status = %q, want fail", c.Status)
		}
	}
}
