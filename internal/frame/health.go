package frame

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/matthewgall/openframe/internal/platform"
	"github.com/matthewgall/openframe/internal/updater"
	"github.com/matthewgall/openframe/internal/version"
)

// Health reporting (M5, spec §13). The heartbeat doubles as the
// health reporter: every 30s the frame posts a summary of itself and
// learns the server's remote-sync request sequence (how an admin's
// "trigger synchronisation" reaches the frame — no inbound
// connections, spec §12).
//
// The payload is summaries only: no PII, no tokens, no local paths
// (spec §13 "Do not collect unnecessary personal information").

const (
	// maxRecentErrors bounds the error summary (mirrors the server's).
	maxRecentErrors = 10
	maxErrorLen     = 200
)

// healthPayload mirrors the server's devices.HealthReport wire shape
// (defined here so the frame does not import the server packages).
type healthPayload struct {
	Version          string   `json:"version"`
	UptimeSeconds    int64    `json:"uptime_seconds"`
	LastSyncAt       string   `json:"last_sync_at"`
	LastSyncOK       bool     `json:"last_sync_ok"`
	ManifestRevision int64    `json:"manifest_revision"`
	PendingDownloads int      `json:"pending_downloads"`
	StorageTotal     int64    `json:"storage_total"`
	StorageUsed      int64    `json:"storage_used"`
	CacheBytes       int64    `json:"cache_bytes"`
	MemTotalMB       int64    `json:"mem_total_mb"`
	MemUsedMB        int64    `json:"mem_used_mb"`
	CPUTempC         *float64 `json:"cpu_temp_c"`
	DisplayDriver    string   `json:"display_driver"`
	DisplayStatus    string   `json:"display_status"`
	ErrorCount24h    int      `json:"error_count_24h"`
	RecentErrors     []string `json:"recent_errors"`
	UpdateState      string   `json:"update_state"`
}

// errorRing is a bounded, mutex-guarded ring of recent error summaries.
type errorRing struct {
	mu   sync.Mutex
	refs []string
}

func (r *errorRing) Record(summary string) {
	if summary == "" {
		return
	}
	if len(summary) > maxErrorLen {
		summary = summary[:maxErrorLen]
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refs = append(r.refs, summary)
	if len(r.refs) > maxRecentErrors {
		r.refs = r.refs[len(r.refs)-maxRecentErrors:]
	}
}

func (r *errorRing) Snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.refs))
	copy(out, r.refs)
	return out
}

// RecordError captures a short error summary for the health report.
// Callers pass terse, secret-free strings ("sync: ...", "display: ...").
func (f *Frame) RecordError(summary string) { f.errors.Record(summary) }

// buildHealth assembles the current health payload.
func (f *Frame) buildHealth(ctx context.Context) healthPayload {
	h := healthPayload{
		Version:       version.Version,
		UptimeSeconds: int64(time.Since(f.started).Seconds()),
		UpdateState:   f.updateStatus(),
	}

	if f.engine != nil {
		if t, ok := f.engine.LastAttempt(ctx); ok {
			h.LastSyncAt = t.UTC().Format(time.RFC3339)
		}
		h.LastSyncOK = f.engine.LastError(ctx) == ""
		h.ManifestRevision = f.engine.ActiveRevision(ctx)
		h.PendingDownloads = f.engine.PendingDownloads(ctx)
	}

	eff := f.effectiveConfig()
	_, frameDir, _ := eff.Paths()
	if d, err := platform.Disk(frameDir); err == nil {
		h.StorageTotal = d.TotalBytes
		h.StorageUsed = d.TotalBytes - d.FreeBytes
	}
	if m := platform.Memory(); m.TotalBytes > 0 {
		h.MemTotalMB = m.TotalBytes / (1024 * 1024)
		h.MemUsedMB = (m.TotalBytes - m.Available) / (1024 * 1024)
	}
	if f.ui != nil {
		if c, err := f.ui.mon.MaxTempC(); err == nil {
			h.CPUTempC = &c
		}
	}
	if f.drv != nil {
		h.DisplayDriver = f.drv.Name()
		if f.ui != nil && f.ui.asleep {
			h.DisplayStatus = "asleep"
		} else {
			h.DisplayStatus = "active"
		}
	}
	if f.cache != nil {
		if n, err := f.cache.TotalSize(); err == nil {
			h.CacheBytes = n
		}
	}
	h.RecentErrors = f.errors.Snapshot()
	if h.RecentErrors == nil {
		h.RecentErrors = []string{}
	}
	h.ErrorCount24h = len(h.RecentErrors)
	return h
}

// updateStatus reads the updater's state file (M6). The frame only
// reads it (unprivileged); the root openframe-updater writes it. A
// missing file means no updater state is visible to this frame (dev
// headless runs) and reports "none".
func (f *Frame) updateStatus() string {
	eff := f.effectiveConfig()
	_, _, updatesDir := eff.Paths()
	b, err := os.ReadFile(filepath.Join(updatesDir, updater.StateFileName))
	if err != nil {
		return "none"
	}
	var st updater.State
	if err := json.Unmarshal(b, &st); err != nil {
		return "unknown"
	}
	return st.UpdateStatusForHealth()
}

// heartbeatComponent is the frame's 30s pulse (M1) upgraded to the
// M5 health reporter + remote-sync listener. It also writes the local
// last_heartbeat state that `openframe status` displays.
func (f *Frame) heartbeatComponent(ctx context.Context) error {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	lastSyncSeq := f.loadSyncSeq(ctx)
	lastErrSeen := ""
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := setState(ctx, f.db, StateKeyLastHeartbeat,
				time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				f.log.Warn("heartbeat state write failed", "error", err)
			}
			if !f.engine.Paired() {
				continue // the pairing component drives the unpaired flow
			}

			// Surface sync/display errors into the health summary
			// (recorded once per change, not once per tick).
			if e := f.engine.LastError(ctx); e != "" && e != lastErrSeen {
				lastErrSeen = e
				f.errors.Record("sync: " + e)
			}
			if f.ui != nil {
				if e := f.ui.eng.LastPresentError(); e != "" && e != f.lastPresentErr {
					f.lastPresentErr = e
					f.errors.Record("display: " + e)
				}
			}

			h := f.buildHealth(ctx)
			payload, err := json.Marshal(&h)
			if err != nil {
				f.log.Warn("health marshal failed", "error", err)
				continue
			}
			seq, err := f.engine.ReportHealth(ctx, payload)
			if err != nil {
				f.log.Debug("health report failed", "error", err)
				continue
			}
			if seq > lastSyncSeq {
				lastSyncSeq = seq
				f.saveSyncSeq(ctx, seq)
				f.log.Info("remote sync requested by administrator", "seq", seq)
				f.engine.Check(ctx)
			}
		}
	}
}

// syncSeq persistence: the sequence survives restarts, so an admin
// trigger seen before a restart is not re-fired (and a trigger that
// happened during downtime is picked up after it).
const stateKeySyncSeq = "sync_request_seq"

func (f *Frame) loadSyncSeq(ctx context.Context) int64 {
	v, _, _ := getState(ctx, f.db, stateKeySyncSeq)
	var n int64
	fmt.Sscanf(v, "%d", &n)
	return n
}

func (f *Frame) saveSyncSeq(ctx context.Context, seq int64) {
	setState(ctx, f.db, stateKeySyncSeq, fmt.Sprint(seq))
}
