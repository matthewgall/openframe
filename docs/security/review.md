# Security review (M7)

Date: milestone 7. Scope: the complete codebase as shipped in M1–M7.
This is the review the threat model reserved for M7 ("document is
completed in M7"): a structured pass over the trust boundaries in
`threat-model.md`, with findings, their status, and the checks that
now exist in the code or CI.

Method: threat-walking each boundary (admin session, device
credential, public endpoints, frame local files, updater), a
grep-level sweep for the specific failure classes below, and the
automated gates (CI race tests, `govulncheck`, `gofmt`/`go vet`).

## 1. Authentication and authorisation

| Area | Finding | Status |
|---|---|---|
| Admin sessions | Argon2id (OWASP params), hashed session IDs, TTL + revocation (single and all), last-active-admin lockout | ✅ tested |
| CSRF | Synchronizer token bound to the session on every mutating route; login exempt; `SameSite=Lax` | ✅ tested |
| Role separation | `requireAuth` on the admin group, `requireAdmin` on every admin-only route (users, devices, pairing, releases, priorities, audit) — verified by route walk in `api.go` | ✅ |
| Device credentials | Per-device bearer token, stored only as SHA-256 hash, constant-time compare, **scoped to that device's URL prefix** (`/frames/{deviceId}/...` with `deviceId` validated against the credential), auth failures audited | ✅ tested |
| Password change | Requires current password, min length enforced | ✅ tested |
| Login throttling | Per-IP and per-account rate limits | ✅ tested |

## 2. Secrets

| Area | Finding | Status |
|---|---|---|
| `InsecureSkipVerify` | grep sweep: **zero occurrences** in `internal/` and `cmd/` | ✅ |
| Hardcoded secrets | none in the tree; the first-run admin password is generated, logged exactly once at bootstrap (documented exception in the threat model, T-14) and never persisted | ✅ |
| Tokens in logs | grep sweep of log call sites: no credential, session or release-signing material in any log attribute; `doctor`/`status` output is secret-free | ✅ |
| Constant-time compares | `subtle.ConstantTimeCompare` for device tokens, CSRF tokens and pairing codes | ✅ |
| Credential file | 0600, atomic write, identity-bound (a copied file is refused); **doctor now fails** if the mode drifts or the data dir becomes group/other accessible (T-02) | ✅ M7 check |
| Update trust file | 0600 in a 0700 dir, device-pinned, never served by the server | ✅ M6 |

## 3. Input handling

| Area | Finding | Status |
|---|---|---|
| Upload bounds | `http.MaxBytesReader` on every JSON body; photo uploads bounded per file; release uploads bounded (metadata 1 MiB class, artifact per config) | ✅ |
| Image decode | dimension caps before decode; malformed files rejected; derivatives rendered from validated pixels | ✅ tested |
| Path handling | storage keys are strict hex(+variant) and path-validated; upload filenames are display metadata only; static serving from a closed embedded FS; **backup manifest paths are re-validated on verify/restore** (absolutely-nested, NUL, and non-clean paths refused) | ✅ M7 check |
| SQL | everything parameterised; no string-built queries with user input | ✅ |
| Config validation | server-side validation of per-frame config (enums, ranges, HH:MM formats) before a frame can receive it; updater channel values validated at three layers (server config, CLI, updater flag) | ✅ tested |

## 4. Release/update pipeline (re-review per ADR-0007)

- Signed-document trust model re-verified end to end: canonical
  payload ordering, `signer_key_id` inside the signature, device
  pinned keys, `new_keys` rotation only under a currently-trusted
  signer (a self-issued key is `ErrUnknownSigner`).
- The unsigned `serving` block's blast radius is bounded by
  construction (eligibility only) — re-confirmed by
  `TestM6RolloutServing`.
- Anti-rollback floor, expiry/not-before, failure cooldown and
  offline rollback unchanged from the M6 test matrix (spec §20).
- New (M7): `openframe backup verify`/restore preflight hash every
  file and `PRAGMA integrity_check` the database, so a **backup** is
  now as authenticated as a release: a tampered backup cannot be
  restored (`TestVerifyDetectsTampering`).

## 5. Dependencies and supply chain

- `govulncheck` in CI on every push; last run: no vulnerabilities.
- Dependency set is deliberately small (pure-Go SQLite, chi, x/*,
  one image library); no runtime dependency executes remote code.
- Release artifacts are signed; the updater executes only its own
  verified binary (metadata is data, never instructions).
- CI: tag-triggered release workflow only (no PR-triggered signing),
  checksums for both binaries.

## 6. Deployment-layer requirements (operator, not code)

These are enforced by documentation + loud startup signals rather
than by the binary (the binary cannot know its own reverse proxy):

1. **TLS termination** (T-09): the server logs a warning at every
   start when `public_base_url` is unset or not HTTPS. The runbook
   shows the nginx/Caddy configuration.
2. **Frame user isolation** (T-01): dedicated `openframe` system
   user, no root; doctor warns when invoked as root and fails when
   credential/data-dir permissions leak.
3. **Network placement**: the frame's port 8080-class bind address
   should not be exposed; only the reverse-proxied HTTPS endpoint is
   public. Frames initiate all connections (no inbound frame ports).
4. **Backup off-host** (T-06/T-10): `openframe backup` default lives
   beside the data; the runbook's cron example ships backups to a
   separate mount, and restore is tested (recovery tests, M7).

## 7. S3-compatible photo storage (ADR-0012, post-M7)

New surface, reviewed at implementation:

- **Credentials**: the S3 access key is a long-lived secret. It is
  never logged, never returned by any API, and only ever used to sign
  requests to the configured `endpoint`. Config file permissions
  (`0640 root:openframe`) or the env form keep it out of world reach;
  the runbook's R2 section says to scope the token to one bucket with
  write access only.
- **Hand-rolled SigV4**: no third-party signer. Validated against the
  official AWS `get-vanilla` vector plus a query-ordering vector
  cross-verified against an independent implementation; the in-test S3
  server re-checks the shape of every request (Authorization layout,
  `X-Amz-Date`, payload hash == body hash). It signs only
  PUT/GET/HEAD/LIST for the `s3` service — re-review if that grows.
- **Transport**: all S3 traffic is HTTPS (operator-supplied
  `endpoint`; an `http://` endpoint is the operator's explicit choice,
  as with any self-hosted MinIO — the runbook examples are HTTPS).
  The frame never talks to S3 directly; only the server does.
- **Integrity**: uploads are content-addressed — the stored object's
  key is its SHA-256, verified client-side before the PUT, so a
  corrupted/truncated upload is rejected rather than stored under a
  wrong key.
- **Backup scope**: with S3 active the on-disk backup holds the
  catalog, not the objects (recorded in `backup-info.json`'s
  `storage_note`); the bucket is the object backup. This is an
  explicit, documented scope, not an accident.

## 8. Open items carried past M7

- **Passkeys** for admin sign-in (T-05) — design is passkey-ready
  (session/credential stores are transport-agnostic); implementation
  deferred as a post-M7 feature.
- **LUKS on the frame data partition** (T-02 hardening) — the wipe
  runbook covers the software level; full-disk encryption is an
  operator choice documented in the runbook.
- Hardware validation of the full stack on the Pi (carried since M4).
