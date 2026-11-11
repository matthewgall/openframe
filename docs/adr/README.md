# Architecture Decision Records

Decisions that are expensive to reverse are recorded here. Format:
context → decision → consequences. Superseded ADRs are not deleted;
they are marked superseded with a pointer.

| ADR | Title | Status |
|---|---|---|
| [ADR-0001](ADR-0001-single-repo-go.md) | Single Go repository, unified CLI | accepted |
| [ADR-0002](ADR-0002-pure-go-sqlite.md) | Pure-Go SQLite driver (modernc.org/sqlite) | accepted |
| [ADR-0003](ADR-0003-config-yaml-env.md) | Configuration: YAML + environment overrides | accepted |
| [ADR-0004](ADR-0004-cli-and-dependencies.md) | Hand-rolled CLI, minimal dependency set | accepted |
| [ADR-0005](ADR-0005-admin-spa.md) | Admin UI: Vite-bundled framework-free TypeScript, embedded | accepted |
| [ADR-0006](ADR-0006-display-driver.md) | Display driver boundary; DRM/KMS leading for M4; headless dev renderer | accepted |
| [ADR-0007](ADR-0007-release-signing.md) | Release signing: custom Ed25519 metadata (TUF evaluated) | accepted |
| [ADR-0008](ADR-0008-offline-first-sync.md) | Offline-first sync: content-addressed cache, staged activation, additive migrations | accepted |
| [ADR-0009](ADR-0009-image-support-v1.md) | v1 image support: JPEG/PNG/WebP; HEIC deferred | accepted |
| [ADR-0010](ADR-0010-logging.md) | Structured logging: log/slog, JSON default | accepted |
