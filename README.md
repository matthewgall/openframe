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

OpenFrame is under active development in milestones. **Milestone 2
(Server MVP)** is complete:

| Milestone | Scope | Status |
|---|---|---|
| 1 | Foundation: CLI, config, logging, migrations, server health, headless frame, CI | ✅ complete |
| 2 | Server MVP: auth, users, photo upload + derivatives, albums, web UI | ✅ complete |
| 3 | Offline-first synchronisation: manifests, ETags, download queue, atomic activation | planned |
| 3 | Offline-first synchronisation: manifests, ETags, download queue, atomic activation | planned |
| 4 | Touchscreen interface: native renderer, slideshow, gallery, favourites, sleep | planned |
| 5 | Multi-frame management: pairing, assignments, health, admin dashboard | planned |
| 6 | Secure updater: A/B slots, signed releases, rollback, channels | planned |
| 7 | Production hardening: backups, security review, long-run reliability | planned |

See [docs/architecture/overview.md](docs/architecture/overview.md) for the
system design and [docs/adr/](docs/adr/) for recorded decisions.

## Commands

```bash
openframe server    # run the central management server
openframe frame     # run the frame application (headless renderer in M1)
openframe pair      # show or initialise frame pairing state
openframe doctor    # diagnose the local system (human or --json)
openframe status    # show local OpenFrame status
openframe version   # print version information (--json)
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
frame:
  server_url: https://frame.example.com
  display:
    driver: headless             # headless (dev) | auto | drm (M4)
    width: 1280
    height: 800
    brightness: 0.8
  slideshow:
    interval: 45s
    order: sequential            # sequential | random
    fit: contain                 # contain | cover | crop
  sync:
    manifest_interval: 60m
    policy: hybrid               # immediate | scheduled | hybrid
    download_window: { start: "01:00", end: "06:00", tz: Europe/London }
  sleep:
    on: "07:00"
    off: "22:00"
    tz: Europe/London
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
│       ├── originals/<sha256>
│       └── derivatives/<sha256>.<variant>
├── frame/
│   ├── frame.db              # frame SQLite (identity, state; M3: cache metadata)
│   └── photos/               # M3: content-addressed photo cache
└── updates/                  # M6: updater state
```

## Building

```bash
make build        # host binary
make build-all    # linux/amd64 + linux/arm64 in dist/ with SHA256SUMS.txt
make test         # unit + integration tests
make test-race    # with race detector
make vuln         # govulncheck
make web          # rebuild the admin SPA (requires Node)
```

The Go build requires no C toolchain (pure-Go SQLite driver) and no Node
(the SPA dist is committed). Cross-compiling for Raspberry Pi 3B+ is a
plain `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build`.

## Repository layout

```
cmd/openframe/          CLI entry point
internal/
  cli/                  command implementations
  config/               YAML + env configuration
  db/                   SQLite (modernc) + migration runner
  frame/                frame agent (identity, state, components)
  frame/display/        display driver boundary + headless renderer
  logging/              slog setup
  platform/             host introspection (doctor)
  protocol/             wire protocol version constants
  server/               management server
  updater/              M6 boundary (update coordinator)
  version/              build metadata
migrations/             embedded SQL migrations (server/, frame/)
web/
  admin/                admin SPA (Vite + TypeScript, source)
  admin/dist/           built SPA (committed, embedded in the binary)
  embed.go              embed + static serving
docs/                   architecture, API, hardware, security, ADRs
.github/workflows/      CI (PR checks) and release pipeline
```

## Documentation

- [Architecture overview](docs/architecture/overview.md)
- [REST API conventions](docs/api/overview.md)
- [Development guide](docs/development.md)
- [Hardware: Raspberry Pi 3B+](docs/hardware/pi-3b-plus.md)
- [Threat model (skeleton)](docs/security/threat-model.md)
- [Architecture decision records](docs/adr/README.md)

## License

Apache License 2.0 — see [LICENSE](LICENSE).
