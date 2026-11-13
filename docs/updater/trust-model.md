# Updater trust model (M6, spec §14, ADR-0007)

This document specifies, and the test matrix in
`internal/updater/{verify,e2e}_test.go` enforces, the complete trust
and anti-rollback model of the OpenFrame update system.

## Components and trust assumptions

| Component | Runs as | Trusted? | Owns |
|---|---|---|---|
| `openframe` (frame) | frame user | — | frame DB, photo cache, display |
| `openframe-updater` | **root** | its own code only | A/B slots, `current` symlink, update state |
| Release server | operator | **no** | signed metadata bytes (stored verbatim), artifact files, unsigned serving block |
| Signing key | release tooling | its public half is pinned per device | Ed25519 signatures |

Trust is *downward*: the frame does not trust the release server, and
the updater does not trust anything except (a) its own verifier and
(b) keys pinned on the device at deployment time.

## The signed document

A release is a JSON document. The signature covers a canonical byte
form (`Release.SignedPayload`): deterministic field ordering, RFC3339
UTC timestamps, sorted rollout device lists, and every field that
affects *what* the frame will run:

```
version, channel, os, arch,
artifact_sha256, artifact_size,
min_protocol_version, min_schema_version,
released_at, not_before, expires_at,
is_rollback, rollout_percent, rollout_devices,
new_keys (rotation additions), signer_key_id
```

Only `signature` (Ed25519, base64) is outside the payload — the
signer's identity is itself signed, so a forger cannot re-attribute
a valid signature. The verifier recomputes the canonical form from
the received fields and checks the signature — a field-level tamper
test (`TestVerifyTamperedField`) mutates *every* signed field
individually and asserts rejection.

## Trust rules (each has a named error and a test)

| Rule | Error |
|---|---|
| Signature verifies under a trusted key | `ErrBadSignature` |
| `signer_key_id` is in the device's pinned set | `ErrUnknownSigner` |
| Now ≥ `not_before` | `ErrNotBefore` |
| Now < `expires_at` | `ErrExpired` |
| `os`/`arch` match the device | `ErrPlatformMismatch` |
| Server manifest protocol ≤ frame's max | `ErrProtocolIncompat` |
| Frame DB schema ≥ release minimum | `ErrSchemaIncompat` |
| Version > floor for this channel (anti-rollback) | `ErrDowngrade` |
| New keys in `signing_keys` valid + non-duplicate | `ErrInvalidNewKey` |
| Downloaded artifact: SHA-256 + size match | `ErrArtifactHash` / `ErrArtifactSize` |

Checks run in a fixed order: identity → time → platform →
compatibility → anti-rollback, so the reported failure is stable and
diagnosable.

### Anti-rollback

`state.json` keeps `max_accepted[channel]` — the highest version
ever *committed* (activated AND verified healthy) on that channel. A
release older than the floor is `ErrDowngrade`, **unless** it is
signed with `is_rollback: true` (an intentional operator action, e.g.
reverting a bad release; the release tooling sets it, the admin sees
the flag in the dashboard). Committing a rollback release moves the
floor to the rolled-back version, so a subsequent normal release must
still be newer.

### Metadata freshness

`expires_at` bounds replay of old (but valid) signed documents;
`not_before` allows scheduled releases. Both are checked against
system time — an update that requires a *future* time is simply
deferred to a later cycle.

## Key rotation

- Trust root: `/opt/openframe/trust/trusted-keys.json`, written by the
  deployment package (or `openframe-updater` tooling) **on the
  device**. The release server never serves keys.
- Rotation is a signed event: a release may include
  `new_keys: [ {key_id, ed25519} ]` adding keys to the trust set,
  but only if the release itself is signed by a key the device
  already trusts (a self-issued key cannot bootstrap itself — that
  signature would be `ErrUnknownSigner`). At the moment a rotation
  release verifies, the updater atomically persists the merged key
  set to the device trust file before any install proceeds.
- Old keys remain trusted until explicitly removed by the operator
  (editing the device's trust file), so a compromised rotation path
  cannot erase existing trust — it can only *add* keys that a
  legitimate signer approved.
- Bandit problem: absent by construction — the client is the verifier
  and its trust set is device-pinned; there is no remote "root" role
  to race against.

## The serving block (unsigned, policy-only)

`latest.json` is an envelope:

```json
{ "release": <exact signed bytes>,
  "serving": { "rollout_percent": 50, "rollout_devices": ["frame-1"] } }
```

The `serving` block is **unsigned and untrusted policy input**: the
server may change it without re-signing (admin UI "Rollout…"). The
client:

1. verifies `release` exactly as signed (never mutated),
2. applies `serving` ONLY to the rollout-eligibility decision.

Consequence for an attacker who controls the envelope: they may
expand or shrink WHO receives an update, but the update is always the
verified signed artifact. Targeted expansion (adding a victim's
device id) is possible only for releases the operator already
uploaded; it does not forge or substitute binaries.

## Rollout membership

Eligibility = named device allow-list OR stable percentage bucket.
The bucket is `SHA-256("openframe-rollout/" + channel + "/" + version
+ "/" + device_id)` mod 100 — stable across checks and per-release
(per version, so a new release re-rolls the bucket). Nested cohorts
are a property of the function (10% ⊂ 50% for the same version),
verified by test. A device with no identity is never eligible.

## Failure semantics

- **Server unavailable** → `nothing`; slideshow and sync unaffected;
  the next cycle retries.
- **Any verification failure** → rejected with the named error,
  recorded in state (`last_failed`) and skipped for 24h
  (failure cooldown) unless forced — a poisoned release document
  cannot flap the device on every check.
- **Post-activation failure** (service won't start, readiness
  timeout, boot-reconcile up to 3 attempts) → **offline rollback**
  to the last committed slot; the cause is persisted so the admin
  dashboard shows `update_failed` with the reason.
- **Power failure** → state is written atomically at every phase
  transition; boot-time reconcile is candidate-centric and re-verifies
  the artifact SHA on disk before acting on it.

## What the metadata is *not*

The document is **data, never instructions**. The updater has no code
path that interprets a metadata field as a command, URL to fetch,
binary to execute, or configuration to apply. The only "effects" of a
field are the trust rules above.
