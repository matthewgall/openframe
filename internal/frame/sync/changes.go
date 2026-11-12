package sync

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/matthewgall/openframe/internal/id"
)

// changesTick is the local-change upload cadence (spec §9: local
// changes upload is independently scheduled from manifest checks and
// downloads).
const changesTick = 5 * time.Minute

// SetFavourite records a local favourite change: it is immediately
// usable offline (favourites table) and queued for idempotent replay
// when the server is reachable (spec §5: "Favourites must remain
// usable offline", spec §9: "Use idempotent operations").
func (e *Engine) SetFavourite(ctx context.Context, photoHash string, value bool) error {
	if photoHash == "" {
		return fmt.Errorf("changes: empty photo hash")
	}
	now := time.Now().UTC()
	rfc := now.Format(time.RFC3339Nano)
	opID, err := id.NewUUID()
	if err != nil {
		return err
	}
	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	v := 0
	if value {
		v = 1
	}
	// Local state is authoritative immediately (offline-first).
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO favourites (photo_hash, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(photo_hash) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		photoHash, v, rfc); err != nil {
		return err
	}
	// Queue the op. An existing queued op for the same photo is
	// superseded: the latest local state is what the server must end
	// up with (conflict rule: last local write wins).
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM favourites_queue WHERE photo_hash = ?", photoHash); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO favourites_queue (op_id, photo_hash, value, ts, created_at) VALUES (?, ?, ?, ?, ?)",
		opID, photoHash, v, rfc, rfc); err != nil {
		return err
	}
	return tx.Commit()
}

// Favourite reports the local favourite state (usable offline).
func (e *Engine) Favourite(ctx context.Context, photoHash string) (bool, error) {
	var v int
	err := e.db.QueryRowContext(ctx,
		"SELECT value FROM favourites WHERE photo_hash = ?", photoHash).Scan(&v)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v == 1, nil
}

// PendingChanges reports how many local changes await upload (status /
// health reporting).
func (e *Engine) PendingChanges(ctx context.Context) (int, error) {
	var n int
	err := e.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM favourites_queue").Scan(&n)
	return n, err
}

// ChangesLoop uploads queued local changes on its own cadence.
func (e *Engine) ChangesLoop(ctx context.Context) error {
	for {
		if e.Paired() {
			e.uploadChanges(ctx)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(changesTick):
		}
	}
}

// uploadChanges drains the local change queue (exported for tests).
// Ops are sent oldest-first; only ops the server accepted (or already
// knew about — duplicates) are removed, so a partial failure replays
// exactly the missing tail. The server dedupes on op_id, making the
// replay safe (spec §9 idempotency).
func (e *Engine) uploadChanges(ctx context.Context) (int, error) {
	rows, err := e.db.QueryContext(ctx,
		"SELECT op_id, photo_hash, value, ts FROM favourites_queue ORDER BY ts, created_at LIMIT 500")
	if err != nil {
		return 0, err
	}
	type qOp struct {
		OpID, Hash, TS string
		Value          int
	}
	var ops []qOp
	for rows.Next() {
		var o qOp
		if err := rows.Scan(&o.OpID, &o.Hash, &o.Value, &o.TS); err != nil {
			rows.Close()
			return 0, err
		}
		ops = append(ops, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(ops) == 0 {
		return 0, nil
	}

	payload := make([]ChangeOp, 0, len(ops))
	for _, o := range ops {
		v := o.Value == 1
		payload = append(payload, ChangeOp{
			OpID: o.OpID, Type: "favourite", PhotoHash: o.Hash,
			Value: &v, TS: o.TS,
		})
	}
	res, err := e.client.PostChanges(ctx, e.deviceID, payload)
	if err != nil {
		e.log.Warn("changes upload deferred", "ops", len(ops), "error", err.Error())
		return 0, err
	}

	// Ack everything the server applied (accepted or duplicate); ops
	// the server named as rejected stay queued for retry and are
	// surfaced in the log (ours are always well-formed, so a
	// rejection is a contract bug, not a transient state).
	rejected := make(map[string]bool, len(res.RejectedOps))
	for _, opID := range res.RejectedOps {
		rejected[opID] = true
	}
	if res.Rejected > 0 {
		e.log.Warn("server rejected local changes",
			"rejected", res.Rejected, "ops", res.RejectedOps)
	}
	removed := 0
	for _, o := range ops {
		if rejected[o.OpID] {
			continue
		}
		if _, err := e.db.ExecContext(ctx,
			"DELETE FROM favourites_queue WHERE op_id = ?", o.OpID); err == nil {
			removed++
		}
	}
	e.log.Debug("changes uploaded",
		"accepted", res.Accepted, "duplicates", res.Duplicate,
		"rejected", res.Rejected, "acked", removed)
	return removed, nil
}
