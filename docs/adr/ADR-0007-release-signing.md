# ADR-0007: Release signing — custom Ed25519 metadata (TUF evaluated)

**Status:** accepted (milestone 1); implementation in M6

## Context

The updater (M6) must verify software releases against infrastructure
that is explicitly *not* trusted (spec §14): compromised release
hosting, tampered metadata, rollbacks and forgeries must all be
rejected offline, using pinned keys, with controlled key rotation and
anti-rollback. The spec suggests "an established update metadata
framework such as TUF where practical; otherwise document and test the
complete trust and anti-rollback model."

## Decision

Use a **purpose-built signed release metadata format** (Ed25519) rather
than the `go-tuf` framework, with the full trust model documented and
tested in M6:

- **Metadata**: version, channel (stable/beta/development), OS/arch,
  artifact size, SHA-256, signature, minimum compatible
  schema/protocol versions, release timestamp, metadata expiry,
  rollout/staging fields.
- **Trust**: trusted public keys pinned in
  `/opt/openframe/trust/trusted-keys.json`, written by the deployment
  package — *never* downloaded from the release server.
- **Anti-rollback**: monotonic version ordering per channel stored in
  the local update state; downgrades rejected unless explicitly signed
  as a rollback release; stale metadata rejected via expiry +
  `not-before`-style checks.
- **Key rotation**: rotation is itself a signed metadata event
  (cross-signing by the outgoing key, or a rotation release signed by
  a key already trusted on the device); no bandit problem because
  trust roots are device-pinned.
- **No arbitrary execution**: metadata is data; the updater executes
  only its own code paths — it never interprets commands from metadata.
- **CI**: releases are signed via OIDC-permissioned external signing
  (private key never in the repo, never in PR-triggered workflows);
  only tag pushes on the release branch can mint signatures.

## Why not TUF

`go-tuf` gives root/targets/timestamp/snapshot roles and a mature
threat model, but (a) its client API pulls in a heavy dependency tree
into the frame binary, (b) its role/key schema is designed for
multi-key delegation we do not need (single signer + pinned rotation
keys), and (c) the spec explicitly permits a documented-and-tested
alternative. The properties we require (pinning, rotation,
anti-rollback, offline verification) are a strict subset of TUF and
are fully specified in M6 with the test matrix of spec §20
("Updaters" list) as the acceptance gate.

## Consequences

- Smaller, auditable updater; every trust rule has a named test.
- We own the threat-model reasoning for rotation; it is documented in
  `docs/security/threat-model.md` (T-07/T-08) and re-reviewed in M7.
