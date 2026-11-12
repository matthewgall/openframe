// Package audit records administrative and security-relevant actions
// (spec §5: "Audit logging for important administrative actions").
//
// Detail is a JSON object and must never contain passwords, tokens or
// other secrets (threat model T-14). Audit writes are best-effort with
// respect to the surrounding request: a failed audit write is logged to
// the server log but does not fail the action (availability of
// security-critical actions like login must not depend on a secondary
// table write).
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"time"
)

// Recorder writes audit entries.
type Recorder struct {
	dbh  *sql.DB
	log  *slog.Logger
	nowf func() time.Time
}

func NewRecorder(dbh *sql.DB, log *slog.Logger) *Recorder {
	return &Recorder{dbh: dbh, log: log, nowf: time.Now}
}

// SetNowFunc is a test seam.
func (r *Recorder) SetNowFunc(f func() time.Time) { r.nowf = f }

// Action records an event. actorUserID may be "" for system actions.
// detail may be nil.
func (r *Recorder) Action(ctx context.Context, actorUserID, action, targetType, targetID, ip string, detail any) {
	detailJSON := "{}"
	if detail != nil {
		if b, err := json.Marshal(detail); err == nil {
			detailJSON = string(b)
		}
	}
	if r.dbh == nil {
		// Audit must never take down the request path.
		return
	}
	_, err := r.dbh.ExecContext(ctx,
		`INSERT INTO audit_log (actor_user_id, action, target_type, target_id, detail, ip, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		actorUserID, action, targetType, targetID, detailJSON, ip,
		r.nowf().UTC().Format(time.RFC3339Nano))
	if err != nil {
		r.log.Error("audit write failed", "action", action, "error", err)
	}
}

// Entry is one audit log row.
type Entry struct {
	ID          int64  `json:"id"`
	ActorUserID string `json:"actor_user_id,omitempty"`
	ActorName   string `json:"actor_name,omitempty"`
	Action      string `json:"action"`
	TargetType  string `json:"target_type,omitempty"`
	TargetID    string `json:"target_id,omitempty"`
	Detail      string `json:"detail"`
	IP          string `json:"ip,omitempty"`
	CreatedAt   string `json:"created_at"`
}

// List returns the most recent entries (newest first).
func (r *Recorder) List(ctx context.Context, limit int) ([]Entry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := r.dbh.QueryContext(ctx,
		`SELECT a.id, COALESCE(a.actor_user_id,''), COALESCE(u.display_name,''),
		        a.action, a.target_type, a.target_id, a.detail, a.ip, a.created_at
		   FROM audit_log a LEFT JOIN users u ON u.id = a.actor_user_id
		  ORDER BY a.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.ActorUserID, &e.ActorName, &e.Action,
			&e.TargetType, &e.TargetID, &e.Detail, &e.IP, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
