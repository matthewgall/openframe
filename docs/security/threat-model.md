# OpenFrame Threat Model (skeleton)

Milestone 1 records the threat landscape and the mitigation strategy
per threat; each control is implemented and tested in the milestone
noted here. This document is completed in M7 (security review).

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

| ID | Threat | Mitigation | Milestone |
|---|---|---|---|
| T-01 | Compromised frame (malicious agent) | Least-privilege frame user; no root; per-device credential revocation from the admin UI; signed releases only | M5/M6 |
| T-02 | Stolen SD card | Photos are cached copies (originals stay on server); device identity is useless without the server pairing record; document card wiping; future: LUKS on the data partition | M5 (documented), later |
| T-03 | Stolen device credentials | Credentials are revocable, rotatable, per-device; server-side audit log of device auth | M5 |
| T-04 | Malicious photograph upload | Magic-byte + decode validation (not just MIME); decoded dimension caps; bounded decode memory; EXIF sanitisation on derivatives; no execution of uploaded content anywhere | M2 |
| T-05 | Compromised family account | Argon2id password hashing; session expiry + revocation; login rate limiting; admin-only recovery; audit log of admin actions; passkey-ready design | M2 |
| T-06 | Compromised management server | Assume breach: no secrets beyond what the server legitimately holds; backups enable rebuild; frames keep operating offline; credentials revocable after rebuild | M7 (runbook) |
| T-07 | Compromised release hosting | Signatures are verified against pinned device-side keys; tampered metadata/artifacts rejected; metadata expiry; anti-rollback | M6 |
| T-08 | Update-signing key compromise | Key rotation scheme with pinned key set and explicit rotation releases; short-lived signing in CI via OIDC, never in PRs | M6 |
| T-09 | Network interception | HTTPS everywhere; certificate verification (no InsecureSkipVerify); no secrets in URLs | M2+ |
| T-10 | Database corruption | WAL + integrity_check in doctor; transactional migrations with checksums; backup/restore procedures | M1 (checks) / M7 (restore tests) |
| T-11 | Path traversal | All user-controlled paths go through validated joins; no user input reaches exec or filesystem roots; static asset serving from a closed embedded FS | M1 (SPA) / M2 (uploads) |
| T-12 | Denial of service | Upload size limits, rate limiting, bounded goroutines/queues on frames, bounded image decode, bounded HTTP timeouts | M2/M3 |
| T-13 | Unsafe image decoders | Decode with dimension bounds; reject malformed files; derivatives generated server-side with resource caps | M2 |
| T-14 | Secrets in logs | Structured logging convention: no credentials, tokens, signed URLs or PII in log attributes (reviewed at each milestone) | M1+ |
| T-15 | Dependency supply chain | Pinned go.sum + package-lock.json; govulncheck in CI; dependabot; minimal dependency set (see ADR-0004) | M1 |

## Standing rules

- No custom cryptographic primitives: Ed25519, SHA-256, Argon2id via
  vetted libraries only.
- No telemetry, no advertising, no third-party calls from either binary.
- `doctor`/`status` output is secret-free by construction (no tokens,
  no cookies, no signed URLs).
- Any code that touches paths, URLs or uploaded content must have a
  test for the adversarial input.
