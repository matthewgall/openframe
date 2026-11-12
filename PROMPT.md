# OpenFrame — Complete Implementation Specification

## Your role

You are a principal software engineer, Go developer, Linux systems engineer, security architect and frontend developer.

Your task is to design and implement **OpenFrame**, a production-quality, open-source, self-hosted digital photo frame platform.

This is a real-world application intended to run unattended on Raspberry Pi devices in family homes for years.

Implement working software, not pseudocode, mock functionality or an architectural demonstration.

Prioritise reliability, security, portability, maintainability, accessibility and resource efficiency.

Work autonomously through the milestones described below, validating each stage before proceeding. Where requirements are ambiguous, make a reasonable engineering decision, document it and continue unless the decision would introduce a significant security, compatibility or data-loss risk.

Do not claim that functionality works unless it has been implemented and tested.

---

# 1. Product vision

OpenFrame is a privacy-respecting alternative to commercial cloud-connected digital photo frames.

Family members should be able to upload photographs remotely through a web interface. Those photographs are stored on infrastructure controlled by the owner and automatically synchronised to one or more physical digital photo frames.

The frames should display photographs throughout the day and allow recipients to browse their family photo albums using a touchscreen.

The entire system must continue working locally when the internet connection or central management server is unavailable.

There must be no dependency on a proprietary digital photo frame provider.

The system must support multiple frames, multiple users, remotely managed configuration and secure automatic software updates.

## Core principles

1. **Offline-first:** Existing photographs, albums and captions remain accessible without internet connectivity.
2. **Self-hosted:** All photographs and metadata remain under the operator's control.
3. **Single codebase:** One Go repository with shared protocol and domain logic.
4. **Low resource usage:** Optimised for Raspberry Pi 3 Model B+ hardware.
5. **Remote management:** Physical access should rarely be necessary.
6. **Recoverability:** Failed downloads, software updates and power interruptions must not render a frame unusable.
7. **Portability:** Avoid dependencies on a particular cloud or hosting provider.
8. **Privacy:** No third-party telemetry, advertising or unnecessary data collection.
9. **Accessibility:** Large controls and straightforward navigation suitable for elderly users.
10. **Extensibility:** Additional frame hardware and display implementations should be possible without rewriting the system.

---

# 2. Target hardware

Primary target:

- Raspberry Pi 3 Model B+
- Broadcom BCM2837B0
- Quad-core ARM Cortex-A53 at 1.4 GHz
- 1 GB RAM
- Integrated dual-band Wi-Fi
- Bluetooth 4.2/BLE
- Full-size HDMI output
- USB-A ports
- 10.1-inch 1280 × 800 IPS capacitive touchscreen
- HDMI for display output
- USB HID for touch input
- 32 GB or larger high-endurance microSD card
- Raspberry Pi OS Lite 64-bit

The intended display is a Waveshare 10.1-inch HDMI capacitive touchscreen, subject to final hardware compatibility validation.

The implementation must not assume that the display exposes a particular backlight control interface.

Support Linux AMD64 development and testing without Raspberry Pi hardware.

The software should remain compatible with future ARM64 Raspberry Pi devices.

## Resource targets

Aim for:

- Frame application idle memory usage below 200 MB, excluding filesystem cache where practical.
- No Electron.
- No mandatory Chromium.
- No heavyweight desktop environment.
- Responsive touch interaction.
- Minimal background CPU usage.
- Server-generated display derivatives and thumbnails.
- Bounded image decoding and memory usage.
- No unnecessary polling or continuous redraws.

Treat these as performance targets, not assumptions. Measure actual resource consumption.

---

# 3. Application architecture

Use Go 1.26 or newer.

Maintain a single repository and a unified CLI.

Required commands:

```bash
openframe server
openframe frame
openframe pair
openframe doctor
openframe status
openframe version
```

The server and frame must run independently.

Running the server must not initialise display hardware, frame-specific services or graphics libraries.

The same repository should produce:

- `openframe` — main application
- `openframe-updater` — independent privileged update helper

The updater must be independently restartable and capable of recovering from a failed application release.

Use:

- Go standard library wherever practical
- `chi` for HTTP routing if beneficial
- SQLite for server and frame persistence
- `log/slog` for structured logging
- YAML configuration with environment overrides
- Embedded frontend assets
- Versioned REST APIs
- OpenAPI documentation
- SHA-256 content hashing
- Ed25519 release signatures
- Context-aware background operations
- Graceful shutdown

Prefer pure Go dependencies for shared and server packages.

Keep platform-specific graphics implementations isolated behind interfaces and build constraints.

Do not force the server to link against SDL, DRM or Raspberry Pi-specific libraries.

---

# 4. Repository structure

Use a structure similar to:

```text
openframe/
├── cmd/
│   ├── openframe/
│   └── openframe-updater/
├── internal/
│   ├── server/
│   │   ├── api/
│   │   ├── auth/
│   │   ├── users/
│   │   ├── albums/
│   │   ├── photos/
│   │   ├── devices/
│   │   ├── releases/
│   │   └── storage/
│   ├── frame/
│   │   ├── agent/
│   │   ├── sync/
│   │   ├── cache/
│   │   ├── display/
│   │   ├── touch/
│   │   ├── gallery/
│   │   └── health/
│   ├── updater/
│   ├── protocol/
│   ├── config/
│   └── platform/
├── web/
│   ├── admin/
│   └── assets/
├── migrations/
│   ├── server/
│   └── frame/
├── packaging/
│   ├── systemd/
│   └── raspberrypi/
├── docs/
│   ├── architecture/
│   ├── api/
│   ├── hardware/
│   ├── security/
│   └── adr/
├── scripts/
├── .github/
│   └── workflows/
├── go.mod
├── Makefile
└── README.md
```

Use this as guidance rather than enforcing unnecessary packages.

Keep package boundaries clear and avoid circular dependencies.

---

# 5. Central management server

Implement a self-hosted management server with a responsive web interface.

## Authentication and users

Support:

- Multiple family members
- Administrator and member roles
- Secure password hashing with Argon2id
- Secure HTTP-only cookies
- CSRF protection
- Session expiration and revocation
- Login rate limiting
- Password reset through administrator-controlled recovery
- Audit logging for important administrative actions

Design for future passkey support without requiring it in the initial release.

## Photo uploads

Support:

- JPEG
- PNG
- WebP
- HEIC where supported by available image-processing dependencies

Preserve original uploaded photographs.

Generate:

- Display-resolution derivatives
- Gallery thumbnails
- Optional portrait and landscape variants

Handle EXIF orientation correctly.

Extract and preserve useful metadata including:

- Original filename
- Capture date
- Upload date
- Uploader
- Caption
- Image dimensions
- Content hash

Allow users to edit captions and capture dates.

Strip sensitive metadata from frame-delivered derivatives unless explicitly required.

Validate image content independently of filenames and MIME headers.

Apply limits to upload size, decoded dimensions and processing resources.

Reject malformed or unsafe files.

## Albums

Support:

- Creating and editing albums
- Adding and removing photographs
- Assigning albums to frames
- Shared albums
- Private albums
- Album cover photographs
- Configurable album ordering
- Soft deletion and recovery

A photograph may belong to multiple albums without duplicating the original stored object.

## Favourites

Allow recipients to favourite photographs directly on the touchscreen.

Synchronise favourites back to the server when connectivity is available.

Favourites must remain usable offline.

Use per-frame or per-recipient favourite state rather than assuming all users share the same preferences.

Define conflict-resolution behaviour for offline changes.

## Storage

Use a storage abstraction supporting:

1. Local filesystem
2. S3-compatible object storage

Do not require S3 for initial deployment.

Store metadata in SQLite.

Use immutable content-addressed objects where appropriate.

Support backups and complete exports of photographs and metadata.

The server must be portable between hosting providers.

---

# 6. Web administration interface

Build a responsive interface suitable for desktop and mobile browsers.

Required sections:

## Dashboard

Display:

- Registered frames
- Online/offline status
- Last successful synchronisation
- Software version
- Pending downloads
- Storage usage
- Update status
- Recent uploads

## Photo library

Provide:

- Drag-and-drop uploads
- Multiple-file uploads
- Thumbnail grid
- Photograph previews
- Caption editing
- Date editing
- Album assignment
- Search and filtering
- Duplicate detection
- Soft deletion and recovery

## Albums

Allow family members to create albums and organise photographs according to permissions.

## Frames

Administrators must be able to:

- Register frames
- Rename frames
- Assign albums
- Change slideshow settings
- Configure synchronisation schedules
- Configure sleep schedules
- Trigger synchronisation
- View health information
- Configure software update policies
- Revoke a frame's credentials

## Users

Support inviting and managing family members with appropriate access permissions.

## Releases

Provide a release dashboard showing:

- Available software versions
- Stable, beta and development channels
- Installed versions by frame
- Pending updates
- Successful deployments
- Failed deployments
- Rollbacks
- Rollout progress

Use accessible components, clear navigation and large touch-friendly controls where relevant.

---

# 7. Frame software

The frame must be capable of operating indefinitely without a live server connection.

The frame agent is responsible for:

- Device authentication
- Configuration synchronisation
- Manifest retrieval
- Photograph downloads
- Local caching
- Local metadata
- Health reporting
- Update coordination

The display renderer must operate independently of network connectivity.

The agent and renderer may run within the same application process but must have clearly separated responsibilities.

A renderer failure must not corrupt synchronisation state.

Use bounded queues and avoid unbounded goroutine creation.

---

# 8. Offline-first storage

Use a local SQLite database.

Track:

- Device identity
- Assigned albums
- Photograph metadata
- Captions
- Favourites
- Thumbnail paths
- Display image paths
- Content hashes
- Manifest revisions
- Pending downloads
- Last-known-good collection
- Last-known-good configuration
- Synchronisation attempts
- Synchronisation errors

Use a content-addressed cache.

Downloads must never directly overwrite an active photograph.

Apply updates transactionally.

A new manifest must not become active until all mandatory files have been verified.

Keep the previous working collection available until the new collection is safely activated.

Garbage collection must never remove photographs referenced by the active collection.

Support storage quotas and disk-space monitoring.

---

# 9. Synchronisation engine

Synchronisation is a central feature.

Separate:

1. Manifest checking
2. Photograph downloading
3. Local collection activation
4. Uploading local changes
5. Software update checking

These must be independently scheduled.

## Default schedule

- Manifest checks every 60 minutes
- Photograph downloads between 01:00 and 06:00
- Time zone: Europe/London
- Software updates during a separate configurable maintenance window
- Randomised scheduling jitter
- Exponential retry backoff

## Download policies

Support:

- Immediate
- Scheduled
- Hybrid

Hybrid mode downloads small changes immediately and defers large batches until the scheduled window.

Allow configurable thresholds.

## Priority delivery

Administrators may mark a photograph or collection as priority.

Priority delivery may bypass the normal photo-download schedule, subject to configured device policies.

Priority delivery must not bypass security checks or content verification.

## Manifest protocol

Use revision identifiers and HTTP ETags.

Support conditional requests returning HTTP 304.

Manifests should contain:

- Revision
- Album assignments
- Photograph identifiers
- Content hashes
- File sizes
- Download URLs or authenticated resource identifiers
- Relevant metadata
- Configuration revision

Do not expose permanent unrestricted object-storage URLs.

## Download safety

For each photograph:

1. Verify the expected identifier and destination.
2. Download to a temporary file.
3. Enforce download size limits.
4. Verify SHA-256.
5. Flush and safely commit the file.
6. Update local metadata transactionally.
7. Make the photograph available to the renderer.

Support resumable downloads where the storage backend and HTTP semantics permit.

If a download fails, retain the previous collection.

If the server disappears, continue displaying cached photographs.

If the disk becomes full, stop downloading safely without corrupting the cache.

## Local changes

Favourites and other supported local changes must be queued offline.

Synchronise them when connectivity returns.

Use idempotent operations and explicit conflict-resolution rules.

---

# 10. Touchscreen display interface

Implement a lightweight native interface optimised for 1280 × 800.

Avoid Chromium, Electron and heavyweight desktop environments.

Evaluate suitable Linux graphics backends such as SDL2 or DRM/KMS.

Document the chosen approach and validate compatibility with Raspberry Pi OS Lite and the Pi 3B+.

Provide a development renderer that works without physical touchscreen hardware.

## Slideshow mode

This is the default interface.

Features:

- Fullscreen photograph display
- Configurable interval
- Smooth transitions
- Sequential or random ordering
- Aspect-ratio-preserving rendering
- Contain, cover and crop modes
- Optional captions
- Optional clock
- Swipe navigation
- Tap to reveal controls
- Automatic return after inactivity

Default slideshow interval: 45 seconds.

## Gallery mode

Allow users to browse their photographs.

Features:

- Album selection
- Thumbnail grid
- Fullscreen photograph viewer
- Swipe navigation
- Favourites
- Captions
- Photograph dates
- Uploader information where available
- Simple back navigation
- Large touch targets

Default gallery inactivity timeout: 120 seconds.

## Settings mode

Provide controls for:

- Brightness
- Slideshow interval
- Display orientation
- Sleep schedule
- Network status
- Last synchronisation
- Available storage
- Software version

Administrative actions must be protected against accidental activation.

## Accessibility

Prioritise:

- Large buttons
- Readable typography
- High contrast
- Predictable gestures
- Minimal interface complexity
- Clear loading and error states
- No unnecessary animations

The frame should remain pleasant to use for someone unfamiliar with computers.

---

# 11. Display power management

Support configurable sleep schedules.

Default:

- Display on: 07:00
- Display off: 22:00

The frame agent must continue operating while the display is asleep.

Where supported, turn off the actual LCD backlight.

Do not assume that a black image turns off the backlight.

Implement display power controls behind a hardware abstraction.

Provide a fallback where hardware brightness control is unavailable.

Allow administrators to override the schedule remotely.

---

# 12. Device provisioning

Implement secure device pairing.

A new frame should:

1. Start unpaired.
2. Generate or obtain a short-lived pairing challenge.
3. Display a pairing code.
4. Allow an administrator to approve pairing through the management interface.
5. Receive unique device credentials.
6. Store credentials securely.
7. Download its assigned configuration.
8. Begin synchronisation.

Pairing codes must be short-lived and single-use.

Prevent brute-force pairing attempts.

Support credential rotation and revocation.

Do not require inbound connections to the frame.

Normal communication must be initiated by the frame over HTTPS.

Bluetooth-assisted Wi-Fi provisioning may be added later but must not be required for the initial release.

---

# 13. Remote health monitoring

Frames should periodically report:

- Device identifier
- Software version
- Uptime
- Last successful synchronisation
- Current manifest revision
- Pending downloads
- Storage utilisation
- Memory usage
- CPU temperature where available
- Display status
- Recent error summaries
- Update state

Do not collect unnecessary personal information.

The management interface should distinguish between:

- Online
- Recently seen
- Offline
- Synchronisation error
- Update in progress
- Update failed
- Rollback completed

Avoid claiming that a frame is healthy merely because its HTTP client can contact the server.

---

# 14. Secure automatic software updates

This is a mandatory core feature.

The system must support unattended updates without requiring physical access to the frame.

## Architecture

Build a separate executable:

`openframe-updater`

The updater must operate independently of the main application.

Use an A/B binary deployment system.

Example layout:

```text
/opt/openframe/
├── slots/
│   ├── a/
│   │   └── openframe
│   └── b/
│       └── openframe
├── current -> slots/a/openframe
├── updater/
│   └── openframe-updater
└── trust/
    └── trusted-keys.json

/var/lib/openframe/
├── frame.db
├── photos/
├── thumbnails/
├── downloads/
└── updates/
    └── state.json
```

The current application slot must never be overwritten during an update.

The updater must be root-owned and operate with narrowly scoped privileges.

The frame application must run as an unprivileged user.

## Release verification

Use Ed25519 signatures.

Release metadata must identify:

- Version
- Release channel
- Target operating system
- Target architecture
- Artifact size
- SHA-256 digest
- Signature information
- Minimum compatible schema/protocol versions
- Release timestamp
- Metadata expiration

Pin trusted verification keys independently of the release server.

Support controlled signing-key rotation.

Reject invalid signatures, expired metadata, incompatible releases and unauthorised downgrades.

Use an established update metadata framework such as TUF where practical; otherwise document and test the complete trust and anti-rollback model.

Do not execute arbitrary commands supplied by release metadata.

## Update process

1. Check for an available release.
2. Validate signed metadata.
3. Check architecture and compatibility.
4. Check available disk space.
5. Download into the inactive slot.
6. Verify artifact integrity and authenticity.
7. Prepare a recoverable update transaction.
8. Stop the main frame service.
9. Atomically activate the candidate slot.
10. Restart the frame service.
11. Wait for application readiness.
12. Verify renderer and local collection health.
13. Commit the update only after successful validation.
14. Retain the previous known-good release for rollback.

## Automatic rollback

If the candidate:

- Fails to start
- Crashes repeatedly
- Fails readiness checks
- Cannot access its required local data
- Cannot initialise the renderer
- Exceeds its validation timeout

the updater must restore the previous working slot.

Rollback must not require internet connectivity.

The system must recover safely from power failure at every update stage.

A failed update must never delete cached photographs.

## Database compatibility

Binary rollback is insufficient if database migrations are incompatible.

Design explicit migration compatibility rules.

Use backward-compatible migrations where possible.

For incompatible migrations, require verified backups and a tested restoration strategy.

Never restore an old database snapshot in a way that silently discards newer family data.

Reject an update if safe rollback cannot be guaranteed.

## Release channels

Support:

- Development
- Beta
- Stable

Each frame has an independently configurable release channel.

Support staged rollouts.

Allow an administrator to approve or pause a release.

A development frame should receive updates before production frames.

## Update scheduling

Support:

- Automatic installation
- Download-only pending approval
- Manual installation

Allow configurable maintenance windows.

Default production maintenance window: 02:00–05:00 local time.

Software updates must not interrupt active photograph synchronisation transactions.

An unavailable update server must not affect slideshow or gallery functionality.

---

# 15. GitHub Actions and release engineering

Implement CI/CD workflows.

On pull requests:

- Format checks
- Static analysis
- Unit tests
- Integration tests
- Race detection where supported
- Dependency vulnerability checks
- Linux AMD64 build
- Linux ARM64 build

For releases:

- Build versioned binaries
- Produce SHA-256 checksums
- Produce release metadata
- Sign release metadata using protected signing infrastructure
- Publish immutable artifacts
- Generate release notes
- Support promotion between release channels

Never commit signing private keys.

Do not expose signing credentials to untrusted pull-request workflows.

Use reproducible or verifiable build practices where practical.

Document how releases are signed, promoted and revoked.

---

# 16. Linux deployment

Provide Raspberry Pi OS installation instructions and systemd units.

Required services:

- Main frame service
- Independent updater service/timer

Use a dedicated unprivileged application account.

Provide:

- Secure filesystem permissions
- Appropriate restart policies
- Startup ordering
- Graceful shutdown
- Persistent state directories
- Log management
- Watchdog integration where appropriate

Do not run the main frame application as root.

Do not require inbound SSH or port forwarding for normal operation.

Support optional outbound WireGuard access for administrative troubleshooting.

---

# 17. Diagnostics

Implement:

```bash
openframe doctor
openframe status
openframe version
```

The diagnostics command should inspect:

- Operating system
- CPU architecture
- Available memory
- Disk space
- SQLite integrity
- Display detection
- Touch input detection
- Network connectivity
- Server reachability
- Device pairing status
- Local cache health
- Active software slot
- Updater status

Do not reveal secrets in diagnostic output.

Provide both human-readable and JSON output where useful.

---

# 18. Backups and recovery

The server must support complete backups containing:

- SQLite database
- Original photographs
- Derived image metadata
- Albums
- Captions
- User and device configuration
- Necessary export metadata

Provide documented restore procedures.

Ensure backups are internally consistent.

For S3 storage, document how to reconcile object storage with SQLite metadata.

For frames, prioritise preservation of:

- Device identity
- Local configuration
- Cached photographs
- Last-known-good application release

A frame should be recoverable from a fresh operating-system installation using a documented provisioning process.

---

# 19. Security

Create a documented threat model.

Consider:

- Compromised frame
- Stolen SD card
- Stolen device credentials
- Malicious photograph uploads
- Compromised family account
- Compromised management server
- Compromised release hosting
- Update-signing key compromise
- Network interception
- Database corruption
- Path traversal
- Denial of service
- Unsafe image decoders

Security requirements:

- HTTPS
- Least-privilege credentials
- Device-specific authentication
- Secure session handling
- CSRF protection
- Input validation
- Upload limits
- Rate limiting
- Restrictive filesystem permissions
- No sensitive information in logs
- Signed software releases
- Secure update rollback
- Dependency scanning

Use established cryptographic libraries.

Do not invent custom cryptographic primitives.

---

# 20. Testing strategy

Implement automated unit and integration tests.

Test at least:

### Server

- Authentication
- Authorisation
- Upload validation
- Album permissions
- Duplicate photographs
- Metadata updates
- Database migrations
- Backup and restore

### Synchronisation

- Successful initial synchronisation
- Incremental synchronisation
- HTTP 304 responses
- Interrupted downloads
- Invalid content hashes
- Disk full
- Network outage
- Server outage
- Manifest corruption
- Concurrent local changes
- Offline favourites
- DST transitions
- Time-zone changes

### Display

- Empty photo library
- Corrupted cached image
- Album switching
- Touch navigation
- Inactivity timeout
- Sleep scheduling
- Renderer restart

### Updater

- Successful A/B update
- Invalid signatures
- Expired metadata
- Corrupted artifact
- Interrupted download
- Power failure before activation
- Power failure after activation
- Failed application startup
- Failed health checks
- Incompatible database migration
- Interrupted rollback
- Insufficient disk space
- Unavailable release server
- Attempted downgrade
- Repeated boot failures

Create a simulator for frame behaviour on Linux AMD64.

Use real temporary SQLite databases and filesystem operations in integration tests.

Do not rely exclusively on mocks for persistence and update recovery.

---

# 21. Hardware documentation

Maintain `docs/hardware/`.

Document:

- Raspberry Pi 3B+ compatibility
- HDMI configuration
- USB touchscreen input
- Display resolution
- Power requirements
- Backlight controls
- Thermal considerations
- Mounting-hole locations
- Cable clearance
- Enclosure dimensions
- Cooling requirements

Do not invent physical dimensions.

Record measured values separately from manufacturer specifications.

The eventual enclosure will be designed using `@jscad/modeling` and printed on Flashforge AD5M/AD5X printers.

Design software and hardware assumptions so the electronics can be replaced without rebuilding the management platform.

---

# 22. Development milestones

Work in the following order.

## Milestone 1 — Foundation

Implement:

- Repository structure
- Go module
- CLI
- Configuration
- Logging
- Database migration framework
- Server bootstrap
- Frame bootstrap
- Health endpoint
- Build scripts
- Initial CI
- Tests

## Milestone 2 — Server MVP

Implement:

- Authentication
- Users
- Photo uploads
- Image derivatives
- Albums
- SQLite metadata
- Local filesystem storage
- Basic web interface

## Milestone 3 — Offline-first synchronisation

Implement:

- Device identity
- Manifests
- ETags
- Download queue
- Scheduled synchronisation
- Local SQLite cache
- Atomic activation
- Offline resilience
- Headless simulator

## Milestone 4 — Touchscreen interface

Implement:

- Native renderer
- Slideshow
- Gallery
- Albums
- Captions
- Favourites
- Touch input
- Sleep scheduling

## Milestone 5 — Multi-frame management

Implement:

- Pairing
- Device assignment
- Per-frame configuration
- Health reporting
- Remote sync requests
- Administration dashboard

## Milestone 6 — Secure updater

Implement:

- Independent updater
- A/B binary slots
- Signed release metadata
- Artifact verification
- Atomic activation
- Health validation
- Automatic rollback
- Update scheduling
- Release channels

## Milestone 7 — Production hardening

Implement:

- Backup and restore
- Recovery testing
- Security review
- Performance optimisation
- Deployment documentation
- Long-duration reliability tests

---

# 23. Engineering workflow

At the beginning of each milestone:

1. Inspect the existing repository.
2. Review relevant architectural decisions.
3. Identify dependencies and risks.
4. Produce a concise implementation plan.

During implementation:

- Write real code.
- Keep changes focused.
- Add meaningful tests.
- Avoid unnecessary dependencies.
- Maintain clear interfaces.
- Document non-obvious decisions.
- Preserve backward compatibility where required.

At the end of each milestone:

1. Format the code.
2. Run static analysis.
3. Run relevant tests.
4. Build supported targets.
5. Update documentation.
6. Summarise what works.
7. Report anything incomplete.
8. Record test results.
9. Commit the changes with a meaningful message.

Never fabricate test results.

Do not mark a milestone complete while its acceptance criteria remain unmet.

Do not silently replace requested functionality with placeholders.

---

# 24. Initial implementation instructions

Begin by inspecting the repository.

If the repository is empty, initialise it.

Create a concise architectural plan and the necessary architecture decision records.

Then implement **Milestone 1 — Foundation**.

The initial deliverable must provide:

- A working `openframe` CLI
- A functioning server health endpoint
- A functioning headless frame mode
- Configuration loading
- Structured logging
- SQLite migrations
- Graceful shutdown
- Linux AMD64 and ARM64 builds
- Automated tests
- GitHub Actions CI
- Installation and development documentation

Do not begin the touchscreen renderer or software updater implementation during Milestone 1.

However, establish the package boundaries and architectural decisions needed to support them later.

After Milestone 1 is complete, report the implementation, tests, architectural decisions and remaining risks before proceeding to Milestone 2.

The final objective is a reliable, remotely managed, self-hosted digital photo frame platform that can operate unattended for years without dependence on a proprietary cloud service.