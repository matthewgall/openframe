# Hardware — Raspberry Pi 3 Model B+

## Target

- Broadcom BCM2837B0, quad-core Cortex-A53 @ 1.4 GHz, 1 GB RAM
- Full-size HDMI, USB 2.0 ports, dual-band Wi-Fi, Bluetooth 4.2/BLE
- 32 GB+ **high-endurance** microSD (consumer cards wear out under
  constant write load; the frame's cache and SQLite WAL are write-heavy)
- Raspberry Pi OS Lite 64-bit (no desktop environment)

The intended display is a Waveshare 10.1" 1280×800 HDMI capacitive
touchscreen (USB HID touch), subject to validation on real hardware.

> **Specified vs measured.** Everything in this file that comes from
> manufacturer documentation is a *specification*. Measured values
> (real voltages, temperatures, touch calibration, frame draw) are
> recorded in `docs/hardware/measurements.md` as they are taken — this
> document will not pretend to data we do not have. The enclosure will
> be designed from measurements, not assumptions.

## Compatibility notes (to validate on hardware)

- **HDMI output (M4, validated on paper — confirm on the bench)**:
  the M4 DRM driver probes the panel's connected connector and selects
  its preferred mode (1280×800 for the Waveshare panel); the canvas is
  sized to match exactly — a mismatch is a configuration error, not a
  silent scale. If the panel's EDID misbehaves, set the `hdmi_*`
  options in `/boot/config.txt` and re-check `openframe doctor`, which
  reports the exact mode the driver will use. The frame user needs
  access to the DRI node (Pi OS default group `video`).
- **USB touch input (M4)**: the panel presents as a USB HID multitouch
  device under `/dev/input/event*`. The touch layer reads evdev
  directly (no X11); with no `frame.touch.device` set it auto-detects
  the first touchscreen-class input by name. `openframe doctor`
  reports the resolved device path. The frame user needs the `input`
  group.
- **Backlight (M4)**: brightness/power go through the sysfs backlight
  class (`/sys/class/backlight`). If the panel exposes one,
  `SetBrightness`/`SetPower` use it; otherwise the documented
  degradation applies (no-op brightness, black frame for power off) —
  ADR-0006.
- **Power**: 5 V / 2.5 A (Pi 3B+) plus panel consumption — measure the
  combined load before choosing the PSU; under-powering shows up as
  SD card corruption, which we must never ship a box that is prone to.
- **Thermals (M4)**: the frame polls the sysfs thermal zones and
  doubles the slideshow interval above `frame.thermal.threshold_c`
  (default 70 °C); a missing sensor fails open. Enclosed Pi 3B+ still
  needs passive cooling (heatsink + case airflow); sustained
  throttling becomes visible in health reports (M5).

## Diagnostics on the device

```bash
openframe doctor     # os, arch, memory, disk, sqlite integrity,
                     # display backends, touch devices, server reachability,
                     # pairing state, cache, updater layout
openframe status     # local frame state, heartbeat freshness
```

`doctor` exits non-zero only on *fail* checks (corrupt database);
missing-but-not-yet-applicable items (touch screen in CI, updater
before M6) are warnings by design.

## Enclosure

The eventual enclosure is designed with `@jscad/modeling` and printed
on Flashforge AD5M/AD5X. Design constraints that shape the electronics
interface:

- The whole electronics stack (Pi + panel + PSU) must be replaceable
  without rebuilding the management platform: the frame communicates
  only over its own storage and outbound HTTPS, so a new board is just
  new provisioning, not a new protocol.
- Mounting-hole locations, cable clearance and enclosure dimensions are
  *measurements to be taken*, not values to be guessed. They will be
  recorded in `docs/hardware/measurements.md`.
