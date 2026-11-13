package devices

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// HealthReport is the frame's latest self-report (spec §13). It is
// summaries only — no PII, no tokens, no local paths.
type HealthReport struct {
	Version          string   `json:"version"`
	UptimeSeconds    int64    `json:"uptime_seconds"`
	LastSyncAt       string   `json:"last_sync_at"` // RFC3339 or ""
	LastSyncOK       bool     `json:"last_sync_ok"`
	ManifestRevision int64    `json:"manifest_revision"`
	PendingDownloads int      `json:"pending_downloads"`
	StorageTotal     int64    `json:"storage_total"` // bytes
	StorageUsed      int64    `json:"storage_used"`  // bytes
	CacheBytes       int64    `json:"cache_bytes"`
	MemTotalMB       int64    `json:"mem_total_mb"`
	MemUsedMB        int64    `json:"mem_used_mb"`
	CPUTempC         *float64 `json:"cpu_temp_c"` // nil when no sensor
	DisplayDriver    string   `json:"display_driver"`
	DisplayStatus    string   `json:"display_status"` // active|asleep|off|error
	ErrorCount24h    int      `json:"error_count_24h"`
	RecentErrors     []string `json:"recent_errors"` // short summaries
	UpdateState      string   `json:"update_state"`  // unknown|idle|downloading|installing|activating|verifying|failed|rolled_back
}

// Sanitize bounds untrusted frame input before storage.
func (h *HealthReport) Sanitize() {
	if len(h.Version) > 64 {
		h.Version = h.Version[:64]
	}
	if h.UptimeSeconds < 0 || h.UptimeSeconds > 1e9 {
		h.UptimeSeconds = 0
	}
	if len(h.LastSyncAt) > 40 {
		h.LastSyncAt = h.LastSyncAt[:40]
	}
	if h.ManifestRevision < 0 {
		h.ManifestRevision = 0
	}
	if h.PendingDownloads < 0 || h.PendingDownloads > 1e6 {
		h.PendingDownloads = 0
	}
	if h.StorageTotal < 0 {
		h.StorageTotal = 0
	}
	if h.StorageUsed < 0 {
		h.StorageUsed = 0
	}
	if h.CacheBytes < 0 {
		h.CacheBytes = 0
	}
	if h.MemTotalMB < 0 || h.MemTotalMB > 1e7 {
		h.MemTotalMB = 0
	}
	if h.MemUsedMB < 0 || h.MemUsedMB > 1e7 {
		h.MemUsedMB = 0
	}
	if h.CPUTempC != nil && (*h.CPUTempC < -50 || *h.CPUTempC > 150) {
		h.CPUTempC = nil
	}
	for _, f := range []*string{&h.DisplayDriver, &h.DisplayStatus, &h.UpdateState} {
		if len(*f) > 32 {
			*f = (*f)[:32]
		}
	}
	if h.ErrorCount24h < 0 || h.ErrorCount24h > 1e6 {
		h.ErrorCount24h = 0
	}
	// At most 10 recent errors, each at most 200 bytes: a summary,
	// not a log mirror.
	out := h.RecentErrors[:0]
	for _, e := range h.RecentErrors {
		if len(out) >= 10 {
			break
		}
		if len(e) > 200 {
			e = e[:200]
		}
		out = append(out, e)
	}
	h.RecentErrors = out
}

// DisplayJSON renders recent_errors for the API.
func (h *HealthReport) DisplayJSON() (string, error) {
	b, err := json.Marshal(h.RecentErrors)
	if err != nil {
		return "[]", nil
	}
	return string(b), nil
}

// DeviceStatus is the derived administrative state (spec §13: the UI
// must distinguish online / recently seen / offline / sync error /
// update in progress / update failed / rollback completed).
const (
	StatusOnline       = "online"
	StatusRecentlySeen = "recently_seen"
	StatusOffline      = "offline"
	StatusSyncError    = "sync_error"
	StatusUpdating     = "updating"
	StatusUpdateFailed = "update_failed"
	StatusRolledBack   = "rollback"
)

const (
	onlineWithin   = 90 * time.Second // 3+ missed heartbeats
	recentlyWithin = 24 * time.Hour
)

// HealthWithDevice joins a device with its latest report for the
// admin API.
type HealthWithDevice struct {
	Device
	LastHeartbeatAt *time.Time    `json:"last_heartbeat_at,omitempty"`
	SyncRequestSeq  int64         `json:"sync_request_seq"`
	Health          *HealthReport `json:"health,omitempty"`
	Status          string        `json:"status"`
}

// ReportHealth upserts the device's latest report and records
// heartbeat freshness.
func (s *Store) ReportHealth(ctx context.Context, deviceID string, h *HealthReport) error {
	now := time.Now().UTC().Format(time.RFC3339)
	errs, err := h.DisplayJSON()
	if err != nil {
		return err
	}
	var temp any
	if h.CPUTempC != nil {
		temp = *h.CPUTempC
	}
	_, err = s.dbh.ExecContext(ctx,
		`INSERT INTO device_health (
		 device_id, version, uptime_seconds, last_sync_at, last_sync_ok,
		 manifest_revision, pending_downloads, storage_total, storage_used,
		 cache_bytes, mem_total_mb, mem_used_mb, cpu_temp_c, display_driver,
		 display_status, error_count_24h, recent_errors, update_state, reported_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(device_id) DO UPDATE SET
		   version = excluded.version,
		   uptime_seconds = excluded.uptime_seconds,
		   last_sync_at = excluded.last_sync_at,
		   last_sync_ok = excluded.last_sync_ok,
		   manifest_revision = excluded.manifest_revision,
		   pending_downloads = excluded.pending_downloads,
		   storage_total = excluded.storage_total,
		   storage_used = excluded.storage_used,
		   cache_bytes = excluded.cache_bytes,
		   mem_total_mb = excluded.mem_total_mb,
		   mem_used_mb = excluded.mem_used_mb,
		   cpu_temp_c = excluded.cpu_temp_c,
		   display_driver = excluded.display_driver,
		   display_status = excluded.display_status,
		   error_count_24h = excluded.error_count_24h,
		   recent_errors = excluded.recent_errors,
		   update_state = excluded.update_state,
		   reported_at = excluded.reported_at`,
		deviceID, h.Version, h.UptimeSeconds, h.LastSyncAt, btoi(h.LastSyncOK),
		h.ManifestRevision, h.PendingDownloads, h.StorageTotal, h.StorageUsed,
		h.CacheBytes, h.MemTotalMB, h.MemUsedMB, temp, h.DisplayDriver,
		h.DisplayStatus, h.ErrorCount24h, errs, h.UpdateState, now)
	if err != nil {
		return err
	}
	_, err = s.dbh.ExecContext(ctx,
		`UPDATE devices SET last_heartbeat_at = ? WHERE id = ?`, now, deviceID)
	return err
}

// GetHealth returns the latest report for a device (nil when never
// reported).
func (s *Store) GetHealth(ctx context.Context, deviceID string) (*HealthReport, error) {
	var (
		h        HealthReport
		temp     sql.NullFloat64
		errsJSON string
		reported string
	)
	err := s.dbh.QueryRowContext(ctx,
		`SELECT version, uptime_seconds, last_sync_at, last_sync_ok,
		        manifest_revision, pending_downloads, storage_total, storage_used,
		        cache_bytes, mem_total_mb, mem_used_mb, cpu_temp_c, display_driver,
		        display_status, error_count_24h, recent_errors, update_state, reported_at
		   FROM device_health WHERE device_id = ?`, deviceID).Scan(
		&h.Version, &h.UptimeSeconds, &h.LastSyncAt, &h.LastSyncOK,
		&h.ManifestRevision, &h.PendingDownloads, &h.StorageTotal, &h.StorageUsed,
		&h.CacheBytes, &h.MemTotalMB, &h.MemUsedMB, &temp, &h.DisplayDriver,
		&h.DisplayStatus, &h.ErrorCount24h, &errsJSON, &h.UpdateState, &reported)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if temp.Valid {
		v := temp.Float64
		h.CPUTempC = &v
	}
	if errsJSON != "" {
		_ = json.Unmarshal([]byte(errsJSON), &h.RecentErrors)
	}
	return &h, nil
}

// LastHeartbeat returns the device's last heartbeat timestamp.
func (s *Store) LastHeartbeat(ctx context.Context, deviceID string) (*time.Time, error) {
	var v sql.NullString
	err := s.dbh.QueryRowContext(ctx,
		"SELECT last_heartbeat_at FROM devices WHERE id = ?", deviceID).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !v.Valid || v.String == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, v.String)
	if err != nil {
		return nil, nil
	}
	return &t, nil
}

// DeriveStatus computes the admin-visible device state (spec §13).
// A frame is only "online" when its heartbeat is fresh AND its last
// sync succeeded — reaching the server over HTTP is not health
// (spec §13 final paragraph).
func DeriveStatus(lastHeartbeat *time.Time, h *HealthReport, now time.Time) string {
	if h != nil {
		switch h.UpdateState {
		case "downloading", "installing", "activating", "verifying":
			return StatusUpdating
		case "failed":
			return StatusUpdateFailed
		case "rolled_back":
			return StatusRolledBack
		}
	}
	if lastHeartbeat == nil {
		return StatusOffline
	}
	age := now.Sub(*lastHeartbeat)
	if age <= onlineWithin {
		if h != nil && !h.LastSyncOK && h.LastSyncAt != "" {
			return StatusSyncError
		}
		return StatusOnline
	}
	if age <= recentlyWithin {
		return StatusRecentlySeen
	}
	return StatusOffline
}

// ListWithHealth returns every device joined with its latest report,
// heartbeat freshness and derived status (admin list view, spec §6/§13).
func (s *Store) ListWithHealth(ctx context.Context) ([]*HealthWithDevice, error) {
	rows, err := s.dbh.QueryContext(ctx,
		`SELECT d.id, d.name, d.created_at, d.revoked_at,
		        (SELECT COUNT(*) FROM album_devices ad WHERE ad.device_id = d.id),
		        (SELECT COUNT(*) FROM device_credentials c
		          WHERE c.device_id = d.id AND (c.revoked_at IS NULL OR c.revoked_at = '')) > 0,
		        d.sync_request_seq, d.last_heartbeat_at
		   FROM devices d ORDER BY d.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type row struct {
		Device
		Revoked   sql.NullString
		Created   string
		SyncSeq   int64
		Heartbeat sql.NullString
	}
	var raw []*row
	for rows.Next() {
		r := &row{}
		if err := rows.Scan(&r.ID, &r.Name, &r.Created, &r.Revoked, &r.AlbumCount,
			&r.HasCredential, &r.SyncSeq, &r.Heartbeat); err != nil {
			return nil, err
		}
		raw = append(raw, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	healths, err := s.allHealth(ctx)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	out := make([]*HealthWithDevice, 0, len(raw))
	for _, r := range raw {
		w := &HealthWithDevice{Device: r.Device, SyncRequestSeq: r.SyncSeq}
		w.CreatedAt = parseTS(r.Created)
		if r.Revoked.Valid && r.Revoked.String != "" {
			t := parseTS(r.Revoked.String)
			w.RevokedAt = &t
		}
		if r.Heartbeat.Valid && r.Heartbeat.String != "" {
			if t, err := time.Parse(time.RFC3339, r.Heartbeat.String); err == nil {
				w.LastHeartbeatAt = &t
			}
		}
		w.Health = healths[r.ID]
		w.Status = DeriveStatus(w.LastHeartbeatAt, w.Health, now)
		out = append(out, w)
	}
	return out, nil
}

// allHealth loads every device's latest report in one query.
func (s *Store) allHealth(ctx context.Context) (map[string]*HealthReport, error) {
	out := map[string]*HealthReport{}
	rows, err := s.dbh.QueryContext(ctx,
		`SELECT device_id, version, uptime_seconds, last_sync_at, last_sync_ok,
		        manifest_revision, pending_downloads, storage_total, storage_used,
		        cache_bytes, mem_total_mb, mem_used_mb, cpu_temp_c, display_driver,
		        display_status, error_count_24h, recent_errors, update_state
		   FROM device_health`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		h := &HealthReport{}
		var (
			id       string
			temp     sql.NullFloat64
			errsJSON string
			ok       int
		)
		if err := rows.Scan(&id, &h.Version, &h.UptimeSeconds, &h.LastSyncAt, &ok,
			&h.ManifestRevision, &h.PendingDownloads, &h.StorageTotal, &h.StorageUsed,
			&h.CacheBytes, &h.MemTotalMB, &h.MemUsedMB, &temp, &h.DisplayDriver,
			&h.DisplayStatus, &h.ErrorCount24h, &errsJSON, &h.UpdateState); err != nil {
			return nil, err
		}
		h.LastSyncOK = ok != 0
		if temp.Valid {
			v := temp.Float64
			h.CPUTempC = &v
		}
		if errsJSON != "" {
			_ = json.Unmarshal([]byte(errsJSON), &h.RecentErrors)
		}
		out[id] = h
	}
	return out, rows.Err()
}

// RequestSync bumps the device's remote-sync sequence number (spec §6
// "Trigger synchronisation"). The frame picks it up on its next
// heartbeat poll.
func (s *Store) RequestSync(ctx context.Context, deviceID string) (int64, error) {
	res, err := s.dbh.ExecContext(ctx,
		`UPDATE devices SET sync_request_seq = sync_request_seq + 1 WHERE id = ?`,
		deviceID)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotFound
	}
	var seq int64
	err = s.dbh.QueryRowContext(ctx,
		"SELECT sync_request_seq FROM devices WHERE id = ?", deviceID).Scan(&seq)
	return seq, err
}

// GetSyncRequestSeq returns the current remote-sync sequence.
func (s *Store) GetSyncRequestSeq(ctx context.Context, deviceID string) (int64, error) {
	var seq int64
	err := s.dbh.QueryRowContext(ctx,
		"SELECT sync_request_seq FROM devices WHERE id = ?", deviceID).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return seq, err
}
