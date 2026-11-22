package users

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/matthewgall/openframe/internal/server/auth"
)

// TOTPStatus describes a user's two-factor state.
type TOTPStatus struct {
	Enabled bool
	// Provisional means a secret was issued (QR shown) but not yet
	// confirmed with a live code; login enforcement waits for Enabled.
	Provisional bool
	// RecoveryRemaining is how many unused recovery codes are left.
	RecoveryRemaining int
}

// ProvisionalTOTP stores a freshly generated secret WITHOUT enabling
// enforcement (enabled_at NULL) and installs the fresh recovery-code
// set (previous set, if any, is replaced: a new enablement issues a
// new secret, so old recovery codes would belong to a dead secret).
// The caller keeps the plain recovery codes to display/email ONCE.
func (s *Store) ProvisionalTOTP(ctx context.Context, userID, secret string, recoveryCodes []string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := s.dbh.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx,
		`INSERT INTO user_totp (user_id, secret, enabled_at, created_at)
		 VALUES (?, ?, NULL, ?)
		 ON CONFLICT(user_id) DO UPDATE SET secret = excluded.secret, enabled_at = NULL, created_at = excluded.created_at`,
		userID, secret, now)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM user_recovery_codes WHERE user_id = ?`, userID); err != nil {
		return err
	}
	for _, code := range recoveryCodes {
		if _, err = tx.ExecContext(ctx,
			`INSERT INTO user_recovery_codes (user_id, code_hash, created_at) VALUES (?, ?, ?)`,
			userID, auth.HashRecoveryCode(code), now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ConfirmTOTP activates a provisional secret after the user proved
// possession of an authenticator (verified by the caller). Returns
// ErrNotFound when there is no provisional secret to confirm.
func (s *Store) ConfirmTOTP(ctx context.Context, userID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.dbh.ExecContext(ctx,
		`UPDATE user_totp SET enabled_at = ? WHERE user_id = ? AND enabled_at IS NULL`,
		now, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DisableTOTP removes the secret and all recovery codes.
func (s *Store) DisableTOTP(ctx context.Context, userID string) error {
	tx, err := s.dbh.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM user_totp WHERE user_id = ?`, userID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM user_recovery_codes WHERE user_id = ?`, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// GetTOTP returns the user's 2FA state (nil status when disabled).
func (s *Store) GetTOTP(ctx context.Context, userID string) (*TOTPStatus, error) {
	var enabledAt sql.NullString
	err := s.dbh.QueryRowContext(ctx,
		`SELECT enabled_at FROM user_totp WHERE user_id = ?`, userID).Scan(&enabledAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var n int
	if err := s.dbh.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM user_recovery_codes WHERE user_id = ? AND used_at IS NULL`,
		userID).Scan(&n); err != nil {
		return nil, err
	}
	return &TOTPStatus{
		Enabled:           enabledAt.Valid,
		Provisional:       !enabledAt.Valid,
		RecoveryRemaining: n,
	}, nil
}

// VerifyTOTPSecret returns the stored secret (for the login MFA step)
// and whether enforcement applies.
func (s *Store) TOTPEnforced(ctx context.Context, userID string) (secret string, ok bool, err error) {
	var enabledAt sql.NullString
	err = s.dbh.QueryRowContext(ctx,
		`SELECT secret, enabled_at FROM user_totp WHERE user_id = ?`, userID).
		Scan(&secret, &enabledAt)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return secret, enabledAt.Valid, nil
}

// RedeemRecoveryCode marks one matching recovery code as used
// (single-use: the second attempt finds no unused row). Returns true
// when a code was redeemed.
func (s *Store) RedeemRecoveryCode(ctx context.Context, userID, code string) (bool, error) {
	hash := auth.HashRecoveryCode(code)
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.dbh.ExecContext(ctx,
		`UPDATE user_recovery_codes SET used_at = ?
		 WHERE user_id = ? AND code_hash = ? AND used_at IS NULL`,
		now, userID, hash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ErrRecoveryNotFound distinguishes "wrong code" from store errors at
// the API boundary (both are 401 to the client; this keeps the audit
// reason accurate).
var ErrRecoveryNotFound = errors.New("users: no matching unused recovery code")
