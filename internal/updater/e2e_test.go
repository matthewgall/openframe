package updater

// End-to-end tests for the updater against the spec §20 "Updaters"
// acceptance matrix. A fake "frame" is a shell script installed into
// the A/B slots whose behaviour is encoded in its own content
// (healthy / crashes / never-ready), driven by the OPENFRAME_TEST_READY
// environment variable; a real HTTP release server (httptest) serves
// the signed metadata and artifacts. Power loss is simulated by
// simply not finishing a run and letting a fresh coordinator
// reconcile.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- fake frame scripts (the "artifact" is the script itself) ---

const (
	scriptHealthy = `#!/bin/sh
rm -f "$OPENFRAME_TEST_READY"
touch "$OPENFRAME_TEST_READY"
while true; do sleep 0.2; done
`
	scriptCrash = `#!/bin/sh
rm -f "$OPENFRAME_TEST_READY"
exit 1
`
	scriptNeverReady = `#!/bin/sh
rm -f "$OPENFRAME_TEST_READY"
while true; do sleep 0.2; done
`
)

// --- release server ---

type releaseServer struct {
	t            *testing.T
	srv          *httptest.Server
	latest       map[string]Release // channel -> release
	artifacts    map[string][]byte  // version -> bytes
	corrupt      bool               // serve artifact bytes that mismatch
	truncated    bool               // serve a partial artifact then error
	down         bool               // serve 500
	metadataHits int
	artifactHits int
}

func newReleaseServer(t *testing.T) *releaseServer {
	rs := &releaseServer{
		t:         t,
		latest:    map[string]Release{},
		artifacts: map[string][]byte{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/latest/", func(w http.ResponseWriter, r *http.Request) {
		rs.metadataHits++
		if rs.down {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		channel := strings.TrimPrefix(r.URL.Path, "/latest/")
		rel, ok := rs.latest[channel]
		if !ok {
			http.Error(w, "no release", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rel)
	})
	mux.HandleFunc("/artifact/", func(w http.ResponseWriter, r *http.Request) {
		rs.artifactHits++
		if rs.down {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		version := strings.TrimPrefix(r.URL.Path, "/artifact/")
		data, ok := rs.artifacts[version]
		if !ok {
			http.Error(w, "no artifact", http.StatusNotFound)
			return
		}
		if rs.corrupt {
			data = append(append([]byte{}, data...), 0xff) // size+hash mismatch
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		if rs.truncated {
			// Simulate a connection drop mid-stream: the client gets a
			// short body. io.Copy may report an error or a short
			// success — either way the integrity check must reject it
			// and nothing may be installed.
			_, _ = w.Write(data[:len(data)/2])
			return
		}
		_, _ = w.Write(data)
	})
	rs.srv = httptest.NewServer(mux)
	t.Cleanup(rs.srv.Close)
	return rs
}

func (rs *releaseServer) fetchRelease(ctx context.Context, channel string) (Release, error) {
	var rel Release
	if err := rs.getJSON("/latest/"+channel, &rel); err != nil {
		return Release{}, err
	}
	return rel, nil
}

func (rs *releaseServer) downloadArtifact(ctx context.Context, rel Release) (io.ReadCloser, error) {
	resp, err := http.Get(rs.srv.URL + "/artifact/" + rel.Version)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("artifact fetch: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return resp.Body, nil
}

func (rs *releaseServer) getJSON(path string, v any) error {
	resp, err := http.Get(rs.srv.URL + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", path, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// --- harness ---

type harness struct {
	t        *testing.T
	root     string
	stateDir string
	ready    string
	layout   *Layout
	trust    *testTrust
	srv      *releaseServer
	svc      *ProcessService
	coord    *Coordinator
	now      time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	stateDir := t.TempDir()
	trust := newTestTrust(t)
	layout := NewLayout(root, "openframe")
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	// Initial v1.0.0 in slot A (active) — deployment install.
	if err := layout.InstallAnySlot(SlotA, strings.NewReader(scriptHealthy), int64(len(scriptHealthy))); err != nil {
		t.Fatal(err)
	}
	// Pinned trust file.
	tb, err := trust.trust.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(layout.TrustPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layout.TrustPath(), tb, 0o600); err != nil {
		t.Fatal(err)
	}
	// Initial state: v1.0.0 active and last-good.
	s, err := DefaultState(layout)
	if err != nil {
		t.Fatal(err)
	}
	s.ActiveVersion = "1.0.0"
	s.LastGoodVersion = "1.0.0"
	if err := s.Save(stateDir); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(root, "ready")
	t.Setenv("OPENFRAME_TEST_READY", ready)

	srv := newReleaseServer(t)
	svc := &ProcessService{
		BinaryFunc:   layout.ActiveBinaryPath,
		PollInterval: 20 * time.Millisecond,
		ReadyFile:    ready,
	}
	coord := &Coordinator{
		Layout:           layout,
		StateDir:         stateDir,
		TrustPath:        layout.TrustPath(),
		Service:          svc,
		FetchRelease:     srv.fetchRelease,
		DownloadArtifact: srv.downloadArtifact,
		Now:              func() time.Time { return time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC) },
		OS:               "linux",
		Arch:             "arm64",
		SchemaVersion:    3,
		MaxProtocol:      1,
		DeviceID:         "test-device-1",
		ValidateTimeout:  900 * time.Millisecond,
		Log:              logf(t),
	}
	h := &harness{
		t: t, root: root, stateDir: stateDir, ready: ready,
		layout: layout, trust: trust, srv: srv, svc: svc, coord: coord,
		now: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC),
	}
	return h
}

// running reports whether the fake frame process is alive.
func (h *harness) running() bool {
	ok, err := h.svc.Running(context.Background())
	if err != nil {
		h.t.Fatalf("running: %v", err)
	}
	return ok
}

// startFrame boots the currently active fake frame (v1.0.0 healthy) so
// tests can assert the frame is unaffected by updater failures.
func (h *harness) startFrame(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if err := h.svc.Start(ctx); err != nil {
		t.Fatalf("start initial frame: %v", err)
	}
	if err := h.svc.WaitReady(ctx, 3*time.Second); err != nil {
		t.Fatalf("initial frame not ready: %v", err)
	}
}

// stopAll tears down any child frame process at test end.
func (h *harness) stopAll() {
	_ = h.svc.Stop(context.Background())
}

// serveRelease registers a signed release + artifact on the server.
func (h *harness) serveRelease(version, script string, mut func(*Release)) Release {
	h.t.Helper()
	now := h.now
	rel := Release{
		Version:            version,
		Channel:            ChannelStable,
		OS:                 "linux",
		Arch:               "arm64",
		MinProtocolVersion: 1,
		MinSchemaVersion:   3,
		ReleasedAt:         now.Add(-time.Hour),
		ExpiresAt:          now.Add(72 * time.Hour),
		RolloutPercent:     100,
	}
	if mut != nil {
		mut(&rel)
	}
	artifact := []byte(script)
	rel.ArtifactSHA256 = ArtifactSHA256Hex(artifact)
	rel.ArtifactSize = int64(len(artifact))
	signed, err := SignRelease(rel, h.trust.keyAID, h.trust.keyA)
	if err != nil {
		h.t.Fatal(err)
	}
	h.srv.latest[ChannelStable] = signed
	h.srv.artifacts[version] = artifact
	return signed
}

func (h *harness) run(t *testing.T, policy string, window *Window, force bool) *Result {
	t.Helper()
	ctx := context.Background()
	res, err := h.coord.Run(ctx, ChannelStable, policy, window, force)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

var alwaysWindow = &Window{Start: "00:00", End: "23:59", TZ: "UTC"}

func (h *harness) state() *State {
	s, err := LoadState(h.stateDir, h.layout)
	if err != nil {
		h.t.Fatalf("load state: %v", err)
	}
	return s
}

func (h *harness) activeSlot(t *testing.T) string {
	t.Helper()
	slot, err := h.layout.ActiveSlot()
	if err != nil {
		t.Fatalf("active slot: %v", err)
	}
	return slot
}

// --- §20 matrix ---

// 1. Successful A/B update.
func TestE2ESuccessfulUpdate(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.startFrame(t)
	h.serveRelease("1.1.0", scriptHealthy, nil)

	res := h.run(t, PolicyAutomatic, alwaysWindow, false)
	if res.Action != ResultUpdated {
		t.Fatalf("action = %q (err=%v), want updated", res.Action, res.Err)
	}
	if got := h.activeSlot(t); got != SlotB {
		t.Fatalf("active slot = %s, want b", got)
	}
	s := h.state()
	if s.Phase != PhaseIdle || s.ActiveVersion != "1.1.0" || s.LastGoodSlot != SlotB || s.LastGoodVersion != "1.1.0" {
		t.Fatalf("state = %+v", s)
	}
	if s.MaxAccepted[ChannelStable] != "1.1.0" {
		t.Fatalf("anti-rollback floor = %v, want 1.1.0", s.MaxAccepted)
	}
	if !h.running() {
		t.Fatal("frame not running after update")
	}
	// The new binary is the one on disk under current.
	shaNew, _ := SHA256File(h.layout.BinaryPath(SlotB))
	shaActive, _ := h.layout.ActiveBinarySHA256()
	if shaNew != shaActive {
		t.Fatal("current does not point at the new artifact")
	}
}

// 2. Invalid signature (unknown signer) is rejected; frame untouched.
func TestE2ERejectsUnknownSigner(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.startFrame(t)
	// A correctly-shaped release signed by a key that is not pinned.
	artifact := []byte(scriptHealthy)
	forged := Release{
		Version: "1.1.0", Channel: ChannelStable, OS: "linux", Arch: "arm64",
		ArtifactSHA256: ArtifactSHA256Hex(artifact), ArtifactSize: int64(len(artifact)),
		MinProtocolVersion: 1, MinSchemaVersion: 3,
		ReleasedAt: h.now.Add(-time.Hour), ExpiresAt: h.now.Add(72 * time.Hour),
		RolloutPercent: 100,
	}
	_, priv, _ := genKey()
	signed, err := SignRelease(forged, "key-evil", priv)
	if err != nil {
		t.Fatal(err)
	}
	h.srv.latest[ChannelStable] = signed

	res := h.run(t, PolicyAutomatic, alwaysWindow, false)
	if res.Action != ResultRejected {
		t.Fatalf("action = %q, want rejected", res.Action)
	}
	if !errors.Is(res.Err, ErrUnknownSigner) {
		t.Fatalf("err = %v, want ErrUnknownSigner", res.Err)
	}
	if got := h.activeSlot(t); got != SlotA {
		t.Fatalf("active slot changed to %s", got)
	}
	if !h.running() {
		t.Fatal("frame must keep running after a rejected release")
	}
	if h.state().Phase != PhaseIdle {
		t.Fatalf("phase = %s", h.state().Phase)
	}
}

// 2b. Valid key, corrupted signature bytes.
func TestE2ERejectsBadSignature(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.serveRelease("1.1.0", scriptHealthy, nil)
	rel := h.srv.latest[ChannelStable]
	rel.Signature = rel.Signature[:len(rel.Signature)-4] + "AAAA"
	h.srv.latest[ChannelStable] = rel

	res := h.run(t, PolicyAutomatic, alwaysWindow, false)
	if !errors.Is(res.Err, ErrBadSignature) {
		t.Fatalf("err = %v, want ErrBadSignature", res.Err)
	}
	if h.activeSlot(t) != SlotA {
		t.Fatal("slot changed despite bad signature")
	}
}

// 3. Expired metadata is rejected.
func TestE2ERejectsExpired(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.serveRelease("1.1.0", scriptHealthy, func(r *Release) {
		r.ExpiresAt = h.now.Add(-time.Minute)
		r.ReleasedAt = h.now.Add(-48 * time.Hour)
	})
	res := h.run(t, PolicyAutomatic, alwaysWindow, false)
	if !errors.Is(res.Err, ErrExpired) {
		t.Fatalf("err = %v, want ErrExpired", res.Err)
	}
	if h.activeSlot(t) != SlotA {
		t.Fatal("slot changed despite expired metadata")
	}
}

// 4. Corrupted artifact (served bytes mismatch signed digest).
func TestE2ERejectsCorruptArtifact(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.startFrame(t)
	h.serveRelease("1.1.0", scriptHealthy, nil)
	h.srv.corrupt = true

	res := h.run(t, PolicyAutomatic, alwaysWindow, false)
	if res.Action != ResultFailed {
		t.Fatalf("action = %q, want failed", res.Action)
	}
	if !errors.Is(res.Err, ErrArtifactHash) && !errors.Is(res.Err, ErrArtifactSize) {
		t.Fatalf("err = %v, want artifact integrity error", res.Err)
	}
	if h.activeSlot(t) != SlotA {
		t.Fatal("slot changed despite corrupt artifact")
	}
	if !h.running() {
		t.Fatal("frame must keep running")
	}
	if h.state().Phase != PhaseIdle {
		t.Fatal("state must return to idle")
	}
}

// 5. Interrupted download (truncated stream) → safe failure.
func TestE2EInterruptedDownload(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.startFrame(t)
	h.serveRelease("1.1.0", scriptHealthy, nil)
	h.srv.truncated = true

	res := h.run(t, PolicyAutomatic, alwaysWindow, false)
	if res.Action != ResultFailed {
		t.Fatalf("action = %q, want failed", res.Action)
	}
	if h.activeSlot(t) != SlotA {
		t.Fatal("slot changed despite interrupted download")
	}
	if !h.running() {
		t.Fatal("frame must keep running")
	}
	// Recovery: next run with a healthy server updates normally.
	h.srv.truncated = false
	res = h.run(t, PolicyAutomatic, alwaysWindow, false)
	if res.Action != ResultUpdated {
		t.Fatalf("recovery action = %q, want updated", res.Action)
	}
}

// 6. Power failure before activation: a staged (downloaded +
// verified, installed to inactive slot, not activated) candidate is
// resumed and completed by a fresh coordinator.
func TestE2EPowerLossBeforeActivation(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.startFrame(t)
	h.serveRelease("1.1.0", scriptHealthy, nil)

	// Stage with download_only (exactly the "downloaded but not
	// activated" state a power loss would leave).
	res := h.run(t, PolicyDownloadOnly, alwaysWindow, false)
	if res.Action != ResultStaged {
		t.Fatalf("stage action = %q (err=%v), want staged", res.Action, res.Err)
	}
	if h.activeSlot(t) != SlotA {
		t.Fatal("staging must not activate")
	}
	if !h.running() {
		t.Fatal("frame must keep running after staging")
	}

	// Power loss: coordinator vanishes mid-cycle. Fresh coordinator,
	// policy now automatic → reconcile must resume and finish.
	h.coord = h.freshCoordinator()
	res = h.run(t, PolicyAutomatic, alwaysWindow, false)
	if res.Action != ResultUpdated {
		t.Fatalf("resume action = %q (err=%v), want updated", res.Action, res.Err)
	}
	if h.activeSlot(t) != SlotB {
		t.Fatal("active slot = a, want b after resumed update")
	}
	if s := h.state(); s.LastGoodSlot != SlotB || s.ActiveVersion != "1.1.0" {
		t.Fatalf("state = %+v", s)
	}
}

// 7a. Power failure after activation, candidate healthy: a fresh
// coordinator commits it.
func TestE2EPowerLossAfterActivationHealthy(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.serveRelease("1.1.0", scriptHealthy, nil)

	// Simulate the post-swap, pre-commit crash: candidate installed,
	// activated, service running on it, state stuck at validating.
	if err := h.layout.InstallBinary(SlotB, strings.NewReader(scriptHealthy), int64(len(scriptHealthy))); err != nil {
		t.Fatal(err)
	}
	s := h.state()
	s.CandidateSlot = SlotB
	s.TargetVersion = "1.1.0"
	s.TargetSHA256 = ArtifactSHA256Hex([]byte(scriptHealthy))
	s.Channel = ChannelStable
	s.Phase = PhaseValidating
	if err := s.Save(h.stateDir); err != nil {
		t.Fatal(err)
	}
	if err := h.layout.Activate(SlotB); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.WaitReady(context.Background(), 3*time.Second); err != nil {
		t.Fatal(err)
	}

	h.coord = h.freshCoordinator()
	res := h.run(t, PolicyAutomatic, alwaysWindow, false)
	if res.Action != ResultReconciled && res.Action != ResultAlreadyCurrent {
		t.Fatalf("action = %q (err=%v), want reconciled/already-current", res.Action, res.Err)
	}
	st := h.state()
	if st.Phase != PhaseIdle || st.LastGoodSlot != SlotB || st.ActiveVersion != "1.1.0" {
		t.Fatalf("state = %+v", st)
	}
	if !h.running() {
		t.Fatal("frame must be running on the committed candidate")
	}
}

// 7b. Power failure after activation, candidate unhealthy: a fresh
// coordinator rolls back offline.
func TestE2EPowerLossAfterActivationUnhealthy(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.serveRelease("1.1.0", scriptNeverReady, nil)

	if err := h.layout.InstallBinary(SlotB, strings.NewReader(scriptNeverReady), int64(len(scriptNeverReady))); err != nil {
		t.Fatal(err)
	}
	s := h.state()
	s.CandidateSlot = SlotB
	s.TargetVersion = "1.1.0"
	s.TargetSHA256 = ArtifactSHA256Hex([]byte(scriptNeverReady))
	s.Channel = ChannelStable
	s.Phase = PhaseValidating
	if err := s.Save(h.stateDir); err != nil {
		t.Fatal(err)
	}
	if err := h.layout.Activate(SlotB); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	h.coord = h.freshCoordinator()
	res := h.run(t, PolicyAutomatic, alwaysWindow, false)
	if res.Action != ResultRollback {
		t.Fatalf("action = %q (err=%v), want rollback", res.Action, res.Err)
	}
	if h.activeSlot(t) != SlotA {
		t.Fatal("rollback did not restore slot a")
	}
	st := h.state()
	if st.Phase != PhaseIdle || st.LastGoodSlot != SlotA || st.ActiveVersion != "1.0.0" {
		t.Fatalf("state = %+v", st)
	}
	if st.LastError == "" {
		t.Fatal("rollback must record the failure reason")
	}
	if st.LastFailed.Version != "1.1.0" {
		t.Fatalf("failed target must be recorded for cooldown: %+v", st.LastFailed)
	}
	if !h.running() {
		t.Fatal("frame must be running on the rolled-back binary")
	}
}

// 8. Frame cannot start after update (crashes immediately) → automatic
// rollback within the same run.
func TestE2ERollbackOnFailedStartup(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.startFrame(t)
	h.serveRelease("1.1.0", scriptCrash, nil)

	res := h.run(t, PolicyAutomatic, alwaysWindow, false)
	if res.Action != ResultRollback {
		t.Fatalf("action = %q (err=%v), want rollback", res.Action, res.Err)
	}
	if h.activeSlot(t) != SlotA {
		t.Fatal("rollback did not restore slot a")
	}
	if !h.running() {
		t.Fatal("frame must be running after rollback")
	}
	st := h.state()
	if st.Phase != PhaseIdle || st.LastGoodSlot != SlotA || st.ActiveVersion != "1.0.0" {
		t.Fatalf("state = %+v", st)
	}
	if st.LastError == "" {
		t.Fatal("failure must be recorded")
	}
}

// 8b. Health check fails (process up, never ready) → automatic rollback.
func TestE2ERollbackOnFailedHealth(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.startFrame(t)
	h.serveRelease("1.1.0", scriptNeverReady, nil)

	res := h.run(t, PolicyAutomatic, alwaysWindow, false)
	if res.Action != ResultRollback {
		t.Fatalf("action = %q (err=%v), want rollback", res.Action, res.Err)
	}
	if h.activeSlot(t) != SlotA {
		t.Fatal("rollback did not restore slot a")
	}
	if !h.running() {
		t.Fatal("frame must be running after rollback")
	}
}

// 9. Incompatible schema: the update is rejected before download.
func TestE2ERejectsIncompatibleSchema(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.startFrame(t)
	h.serveRelease("1.1.0", scriptHealthy, func(r *Release) { r.MinSchemaVersion = 4 })

	res := h.run(t, PolicyAutomatic, alwaysWindow, false)
	if !errors.Is(res.Err, ErrSchemaIncompat) {
		t.Fatalf("err = %v, want ErrSchemaIncompat", res.Err)
	}
	if h.srv.artifactHits != 0 {
		t.Fatal("artifact must not be downloaded for an incompatible release")
	}
	if h.activeSlot(t) != SlotA {
		t.Fatal("slot changed despite schema incompatibility")
	}
}

// 9b. Incompatible protocol: same guarantee.
func TestE2ERejectsIncompatibleProtocol(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.serveRelease("1.1.0", scriptHealthy, func(r *Release) { r.MinProtocolVersion = 2 })

	res := h.run(t, PolicyAutomatic, alwaysWindow, false)
	if !errors.Is(res.Err, ErrProtocolIncompat) {
		t.Fatalf("err = %v, want ErrProtocolIncompat", res.Err)
	}
	if h.srv.artifactHits != 0 {
		t.Fatal("artifact must not be downloaded")
	}
}

// 10. Interrupted rollback: a rollback that was cut off (state stuck in
// the rollback phase, candidate still active) is finished at the next
// run.
func TestE2EInterruptedRollback(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.serveRelease("1.1.0", scriptNeverReady, nil)

	if err := h.layout.InstallBinary(SlotB, strings.NewReader(scriptNeverReady), int64(len(scriptNeverReady))); err != nil {
		t.Fatal(err)
	}
	s := h.state()
	s.CandidateSlot = SlotB
	s.TargetVersion = "1.1.0"
	s.TargetSHA256 = ArtifactSHA256Hex([]byte(scriptNeverReady))
	s.Channel = ChannelStable
	s.Phase = PhaseRollback // cut off mid-rollback
	if err := s.Save(h.stateDir); err != nil {
		t.Fatal(err)
	}
	if err := h.layout.Activate(SlotB); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	h.coord = h.freshCoordinator()
	res := h.run(t, PolicyAutomatic, alwaysWindow, false)
	if res.Action != ResultRollback {
		t.Fatalf("action = %q (err=%v), want rollback", res.Action, res.Err)
	}
	if h.activeSlot(t) != SlotA {
		t.Fatal("rollback did not complete")
	}
	if !h.running() {
		t.Fatal("frame must be running on the rolled-back binary")
	}
}

// 11. Insufficient disk space: rejected before download, frame intact.
func TestE2ERejectsInsufficientDisk(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.startFrame(t)
	h.serveRelease("1.1.0", scriptHealthy, nil)
	h.coord.SpaceCheck = func(dir string, required int64) error {
		return fmt.Errorf("%w: simulated full disk", ErrInsufficientDisk)
	}

	res := h.run(t, PolicyAutomatic, alwaysWindow, false)
	if !errors.Is(res.Err, ErrInsufficientDisk) {
		t.Fatalf("err = %v, want ErrInsufficientDisk", res.Err)
	}
	if h.srv.artifactHits != 0 {
		t.Fatal("artifact must not be downloaded without space")
	}
	if !h.running() {
		t.Fatal("frame must keep running")
	}
}

// 12. Release server unavailable: reported, retriable, frame unaffected.
func TestE2EReleaseServerUnavailable(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.startFrame(t)
	h.serveRelease("1.1.0", scriptHealthy, nil)
	h.srv.down = true

	res := h.run(t, PolicyAutomatic, alwaysWindow, false)
	if res.Action != ResultNothing {
		t.Fatalf("action = %q, want nothing", res.Action)
	}
	if res.Err == nil {
		t.Fatal("the failure must be reported (for logs/admin visibility)")
	}
	if !h.running() {
		t.Fatal("frame must keep running")
	}
	if h.activeSlot(t) != SlotA {
		t.Fatal("slot changed")
	}
	// Recovers when the server is back.
	h.srv.down = false
	res = h.run(t, PolicyAutomatic, alwaysWindow, false)
	if res.Action != ResultUpdated {
		t.Fatalf("recovery action = %q, want updated", res.Action)
	}
}

// 13. Downgrade rejected (anti-rollback); signed rollback release
// allowed.
func TestE2EAntiRollbackAndSignedRollback(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.startFrame(t)

	// Update to 1.2.0 first (raises the floor).
	h.serveRelease("1.2.0", scriptHealthy, nil)
	if res := h.run(t, PolicyAutomatic, alwaysWindow, false); res.Action != ResultUpdated {
		t.Fatalf("setup update: %q (%v)", res.Action, res.Err)
	}
	afterSetup := h.activeSlot(t)

	// An older, correctly-signed release is refused.
	h.serveRelease("1.1.0", scriptHealthy, nil)
	res := h.run(t, PolicyAutomatic, alwaysWindow, false)
	if !errors.Is(res.Err, ErrDowngrade) {
		t.Fatalf("err = %v, want ErrDowngrade", res.Err)
	}
	if got := h.activeSlot(t); got != afterSetup {
		t.Fatalf("slot changed by a downgrade: %s -> %s", afterSetup, got)
	}

	// A signed *rollback* release to the older version is honoured.
	h.serveRelease("1.1.0", scriptHealthy, func(r *Release) { r.IsRollback = true })
	res = h.run(t, PolicyAutomatic, alwaysWindow, false)
	if res.Action != ResultUpdated {
		t.Fatalf("rollback update: %q (%v)", res.Action, res.Err)
	}
	if h.state().ActiveVersion != "1.1.0" {
		t.Fatalf("active version = %s, want 1.1.0", h.state().ActiveVersion)
	}
}

// 14. Repeated boot failures: when even the rollback target will not
// become ready, boot attempts are counted and the device ends in a
// terminal failed state with a safe (known) slot active — no infinite
// flap.
func TestE2ERepeatedBootFailures(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()

	// Slot A (last-good) is ALSO broken: never-ready.
	if err := h.layout.InstallAnySlot(SlotA, strings.NewReader(scriptNeverReady), int64(len(scriptNeverReady))); err != nil {
		t.Fatal(err)
	}
	// Candidate 1.1.0 crashes.
	h.serveRelease("1.1.0", scriptCrash, nil)

	ctx := context.Background()
	for i := 0; i < MaxBootAttempts; i++ {
		h.coord = h.freshCoordinator()
		res, err := h.coord.Run(ctx, ChannelStable, PolicyAutomatic, alwaysWindow, false)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if res.Action != ResultRollback {
			t.Fatalf("run %d action = %q (err=%v), want rollback", i, res.Action, res.Err)
		}
		if got := h.state().BootAttempts; got != i+1 {
			t.Fatalf("run %d boot attempts = %d, want %d", i, got, i+1)
		}
	}
	// Terminal: phase failed, slot A still active (the known binary,
	// even though unhealthy — nothing safer exists), no further flap.
	st := h.state()
	if st.Phase != PhaseFailed {
		t.Fatalf("phase = %s, want failed", st.Phase)
	}
	if h.activeSlot(t) != SlotA {
		t.Fatal("active slot must remain the last-known slot")
	}
	// Next boot does not change the terminal state.
	h.coord = h.freshCoordinator()
	res, err := h.coord.Run(ctx, ChannelStable, PolicyAutomatic, alwaysWindow, false)
	if err != nil {
		t.Fatal(err)
	}
	if h.state().BootAttempts != MaxBootAttempts {
		t.Fatalf("boot attempts advanced past terminal: %d", h.state().BootAttempts)
	}
	if h.activeSlot(t) != SlotA {
		t.Fatal("terminal state must not flap")
	}
	if h.state().UpdateStatusForHealth() != "update_failed" {
		t.Fatalf("health status = %q, want update_failed", h.state().UpdateStatusForHealth())
	}
	_ = res
}

// 15. Maintenance window: outside the window nothing happens (not even
// a metadata fetch); inside, the update proceeds.
func TestE2EMaintenanceWindow(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.startFrame(t)
	h.serveRelease("1.1.0", scriptHealthy, nil)

	// 12:00 is outside 02:00–05:00.
	off := &Window{Start: "02:00", End: "05:00", TZ: "UTC"}
	res := h.run(t, PolicyAutomatic, off, false)
	if res.Action != ResultSkippedWindow {
		t.Fatalf("action = %q, want skipped-window", res.Action)
	}
	if h.srv.metadataHits != 0 {
		t.Fatal("must not fetch metadata outside the window")
	}

	// 03:30 is inside.
	h.coord.Now = func() time.Time { return time.Date(2026, 11, 13, 3, 30, 0, 0, time.UTC) }
	res = h.run(t, PolicyAutomatic, off, false)
	if res.Action != ResultUpdated {
		t.Fatalf("action = %q (err=%v), want updated", res.Action, res.Err)
	}

	// force overrides the window.
	h.serveRelease("1.2.0", scriptHealthy, nil)
	h.coord.Now = func() time.Time { return time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC) }
	res = h.run(t, PolicyAutomatic, off, true)
	if res.Action != ResultUpdated {
		t.Fatalf("forced action = %q, want updated", res.Action)
	}
}

// 16. Staged rollout: a device outside the rollout is skipped silently;
// the same release with the device allow-listed is installed.
func TestE2ERolloutEligibility(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.startFrame(t)

	// 0% and not allow-listed → nothing, no download.
	h.serveRelease("1.1.0", scriptHealthy, func(r *Release) { r.RolloutPercent = 0 })
	res := h.run(t, PolicyAutomatic, alwaysWindow, false)
	if res.Action != ResultNothing {
		t.Fatalf("action = %q, want nothing", res.Action)
	}
	if h.srv.artifactHits != 0 {
		t.Fatal("must not download for a non-eligible device")
	}

	// Allow-listed → installed.
	h.serveRelease("1.1.0", scriptHealthy, func(r *Release) {
		r.RolloutPercent = 0
		r.RolloutDevices = []string{"test-device-1"}
	})
	res = h.run(t, PolicyAutomatic, alwaysWindow, false)
	if res.Action != ResultUpdated {
		t.Fatalf("action = %q, want updated", res.Action)
	}
}

// 17. Key rotation: a release signed by the pinned key may add a new
// key; a later release signed by the rotated-in key verifies.
func TestE2EKeyRotation(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.startFrame(t)

	// A fresh key (key-c) that is NOT in the initial trust store.
	pubC, privC, err := genKey()
	if err != nil {
		t.Fatal(err)
	}
	b64C := base64.StdEncoding.EncodeToString(pubC)

	// 1.1.0 signed by key-a, rotates in key-c.
	h.serveRelease("1.1.0", scriptHealthy, func(r *Release) {
		r.NewKeys = []NewKey{{KeyID: "key-c", Ed25519: b64C}}
	})
	res := h.run(t, PolicyAutomatic, alwaysWindow, false)
	if res.Action != ResultUpdated {
		t.Fatalf("rotation update: %q (%v)", res.Action, res.Err)
	}
	// The trust file now contains key-c.
	tb, err := os.ReadFile(h.layout.TrustPath())
	if err != nil {
		t.Fatal(err)
	}
	var store TrustedKeys
	if err := json.Unmarshal(tb, &store); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Keys["key-c"]; !ok {
		t.Fatalf("rotated key not persisted: %s", tb)
	}

	// 1.2.0 signed by the rotated-in key-c.
	artifact := []byte(scriptHealthy)
	rel := Release{
		Version: "1.2.0", Channel: ChannelStable, OS: "linux", Arch: "arm64",
		ArtifactSHA256: ArtifactSHA256Hex(artifact), ArtifactSize: int64(len(artifact)),
		MinProtocolVersion: 1, MinSchemaVersion: 3,
		ReleasedAt: h.now.Add(-time.Hour), ExpiresAt: h.now.Add(72 * time.Hour),
		RolloutPercent: 100,
	}
	signed, err := SignRelease(rel, "key-c", privC)
	if err != nil {
		t.Fatal(err)
	}
	h.srv.latest[ChannelStable] = signed
	h.srv.artifacts["1.2.0"] = artifact

	res = h.run(t, PolicyAutomatic, alwaysWindow, false)
	if res.Action != ResultUpdated {
		t.Fatalf("post-rotation update: %q (%v)", res.Action, res.Err)
	}
}

// 18. Manual policy: check only, never install — even when eligible.
func TestE2EManualPolicy(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.startFrame(t)
	h.serveRelease("1.1.0", scriptHealthy, nil)

	res := h.run(t, PolicyManual, alwaysWindow, false)
	if res.Action != ResultManualCheckOnly {
		t.Fatalf("action = %q, want manual-check-only", res.Action)
	}
	if res.Release == nil || res.Release.Version != "1.1.0" {
		t.Fatalf("manual check must report the available release: %+v", res)
	}
	if h.activeSlot(t) != SlotA {
		t.Fatal("manual policy must not install")
	}
	if h.srv.artifactHits != 0 {
		t.Fatal("manual policy must not download")
	}

	// Forced run under manual policy does install.
	res = h.run(t, PolicyManual, alwaysWindow, true)
	if res.Action != ResultUpdated {
		t.Fatalf("forced action = %q, want updated", res.Action)
	}
}

// 19. The photo cache (sibling data of the updates dir) is never
// touched by update or rollback.
func TestE2EPhotoCacheUntouched(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.startFrame(t)
	cacheDir := filepath.Join(h.root, "frame-cache")
	if err := os.MkdirAll(filepath.Join(cacheDir, "photos"), 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(cacheDir, "photos", "123.jpg")
	if err := os.WriteFile(sentinel, []byte("photo-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A failing update (crash candidate) that rolls back.
	h.serveRelease("1.1.0", scriptCrash, nil)
	if res := h.run(t, PolicyAutomatic, alwaysWindow, false); res.Action != ResultRollback {
		t.Fatalf("action = %q, want rollback", res.Action)
	}
	b, err := os.ReadFile(sentinel)
	if err != nil || string(b) != "photo-bytes" {
		t.Fatalf("photo cache was touched by update/rollback: %v %q", err, b)
	}
}

// 20. download_only stages into the inactive slot without touching the
// running frame.
func TestE2EDownloadOnlyStages(t *testing.T) {
	h := newHarness(t)
	defer h.stopAll()
	h.startFrame(t)
	h.serveRelease("1.1.0", scriptHealthy, nil)

	res := h.run(t, PolicyDownloadOnly, alwaysWindow, false)
	if res.Action != ResultStaged {
		t.Fatalf("action = %q (err=%v), want staged", res.Action, res.Err)
	}
	if h.activeSlot(t) != SlotA {
		t.Fatal("staging must not activate")
	}
	if _, err := os.Stat(h.layout.BinaryPath(SlotB)); err != nil {
		t.Fatalf("staged artifact missing in inactive slot: %v", err)
	}
	if !h.running() {
		t.Fatal("frame must keep running")
	}
}

// helpers -----------------------------------------------------------------

// freshCoordinator builds a coordinator for the same layout/state,
// simulating a process restart (power loss). The service handle is
// shared: in production the service manager (systemctl) is a
// system-global view a restarted updater can still query, which is
// what the shared handle models.
func (h *harness) freshCoordinator() *Coordinator {
	return &Coordinator{
		Layout:           h.layout,
		StateDir:         h.stateDir,
		TrustPath:        h.layout.TrustPath(),
		Service:          h.svc,
		FetchRelease:     h.srv.fetchRelease,
		DownloadArtifact: h.srv.downloadArtifact,
		Now:              func() time.Time { return h.now },
		OS:               "linux",
		Arch:             "arm64",
		SchemaVersion:    3,
		MaxProtocol:      1,
		DeviceID:         "test-device-1",
		ValidateTimeout:  900 * time.Millisecond,
		Log:              logf(h.t),
	}
}

// logf returns a logger that writes to test output.
func logf(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
