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

- **HDMI output**: Pi 3B+ drives 1280×800 over HDMI; the touchscreen's
  EDID must be respected (set `hdmi_*` options in `/boot/config.txt` if
  the panel does not advertise correctly). Validation: boot to the frame
  binary with the headless driver, then switch to the M4 DRM driver and
  confirm the correct mode is selected.
- **USB touch input**: the panel presents as a USB HID multitouch
  device under `/dev/input/event*`. `openframe doctor` lists detected
  touch devices by name; the M4 touch layer reads evdev (no X11).
- **Backlight**: not assumed. The display power path goes through the
  `display.Driver.SetPower` interface; if the panel exposes a PWM
  backlight (e.g. via a GPIO-hat) it will be wired in M4, otherwise the
  fallback is a black frame (ADR-0006).
- **Power**: 5 V / 2.5 A (Pi 3B+) plus panel consumption — measure the
  combined load before choosing the PSU; under-powering shows up as
  SD card corruption, which we must never ship a box that is prone to.
- **Thermals**: enclosed Pi 3B+ needs passive cooling (heatsink on SoC
  + case airflow). Log CPU temperature in health reports (M5) so
  sustained throttling is visible in the admin UI.

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
