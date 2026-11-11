# Hardware Measurements

Recorded, measured values. **Nothing in this file is a manufacturer
specification** — specs live in `pi-3b-plus.md`. Entries are added as
real hardware validation happens (M4/M7). Until then this file is
deliberately empty of numbers: the project does not design or document
from imagined measurements.

Template for each entry:

```
## <what>
- Date:
- Hardware: (Pi board revision, panel model/serial, PSU, cable)
- Measured: <value ± tolerance>
- Method: <how it was measured, instrument used>
- Notes:
```

Pending measurements (tracked):

- [ ] Panel EDID / selected HDMI mode at 1280×800
- [ ] Touch device name, USB power draw, calibration offsets
- [ ] Combined idle/active power draw (Pi + panel + PSU headroom)
- [ ] Sustained CPU temperature under slideshow + sync load
- [ ] microSD write workload profile (GB/month) for endurance sizing
- [ ] Backlight control interface (PWM GPIO? none?)
- [ ] Mounting hole coordinates and panel-to-Pi standoffs
- [ ] Cable clearance envelope for the enclosure
- [ ] Frame idle memory (target < 200 MB, spec §2)
