# Production deployment runbook (M7)

Operational reference for running OpenFrame in production: the
management server behind TLS, scheduled backups, frame card hygiene,
and the incident runbooks (T-06 breach/rebuild, T-10 restore).

## 1. Management server

### Hardware sizing (measured, not theoretical)

| Component | Requirement |
|---|---|
| RAM | **512 MB minimum** (256 MB works if you never upload large photos) |
| CPU | 1 vCPU is fine; the 12 MP pipeline is single-request serial |
| Disk | 1 GB minimum (DB + photos; see backups for growth) |

Why 512 MB: the server idles at ~20–30 MB RSS, but a single 12 MP
photo upload peaks at **~355 MB of allocations** in the derivative
pipeline (M7 benchmarks, `docs/performance.md`). Anything less and
the kernel's OOM killer — not your code — decides what dies.

Lessons from a real 93 MB VPS attempt (2026-11-14):

- A Go process could not even finish `exec` (reading the 14 MB
  binary wedged in the block layer) and the OOM killer took it out
  mid-initialisation. 93 MB is not a workable server box.
- If you must run small: raise `vm.swappiness` (60), back RAM with
  swap (zram preferred), and set `GOMEMLIMIT` in the service
  environment to *below* total capacity (RAM+swap) so the GC
  applies pressure before the kernel does. The 12 MP pipeline will
  then thrash swap instead of being killed — slow, but alive.
- Verify a box can run the binary at all before wiring up services:
  `openframe version` should return in well under a second.

### Non-systemd hosts (Alpine / OpenRC)

The init contract is: run in foreground, write nothing to stdout but
logs, handle SIGTERM with a graceful shutdown — all true. On OpenRC
a foreground daemon needs `command_background=1` plus a pidfile, so
use a wrapper that writes the pidfile then `exec`s the server (so the
PID OpenRC tracks stays stable):

```sh
# /opt/openframe/run  (0755)
#!/bin/sh
echo $$ > /var/lib/openframe/openframe.pid
exec /opt/openframe/openframe server --config /etc/openframe/server.yaml
```

```sh
# /etc/init.d/openframe  (0755)  — then: rc-update add openframe default
#!/sbin/openrc-run
name=openframe
command=/opt/openframe/run
command_user=openframe
command_background=1
pidfile=/var/lib/openframe/openframe.pid
environment="GOMEMLIMIT=400MiB"
outputlog="/var/log/openframe.log"
errorlog="/var/log/openframe.log"
depend="net location"
```

### Reverse proxy + TLS (threat model T-09)

The server listens plain on its bind address and must be fronted by a
TLS-terminating reverse proxy. `server.public_base_url` must be the
public HTTPS URL (the server warns at every start if it is unset or
not HTTPS; the session cookie gets its `Secure` flag from it).

nginx reference:

```nginx
server {
    listen 443 ssl;
    http2 on;
    server_name openframe.example.com;

    ssl_certificate     /etc/ssl/openframe/fullchain.pem;
    ssl_certificate_key /etc/ssl/openframe/privkey.pem;
    ssl_protocols       TLSv1.2 TLSv1.3;

    # Photo uploads and release artifacts can be large:
    client_max_body_size 600m;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host              $host;
        proxy_set_header X-Forwarded-For   $remote_addr;
        proxy_set_header X-Forwarded-Proto https;
        # Long sync downloads:
        proxy_read_timeout 600s;
    }
}
server {
    listen 80;
    server_name openframe.example.com;
    return 301 https://$host$request_uri;
}
```

Notes:

- The frame bind address (`server.listen`) should stay private
  (`127.0.0.1` or the LAN); only the proxy's HTTPS endpoint is
  public. Frames initiate every connection — no inbound ports on
  frames.
- `client_max_body_size` must exceed the largest release artifact
  you will host (the API's own cap is 500 MiB) plus photo uploads.

### systemd

`/etc/systemd/system/openframe-server.service`:

```ini
[Unit]
Description=OpenFrame management server
After=network-online.target
Wants=network-online.target

[Service]
User=openframe
Group=openframe
ExecStart=/usr/local/bin/openframe server --config /etc/openframe/server.yaml
Restart=always
RestartSec=5
MemoryMax=512M

[Install]
WantedBy=multi-user.target
```

`/etc/openframe/server.yaml` (owner `openframe:openframe`, 0600):

```yaml
server:
  listen: 127.0.0.1:8080
  public_base_url: https://openframe.example.com
data_dir: /var/lib/openframe
```

## 2. Backups (T-06 / T-10)

### What a backup contains

`openframe backup server --out DIR` (safe while the server runs —
the database snapshot is a consistent `VACUUM INTO` image):

```
DIR/
  backup-info.json   kind, time, version, counts
  manifest.json      every data file: path, size, sha256
  server.db          consistent snapshot
  photos/originals/<sha256>
  photos/derivatives/<sha256>.<variant>
```

`openframe backup frame --out DIR` (frame user): `frame.db`, the
0600 credential, and the updater `state.json` when readable
(root-owned; otherwise recorded as skipped).

A backup is only "good" when `openframe backup verify --dir DIR`
passes — it re-hashes every file and integrity-checks the database.
`restore` runs the same verification before touching anything.

### Schedule (systemd timer example, off-host)

Backups next to the data only protect against software failure. For
hardware-level safety ship them to a separate mount (NAS, second
disk, remote rsync target):

```ini
# /etc/systemd/system/openframe-backup.service
[Unit]
Description=OpenFrame server backup

[Service]
Type=oneshot
User=openframe
ExecStart=/usr/local/bin/openframe backup server --out /mnt/backups/openframe/server-%Y%m%d-%H%M%S
ExecStartPost=/usr/bin/rsync -a --delete-after /mnt/backups/openframe/server- /backup-nas/openframe-server/
```

```ini
# /etc/systemd/system/openframe-backup.timer
[Unit]
Description=Daily OpenFrame backup

[Timer]
OnCalendar=*-*-* 03:30:00
Persistent=true

[Install]
WantedBy=timers.target
```

Keep at least 7 daily + 4 weekly copies (the rsync target above with
`--keep-extras`/retention policy of your choice). Periodically
**prove** a backup works: `openframe backup verify --dir <copy>` in
the same timer unit's `ExecStartPost`, or a monthly manual restore
onto scratch space.

### Restore procedure (T-10)

1. `systemctl stop openframe-server` (the service being restored).
2. Verify the backup: `openframe backup verify --dir DIR`.
3. Restore: `openframe restore server --from DIR --yes`.
   Existing live data is preserved beside the target as
   `<name>.pre-restore-<stamp>` — nothing is deleted.
4. `openframe doctor` → start the service → confirm
   `GET /healthz` and that a frame syncs.
5. Keep the `.pre-restore-*` files for one week, then delete them
   deliberately.

Frame restore is the same shape: stop the frame, `openframe restore
frame --from DIR --yes`, restart. The restored credential is the
server-issued one — if you restored an old frame backup onto a
frame whose credential was rotated server-side in the meantime,
re-pair or re-issue the credential from the admin UI.

## 3. Compromised management server (T-06)

Assume breach. Goal: rebuild with zero attacker foothold, minimum
frame disruption.

1. **Stop and isolate.** `systemctl stop openframe-server`; take the
   host out of service or firewall it. Keep the disk image for
   forensics (do NOT restore from a compromised host's own backups
   before step 4's verification).
2. **Assume these are gone:** admin passwords, session secrets,
   device credential hashes, audit history. Photos and release
   artifacts are content-addressed and were only ever read — the
   risk there is exfiltration, not corruption.
3. **Rebuild from a verified backup:**
   - Fresh OS + software (no layering on the compromised install).
   - `openframe backup verify --dir <backup>` → `openframe restore
     server --from <backup> --yes`.
   - **Rotate every admin password** (the old password hashes in the
     restored DB are compromised) and **re-issue every device
     credential** from the admin UI (frames pick up the new token on
     their next sync; un-updated frames simply stop syncing until
     then — the slideshow keeps running offline).
4. **Verify and return to service:** doctor, healthz, one full frame
   sync, audit log fresh from the rebuild point.
5. Frames never needed any inbound access from the server, so they
   were never directly attackable through it; their worst case is a
   stale cache until the next successful sync.

## 4. Frame SD card (T-02)

The card holds: `frame.db` (device identity — not a secret), the
0600 **credential file** (a live credential until revoked), and the
photo **cache** (family photos, useful only with credential +
server). A stolen card is therefore treated as "credential
compromise + photo disclosure", never as a server risk.

### Wipe procedure (lost/stolen frame)

1. Admin UI → Frames → device → **revoke credential** (immediate:
   the card's credential stops working server-side).
2. If the card is recoverable, wipe it:
   ```sh
   # Overwrite the data partition (not just delete):
   dd if=/dev/zero of=/dev/mmcblk0p2 bs=1M status=progress
   ```
   (or re-flash a clean OpenFrame image — the frame re-pairs and
   re-downloads its collection from the server; originals never left
   the server).
3. Rotate the affected admin account only if you believe the card's
   credential file was read *and* you cannot rule out further
   exposure.

### Optional: full-disk encryption

For the hardware-grade option, image the card with an encrypted
(LUKS) data partition and unlock it at boot from a key on the frame
(Raspberry Pi OS supports this with `cryptsetup` + a keyfile). The
OpenFrame layout is unchanged (the data dir is a mount point).

## 5. Long-duration reliability (M7 soak)

```
make soak        # repeated -race suites + a sustained server/frame
                 # load test in one go (see Makefile)
```

The soak runs: the full test suite three times with the race
detector, then a sustained mixed-load test (concurrent uploads,
manifest builds, frame sync cycles) for a bounded duration, asserting
no goroutine leaks in the server and no dropped syncs. It is meant
to run unattended overnight; `go test -timeout 0` is not used — the
target has a hard deadline so a hung run fails loudly.

## 6. Routine operations

| Task | Command |
|---|---|
| System health | `openframe doctor` (server and frame hosts) |
| Verify a backup | `openframe backup verify --dir DIR` |
| Update a frame fleet | admin UI → Releases (see `docs/updater/deployment.md`) |
| Check one frame's update state | `openframe update status` / `update check` |
| Rotate a frame credential | admin UI → Frames → device → credential |
| Audit trail | admin UI → Audit log (also `GET /api/v1/audit`) |
