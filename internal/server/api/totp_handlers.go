package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/matthewgall/openframe/internal/server/auth"
)

// The 2FA user endpoints act on the caller's own account only
// (there is deliberately no admin "enable/disable 2FA for user X":
// that would let an admin lock a member in or out of their own
// second factor, and a compromised admin is exactly the threat 2FA
// exists to stop).

// GET /api/v1/users/me/totp
// 200: {"enabled": bool, "provisional": bool, "recovery_remaining": n}
func (s *Server) handleTOTPStatus(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	st, err := s.userStore.GetTOTP(r.Context(), u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not read two-factor state")
		return
	}
	if st == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled": false, "provisional": false, "recovery_remaining": 0,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":            st.Enabled,
		"provisional":        st.Provisional,
		"recovery_remaining": st.RecoveryRemaining,
	})
}

// POST /api/v1/users/me/totp/enable
// Body: {"password": "..."} (password-gated: this issues recovery
// codes that can sign in without the authenticator)
// 200: {"secret": "...", "otpauth_url": "...", "qr_png": "<b64>",
//
//	"recovery_codes": ["ABCDE-FGHIJ", ...]}
//
// The recovery codes are the LAST time they are ever shown (here and,
// when mail is configured, in an email) — the server keeps only their
// hashes.
func (s *Server) handleTOTPEnable(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.Password == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "password is required")
		return
	}
	if ok, err := s.userStore.VerifyPassword(r.Context(), u.ID, req.Password); err != nil || !ok {
		s.auditRec.Action(r.Context(), u.ID, "totp.enable_failed", "user", u.ID, clientIP(r), map[string]string{"reason": "bad_password"})
		writeError(w, http.StatusUnauthorized, "bad_credentials", "invalid password")
		return
	}
	secret, err := auth.GenerateTOTPSecret()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not generate secret")
		return
	}
	codes, err := auth.GenerateRecoveryCodes()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not generate recovery codes")
		return
	}
	if err := s.userStore.ProvisionalTOTP(r.Context(), u.ID, secret, codes); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not store two-factor state")
		return
	}

	otpauthURL := totpURI(secret, u.Email, "OpenFrame")
	png, err := qrcode.Encode(otpauthURL, qrcode.Medium, 256)
	if err != nil {
		// A QR failure must not strand a freshly issued secret: the
		// secret string itself is in the response for manual entry.
		s.log.Error("qr encoding failed; secret returned for manual entry", "error", err)
		png = nil
	}

	// Recovery codes by email (best-effort: they were just shown on
	// screen; a mail outage must not break the enablement).
	if s.mail != nil && s.mail.Enabled() {
		var b strings.Builder
		b.WriteString("You are setting up two-factor sign-in for OpenFrame.\n\n")
		b.WriteString("Recovery codes (one-time; each can be used ONCE when you lose your authenticator):\n\n")
		for _, c := range codes {
			b.WriteString("  " + c + "\n")
		}
		b.WriteString("\nStore these somewhere safe. If you lose BOTH your authenticator and these codes, you will need an administrator to reset your account.\n")
		s.mail.SendAsync("OpenFrame: your 2FA recovery codes", b.String())
	}

	s.auditRec.Action(r.Context(), u.ID, "totp.provisional", "user", u.ID, clientIP(r), nil)
	resp := map[string]any{
		"secret":         secret,
		"otpauth_url":    otpauthURL,
		"recovery_codes": codes,
	}
	if png != nil {
		resp["qr_png"] = base64.StdEncoding.EncodeToString(png)
	}
	writeJSON(w, http.StatusOK, resp)
}

// POST /api/v1/users/me/totp/confirm
// Body: {"code": "123456"} — proves the authenticator was scanned.
// 200: {"status": "enabled"}
// 401: bad code (the provisional secret stays, so the user can retry).
func (s *Server) handleTOTPConfirm(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.Code == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "code is required")
		return
	}
	st, err := s.userStore.GetTOTP(r.Context(), u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not read two-factor state")
		return
	}
	if st == nil || st.Enabled || !st.Provisional {
		writeError(w, http.StatusConflict, "no_provisional", "no pending two-factor setup; start one first")
		return
	}
	secret, _, err := s.userStore.TOTPEnforced(r.Context(), u.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not read two-factor state")
		return
	}
	if !auth.VerifyTOTP(secret, strings.TrimSpace(req.Code), time.Now()) {
		s.auditRec.Action(r.Context(), u.ID, "totp.confirm_failed", "user", u.ID, clientIP(r), nil)
		writeError(w, http.StatusUnauthorized, "bad_code", "the code did not verify")
		return
	}
	if err := s.userStore.ConfirmTOTP(r.Context(), u.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not enable two-factor")
		return
	}
	s.auditRec.Action(r.Context(), u.ID, "totp.enabled", "user", u.ID, clientIP(r), nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "enabled"})
}

// POST /api/v1/users/me/totp/disable
// Body: {"password": "..."} — password-gated like enablement.
// (The SPA additionally shows the destructive-action confirmation:
// disabling 2FA is the kind of action ADR-0013 gates.)
// 200: {"status": "disabled"}
func (s *Server) handleTOTPDisable(w http.ResponseWriter, r *http.Request) {
	u, _ := UserFrom(r.Context())
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.Password == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "password is required")
		return
	}
	if ok, err := s.userStore.VerifyPassword(r.Context(), u.ID, req.Password); err != nil || !ok {
		s.auditRec.Action(r.Context(), u.ID, "totp.disable_failed", "user", u.ID, clientIP(r), map[string]string{"reason": "bad_password"})
		writeError(w, http.StatusUnauthorized, "bad_credentials", "invalid password")
		return
	}
	if err := s.userStore.DisableTOTP(r.Context(), u.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "could not disable two-factor")
		return
	}
	s.auditRec.Action(r.Context(), u.ID, "totp.disabled", "user", u.ID, clientIP(r), nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "disabled"})
}

func totpURI(secret, account, issuer string) string {
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", "6")
	q.Set("period", "30")
	return "otpauth://totp/" + url.PathEscape(issuer+": "+account) + "?" + q.Encode()
}
