# OpenFrame Threat Model

Milestone 1 records the threat landscape and the mitigation strategy
per threat; each control is implemented and tested in the milestone
noted here. Status is refreshed at each milestone checkpoint; the
document is completed in M7 (security review).

Status legend: ✅ implemented & tested · 🟨 partial (scope noted) ·
⬜ planned for the listed milestone.

## Trust boundaries

1. **Internet ↔ server**: untrusted network. HTTPS only, rate limiting,
   strict input validation, upload content validation.
2. **Server ↔ frames**: authenticated, device-specific credentials
   (M5), frames initiate all connections (no inbound to frames).
3. **Server ↔ storage backend**: local FS or S3-compatible; no
   permanent public URLs are ever exposed to frames or browsers.
4. **Local ↔ frame disk**: SD card may be lost/stolen; the card holds
   the device identity and cached family photos.
5. **Updater ↔ release infrastructure**: the release server and its
   artifacts are *not* trusted; only Ed25519 signatures under pinned
   keys are trusted.

## Threats and mitigations

| ID | Threat | Mitigation | Milestone | Status |
|---|---|---|---|---|
| T-01 | Compromised frame (malicious agent) | Least-privilege frame user; no root; per-device credential revocation/rotation from the admin UI; the frame credential is scoped to that device and cannot read other devices' resources or the admin API; signed releases only | M3 (revocation) / M6 (signing) | 🟨 — per-device bearer credential issued/rotated/revoked in M3 (API + admin UI); frame user isolation in M7, signing in M6 |
| T-02 | Stolen SD card | Photos are cached copies (originals stay on server); the frame cache is content-addressed and only useful with a valid credential + server pairing record; the credential (M5) lives in a dedicated 0600 file outside frame.db and is per-device scoped, hash-verified and instantly revocable/rotatable from the admin UI; frame.db holds no other secrets or PII (only the device id, which is not a secret); document card wiping; future: LUKS on the data partition | M3 (verified layout) / M5 (credential file) / M7 (documented) | 🟨 — cache + frame.db layout verified M3; M5 credential file is 0600, atomic (temp+rename), identity-bound (a file from another frame is refused), and revocable server-side; wiping runbook in M7 |
| T-03 | Stolen device credentials | Credentials are revocable, rotatable, per-device, and never stored in the clear — the server keeps only SHA-256 hashes and compares in constant time; rotation invalidates the previous token immediately; revocation stops sync; all device downloads/changes are audit-logged; the token is shown exactly once at issue | M3 | ✅ — `device_credentials` hash-only storage, constant-time verify, one-time display, rotate/revoke endpoints + admin UI, per-device scoping enforced in the frame API, audit entries for manifest/download/changes |
| T-04 | Malicious photograph upload | Magic-byte + decode validation (not just MIME); decoded dimension caps; bounded decode memory; EXIF sanitisation on derivatives; no execution of uploaded content anywhere | M2 | ✅ — JPEG/PNG/lossy-WebP accepted; HEIC & lossless/extended WebP rejected with 415 (ADR-0009); orientation applied; derivatives are re-encoded, EXIF-free JPEG |
| T-05 | Compromised family account | Argon2id password hashing; session expiry + revocation; login rate limiting; admin-only recovery; audit log of admin actions; passkey-ready design | M2 | ✅ except passkeys — Argon2id (OWASP params), hashed sessions with TTL + revoke + revoke-all, per-IP & per-account login throttling, last-active-admin lockout, admin password reset, full audit trail |
| T-06 | Compromised management server | Assume breach: no secrets beyond what the server legitimately holds; backups enable rebuild; frames keep operating offline; credentials revocable after rebuild | M7 (runbook) | ⬜ |
| T-07 | Compromised release hosting | Signatures are verified against pinned device-side keys; tampered metadata/artifacts rejected; metadata expiry; anti-rollback | M6 | ✅ — verifier rejects every named failure (`ErrBadSignature`, `ErrUnknownSigner`, `ErrExpired`, `ErrNotBefore`, `ErrPlatformMismatch`, `ErrProtocolIncompat`, `ErrSchemaIncompat`, `ErrDowngrade`, `ErrArtifactHash`, `ErrArtifactSize`), each with a tamper test; trusted keys are device-pinned (`/opt/openframe/trust/trusted-keys.json`) and never served by the release server; the server stores and re-serves the exact signed bytes it was given; the unsigned `serving` block can only change WHO is rollout-eligible, never WHAT (version/digest/size are signed); unavailable update server never affects slideshow or sync |
| T-08 | Update-signing key compromise | Key rotation scheme with pinned key set and explicit rotation releases; short-lived signing in CI via OIDC, never in PRs | M6 | ✅ — rotation is a signed event: a release may add a new trusted key only when signed by a key the device already trusts (`TrustedKeys.AddTrusted`, invalid new keys rejected with `ErrInvalidNewKey`); old keys stay trusted until explicitly removed, so rotation cannot be used to erase trust; the signing key never reaches the frame or the release server — only its public half is pinned; CI signing is tag-triggered with OIDC permissions (no PR-triggered minting) |
| T-09 | Network interception | HTTPS everywhere; certificate verification (no InsecureSkipVerify); no secrets in URLs | M2+ | 🟨 — session cookies `HttpOnly`+`SameSite=Lax` (`Secure` when the public URL is HTTPS), CSRF synchronizer token on all mutations, login rate limiting; TLS itself is a deployment requirement (reverse proxy) and enforced in the M7 runbook |
| T-10 | Database corruption | WAL + integrity_check in doctor; transactional migrations with checksums; backup/restore procedures | M1 (checks) / M7 (restore tests) | ✅ M1 scope |
| T-11 | Path traversal | All user-controlled paths go through validated joins; no user input reaches exec or filesystem roots; static asset serving from a closed embedded FS | M1 (SPA) / M2 (uploads) | ✅ — SPA from embedded FS; storage keys are strict (hex hash / hash.variant) and path-validated; upload filenames are display metadata only, never filesystem keys |
| T-12 | Denial of service | Upload size limits, rate limiting, bounded goroutines/queues on frames, bounded image decode, bounded HTTP timeouts | M2/M3 | ✅ — per-file/per-request upload caps, decode dimension caps, login rate limiting, bounded HTTP timeouts; frame-side: bounded download concurrency + per-file size caps (manifest `size_bytes` enforced before commit), bounded queue table, per-attempt timeouts, backoff+jitter on failure, cache quota + free-space guard that stops downloads safely on a full disk; M4 UI: single bounded canvas (one 1280×800 NRGBA framebuffer), bounded gesture channel (32, drops when the UI is stalled), bounded snapshot ring, per-photo decode cache bounded by the canvas, touch reader reassembles fixed-size 24-byte evdev records (malformed reads close the stream) |
| T-13 | Unsafe image decoders | Decode with dimension bounds; reject malformed files; derivatives generated server-side with resource caps | M2 | ✅ — decode bounded by `max_decode_dimension`; malformed payloads rejected (tested); derivatives rendered from validated pixel data |
| T-14 | Secrets in logs | Structured logging convention: no credentials, tokens, signed URLs or PII in log attributes (reviewed at each milestone) | M1+ | ✅ with one documented exception — the first-run bootstrap logs the initial admin's one-time temporary password exactly once (Info level, `first_run` marker); it is a recovery credential for a not-yet-existing account and is not a long-lived secret |
| T-15 | Dependency supply chain | Pinned go.sum + package-lock.json; govulncheck in CI; dependabot; minimal dependency set (see ADR-0004) | M1 | ✅ — M2 adds x/crypto, x/image, goexif only (all pure-Go); `chai2010/webp` evaluated and rejected for cgo coupling (ADR-0009) |
| T-16 | Pairing-protocol attacks (code brute-force, challenge flooding, eavesdrop) | Codes are 8 chars from an unambiguous 31-symbol alphabet (~40 bits), short-lived (15 min TTL), single-use; the server stores only SHA-256 hashes, compares in constant time, and rate-limits challenge creation per source IP (max 3 pending + 6 per 30 min); approval additionally requires a valid admin session, so an attacker who can reach the API cannot approve a device they cannot see; the public endpoints reveal nothing about existing devices (opaque 404/401); the one-time credential claim is null-ed server-side after the first poll, so a replayed status request gets nothing | M5 | ✅ — challenge store with hash-only codes, constant-time verify, per-IP creation caps, one-time claim (all tested); LAN assumption: on an untrusted network the pairing exchange is as exposed as any HTTP without TLS — the documented deployment is a home LAN or a reverse-proxied HTTPS endpoint (T-09) |

## Notes (M4)

- **Physical access = local control, by design.** The frame is a
  household kiosk: anyone who can touch it can browse, favourite and
  change local display settings. There are no local administrative
  operations (account management, uploads, pairing all live on the
  server behind authentication), local settings are hold-protected
  against accidental activation, and nothing the touch UI can do is
  visible to other users or the server beyond a queued favourite
  change.
- **DRM master.** The DRM renderer becomes the KMS master for its
  card; a second DRM master (e.g. an X/Wayland session on the same
  machine) will prevent it from attaching. Frames run headless (no
  display server) by deployment; `doctor` reports the probe outcome
  when a card is held by another client.

## Notes (M5)

- **Physical access now includes the pairing surface.** Pairing is
  designed for the operator standing in front of the frame: the code
  is displayed on the device and must be entered by a logged-in
  admin. A passer-by can read the code, but approval requires an
  admin session *and* the code — and a rejected/expired challenge
  cannot be approved later. A passer-by can also create challenges
  (public endpoint), but each is rate-limited, transient and useless
  without an admin approving it; the admin UI shows who is asking
  (offered name + source IP) before approval.
- **The credential file is the frame's crown jewel.** It is 0600,
  written atomically, and bound to the frame identity (a copied file
  is refused). It grants only that device's resources. The standing
  mitigation is unchanged: revocation/rotation from the admin UI is
  one click and takes effect on the frame's next request.
- **Server config is trusted input to a kiosk.** The per-frame config
  document is validated server-side (enum/numeric checks, time-format
  checks) before it can reach a frame, and the frame re-applies it
  under a mutex so a mid-render update cannot tear UI state. The
  admin is the trusted principal here (they already can rename,
  unassign and revoke the frame); the validation exists so a bad
  document can never wedge a frame into a broken display state.
- **Health reports are frame-supplied.** The server displays them but
  never acts on them (no auto-remediation); a lying frame can only
  mislabel its own status. Status derivation (`online`/`offline`)
  is anchored on server-side heartbeat time, not the frame's
  self-reported uptime.

## Notes (M6)

- **The updater is the only root process and it is not trusted with
  decisions — only with mechanics.** The release *decision* is made by
  the verifier against the signed document; the coordinator's role is
  to execute (download, verify artifact, install to the inactive
  slot, stop service, swap the `current` symlink with one atomic
  rename, verify the new binary is ready, commit or roll back). The
  running binary is never overwritten in place: the A/B invariant
  means a power cut at any point leaves either the old or the new
  binary active, and boot-time reconcile (bounded to 3 attempts per
  candidate, then 24h failure cooldown) converges offline without
  contacting the server. Rollback is offline by construction.
- **Rollout override vs. signed rollout.** The signed document
  carries an initial rollout policy (operator intent at signing
  time). The server may later serve a `serving` override (admin UI
  "Rollout…"). The client verifies the document untouched, then
  applies the override ONLY to the eligibility decision. An attacker
  who can rewrite the envelope can therefore at most expose more
  (or fewer) frames to an already-verified build — the artifact's
  integrity and authenticity are unaffected.
- **Per-frame channel lives in the frame DB.** The admin UI sets it
  per device (validated server-side, pushed via the manifest, stored
  by the frame); the CLI sets a local override
  (`openframe update set-channel`, which wins, matching the normal
  settings precedence). The root updater reads the frame DB
  read-only. The `--channel` flag on the updater is a deployment-time
  escape hatch that beats both.
- **Update state is health-visible.** The frame reads the updater's
  `state.json` (unprivileged, read-only) and reports
  `update_status` (`none|updating|update_failed|rollback|unknown`)
  in its heartbeat; the admin dashboard's device status is derived
  from it (updating/failed/rollback override the online/offline
  derivation). A frame that cannot read the file reports `none` and
  keeps working — the updater is a separate concern by design.

## Standing rules

- No custom cryptographic primitives: Ed25519, SHA-256, Argon2id via
  vetted libraries only.
- No telemetry, no advertising, no third-party calls from either binary.
- `doctor`/`status` output is secret-free by construction (no tokens,
  no cookies, no signed URLs).
- Any code that touches paths, URLs or uploaded content must have a
  test for the adversarial input.
