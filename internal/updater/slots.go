package updater

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Slot names. An A/B layout keeps two complete copies of the binary
// (spec §14: "Use A/B binary slots" / "Atomic activation" / "Automatic
// rollback"). The active one is selected by the `current` symlink; the
// updater never writes to the active slot, so a power failure at any
// point leaves either the old or the new complete binary active —
// never a torn one.
const (
	SlotA = "a"
	SlotB = "b"
)

// Layout is the on-disk A/B layout under a root directory (e.g.
// /opt/openframe on a production frame, a temp dir in tests):
//
//	<root>/
//	  slots/a/openframe     slot A binary
//	  slots/b/openframe     slot B binary
//	  current               symlink -> slots/<a|b>/openframe
//	  trust/trusted-keys.json
//
// Activation is the single atomic operation in the whole updater:
// renaming a temporary symlink over `current`.
type Layout struct {
	Root        string
	SlotsDir    string
	CurrentLink string
	BinName     string
}

// NewLayout builds a Layout under root with the given binary name
// (production: "openframe").
func NewLayout(root, binName string) *Layout {
	slots := filepath.Join(root, "slots")
	return &Layout{
		Root:        root,
		SlotsDir:    slots,
		CurrentLink: filepath.Join(root, "current"),
		BinName:     binName,
	}
}

// TrustPath is where the pinned trust store lives in this layout.
func (l *Layout) TrustPath() string { return filepath.Join(l.Root, "trust", "trusted-keys.json") }

// SlotPath returns the directory of one slot.
func (l *Layout) SlotPath(slot string) string {
	if slot != SlotA && slot != SlotB {
		return ""
	}
	return filepath.Join(l.SlotsDir, slot)
}

// BinaryPath returns the binary path inside one slot.
func (l *Layout) BinaryPath(slot string) string {
	return filepath.Join(l.SlotPath(slot), l.BinName)
}

// Ensure creates the layout directories (and an initial current link
// to slot A if none exists). Idempotent.
func (l *Layout) Ensure() error {
	for _, d := range []string{l.SlotsDir, l.SlotPath(SlotA), l.SlotPath(SlotB), filepath.Join(l.Root, "trust")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("updater: mkdir %s: %w", d, err)
		}
	}
	if _, err := os.Lstat(l.CurrentLink); os.IsNotExist(err) {
		// Initial link: created directly (the slot binary may not
		// exist yet — deployment installs it via InstallAnySlot).
		rel, err := filepath.Rel(l.Root, l.BinaryPath(SlotA))
		if err != nil {
			return fmt.Errorf("updater: rel: %w", err)
		}
		if err := os.Symlink(rel, l.CurrentLink); err != nil {
			return fmt.Errorf("updater: initial link: %w", err)
		}
		if err := fsyncDir(l.Root); err != nil {
			return err
		}
	}
	return nil
}

// ActiveSlot resolves the current symlink and returns "a" or "b".
// The link targets the binary inside the slot (root/slots/<slot>/bin).
func (l *Layout) ActiveSlot() (string, error) {
	target, err := os.Readlink(l.CurrentLink)
	if err != nil {
		return "", fmt.Errorf("updater: resolve current: %w", err)
	}
	rel, err := filepath.Rel(l.Root, filepath.Join(filepath.Dir(l.CurrentLink), target))
	if err != nil {
		return "", fmt.Errorf("updater: resolve current: %w", err)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) == 3 && parts[0] == "slots" && parts[2] == l.BinName &&
		(parts[1] == SlotA || parts[1] == SlotB) {
		return parts[1], nil
	}
	return "", fmt.Errorf("updater: current points at unexpected path %q", rel)
}

// InactiveSlot returns the slot that is not active.
func (l *Layout) InactiveSlot() (string, error) {
	active, err := l.ActiveSlot()
	if err != nil {
		return "", err
	}
	if active == SlotA {
		return SlotB, nil
	}
	return SlotA, nil
}

// ActiveBinaryPath returns the absolute path the service should run.
func (l *Layout) ActiveBinaryPath() (string, error) {
	slot, err := l.ActiveSlot()
	if err != nil {
		return "", err
	}
	return l.BinaryPath(slot), nil
}

// ActiveBinarySHA256 digests the active binary (for state recording
// and diagnostics).
func (l *Layout) ActiveBinarySHA256() (string, error) {
	p, err := l.ActiveBinaryPath()
	if err != nil {
		return "", err
	}
	return SHA256File(p)
}

// InstallBinary writes data to a slot's binary, atomically (temp file
// + fsync + rename), 0755. If slot is the active slot the call is
// refused: the A/B invariant is that the running copy is never
// overwritten in place.
func (l *Layout) InstallBinary(slot string, r io.Reader, size int64) error {
	if active, err := l.ActiveSlot(); err == nil && slot == active {
		return fmt.Errorf("updater: refusing to install over active slot %s", slot)
	}
	return l.installBinary(slot, r, size)
}

// InstallAnySlot is the deployment path (first install, reimage,
// repair): it may write either slot, including the active one, because
// on a fresh device there is no other binary to protect. The update
// flow must use InstallBinary, which refuses the active slot.
func (l *Layout) InstallAnySlot(slot string, r io.Reader, size int64) error {
	return l.installBinary(slot, r, size)
}

func (l *Layout) installBinary(slot string, r io.Reader, size int64) error {
	dir := l.SlotPath(slot)
	if dir == "" {
		return fmt.Errorf("updater: unknown slot %q", slot)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("updater: mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, l.BinName+".new-*")
	if err != nil {
		return fmt.Errorf("updater: temp: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	written, err := io.Copy(tmp, r)
	if err != nil {
		cleanup()
		return fmt.Errorf("updater: write: %w", err)
	}
	if size > 0 && written != size {
		cleanup()
		return fmt.Errorf("%w: wrote %d, want %d", ErrArtifactSize, written, size)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("updater: fsync: %w", err)
	}
	if err := tmp.Chmod(0o755); err != nil {
		cleanup()
		return fmt.Errorf("updater: chmod: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("updater: close: %w", err)
	}
	if err := os.Rename(tmpName, l.BinaryPath(slot)); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("updater: rename: %w", err)
	}
	return fsyncDir(dir)
}

// Activate atomically switches the current symlink to slot. The
// operation is a single rename(2): either the old or the new link is
// visible, never a partial one, so it is safe under power failure.
// A package-wide mutex serialises concurrent activations (the
// updater is single-process, this is defence in depth).
var activateMu sync.Mutex

// Activate implements the atomic symlink swap.
func (l *Layout) Activate(slot string) error {
	if slot != SlotA && slot != SlotB {
		return fmt.Errorf("updater: unknown slot %q", slot)
	}
	activateMu.Lock()
	defer activateMu.Unlock()
	return activate(l, slot)
}

func activate(l *Layout, slot string) error {
	if _, err := os.Stat(l.BinaryPath(slot)); err != nil {
		return fmt.Errorf("updater: cannot activate %s: %w", slot, err)
	}
	// Symlinks are relative so the tree can move (backups, re-imaging).
	rel, err := filepath.Rel(l.Root, l.BinaryPath(slot))
	if err != nil {
		return fmt.Errorf("updater: rel: %w", err)
	}
	tmp := fmt.Sprintf("%s.tmp-%d", l.CurrentLink, os.Getpid())
	if err := os.Symlink(rel, tmp); err != nil {
		return fmt.Errorf("updater: symlink: %w", err)
	}
	if err := os.Rename(tmp, l.CurrentLink); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("updater: swap: %w", err)
	}
	return fsyncDir(l.Root)
}

// RemoveBinary deletes a slot's binary (used after a verified rollback
// when the bad artifact is confirmed unrecoverable — normally kept for
// post-mortem). The active slot's binary is never removed.
func (l *Layout) RemoveBinary(slot string) error {
	active, err := l.ActiveSlot()
	if err == nil && slot == active {
		return fmt.Errorf("updater: refusing to remove active slot %s", slot)
	}
	if err := os.Remove(l.BinaryPath(slot)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("updater: remove %s: %w", slot, err)
	}
	return fsyncDir(l.SlotPath(slot))
}

// SHA256File returns the hex sha256 of the file at path.
func SHA256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fsyncDir flushes a directory's entries so the rename is durable
// across power loss. (Linux-only target platform; see ADR-0002.)
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
