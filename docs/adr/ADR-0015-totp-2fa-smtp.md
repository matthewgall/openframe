# ADR-0015: Two-factor sign-in (TOTP) and SMTP notification delivery

**Status:** accepted (user-approved scope, 2026-11-22)

## Context

OpenFrame is an offline-first family install, but the admin console
sits behind a single password on an account that can rotate every
frame credential, read every photo, and approve new devices. A
stolen or phished password is a full compromise (T-05). At the same
time, the first-run flow delivered the one-time admin password via a
journal log entry — a documented exception to the no-secrets-in-logs
rule (T-14) that is only acceptable because the install is local.

The project owner asked for 2FA and email (SMTP) delivery. Decisions
taken at scope review: TOTP + recovery codes (not email one-time
codes), per-user opt-in (no forced enrollment), and v1 emails
limited to first-run credentials + 2FA recovery codes (+ the doctor
test send).

## Decision

### Two-factor: TOTP (RFC 6238) + single-use recovery codes

- **Mechanism:** HMAC-SHA1, 6 digits, 30 s period, ±1 step window —
  Google-Authenticator-compatible. The verifier is implemented in
  `internal/server/auth/totp.go` and cross-verified in tests against
  `pquerna/otp` and an independent Python stdlib implementation
  (fixed vectors, window semantics, garbage rejection).
- **Flow:** password OK + 2FA enabled → `202 {mfa_required, mfa_id}`;
  `POST /auth/mfa` verifies a 6-digit TOTP code **or** an 11-char
  recovery code (routed by shape) and then creates the session.
  Challenges are 128-bit single-use tokens with a 5-minute TTL and a
  10-attempt cap (then destroyed → 429). In-memory by design: a
  restart just means re-entering the password.
- **Recovery codes:** 10 per enablement, alphabet excludes
  0/O/1/I, stored **only as SHA-256 hashes**, redeemed atomically
  (single-use), remaining count visible to the owner.
- **Enablement is two-step:** `enable` (password-gated) issues the
  secret + QR + recovery codes and stores the secret as *provisional*
  (login is NOT yet enforced); `confirm` (a live code) activates it.
  A scanned-but-unconfirmed setup therefore cannot lock anyone out.
- **No admin override.** There is deliberately no
  "admin disables 2FA for user X": 2FA exists against a compromised
  *admin*, and an enrollment control held by the admin would defeat
  it. Users manage their own second factor; an admin's recourse for a
  lost account is the existing password reset (which does not touch
  the 2FA row — see Consequences).
- **Opt-in.** Any user can enable; nobody is forced. Login cost for
  non-enrolled users is unchanged.

### SMTP: best-effort notification sender

- **Stdlib only** (`net/smtp`): port 465 implicit TLS, STARTTLS
  otherwise (mandatory on 587), AUTH PLAIN when a username is set.
  Every send is bounded by a connection deadline (net/smtp accepts no
  context; a silent server cannot hang the sender).
- **The best-effort contract:** a mail failure must never prevent the
  server starting, block a request, or lose a credential. First-run
  delivery: email first, **journal fallback on failure**; recovery
  codes: always shown on screen, email is the backup copy.
- **Single fixed recipient** (`server.admin_email`): the v1 audience
  is the installer. Per-user notification fan-out needs a
  recipients table and is out of scope.
- **Verification path:** `openframe email test` sends a live test
  message; `doctor` reports the configured state (read-only, never
  sends).

## Security notes

- TOTP secrets live in `user_totp.secret` in the server database —
  the same trust domain as the password hashes. A server breach
  (T-06) exposes them; the runbook's "rotate all passwords" step
  should be read as "re-provision all 2FA" as well (disable +
  re-enable per account after a rebuild).
- The MFA step is CSRF-exempt like login (no session exists yet);
  the mfa_id is an unguessable 128-bit single-use token, and the
  endpoint is attempt-capped — the residual risk matches the login
  exemption's (T-09 notes).
- Recovery codes authenticate like TOTP: 40+ bits each, single-use,
  hash-only at rest. Ten codes give at most ten guesses total, not
  ten per attempt.
- SMTP credentials in config/env are the operator's mail-server
  secret; the no-secrets-in-logs rule applies (the sender logs
  `to`/`subject`, never the body or credentials).

## Consequences

- Positive: one stolen password no longer compromises an enrolled
  account; the first-run secret no longer needs a log exception when
  mail is configured; the whole auth chain stays pure-Go/cgo-free.
- Negative: an admin cannot disable a member's 2FA (by design); a
  user who loses both authenticator and recovery codes needs an
  account-level intervention (documented: fresh credential via
  server reset — the 2FA row must be removed directly, which is the
  documented escape hatch, not a UI path).
- Follow-ups (explicitly out of v1): passkeys/WebAuthn, forced
  enrollment, email one-time codes, per-user notification targets.
