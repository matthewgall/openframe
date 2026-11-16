# ADR-0014: On-screen Wi-Fi setup via a platform seam

**Status:** accepted (user-approved, 2026-11-16)

## Context

A frame that cannot reach the network is stuck: at first boot the
pairing code can never reach the admin console, and a paired frame
whose Wi-Fi later dies needs a person at the device. The worst
support outcome a photo frame can have.

The fix must work **offline** (scanning needs no server) and must not
depend on a panel being present at development time — the dev-input
tool (tap/swipe injection, headless snapshots) is the verification
surface.

The Pi target runs Debian/Raspberry Pi OS, where Wi-Fi is managed
either by **NetworkManager** (Raspberry Pi OS default; persists
connections itself) or by **wpa_supplicant** directly (plain Debian,
minimal images). The frame runs **root** on the Pi (it must own the
display, thermal sysfs and the updater hand-off), which is the
privilege Wi-Fi configuration needs.

## Decision

A single platform seam — `internal/wifi` — owns all Wi-Fi
management. The UI, doctor and frame wiring depend only on its
`Backend` interface (`Info`, `Scan`, `Connect`, `Forget`,
`AutoConnect`); nothing else in the tree knows which tool is in use.

- **Autodetection** at startup, in order:
  1. NetworkManager (`nmcli` present and reports a Wi-Fi device) —
     preferred; it persists connections, so `AutoConnect` is a
     no-op and `Forget` is `connection forget`.
  2. wpa_supplicant + `iw` — OpenFrame manages **its own** 0600
     config file (`/etc/wpa_supplicant/openframe.conf`), newest
     network first (supplicant file order = preference), and starts
     its **own** supplicant instance. It refuses to touch an
     interface a *foreign* supplicant manages (detected via /proc
     cmdline) rather than fight over it. Scans use `iw dev <if>
     scan dump` (works with no supplicant running); `wpa_cli
     scan_results` is unusable because it does not carry SSIDs.
     DHCP via `dhclient`, falling back to `dhcpcd`.
  3. Radio present but no tooling — state is reported; actions
     explain what to install.
  4. No radio / non-Linux — the UI hides the feature entirely
     (cross-compilation stays clean; the admin SPA is unaffected).

- **Entry points**: a live **Wi-Fi row in Settings** (wired /
  connected SSID / not connected) and, on the pairing/empty
  screens, a "TAP to set up Wi-Fi" button shown **only while the
  server is unreachable** — when the network is fine, the hint
  must not exist.

- **On boot**: `AutoConnect` resumes saved networks in the
  background (a powered-off frame rejoins its network alone).

## Security rules (non-negotiable)

- A passphrase is **never logged, never sent to the server** (no
  health-report field carries it), and written only to the 0600
  config file.
- Every command runs via `exec` with an argument list — **never a
  shell** — and the command set is fixed (`nmcli`, `iw`,
  `wpa_supplicant`, `wpa_cli`, `dhclient`/`dhcpcd`).
- SSIDs/PSKs are escaped for the wpa_supplicant config format
  (quotes and backslashes), round-trip tested.

## v1 scope and known limits

- WPA2/WPA3-Personal (PSK) and open networks. **Not**
  WPA-Enterprise (802.1X) — a corporate use case; a later
  extension.
- DHCP only (no static-IP entry).
- The on-screen keyboard is ASCII (the frame font is ASCII-only):
  SSIDs and passphrases with non-ASCII characters cannot be
  entered.
- Requires root on the Pi (documented in the deployment guide).

## Consequences

- The stuck-at-first-boot case has a first-class fix that works
  with zero prior configuration.
- Verification is possible without a panel (dev input + headless
  snapshots); the parsing/config layers are unit-tested against
  fixtures and fake /sys and /proc trees.
- The seam isolates distro differences in one package; a
  different tool (e.g. a future `systemd-networkd-wifi` path) is a
  new backend behind the same interface.
