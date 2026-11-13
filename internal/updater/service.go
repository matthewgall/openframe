package updater

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/matthewgall/openframe/internal/platform"
)

// Service controls the frame application the updater is swapping
// binaries underneath (spec §14: the updater manages the frame
// service; the frame never updates itself). Production runs the frame
// under systemd; tests and headless development use ProcessService so
// the entire update/rollback state machine is exercised without root.
type Service interface {
	// Start starts the frame service. Idempotent.
	Start(ctx context.Context) error
	// Stop stops the frame service, waiting up to the ctx deadline.
	Stop(ctx context.Context) error
	// Running reports whether the service is up.
	Running(ctx context.Context) (bool, error)
	// WaitReady blocks until the service is up *and* ready (health
	// endpoint / ready marker), or the timeout elapses. Returns a
	// deadline error when it does not become ready in time.
	WaitReady(ctx context.Context, timeout time.Duration) error
	// Healthy is a point-in-time readiness probe.
	Healthy(ctx context.Context) (bool, error)
}

// ---------------------------------------------------------------------
// ProcessService
// ---------------------------------------------------------------------

// ProcessService runs the frame as a child process. It is used by the
// test suite (a fake "frame" script) and headless local development.
//
// Readiness semantics: if ReadyFile is set, the service is ready when
// that file exists (the fake frame creates it after "startup"); if
// HealthURL is set, Healthy probes it. The process exiting at any
// point makes Running/Healthy false — which is exactly the signal the
// rollback path needs ("if the frame cannot start after an update,
// the updater must roll back").
type ProcessService struct {
	// Binary is the executable to run (usually layout.ActiveBinaryPath).
	Binary string
	// BinaryFunc, when set, re-resolves the executable on every Start
	// — required after a slot swap changes which binary is active.
	BinaryFunc func() (string, error)
	// Args are passed to the binary.
	Args []string
	// Dir is the child's working directory.
	Dir string
	// ReadyFile, when non-empty, marks readiness when it appears.
	ReadyFile string
	// HealthURL, when non-empty, is GET-probed for Healthy/WaitReady.
	HealthURL string
	// PollInterval between readiness probes (default 100ms).
	PollInterval time.Duration

	cmd *exec.Cmd
}

// NewProcessService returns a ProcessService for the given binary.
func NewProcessService(binary string, args ...string) *ProcessService {
	return &ProcessService{Binary: binary, Args: args, PollInterval: 100 * time.Millisecond}
}

func (p *ProcessService) poll() time.Duration {
	if p.PollInterval <= 0 {
		return 100 * time.Millisecond
	}
	return p.PollInterval
}

// Start launches the child (killing any previous child first). The
// ready marker is cleared at start: readiness belongs to the new
// incarnation, never to the previous one (a stale marker would make a
// crashing replacement look ready for one poll interval).
func (p *ProcessService) Start(ctx context.Context) error {
	if p.alive() {
		return nil
	}
	if p.ReadyFile != "" {
		os.Remove(p.ReadyFile)
	}
	binary := p.Binary
	if p.BinaryFunc != nil {
		b, err := p.BinaryFunc()
		if err != nil {
			return fmt.Errorf("updater: resolve binary: %w", err)
		}
		binary = b
	}
	cmd := exec.CommandContext(ctx, binary, p.Args...)
	cmd.Dir = p.Dir
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = ownProcessGroupAttr() // own process group
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("updater: start %s: %w", binary, err)
	}
	p.cmd = cmd
	// Reap asynchronously; the exit code is inspectable via alive().
	go func() { _ = cmd.Wait() }()
	return nil
}

func (p *ProcessService) alive() bool {
	if p.cmd == nil || p.cmd.Process == nil {
		return false
	}
	// Wait is not called synchronously (a goroutine owns it); poll the
	// process table instead.
	// Signal 0 is a liveness probe (no signal is sent).
	err := p.cmd.Process.Signal(syscall.Signal(0))
	return err == nil
}

// Stop terminates the child (SIGTERM, then SIGKILL on deadline).
func (p *ProcessService) Stop(ctx context.Context) error {
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	if !p.alive() {
		return nil
	}
	proc := p.cmd.Process
	_ = proc.Signal(termSignal)
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		if !p.alive() {
			return nil
		}
		select {
		case <-ctx.Done():
			_ = proc.Kill()
			return ctx.Err()
		case <-deadline.C:
			_ = proc.Kill()
			// give it a moment to die
			for i := 0; i < 50 && p.alive(); i++ {
				time.Sleep(20 * time.Millisecond)
			}
			if p.alive() {
				return fmt.Errorf("updater: %s did not exit after SIGKILL", p.Binary)
			}
			return nil
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Running is true while the child process is alive.
func (p *ProcessService) Running(ctx context.Context) (bool, error) {
	return p.alive(), nil
}

// Healthy: process alive, plus ready-file / health-URL checks when set.
func (p *ProcessService) Healthy(ctx context.Context) (bool, error) {
	if !p.alive() {
		return false, nil
	}
	if p.ReadyFile != "" {
		if _, err := os.Stat(p.ReadyFile); err != nil {
			return false, nil
		}
	}
	if p.HealthURL != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.HealthURL, nil)
		if err != nil {
			return false, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false, nil
		}
		defer resp.Body.Close()
		return resp.StatusCode < 500, nil
	}
	return true, nil
}

// WaitReady polls Healthy until it succeeds or the timeout elapses.
func (p *ProcessService) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		ok, err := p.Healthy(ctx)
		if err == nil && ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("updater: service did not become ready within %s", timeout)
		case <-time.After(p.poll()):
		}
	}
}

// ---------------------------------------------------------------------
// SystemdService
// ---------------------------------------------------------------------

// SystemdService drives the production frame unit via systemctl. It
// is deliberately a thin, auditable shell-out (spec: the updater is a
// separate root binary with narrow privileges; delegating service
// lifecycle to systemd keeps that surface minimal).
type SystemdService struct {
	Unit         string // e.g. "openframe-frame"
	HealthURL    string // optional readiness probe (frame health endpoint)
	Systemctl    string // path to systemctl (injectable for tests)
	PollInterval time.Duration
}

// NewSystemdService returns the production service controller.
func NewSystemdService(unit, healthURL string) *SystemdService {
	return &SystemdService{Unit: unit, HealthURL: healthURL, Systemctl: "systemctl", PollInterval: 500 * time.Millisecond}
}

func (s *SystemdService) systemctl(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, s.Systemctl, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (s *SystemdService) active(ctx context.Context) bool {
	out, err := s.systemctl(ctx, "is-active", s.Unit)
	if err != nil {
		return false
	}
	return out == "active"
}

// Start runs `systemctl start <unit>` (no-op if already active).
func (s *SystemdService) Start(ctx context.Context) error {
	if s.active(ctx) {
		return nil
	}
	_, err := s.systemctl(ctx, "start", s.Unit)
	return err
}

// Stop runs `systemctl stop <unit>` (no-op if already inactive).
func (s *SystemdService) Stop(ctx context.Context) error {
	if !s.active(ctx) {
		return nil
	}
	_, err := s.systemctl(ctx, "stop", s.Unit)
	return err
}

// Running is `systemctl is-active` == active.
func (s *SystemdService) Running(ctx context.Context) (bool, error) {
	return s.active(ctx), nil
}

// Healthy: unit active, plus the health endpoint when configured.
func (s *SystemdService) Healthy(ctx context.Context) (bool, error) {
	if !s.active(ctx) {
		return false, nil
	}
	if s.HealthURL == "" {
		return true, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.HealthURL, nil)
	if err != nil {
		return false, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, nil
	}
	defer resp.Body.Close()
	return resp.StatusCode < 500, nil
}

// WaitReady polls Healthy up to timeout.
func (s *SystemdService) WaitReady(ctx context.Context, timeout time.Duration) error {
	poll := s.PollInterval
	if poll <= 0 {
		poll = 500 * time.Millisecond
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		ok, err := s.Healthy(ctx)
		if err == nil && ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("updater: %s did not become ready within %s", s.Unit, timeout)
		case <-time.After(poll):
		}
	}
}

// ---------------------------------------------------------------------
// Disk space
// ---------------------------------------------------------------------

// ErrInsufficientDisk is returned when the update cannot proceed
// without falling below the safety margin (spec §20: "Insufficient
// disk space" must be a tested, safe failure).
var ErrInsufficientDisk = fmt.Errorf("updater: insufficient disk space")

// MinFreeBytes is the free-space floor enforced after an update: the
// slot swap plus headroom for the photo cache. Production default.
const MinFreeBytes = 2 * 1024 * 1024 * 1024 // 2 GiB

// DiskFreeBytes returns the free bytes on the filesystem containing
// dir.
func DiskFreeBytes(dir string) (int64, error) {
	return platform.FreeBytes(dir)
}

// CheckDiskSpace verifies dir has at least required bytes free.
func CheckDiskSpace(dir string, required int64) error {
	free, err := DiskFreeBytes(dir)
	if err != nil {
		return err
	}
	if free < required {
		return fmt.Errorf("%w: %d free, need %d", ErrInsufficientDisk, free, required)
	}
	return nil
}

// ArtifactSpaceRequirement is the pre-download space check: the
// artifact plus a margin for extraction churn.
func ArtifactSpaceRequirement(artifactSize int64) int64 {
	margin := int64(512 * 1024 * 1024)
	if artifactSize > 100*1024*1024 {
		margin = artifactSize / 4
	}
	return artifactSize + margin
}

// EnsureSlotDir makes sure the updates directory exists (state lives
// there; the frame reads state.json from here unprivileged).
func EnsureSlotDir(dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	return os.MkdirAll(dir, 0o755)
}
