# OpenFrame

A privacy-respecting, self-hosted digital photo frame platform.

Family members upload photographs through a web interface; the photos are
stored on infrastructure you control and synchronised automatically to one
or more physical digital photo frames running on Raspberry Pi hardware.
The frames display photographs all day and let recipients browse albums on
a touchscreen — and everything keeps working when the internet doesn't.

There is no proprietary frame provider, no third-party telemetry, and no
cloud dependency. One Go codebase produces the management server, the
frame application and (from milestone 6) the signed auto-updater.

## Status

OpenFrame is under active development in milestones. **Milestone 7
(Production hardening)** is complete — the platform is feature-
complete against the specification:

| Milestone | Scope | Status |
|---|---|---|
| 1 | Foundation: CLI, config, logging, migrations, server health, headless frame, CI | ✅ complete |
| 2 | Server MVP: auth, users, photo upload + derivatives, albums, web UI | ✅ complete |
| 3 | Offline-first synchronisation: manifests, ETags, download queue, atomic activation | ✅ complete |
| 4 | Touchscreen interface: DRM/KMS renderer, slideshow, gallery, search, settings, touch, sleep, thermal | ✅ complete (hardware validation pending) |
| 5 | Multi-frame management: pairing protocol, health reporting, per-frame config, admin dashboard | ✅ complete |
| 6 | Secure updater: A/B slots, signed releases, rollback, channels, staged rollout, admin releases | ✅ complete (hardware validation pending) |
| 7 | Production hardening: backup/restore + recovery tests, security review, performance, production runbook, soak | ✅ complete (Pi hardware validation carried as the sole open item) |

Since M7: **S3-compatible photo storage** (AWS S3, Cloudflare R2,
MinIO, ...) — `server.photo_storage.backend: s3`, hand-rolled SigV4,
no cloud SDK (ADR-0012). Local filesystem remains the default.

See [docs/architecture/overview.md](docs/architecture/overview.md) for the
system design and [docs/adr/](docs/adr/) for recorded decisions.

## Commands

```bash
openframe server    # run the central management server
openframe frame     # run the frame application (headless renderer in M1)
openframe pair      # device identity + pairing state and next steps
openframe doctor    # diagnose the local system (human or --json)
openframe status    # show local OpenFrame status
openframe update    # frame-side update ops: status | check | request | reset | set-channel
openframe backup    # back up + verify data (M7): server | frame | verify
openframe restore   # restore from a verified backup (M7): server | frame
openframe version   # print version information (--json)

openframe-updater   # root A/B slot updater (separate binary, M6)
```

## Quickstart (development)

Prerequisites: Go ≥ 1.26. Node ≥ 20 only needed to rebuild the web UI
(the built assets are committed and embedded in the binary).

```bash
make build          # build bin/openframe
make run-server     # server on :8080, state in ./.data-dev
# second terminal
make run-frame      # headless frame agent, same state dir

openframe doctor    # what does this machine look like?
openframe status    # what is the local frame doing?
```

Open http://localhost:8080 for the admin web UI.

### First run (server)

The very first time the server starts with an empty database it creates
an initial administrator and logs the sign-in credentials exactly once:

```json
{"level":"INFO","msg":"first run: created initial administrator (one-time temporary password — change it after login)","first_run":true,"email":"admin@localhost","temporary_password":"OFPD-K4XT-9M2Q-V7ZR"}
```

Sign in at http://localhost:8080 with that password (change it via the
account menu afterwards). This is the single documented exception to the
“no secrets in logs” rule — see `docs/security/threat-model.md` (T-14).

### Pairing a frame (M5)

A frame pairs itself — no one types a token anywhere:

1. Point the frame at the server (`frame.server_url` in config) and
   start it: `openframe frame`. The frame shows a **pairing screen**
   with an 8-character code.
2. In the admin UI, open **Frames → pending requests** and enter the
   code. Approving registers the device (with the name the frame
   offered) and issues its credential.
3. The frame detects the approval within ~10 s, claims the credential
   (one-time — the server nulls it after the first claim) and starts
   syncing. The credential is stored in a 0600 file in the frame state
   directory; the server stores only its SHA-256 hash.

The manual path still works for scripted deployments: register the
device with `openframe pair`'s id on the Frames page, issue a
credential from the UI, and set `frame.device_token` — a config token
always wins over a paired one.

Once paired, the frame polls the conditional manifest (ETag/304),
downloads assigned photo derivatives into the content-addressed cache,
verifies SHA-256, and atomically activates new collections. While
offline it keeps displaying the last good collection; local
favourites are queued and replayed on reconnect.

The admin UI's **Frames** page also shows live health (last seen,
sync state, disk/memory/temperature, display driver), lets you rename
a frame, **trigger a sync** (delivered on the frame's next heartbeat —
the server never initiates connections), and set **per-frame
configuration** (slideshow interval, transition, sleep window,
brightness, orientation, clock). Precedence on the frame:
local settings > server config > config file.

## Configuration

Configuration is YAML with environment overrides. Precedence:

1. built-in defaults
2. `--config openframe.yaml`
3. `OPENFRAME_*` environment variables

```yaml
data_dir: /var/lib/openframe     # defaults to ~/.openframe
log:
  level: info                    # debug | info | warn | error
  format: json                   # json | text
server:
  listen: ":8080"
  public_base_url: https://frame.example.com
  session_ttl: 720h                # admin session lifetime (default 30d)
  max_upload_bytes: 52428800       # per-file upload cap (default 50 MiB)
  max_upload_files: 50             # per-request file cap
  max_decode_dimension: 8000       # px, reject larger images
  login_rate_limit_per_min: 5      # per-IP failed/at-all login attempts
  photo_storage:                   # where photo bytes live (ADR-0012)
    backend: local                 # local | s3 (default local)
    s3:                            # S3-compatible: AWS S3, Cloudflare R2, MinIO, ...
      endpoint: ""                 # empty = https://s3.<region>.amazonaws.com
      region: us-east-1
      bucket: family-photos
      access_key_id: ""
      secret_access_key: ""
      path_style: false            # true for R2/MinIO; false for AWS S3
      prefix: ""                   # optional key namespace inside the bucket
frame:
  server_url: https://frame.example.com
  display:
    driver: auto                 # headless | auto | drm
    width: 1280                  # headless canvas (drm uses the panel mode)
    height: 800
    brightness: 0.8
    orientation: auto            # auto | 0 | 90 | 180 | 270
    card: /dev/dri/card0         # driver=drm only
    snapshot_dir: /var/log/openframe/snapshots   # headless visual simulator (dev)
  slideshow:
    interval: 45s
    order: sequential            # sequential | random
    fit: contain                 # contain | cover | crop
    show_caption: true
    show_clock: true
    transition: crossfade        # none | crossfade | slide
    transition_duration: 600ms
  sync:
    manifest_interval: 60m
    policy: hybrid               # immediate | scheduled | hybrid
    download_window: { start: "01:00", end: "06:00", tz: Europe/London }
  sleep:
    on: "07:00"
    off: "22:00"
    tz: Europe/London
  touch:
    enabled: true
    device: ""                   # empty = auto-detect the touchscreen
  thermal:
    enabled: true
    threshold_c: 70              # slideshow interval doubles above this
  update:
    channel: stable              # stable | beta | development
    policy: automatic            # automatic | download_only | manual
    maintenance_window: { start: "02:00", end: "05:00", tz: Europe/London }
```

Every key can be set via environment: the YAML path becomes an uppercased
`OPENFRAME_`-prefixed name, e.g. `OPENFRAME_SERVER_LISTEN`,
`OPENFRAME_FRAME_SYNC_POLICY`, `OPENFRAME_LOG_LEVEL`.

## State layout

```
/var/lib/openframe/           # data_dir
├── server/
│   ├── server.db             # server SQLite metadata
│   └── photos/               # M2: content-addressed originals + derivatives
│       ├── originals/<sha256>  # (with server.photo_storage.backend: s3 these
│       └── derivatives/<sha256>.<variant>  # live in the bucket instead, ADR-0012)
├── frame/
│   ├── frame.db              # frame SQLite (identity, state; M3: sync/cache;
│   │                         #        M4: local settings; M5: server config summary)
│   ├── credential            # M5: device credential (0600, written atomically)
│   └── photos/               # M3: content-addressed photo cache
└── updates/                  # M6: updater state.json + downloaded artifacts
```

The updater's **install root** (A/B binary slots, the `current`
symlink and the device-pinned trust file) is separate from the data
dir and defaults to `/opt/openframe`:

```
/opt/openframe/
├── current        -> slots/a/openframe           (atomic activation target)
├── slots/a/openframe, slots/b/openframe         (fixed A/B slots)
└── trust/trusted-keys.json                        (device-pinned Ed25519 keys)
```

Slot versions live in `state.json`, not in directory names. See
[docs/updater/deployment.md](docs/updater/deployment.md).

## Running the frame (M4–M5)

```bash
openframe frame        # on the Pi: driver=auto picks the DRM panel
openframe doctor       # display / touch / thermal / pairing checks
openframe status       # pairing + sync state of the local frame
```

The frame UI is touch-first (swipe/tap/double-tap/hold). On development
machines without a panel, set `frame.display.driver: headless` and
`frame.display.snapshot_dir`: every presented frame is written to
`<snapshot_dir>/latest.png` (plus a small timestamped ring), so the
complete UI — pairing screen, slideshow, gallery, search, settings,
empty states — is observable without hardware (the spec's "development
renderer").

On the Pi the DRM renderer takes the panel directly (KMS atomic
commits, pure Go, no X11/Wayland/SDL). An unpaired frame shows the
pairing screen; pairing and day-to-day operation are documented under
[Pairing a frame](#pairing-a-frame-m5).

## Auto-update (M6)

The updater is a **separate root binary** — the frame never updates
itself. Releases are Ed25519-signed; trust roots are pinned on the
device, never served by the release server; activation is one atomic
symlink rename over an A/B slot pair, and every failure path ends in
an offline rollback.

- Upload a signed release in the admin UI (Releases), approve it, and
  expand the rollout (allow-listed devices or a stable percentage).
- Per-frame channel: admin UI (per device) or on-device
  `openframe update set-channel beta`.
- Frame-side read-only ops: `openframe update status|check` (check
  verifies the release against the pinned keys before reporting).
- Deployment layout, systemd units and operations (rollback, key
  rotation): [docs/updater/deployment.md](docs/updater/deployment.md)
- The complete trust and anti-rollback model, with its test matrix:
  [docs/updater/trust-model.md](docs/updater/trust-model.md)

An unavailable update server never affects the slideshow or
synchronisation.

## Backups and recovery (M7)

Backups are self-verifying directories (per-file SHA-256 manifest +
`PRAGMA integrity_check`) that can be taken **while the service runs**
(consistent `VACUUM INTO` database snapshot):

```bash
openframe backup server --out /mnt/backups/server-$(date +%F)
openframe backup verify --dir /mnt/backups/server-...
openframe restore server --from /mnt/backups/server-... --yes
```

Restore re-verifies before touching anything, preserves existing live
data as `.pre-restore-*`, and leaves the backup intact for reuse. The
frame side works the same way (`backup/restore frame`). With an S3
photo backend (`server.photo_storage.backend: s3`) the backup snapshots
the catalog, not the objects — the bucket is the object backup (the
note is recorded in `backup-info.json`). Scheduling, off-host storage,
the R2 setup guide and the breach-rebuild runbook:
[docs/deployment/production.md](docs/deployment/production.md).

Measured performance and the soak procedure:
[docs/performance.md](docs/performance.md).

## Building

```bash
make build        # host binaries (openframe + openframe-updater)
make build-all    # all supported targets in dist/ with SHA256SUMS.txt
make test         # unit + integration tests
make test-race    # with race detector
make vuln         # govulncheck
make bench        # performance benchmarks (photos pipeline, manifest)
make soak         # long-duration reliability: repeated race suites + load
make web          # rebuild the admin SPA (requires Node)
```

The build is pure Go (no cgo, no Node), so `make build-all` produces
both binaries for **every** supported target on any host:
linux/{amd64,arm64,armv6,armv7,386,riscv64}, darwin/{amd64,arm64},
windows/{amd64,arm64}. The DRM display and evdev touch backends are
Linux-only (build-tagged); elsewhere the frame runs the headless dev
renderer. CI cross-compiles the same matrix on every push.

## Repository layout

```
cmd/openframe/          CLI entry point
  openframe-updater/    root A/B slot updater (M6)
internal/
  cli/                  command implementations
  config/               YAML + env configuration
  db/                   SQLite (modernc) + migration runner
  frame/                frame agent (identity, state, components, UI wiring)
  frame/cache/          content-addressed photo cache (M3)
  frame/sync/           manifest/download/changes sync engine (M3)
  frame/display/        display driver boundary + headless simulator
  frame/display/drm/    pure-Go DRM/KMS renderer (M4, ADR-0006)
  frame/canvas/         framebuffer compositing + bitmap font (M4)
  frame/ui/             touchscreen UI: slideshow/gallery/search/settings (M4)
  frame/touch/          evdev reader + gesture recognizer (M4)
  frame/sleep/          daily display sleep schedule (M4)
  frame/thermal/        overheat monitor (M4)
  frame/pairing/        M5 pairing client (challenge/poll/claim)
  server/pairing/       M5 pairing store (codes, rate limits, one-time claims)
  server/devices/       M3+M5 credentials, health, per-frame config
  server/manifest/      frame manifest builder (M3, M5 config block)
  backup/               M7 backup/restore (self-verifying snapshot dirs)
  logging/              slog setup
  platform/             host introspection (doctor)
  protocol/             wire protocol version constants
  server/               management server
  updater/              M6 release metadata, trust store, verifier, slots, state, coordinator
  version/              build metadata
migrations/             embedded SQL migrations (server/, frame/)
web/
  admin/                admin SPA (Vite + TypeScript, source)
  admin/dist/           built SPA (committed, embedded in the binary)
  embed.go              embed + static serving
docs/                   architecture, API, deployment, hardware, security,
                        performance, updater, ADRs
.github/workflows/      CI (PR checks) and release pipeline
```

## Documentation

- [Architecture overview](docs/architecture/overview.md)
- [REST API conventions](docs/api/overview.md)
- [Development guide](docs/development.md)
- [Production deployment runbook](docs/deployment/production.md)
- [Hardware: Raspberry Pi 3B+](docs/hardware/pi-3b-plus.md)
- [Threat model](docs/security/threat-model.md) + [M7 security review](docs/security/review.md)
- [Updater trust model](docs/updater/trust-model.md)
- [Updater deployment + operations](docs/updater/deployment.md)
- [Performance + soak](docs/performance.md)
- [Architecture decision records](docs/adr/README.md)

## License

Apache License 2.0 — see [LICENSE](LICENSE).
