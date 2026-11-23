---
date: 2026-11-23
topic: living-presentation
---

# Living Presentation — Breathing Photos & Arrivals

## Problem Frame

The frame currently renders a correct, smooth slideshow — and still reads as a
screen rather than a living object. Photos sit perfectly frozen between
advances; a new photo landing from a family member is indistinguishable from
the rotation quietly cycling. For the recipient (an older family member who
lives with the frame), two things are missing: *aliveness* (the image moves
the way memories do) and *moments* (receiving a photo should feel like being
sent one). Both features exploit capabilities verified on real hardware during
this milestone week: the Pi 3's HVS compositor pans/zooms the scanout source
rectangle at ~0.25 ms per atomic commit with no CPU pixel work (proven by the
half-resolution dissolve path), and manifest activation already has the
pre-change photo set visible beneath its transaction.

---

## Actors

- A1. **The recipient** ("Granny"): lives with the frame; touches it to browse; never configures it.
- A2. **The sender** (family member): uploads photos from the web app; wants to feel noticed when they do.
- A3. **The admin** (deployment owner): may want fleet-level overrides; expects the frame to behave well unattended.

---

## Key Flows

- F1. **Breathing drift**
  - **Trigger:** slideshow displays a photo (motion not off).
  - **Steps:** the displayed frame slowly pushes in one direction (subtle zoom + pan, randomized per photo); on advance, the next photo starts its own fresh drift; touching the screen or entering the sleep window freezes/pauses drift immediately.
  - **Outcome:** the image is never pixel-frozen, yet the motion is never showy.
  - **Covered by:** R1–R6

- F2. **Arrival debut**
  - **Trigger:** a manifest activation adds photos the frame has not shown.
  - **Steps:** at the next advance slot, the first newcomer is displayed with a fading "from {Sender}" ribbon (~8 s); remaining newcomers join the normal rotation quietly; if the frame sleeps before the debut, it happens at the first advance after waking.
  - **Outcome:** the recipient sees *someone sent something*, by name; senders' effort lands visibly.
  - **Covered by:** R7–R12

---

## Requirements

**Motion (Ken Burns drift)**
- R1. While a photo is displayed in the slideshow, the visible image drifts continuously: a slow zoom-in (≤ ~1.2× total over the display duration) combined with a gentle pan in one direction; direction and pan angle are randomized per photo.
- R2. Motion is achieved by moving the scanout source window on the hardware compositor — no canvas re-render and no per-frame pixel work; the displayed framebuffer stays still while the plane window moves.
- R3. The drift never reveals black beyond the photo: for `contain`-letterboxed images, the pan window stays inside the fitted image rect; for full-bleed images it stays inside the frame.
- R4. Motion pauses while the recipient is interacting (touch activity, matching the existing slideshow-pause behavior) and is fully stopped during the sleep screen; it resumes on the next displayed photo after either ends.
- R5. Settings gains one row, **Motion**, cycling `calm` (default) ↔ `off`. Server per-frame config can override the default for a device or the fleet. `off` disables drift entirely; the arrival ribbon is unaffected.
- R6. Motion is implemented on the DRM path only; other drivers (headless/dev) degrade to no motion safely. With motion on, the frame's added steady-state CPU cost must be negligible (≤ ~2% of one core) and must not increase thermal throttling or clock drops on a Raspberry Pi 3.

**Arrival ("New from Sarah!")**
- R7. On manifest activation, photos whose hashes were not in the previously-active set are marked as *new arrivals*, queued in upload-time order.
- R8. At the next slideshow advance (not interrupting the current photo), the first queued arrival is shown with an overlay ribbon reading `from {uploader display name}`, which fades out after ~8 s; the ribbon must not obscure captions.
- R9. One debut per activation event; if several photos arrive at once, at most a small queue (max 3) awaits debuts at subsequent advances, and the remainder join the rotation silently.
- R10. First population is silent: when the frame had no photos before the activation (fresh pairing/reset), no ribbons or debuts fire — newcomers just begin their normal rotation.
- R11. Arrivals detected during the sleep window debut at the first advance after waking (subject to R9's queue cap; expired-overnight bulk arrivals still follow R10/R9 caps).
- R12. The manifest photo object gains the uploader's display name, and the frame's photo store persists it, completing the existing-but-dead `ui.Photo.Uploader` field (already rendered on the details screen as "By {name}"). If a photo has no uploader name (deleted account, manual DB entry), the ribbon falls back to `new photo`; the details screen omits the "By" line as it does today.

---

## Acceptance Examples

- AE1. **Covers R1, R2, R6.** Frame idles on the slideshow for 10 minutes: the image drifts imperceptibly-but-visibly with no stutter, no visible stepping, and the Pi reports no thermal/armdrops change vs. motion off.
- AE2. **Covers R3.** A portrait photo letterboxed on the landscape panel drifts without any black bar ever entering the visible area.
- AE3. **Covers R4.** Recipient taps to reveal controls mid-drift: motion freezes; after controls auto-hide and activity settles, drift resumes on the current or next photo without jump-cut artifacts.
- AE4. **Covers R7, R8.** Sarah uploads at 14:02; the frame activates the revision at 14:03; at the next advance slot (≤ interval later) the photo debuts with "from Sarah" fading over 8 s.
- AE5. **Covers R9.** Five photos arrive in one activation: one debuts now, up to two more debut at the following two advances, the rest enter rotation silently.
- AE6. **Covers R10.** A brand-new frame completes pairing and syncs its first 300-photo library: zero ribbons, no debut queue; slideshow simply starts.
- AE7. **Covers R11.** A photo arrives at 23:40 (frame asleep since 22:00): nothing displays; at 07:00 wake, the first advance debuts it with ribbon.
- AE8. **Covers R12.** After upgrading the server + frame, the photo details screen now shows "By {display name}" for newly synced photos (currently dead).

---

## Success Criteria

- A first-time viewer (Granny) remarks — unprompted — that the frame "moves" or feels "alive" within a week; or the admin observes it in use without motion being disabled via settings within a week (motion-on retention as a proxy).
- Senders' uploads produce a visible on-frame moment end-to-end within one slideshow interval, verified on real hardware.
- No regressions in the behaviors hardened this week: zero present failures ("flip event stalled" absent), dissolve timings unchanged, sleep/wake and touch-wake unaffected.
- The implementation handoff is clean: planning can size both features without inventing new product behavior; every requirement maps to a testable outcome above.

---

## Scope Boundaries

- **Not this feature:** video clips, per-album motion styles, motion in gallery/album/search views (slideshow only), face/person clustering (separate vision spike), real-time freshness push (follow-up), sender-facing reply loop (follow-up).
- The ribbon is text + fade — no confetti, no sound, no photo-specific styling.
- No new user-visible settings beyond the single Motion row; arrival has no off-switch in v1 (it is the product's reason to exist).
- Motion quality bar is "glacial and composed"; aggressive pans/rotates are explicitly out.
- Durable persistence of the debut queue across process restarts is not required — a restart may drop pending debuts (photos still rotate normally).

---

## Key Decisions

- **Ken Burns drift over "breathing" in/out loops** — directional, randomized per photo; chosen for a composed, non-looping feel.
- **Next-slot debut over instant cut** — arrival is a moment, not an interruption; recipients are often mid-look.
- **One dial (Motion: calm/off), ribbon always-on** — this persona configures nothing; defaults carry the experience; server config is the admin's escape hatch.
- **Hardware-window motion, not re-rendered zoom** — validated on the live panel this week; it is the reason motion is nearly free.
- **Silent first population** — arriving to a frame full of "new!" ribbons is noise, not delight.
- **Uploader display name joins the manifest** — `users.display_name` already exists and is already joined for albums; this completes an existing dead path rather than inventing one.

---

## Dependencies / Assumptions

- vc4 plane `SRC_X/SRC_Y/SRC_W/SRC_H` properties support offset windows for pan (verified: these are the exact properties resolved during this week's driver fix; SRC offsets need a live spot-check during planning spike).
- Server manifest build gains `uploader_name` without breaking old frames (frames ignore unknown JSON fields — verify protocol tolerance; ETag covers the change).
- Frame DB migration adds a photos.uploader column (existing migration framework, checksummed).
- Under-voltage on the current test Pi (throttle flags observed during benchmarking) is environmental, not a design constraint — but motion acceptance should be measured on a properly powered board.
- Fitted photo cache: drift operates on the already-displayed framebuffer; no decode cost.

---

## Outstanding Questions

- Exact drift rate and zoom curve (e.g. %/minute, ease in/out vs linear) — tunable constant; settle with a live spike on the panel.
- Ribbon visual: pill placement (top vs above caption bar) — decide with a rendered snapshot review during implementation.
- Should `priority` photos get preferential debut ordering within an activation's arrivals? (Leaning no for v1.)

---

## Next Steps

1. Spike on the Pi: verify `SRC_X/Y`-offset commits pan correctly on the primary plane and measure drift smoothness at 4–5 Hz update cadence.
2. `ce-plan` this document once the spike confirms the motion mechanism.
3. Sequence: manifest uploader field first (server + frame, independently shippable), then arrival, then motion.
