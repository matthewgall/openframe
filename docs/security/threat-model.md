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
| T-02 | Stolen SD card | Photos are cached copies (originals stay on server); the frame cache is content-addressed and only useful with a valid credential + server pairing record; the frame.db holds no credentials or PII (only the device id, which is not a secret); document card wiping; future: LUKS on the data partition | M3 (verified layout) / M7 (documented) | 🟨 — cache + frame.db layout verified M3 (no secrets at rest); wiping runbook in M7 |
| T-03 | Stolen device credentials | Credentials are revocable, rotatable, per-device, and never stored in the clear — the server keeps only SHA-256 hashes and compares in constant time; rotation invalidates the previous token immediately; revocation stops sync; all device downloads/changes are audit-logged; the token is shown exactly once at issue | M3 | ✅ — `device_credentials` hash-only storage, constant-time verify, one-time display, rotate/revoke endpoints + admin UI, per-device scoping enforced in the frame API, audit entries for manifest/download/changes |
| T-04 | Malicious photograph upload | Magic-byte + decode validation (not just MIME); decoded dimension caps; bounded decode memory; EXIF sanitisation on derivatives; no execution of uploaded content anywhere | M2 | ✅ — JPEG/PNG/lossy-WebP accepted; HEIC & lossless/extended WebP rejected with 415 (ADR-0009); orientation applied; derivatives are re-encoded, EXIF-free JPEG |
| T-05 | Compromised family account | Argon2id password hashing; session expiry + revocation; login rate limiting; admin-only recovery; audit log of admin actions; passkey-ready design | M2 | ✅ except passkeys — Argon2id (OWASP params), hashed sessions with TTL + revoke + revoke-all, per-IP & per-account login throttling, last-active-admin lockout, admin password reset, full audit trail |
| T-06 | Compromised management server | Assume breach: no secrets beyond what the server legitimately holds; backups enable rebuild; frames keep operating offline; credentials revocable after rebuild | M7 (runbook) | ⬜ |
| T-07 | Compromised release hosting | Signatures are verified against pinned device-side keys; tampered metadata/artifacts rejected; metadata expiry; anti-rollback | M6 | ⬜ |
| T-08 | Update-signing key compromise | Key rotation scheme with pinned key set and explicit rotation releases; short-lived signing in CI via OIDC, never in PRs | M6 | ⬜ |
| T-09 | Network interception | HTTPS everywhere; certificate verification (no InsecureSkipVerify); no secrets in URLs | M2+ | 🟨 — session cookies `HttpOnly`+`SameSite=Lax` (`Secure` when the public URL is HTTPS), CSRF synchronizer token on all mutations, login rate limiting; TLS itself is a deployment requirement (reverse proxy) and enforced in the M7 runbook |
| T-10 | Database corruption | WAL + integrity_check in doctor; transactional migrations with checksums; backup/restore procedures | M1 (checks) / M7 (restore tests) | ✅ M1 scope |
| T-11 | Path traversal | All user-controlled paths go through validated joins; no user input reaches exec or filesystem roots; static asset serving from a closed embedded FS | M1 (SPA) / M2 (uploads) | ✅ — SPA from embedded FS; storage keys are strict (hex hash / hash.variant) and path-validated; upload filenames are display metadata only, never filesystem keys |
| T-12 | Denial of service | Upload size limits, rate limiting, bounded goroutines/queues on frames, bounded image decode, bounded HTTP timeouts | M2/M3 | ✅ — per-file/per-request upload caps, decode dimension caps, login rate limiting, bounded HTTP timeouts; frame-side: bounded download concurrency + per-file size caps (manifest `size_bytes` enforced before commit), bounded queue table, per-attempt timeouts, backoff+jitter on failure, cache quota + free-space guard that stops downloads safely on a full disk; M4 UI: single bounded canvas (one 1280×800 NRGBA framebuffer), bounded gesture channel (32, drops when the UI is stalled), bounded snapshot ring, per-photo decode cache bounded by the canvas, touch reader reassembles fixed-size 24-byte evdev records (malformed reads close the stream) |
| T-13 | Unsafe image decoders | Decode with dimension bounds; reject malformed files; derivatives generated server-side with resource caps | M2 | ✅ — decode bounded by `max_decode_dimension`; malformed payloads rejected (tested); derivatives rendered from validated pixel data |
| T-14 | Secrets in logs | Structured logging convention: no credentials, tokens, signed URLs or PII in log attributes (reviewed at each milestone) | M1+ | ✅ with one documented exception — the first-run bootstrap logs the initial admin's one-time temporary password exactly once (Info level, `first_run` marker); it is a recovery credential for a not-yet-existing account and is not a long-lived secret |
| T-15 | Dependency supply chain | Pinned go.sum + package-lock.json; govulncheck in CI; dependabot; minimal dependency set (see ADR-0004) | M1 | ✅ — M2 adds x/crypto, x/image, goexif only (all pure-Go); `chai2010/webp` evaluated and rejected for cgo coupling (ADR-0009) |

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

## Standing rules

- No custom cryptographic primitives: Ed25519, SHA-256, Argon2id via
  vetted libraries only.
- No telemetry, no advertising, no third-party calls from either binary.
- `doctor`/`status` output is secret-free by construction (no tokens,
  no cookies, no signed URLs).
- Any code that touches paths, URLs or uploaded content must have a
  test for the adversarial input.
