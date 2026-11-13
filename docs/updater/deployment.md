# Updater deployment (M6)

## Layout on a frame (Raspberry Pi, Pi 3B+ or later)

```
/opt/openframe/
  current        -> slots/a/openframe             (atomic activation target)
  slots/
    a/openframe                                (slot A: the running binary)
    b/openframe                                (slot B: candidate/last-good)
  trust/
    trusted-keys.json                            (device-pinned Ed25519 keys)
/var/lib/openframe/
  updates/
    state.json                                   (update state machine, root-owned)
    openframe-v1.3.0-<sha256-prefix>             (downloaded artifact, verified)
    update-request.json                          (frame CLI "request" marker)
```

Slots are fixed names (`a`/`b`), not versioned directories — the
version each slot holds lives in `state.json`, which is the source of
truth for everything version-related. Two slots are enough: one holds
what is running, the other holds either the candidate or the
last-committed good binary.

Rules:

- The running binary is **never overwritten in place**: installs go
  to the inactive slot (after verification); activation is one atomic
  `rename(2)` of a temporary symlink over `current`.
- The previous active slot becomes the rollback target the moment the
  new slot is activated, before the new binary is even probed; a
  failed candidate is left in place (with its state) for inspection.
- `state.json` is written atomically (temp + rename + fsync) at
  every phase transition; the updater recovers from a power cut by
  reconciling the state against the slots on boot.

## Users

```
sudo useradd --system --home-dir /var/lib/openframe \
  --shell /usr/sbin/nologin openframe
sudo mkdir -p /var/lib/openframe
sudo chown openframe:openframe /var/lib/openframe
```

The `openframe` service runs as `openframe` (unprivileged);
`openframe-updater` runs as root. The updater reads the frame DB
read-only (schema version, effective update channel) and never
writes to it; the frame reads the updater's `state.json` read-only
(health reporting). The two binaries share no writable files.

## systemd units

`/etc/systemd/system/openframe-frame.service`:

```ini
[Unit]
Description=OpenFrame digital photo frame
After=network-online.target
Wants=network-online.target

[Service]
User=openframe
Group=openframe
ExecStart=/opt/openframe/current --config /etc/openframe/frame.yaml
Restart=always
RestartSec=5
# The frame must come back even after a slot swap: it execs whatever
# "current" points at.
ExecStartPre=+/usr/bin/test -x /opt/openframe/current

[Install]
WantedBy=multi-user.target
```

`/etc/systemd/system/openframe-updater.service`:

```ini
[Unit]
Description=OpenFrame updater (root, A/B slots)
After=openframe-frame.service network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/openframe-updater \
  --root /opt/openframe \
  --state-dir /var/lib/openframe/updates \
  --server https://openframe.example.com \
  --service systemd \
  --unit openframe-frame \
  --device-id <DEVICE_ID> \
  --window-start 02:00 --window-end 05:00 --window-tz Europe/London \
  --check-interval 6h
Restart=always
RestartSec=30
# The updater is small and idle between checks.
MemoryMax=200M

[Install]
WantedBy=multi-user.target
```

Notes:

- `--device-id` must match the frame's device id (shown on the
  pairing screen and in the admin dashboard) for staged-rollout
  membership to work.
- Post-activation readiness in systemd mode is "the unit is active"
  (plus an optional `--health-url` probe if the frame exposes a
  local health endpoint; the v1 frame reports health outbound to the
  management server instead, so the unit-active check is the gate and
  boot-reconcile is the backstop).
- Channel is **not** set here: it is resolved at runtime — explicit
  `--channel` flag > frame DB (admin UI / `openframe update
  set-channel`) > `stable`.
- `--health-url` is the frame's local health endpoint used for
  post-activation readiness (systemd mode).

## Installation steps (operator)

1. Build or fetch the release from the server's artifact endpoint:
   `GET /api/v1/releases/latest.json?channel=stable&os=linux&arch=arm64`
   gives the signed metadata; download the artifact it names and verify
   its SHA-256 before installing.
2. Install `openframe` into slot `a` plus the trust file:
   ```sh
   sudo install -d -m 0755 /opt/openframe/slots/a /opt/openframe/slots/b
   sudo install -m 0755 openframe /opt/openframe/slots/a/openframe
   sudo install -d -m 0700 /opt/openframe/trust
   sudo install -m 0600 trusted-keys.json /opt/openframe/trust/
   ```
3. Create the symlink: `sudo ln -sfn slots/a/openframe /opt/openframe/current`
4. Install the units, then `systemctl daemon-reload && systemctl
   enable --now openframe-frame openframe-updater`.

The updater's first run reconciles the existing layout (it adopts the
active slot as `last_good` when `state.json` is absent) — a manual
install needs no updater-side registration.

## Operations

### Check what a frame would do (read-only)

```sh
openframe update check            # as the frame user
openframe update check --json
openframe-updater --status        # as root: slots, phase, floors, cooldown
```

### Force an update now

```sh
sudo systemctl restart openframe-updater   # or, one-shot:
sudo openframe-updater --once --force
# or, from the frame side (marks a request the updater honours):
openframe update request
```

### Rollback (admin, from the server side)

Publish the previous version as a **rollback release** (`is_rollback:
true` in the signed metadata — the release tooling sets this when
cutting a revert). The frame's updater installs it through the normal
verified flow. If the frame is broken before it can update (bad
active binary), the updater's boot-reconcile already rolled back to
the last committed slot offline; if that is insufficient, an operator
with physical access can point `current` at the other slot manually
(`openframe-updater --status` shows which slot is last-good):

```sh
sudo systemctl stop openframe-frame
sudo ln -sfn slots/b/openframe /opt/openframe/current
sudo systemctl start openframe-frame
```

After a manual swap, check `openframe-updater --status`: if the state
file no longer matches the layout (phase, active slot), clear it with
`openframe update reset --state` so the updater re-derives from the
layout on its next run.

### Change a frame's channel

- Admin UI → Frames → device → *Configuration* → **Update channel**
  (pushed via the manifest; the frame persists it).
- On-device: `openframe update set-channel beta` (local override,
  wins over the admin value).
- Deployment-time escape hatch: `--channel` on the updater unit.

### Key rotation

1. Generate the new key pair (release tooling).
2. Publish a release signed with the **old** key whose
   `new_keys` includes the new key's public half.
3. Frames verify and persist the merged trust set during the update.
4. Once all frames have accepted the rotation, the operator may
   remove the old key from device trust files at their discretion
   (leaving it in is safe — trust is additive and device-pinned).

## Security posture

- Release server compromise: every rejection rule and the unsigned
  serving block's limits are specified in
  [`trust-model.md`](./trust-model.md); the frame only ever runs a
  binary whose signed digest was checked byte-for-byte.
- Updater compromise: the updater can only install binaries that the
  verifier accepts (its code is the verifier). Its blast radius is
  the frame machine; it holds no family data and no network
  credentials (release downloads are unauthenticated; integrity is
  the signature's job).
- A broken release is contained: 24h failure cooldown, offline
  rollback, bounded boot-reconcile attempts, and the previous slot is
  never touched until the new one is proven ready.
