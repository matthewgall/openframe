---
title: "feat: Living presentation — layer-separated motion and photo arrivals"
type: feat
status: active
date: 2026-11-23
origin: docs/brainstorms/2026-11-23-living-presentation-requirements.md
deepened: 2026-11-23
---

# feat: Living presentation — breathing photos & arrivals

## Overview

Two capabilities make the frame feel alive: a Ken Burns drift of the photo
executed purely by the vc4 hardware compositor (the scanout source window
moves; no pixel work, no re-render), and an arrival debut that shows a newly
synced photo with a fading "from {name}" ribbon. Both require a structural
change the requirements doc did not anticipate and the user has approved:
**render layer separation.** Today the photo and all chrome (clock, caption,
controls, ribbon) are baked into one framebuffer, so a plane-level window
would drag the clock along with the photo. The frame moves to a two-plane
compositing model on DRM hardware: primary plane = photo (drifts), ARGB
overlay plane = chrome (pixel-still). If the overlay path proves unavailable
on a device, motion reports unsupported and stays off — it never degrades to
whole-frame drift.

---

## Problem Frame

The frame renders smooth but static: images are pixel-frozen between advances
and new photos arrive indistinguishable from rotation noise. The recipient
("Granny") needs the object to feel alive, and receiving a photo needs to
feel like receiving. (Full frame: origin doc §Problem Frame.) Verified this
week on the target hardware: the HVS accepts half-res→full scanout scaling
at ~0.25 ms per atomic commit with no CPU pixel cost (docs/hardware is to be
updated with these numbers — see U1, U9), and the panel exposes 56 planes,
making a second compositing layer essentially free.

---

## Requirements Trace

Origin: `docs/brainstorms/2026-11-23-living-presentation-requirements.md`
(R1–R12, A1–A3, F1–F2, AE1–AE8 — all carried).

- R1–R4 → U6 (motion controller: drift law, pause/gating, no-black clamp)
- R2, R6 → U1, U4 (hardware window path, commit cost, negligible CPU)
- R5 → U8 (Motion settings row + server override)
- R7–R11 → U3 (arrival diff at activation) + U7 (debut queue, ribbon, caps, sleep/first-population rules)
- R12 → U2 (manifest carries uploader display name) + U3 (persist + populate `ui.Photo.Uploader`)
- R7 arrival ordering → upload-time order honored via an added `uploaded_at` manifest field (U2/U3); no deviation from origin (doc-review: manifest traversal order is hash order, arbitrary across senders for AE5-sized batches)
- Success criterion "chrome stays crisp" → U5 (layer split) — supersedes the origin's implicit single-buffer assumption
- AE1–AE8 → test scenarios under U6, U7, U3, U2

---

## Scope Boundaries

Carried from origin:
- No video clips, no motion in gallery/album/search (slideshow only), no face clustering, no per-album motion styles.
- Ribbon is text + fade; no sound, no confetti; no per-photo ribbon styling.
- No new user-visible settings beyond the single Motion row; the arrival ribbon has no off-switch.
- Drift is glacial; aggressive pans/rotations are out.
- Debut queue durability across restarts is NOT implemented (origin waived it; doc-review confirmed the durable variant would be three mechanisms guarding a ~2 s row lifetime). Arrivals reach the UI in-memory via the extended `OnActivate` callback; a crash between activation and debut drops pending debuts — accepted.

### Delivery increments (doc-review: sequencing)
- **Increment 1 — arrivals (U2, U3, U7):** pure software, no hardware dependency; ships and validates the "from Sarah" moment on the panel. The ribbon draws into the main canvas when no chrome layer exists and onto the chrome layer when it does — one boolean branch; the moment never blocks on hardware work.
- **Increment 2 — motion (U1, U4a, U4b, U5, U6, U8, U9):** the layer-separation architecture, gated by the hardware spike.

Plan additions:
- Whole-frame drift (chrome moving with the photo) is explicitly rejected as a fallback: on devices without a usable overlay plane, motion reports unsupported and remains off.
- Rotating the chrome layer via plane rotation properties is out (rotation currently only records intent on vc4; when implemented it must be applied to both planes — noted in Deferred).

### Deferred to Follow-Up Work
- Real-time freshness push (heartbeat-triggered immediate Check) — separate plan, pairs with U7's debut queue.
- Sender-facing reply loop ("Grandma starred N photos") — separate plan.
- Ambient-clock / watermark use of the new chrome layer — future, enabled by this architecture.

---

## Context & Research

### Relevant Code and Patterns

- `internal/server/manifest/manifest.go` — `Build`, `loadPhotos` (:288, no users join today), `Photo` (:41), `ETag(rev,cfgRev)` (:100); revision = hash of photo content, so an added field bumps revision without download churn.
- Uploader join precedent: `internal/server/photos/photos.go:105,169` (`JOIN users u ON u.id = p.uploaded_by` → `UploaderName json:"uploader_name,omitempty"`).
- `internal/frame/sync/engine.go` — `maybeActivate` (:948) single-tx swap; `activatePhotos` (:1019) already has incoming hashes and can read the prior active set before the upsert; `sync.Config` (:58-76) tri-state pointer fields pattern; `OnActivate`/`OnConfig` callback registration.
- `internal/frame/ui/render.go` — `renderSlideshow` (:124) blit + chrome into one canvas; `drawClock` (:159)/`drawCaption` (:213)/`drawControls` (:244) are the chrome functions to retarget; `fitPhoto` (:111) returns the chrome-free fitted bitmap (`e.curImage`).
- `internal/frame/ui/ui.go` — `Run` (:319) select loop (1 s tick, advance tick, gestures, async) is the motion-tick host; `engine.mu` serializes present calls today.
- `internal/frame/display/drm/drm.go` — `commitFB` (:980) single commit funnel; `planeProps` already resolved; `SRC_X/SRC_Y` exist on the panel's planes (ids 9/10 observed on live hardware) but are never written; `getPlane` returns format lists usable for ARGB probing; no internal lock today (relies on callers — `SetPower`/`SetBrightness` already call off the UI mutex, a latent race this plan fixes).
- Settings machinery: `renderSettings` items list (`render.go:633`), `settingIndex` (:775), `doSetting` cycles (`settings.go:272`), persistence keys (`frame/settings.go:22-30`), `applyLocalSettings` validation, `applyUIState` save (`ui_wiring.go:184-193`), server mirror `internal/server/devices/config.go:23` + `Validate` (:49).
- Migration conventions: `migrations/frame/` next number 0004; additive-only, checksum-enforced (never edit applied files) — `internal/db/migrate.go`.

### Institutional Learnings

None (`docs/solutions/` does not exist). In-repo learnings applied: flip-event bookkeeping and the stall-unlock rule (`drm.go` comments), wall-clock transition pacing (`input.go:startTransition`), HH:MM persistence lesson (`ui_wiring.go:190-193`), "config-only changes must not churn downloads" (`manifest.go:64-69`), per-object property resolution (ADR-0006 M4 notes).

### External References

Not used — kernel DRM uapi semantics were verified directly on the target hardware during this milestone (ioctl matrices, HVS scaling, plane enumeration).

---

## Key Technical Decisions

1. **Two-plane compositing on DRM; single-canvas path preserved everywhere else.** Headless/dev drivers keep receiving the composited `PresentFrame` image, so CI, snapshots, and `env.px` pixel tests continue to pass untouched — the split is a capability (`display.OverlayDriver` optional interface), not a new requirement on drivers. (User-approved architecture.)
2. **No whole-frame-drift fallback.** Without a usable overlay plane the Motion row **pins to `off` and renders "off (n/a)"** (a hidden row reads as a lost setting; an inert pinned row is honest; cycling attempts log once at Debug). Capability is surfaced, not just logged: the heartbeat gains `motion_supported` (+ reason) and `motion_active` (effective value after precedence) so fleet `calm` on an incapable device is visibly unfulfilled in `openframe status` and the admin UI (precedent: `DisplayDriver`/`DisplayStatus` already ride the payload). If the U1 kill-gate fires, motion ships cancelled for this plan and "drift with chrome suppressed" returns to the open questions as an explicit product decision — U5's layer split still lands because U7's ribbon prefers it.
3. **Motion = property-only atomic commits.** Add `SRC_X/SRC_Y` to the commit; animate the window at 200 ms cadence from the engine's existing `Run` select loop (no new goroutine owning UI state); no flip event requested (FB unchanged).
4. **Drift law: full-rect → anchored crop, time-normalized, pause-freezes-phase.** Start window == fitted image rect (never starts cropped); end window == `1/zEnd` at a randomized focal point biased toward centre. `zEnd = clamp(1 + rate × duration, 1.04, 1.08)`, `rate ≈ 0.04/min`, so a 30 s slot and a 5 min slot read as the same glacial speed (U9 tunes rate; R1's ≤1.2 ceiling keeps headroom). Elapsed excludes paused gate-time (controls visible, transition in progress, screen ≠ slideshow) — pausing freezes the phase rather than ageing it, so auto-hide resumes mid-drift and no photo sits pixel-frozen at clamped max zoom under long attention. Black never enters the window (clamped inside the image rect): R3 for `contain` letterboxes falls out for free.
5. **Arrival diff computed inside the activation transaction** (`prev set` vs incoming hashes, ordered by upload time) and delivered to the UI **in memory** through the existing activate callback chain (`OnActivate(revision)` → `OnActivate(revision, arrivals []string)`, capped at 8 at the source). No `sync_state` row, no TTL, no claim-token, no consumption protocol: the unprotected window is the ~2 s callback stall the origin explicitly waived. Queue cap (3) and silent-first-population (R10: empty `prev` set → deliver nothing) live in the UI layer; a second activation while debuts are pending appends within the cap.
6. **Ribbon rides the chrome layer** (main canvas pre-split or when overlay is unavailable), pixel-still while the photo drifts beneath — the origin's motion-pauses-for-ribbon case is deleted. Fade = scrim alpha only, glyph alpha holds to a ≥ 90 contrast floor at `fontBody` scale-2 (older-viewer persona). While controls are visible the ribbon's fade clock is held (the 8 s message is not swallowed by the 5 s controls overlay); any advance or screen change cancels an armed ribbon (debut spent, not deferred), and the ribbon draws only while its photo is displayed (`ribbon.idx == e.idx`).
7. **Add a `sync.Mutex` to the DRM driver now**, serializing all public driver entry points. This closes the pre-existing off-UI-lock mutation race (SetPower/SetBrightness today) and is a hard prerequisite for the motion tick committing concurrently with presents. The lock stays **coarse on purpose**: `beginPaintBack`'s ≤40 ms event wait remains inside it because `pollEvent` mutates the same `pending[]`/`evq`/`front` fields `commitFB` writes — narrowing the section would split the flip FIFO across goroutines and re-open the race the mutex exists to close. Stated invariant: the driver is a leaf — **no DRM entry point calls back into the engine; lock order is always `e.mu → d.mu`**. No `TryLock` for motion: engine-side `e.mu` already serializes motion vs presents; the driver lock only contends with SetPower/SetBrightness/Rotation (sysfs-only paths that never commit).
8. **Server `Motion` config field is tri-state** (`*string`: nil follow default / `"calm"` / `"off"`) mirroring `Brightness`/`ShowClock`; precedence local > server > file unchanged.
9. **Drift phase is clock-derived** (`cfg.Now().Sub(photoStart)` minus paused intervals), never tick-accumulated: render paths already stall `e.mu` for seconds on the reachability probe, so tick-counting loses drift; skipped/late ticks produce the same window scheduled ones would. Production deltas are monotonic-safe (`time.Now` subtraction within one process); tests inject `cfg.Now()`. No feature logic makes cross-process wall-clock comparisons — the one candidate (arrivals TTL) was deleted with Decision 5.
10. **Motion needs no transition gate (revised during doc review):** the motion tick is serviced by the same `Run` goroutine that blocks inside `startTransition`, so no tick lands mid-dissolve; transition nesting unwinds immediately (the gesture branch presents and returns). Where the engine wants the fact it reads existing in-transition state (`transFrom != nil`). No depth counter, no new bookkeeping, no fail-closed mode. The driver-side half survives: W2 forces full-window emission on non-still commits regardless.
11. **Driver errors are classified, not conflated:** `ErrDriverClosed` (shutdown — ignored silently; `t.Cleanup`-style closes under live engines are a real pattern in this repo) vs transient commit failures (back off, retry next tick, tolerate N consecutive before disabling motion **for the current photo only**, re-arm at next advance). A single transient error never disables motion process-wide; any transition to "motion not running" logs one **Warn** with reason and surfaces via `lastPresentErr` so doctor/status report it — protecting the motion-on-retention success criterion from silent death.

---

## Open Questions

### Resolved During Planning
- Chrome coupling during motion → layer separation (user decision this session).
- Arrivals ordering → add `uploaded_at` to the manifest join (server `photos.created_at` exists; same join, same single revision bump) so origin R7's upload-time order holds verbatim; the cheaper hash-order deviation was rejected in doc review as unfair sender selection at AE5 batch sizes.
- Where the motion tick lives → engine `Run` select, 200 ms ticker case; wall-clock phase (Decision 9) because render paths already stall `e.mu` for seconds on the reachability probe.
- Lock discipline → coarse `d.mu`, event wait inside, driver-as-leaf invariant (`e.mu → d.mu` only), per-plane `lastFB`, closed-flag teardown (Decision 7, U4).
- Arrivals vs restart → memory-only via callback; restart may drop pending debuts (origin-sanctioned; doc-review killed the durable-row design as three mechanisms guarding a ~2 s lifetime).
- First-sync ribbon storms → engine-side empty-prev-set gate (R10/AE6).

### Deferred to Implementation
- Plane selection tie-breaks when several overlay-capable planes exist (candidate ordering: different CRTC mask bit, zpos support, fewest properties owned) — settle in U1/U4 spike data.
- Exact drift easing (linear vs ease-in-out) and focal bias spread — tune on hardware in U9 against AE1.
- Display name only; empty → ribbon falls back to `new photo` (R12). Closed in doc review.
- ARGB probing: `IN_FORMATS` blob parsing vs. fixed ARGB8888 assumption for vc4 — U1 decides; prefer probing the existing `getPlane` format list.

---

## High-Level Technical Design

> *This illustrates the intended approach and is directional guidance for review, not implementation specification. The implementing agent should treat it as context, not code to reproduce.*

```
DRM device (motion-capable)                DRM device (no overlay) / headless
┌───────────────────────────────┐          ┌──────────────────────────────┐
│ atomic commit                 │          │ PresentFrame(composited img) │
│  primary plane  (photo):      │          │  photo + chrome in one buf   │
│    FB_ID=photoFB              │          └──────────────────────────────┘
│    SRC_X/Y/W/H = driftWindow  │   ┌──── motion tick (200ms, engine Run)
│  overlay plane (chrome):      │   │    computes window -> SetSourceWindow
│    FB_ID=chromeFB (ARGB)      │◄──┤    (property-only commit, no event)
│    SRC = full panel, static   │   │
└───────────────────────────────┘   │
   engine render paths:             │
   photo → primary buffer (on advance/transition only)
   chrome → overlay buffer (on chrome change: 1 Hz clock, captions, ribbon, controls)

arrivals: sync activation tx ──► sync_state["arrivals"] {ts, hashes[≤8]}
          ──► UI reload ──► debut queue[≤3] ──► advance() jumps to next arrival
          ──► chrome layer: "from Sarah" ribbon, fades ~8s
```

---

## Implementation Units

- [ ] U1. **Hardware spike: overlay plane + windowed primary commits**

**Goal:** de-risk the architecture on the real Pi before code lands; produce the numbers this plan asserts.

**Requirements:** R2, R6 (measurement), unblocks U4/U6.

**Dependencies:** none.

**Files:**
- Create: `docs/hardware/measurements.md` — **Raw spike data** section (plane ids, formats, blend property, per-commit costs, go/no-go). U9 owns the **Validated on device** section; the split rule goes in the file header.
- Create: `docs/hardware/probes/overlay-spike.c` — the probe source, committed, so every recorded number carries a reproducible Method line (the template demands it; `/tmp` artifacts do not survive board swaps).

**Approach:**
- Enumerate planes via `GETPLANERESOURCES` + `GETPLANE`; find one with `possible_crtcs` bit for the active CRTC, non-primary, advertising ARGB8888 (`DRM_FORMAT_ARGB8888`) in its format list.
- Create a dumb ARGB buffer + FB, commit a single atomic with both planes (primary: XRGB FB with `SRC_X/Y/W/H` sub-pixel offset; overlay: full-screen ARGB, `pixel blend mode` = coverage if the property exists, `zpos` above primary).
- Animate primary `SRC_*` at 200 ms property-only commits (FB unchanged, no flip event) for 60 s; confirm no EBUSY, no flicker, correct blending, and CPU/thermal headroom (`vcgencmd measure_cpu_temp`/`get_throttled` before/after; note the board's under-voltage flag separately).
- W2 scenario: set a non-full motion window, then run a half-res-FB commit and a 30 Hz crossfade; record whether `SRC_W/H` are FB-derived or panel-derived and whether a wrong-crop flash appears — the interaction U4's stored-untouched rule depends on.
- Kill-gate: if no overlay-capable ARGB plane exists for the CRTC, record it. Consequences: motion is cancelled for this plan (its headline success criterion is dropped explicitly), U4 ships `motionSupported=false` paths only, "drift with chrome suppressed" returns to the open questions as an explicit product decision — U5's layer split still lands (U7's ribbon prefers it), and Increment 1 (arrivals) is unaffected by this gate either way.

**Patterns to follow:** existing C probe pattern from this session; `docs/hardware/pi-3b-plus.md` validation-log style.

**Test expectation:** none — hardware probe; output is documentation + a go/no-go recorded in this plan file under U4.

**Verification:** measurements committed; a written conclusion (plane id chosen, formats, blend property, commit cost) exists in `docs/hardware/measurements.md`.

---

- [ ] U2. **Server: manifest carries the photo uploader display name**

**Goal:** `manifest.Photo` gains the uploader's human name so frames can attribute arrivals (R12).

**Requirements:** R12.

**Dependencies:** none (ship before or after U3; frame tolerates absence both ways).

**Files:**
- Modify: `internal/server/manifest/manifest.go` (`Photo` struct, `loadPhotos` join)
- Test: `internal/server/manifest/manifest_test.go` (create if absent; check existing coverage first)

**Approach:**
- `loadPhotos`: `LEFT JOIN users u ON u.id = p.uploaded_by`, select `COALESCE(u.display_name, '')` — mirrors the `photos.go` list join but LEFT, because deleted/unnamed users must degrade, not drop rows.
- `Photo.Uploader string \`json:"uploader,omitempty"\`` **and** `Photo.UploaderAt string \`json:"uploaded_at,omitempty"\`` (from `photos.created_at`, already in the row the join touches — costs nothing, keeps origin R7's upload-time debut order honest at AE5 batch sizes; absorbed by the same single revision bump).
- Revision bump on deploy is expected and safe: content hash changes but `fileFor` resources do not, so frames re-stage and re-activate with zero downloads (verified behaviour, `manifest.go:64-69`).
- Document in the manifest protocol comment that `uploader` is additive within `openframe-manifest/1` (old frames ignore unknown fields; new frames default to "").

**Patterns to follow:** `internal/server/photos/photos.go` uploader join; `UploaderName json:"uploader_name,omitempty"`.

**Test scenarios:**
- Happy path: photo uploaded by user with display name "Sarah" → manifest photo JSON contains `"uploader":"Sarah"`; album ordering/hash deterministic across two builds of the same DB state.
- Edge case: uploaded_by references a user with empty display_name → `uploader` omitted (omitempty), row still present.
- Edge case: user row missing (manual data, LEFT JOIN miss) → no join failure, empty uploader.
- Integration: adding uploader to one photo changes the manifest revision but produces no `fileFor` resource changes (no download churn).

**Verification:** `openframe` server + manifest build tests green; a manual fetch of `/api/v1/frames/{id}/manifest` shows the field.

---

- [ ] U3. **Frame sync: persist uploader and detect arrivals at activation**

**Goal:** frame stores per-photo uploader + upload time; activation computes the new-arrival set (R10 silent-first-population) and delivers it to the UI in memory — no persistent arrivals row (Decision 5).

**Requirements:** R7, R9 (cap), R10, R12 (frame side).

**Dependencies:** U2 (field present to store; behavior defined for empty).

**Files:**
- Create: `migrations/frame/0004_photo_uploader.sql` (`ALTER TABLE photos/staging_photos ADD COLUMN uploader TEXT NOT NULL DEFAULT ''` + `uploaded_at TEXT NOT NULL DEFAULT ''` — sqlite permits multiple ALTERs per file; never edit applied migrations)
- Modify: `internal/frame/sync/engine.go` (manifest photo struct mirror, staging insert, `activatePhotos` copy + arrival diff; **`OnActivate(fn func(revision int64))` → `OnActivate(fn func(revision int64, arrivals []string))`**), `internal/frame/ui_wiring.go` (callback consumer → UI hand-off; `frameData.Collection` reads `uploader` into `ui.Photo.Uploader`)
- Test: `internal/frame/sync/sync_test.go` additions; frame-side collection loader test in `internal/frame/` (extend `state_reads_test.go` or new `collection_loader_test.go` — check existing coverage location)

**Approach:**
- `stageManifest` persists `uploader` into staging; `activatePhotos` carries it into the active photos upsert (it already replaces the set).
- Inside the activation transaction, before swapping: `prev := SELECT hash FROM photos`; `arrived = incoming − prev`, **sorted by staging `uploaded_at`** (R7 verbatim). After commit, hand `arrived` (cap 8) to the registered activate callback alongside the revision — in memory, no `sync_state` row, no TTL, no consumption protocol (Decision 5). When `prev` was empty: deliver nothing (R10).
- `Collection` loader selects `uploader` and fills the previously-dead `ui.Photo.Uploader`.

**Patterns to follow:** existing `staging_*` swap pattern; `keyDeviceName` state upsert; JSON-in-sync_state precedent (`StateKeyServerConfig` formatting in effective.go).

**Test scenarios:**
- Happy path: sync rev1 (2 photos) then rev2 (adds photo X) → activate callback delivers [X].
- Covers AE6: fresh frame (empty prev set) activates 300 photos → callback delivers an empty list.
- Edge case: caption/favourite edit bumps revision with identical hashes (also the U2 upgrade bump) → empty arrival list; already-queued UI debuts untouched (writer-side deletes no longer exist — the whole hazard class is gone with the row).
- Happy path: multi-photo arrival delivered sorted by `uploaded_at`, not hash order.
- Edge case: >8 arrivals → delivered list capped at 8, earliest-upload-first preserved.
- Error path: activation transaction failure → callback never fires (fire-after-commit is the existing pattern).
- Integration: `frameData.Collection` returns `Uploader` populated; details screen "By …" renders (AE8) — extend an existing UI rendering test that asserts the By-line.

**Verification:** staged→active data shows uploader in the frame DB; arrivals semantics covered by tests including AE6's empty-prev case.

---

- [ ] U10. **DRM driver: internal serialization and teardown safety** *(carved out of U4 by doc review — lands regardless of the U1 gate; a live-bug fix, not motion work)*

**Goal:** serialize driver entry points and make teardown race-free, fixing the pre-existing off-UI-lock mutation race (`SetPower`/`SetBrightness` are called from scheduler/effective-config goroutines with no lock today) and the nil-file window after `Close` (`t.Cleanup` closes drivers under live engines in `e2e_test.go`/`m5_test.go`).

**Requirements:** prerequisite safety for U6's ticker; independent value today.

**Dependencies:** none.

**Files:**
- Modify: `internal/frame/display/drm/drm.go`
- Test: `internal/frame/display/drm/serialization_test.go` (linux-tagged; closed-flag/idempotent-Close state-machine tests; live-fd concurrency proof rides `go test -race` on the existing suite)

**Approach:**
- `mu sync.Mutex`; lock all public entry points per Decision 7 (coarse section; event wait stays inside — it mutates the same FIFO fields `commitFB` writes; driver-as-leaf invariant stated in code).
- `closed` flag set under the lock; every locked entry returns `ErrDriverClosed` after teardown; double-Close idempotent; the engine treats that error as shutdown-noise (Decision 11).

**Patterns to follow:** existing `beginPaintBack` bounded-wait valuation; never-call-out-while-holding idiom.

**Test scenarios:**
- Happy path: entries after `Close` return `ErrDriverClosed` (no nil-file panic); second `Close` is a no-op.
- Edge case: `Close` racing a present inside the bounded event wait → clean return, no use-after-close.
- Integration: `go test -race ./internal/frame/...` green.

**Verification:** `-race` suite green; `doctor` unchanged; U4 builds on the locked surface.

---

- [ ] U4. **DRM driver: ARGB overlay channel + windowed still commits**

**Goal:** the driver can present a photo plane with an arbitrary sub-pixel source window and a static ARGB chrome plane.

**Requirements:** R2, R6; prerequisite for U5/U6.

**Dependencies:** U1 (plane/format/blend facts; go/no-go), U10 (serialization, closed-flag, error taxonomy).

**Files:**
- Modify: `internal/frame/display/drm/drm.go`, `internal/frame/display/drm/ioctls.go` (ARGB8888 fourcc const), `internal/frame/display/drm/layout_test.go` (const pins), `internal/frame/display/drm/fused_test.go` (writeARGB8888 conversion tests)
- Create: `internal/frame/display/drm/overlay.go` (overlay setup + chrome present + window state; keeps drm.go focused)
- Modify: `internal/frame/display/display.go` (optional interfaces: `OverlayPresenter` with `PresentOverlay(*image.NRGBA) error`, `MotionWindow` setter, `MotionCapable() bool` — names indicative)

**Approach:**
(Serialization, closed-flag, and error taxonomy land in U10. The interaction that keeps motion cheap: event-free commits never join the flip FIFO — `commitFB` requests events only when `wantEvent && slot >= 0 && fbID != lastFB` — so window updates neither extend flip waits nor consume FIFO slots.)
- Refactor plane choice at probe time into a pure function (table-testable): given `getPlane` results and the chosen CRTC id, pick primary (today's rule) and overlay candidate (non-primary, ARGB-in-formats, crtc-bit, not the writeback connector plane).
- Overlay lifecycle: one dumb buffer + FB sized to the panel, format ARGB8888, mapped like primary; single buffer is acceptable (chrome updates ~1 Hz; a torn clock pixel is below perceptual threshold — record as accepted). **`lastFB` becomes per-plane** (photo slot vs overlay FB): `PresentOverlay` must not write the photo plane's `lastFB`/`pending`/`evq`, or a chrome present would make the next photo present request a spurious flip event and re-arm the stall path.
- `commitFB` extension: build the property arrays for both planes in ONE atomic commit; primary carries `SRC_X/Y/W/H` from a driver-held window struct (default = full panel, 16.16 fixed); overlay window always full; include `pixel blend mode`/`zpos` when owned (per-object `planeProps` pattern). **Window application rule (W2, clarified by doc review):** the driver stores the motion window separately from what it emits. The stored window is applied **only to still-photo commits** (`PresentFrame`'s full-panel photo slots). Every other commit path — half-res dissolve, `PresentTransition` full/slide, `presentBlack`, overlay presents — emits the full window for its bound FB and **leaves the stored window untouched**; it becomes effective again on the next still commit, which U6 guarantees follows a phase reset for the current photo (W3/W4). A micro-snap (≤ zEnd−1 scale) is possible on a dissolve's first frame after a mid-drift still; U9 observes it against the panel and the rate/focal knobs exist to soften it if visible.
- `writeARGB8888`: NRGBA `[R,G,B,A]` → ARGB memory `[B,G,R,A]` — same R/B swap, alpha preserved (premultiplied vs straight alpha: use straight and rely on coverage blend mode; if the spike showed artifacts, premultiply during the swap — decide from U1 notes and record the choice).
- When overlay setup fails at Init: log Info, set `overlayOK=false`; driver continues exactly as today (chrome baked into photo presents by the engine — U5 handles that mode).

**Patterns to follow:** existing `commitFB` single-funnel design; per-object property maps; `allocSlot`/mmap idioms; layout_test pinning.

**Test scenarios:**
- Happy path: pure plane-selection function — fake plane list with formats/masks yields expected primary/overlay; overlay skips writeback/self planes; no candidate → not supported.
- Happy path: `writeARGB8888` byte mapping incl. transparent and semi-opaque pixels (mirror TestWriteXRGB8888 style).
- Edge case: window math — full-window default maps to `panel<<16`; a zoomed window keeps `SRC_X + SRC_W ≤ width<<16` at extremes.
- Edge case: commit property counts — `counts[]` per object match array lengths when blend/zpos are present vs absent (fake capability maps).
- Error path: overlay buffer creation failure (simulate via ioctl-failure seam if one exists, else defer to on-device U9) → driver still initializes, `MotionCapable()==false`.
- Window hygiene (moved here from U6 — driver-level invariant): fake ioctl array capture asserting emitted `SRC_*` per call site — still commit carries the stored window; half-res/transition commits carry the full window for their FB and do NOT mutate the stored window.

**Verification:** on the Pi, photo and chrome visibly render via two planes (overlay alpha-correct, clock crisp while U6 drift runs — proven in U9); existing present/flip tests unaffected.

---

- [ ] U5. **UI engine: chrome layer split with capability fallback**

**Goal:** chrome draws to its own buffer; devices with overlay support present it as the static layer; everything else (headless, dev, non-overlay DRM) keeps compositing chrome onto the main canvas exactly as today.

**Requirements:** R2/R6 architecture support; enables AE1's crisp-text outcome; prerequisite for U6 and U7.

**Dependencies:** U4.

**Files:**
- Modify: `internal/frame/ui/ui.go` (second canvas field, capability detection in `New`/`Run`), `internal/frame/ui/render.go` (chrome functions take target canvas via a helper), `internal/frame/ui/input.go` (draw paths that trigger chrome present)
- Test: `internal/frame/ui/*_test.go` (existing suite must pass unmodified — that IS the headless-parity proof), plus new `internal/frame/ui/overlay_split_test.go` using a fake overlay-capable driver

**Approach:**
- Chrome drawing calls (`drawClock`, `drawCaption`, `drawControls`, star, "swipe scroll/hold" hints, ribbon in U7) receive the chrome canvas when overlay-capable, the main canvas otherwise — one boolean branch at the render top, not per-function conditionals.
- Present flow when overlay-capable: photo layer = `e.curImage` via existing `PresentFrame`; chrome layer = `PresentOverlay(chromeCanvas)` **only when chrome content changed** — a `chromeDirty` flag set at the draw sites that today trigger `render()` (minute rollover, controls toggle, caption/ribbon change, screen entry). Presenting on every `render()` is rejected: a full-panel ARGB convert + commit under the coarse `d.mu` would ride every transition and browsing frame, contradicting this unit's own "~1 Hz chrome" acceptance argument.
- Snapshot reality (doc review): `canvas.SnapshotPNG` has no production caller and the snapshot ring is a headless-driver artifact — there is no on-device screenshot path today, and this plan does not add one (the chrome canvas is the future hook). "Verify via snapshot" means headless snapshots only; device evidence lives in U9.
- Transition delta (deliberate, owned here): today chrome vanishes for the dissolve duration; with a static overlay, chrome persists across it. To avoid attributing text to the wrong image, incoming caption/ribbon draws are **deferred until the transition ends** (in-transition state non-nil → previous photo's caption state kept); the persisting clock pill is an intentional improvement over today's dropout, verified against the panel in U9 — not against pre-split screenshots.
- Sleep: the sleep-screen render must clear the chrome layer fully transparent — the black frame is the photo plane; a stale clock pill left on the overlay would glow over the sleep screen.
- Headless/dev drivers never see the new interfaces → all existing tests, `env.px`, and snapshot tests unchanged.

**Execution note:** land with the existing test suite green unedited — any required edit to a chrome pixel test signals the fallback branch is leaking into headless.

**Patterns to follow:** optional-interface capability checks like `display.TransitionDriver`; existing canvas abstraction as the single draw surface.

**Test scenarios:**
- Covers AE8-adjacent / integration: with a fake overlay driver, render advances both buffers exactly once; chrome buffer contains clock pixels, photo buffer contains no chrome pixels (sample pixels at clock pill area).
- Happy path: without overlay capability (headless), output byte-identical to today for the same inputs (golden test on the headless driver's last image).
- Edge case: minute rollover dirties chrome → overlay re-presented; idle seconds within the same minute produce zero `PresentOverlay` calls (mandatory assertion of the dirty-flag rule).
- Covers sleep hygiene: fake overlay driver records a fully transparent `PresentOverlay` after `SetAsleep(true)`, and after wake before the next slideshow chrome draw.
- Error path: `PresentOverlay` returning error sets `lastPresentErr`, does not halt the slideshow.

**Verification:** on the Pi the panel shows the same content as pre-split (eyeballed; no on-device snapshot exists) with the intentional chrome-persists-through-dissolve delta; the headless test suite passes with zero edits (fallback-branch parity proof).

---

- [ ] U6. **Motion controller: per-photo drift on the photo plane**

**Goal:** the calm Ken Burns drift (R1–R4, R6) driving U4's window.

**Requirements:** R1, R2, R3, R4, R6; AE1, AE2, AE3.

**Dependencies:** U4, U5.

**Files:**
- Create: `internal/frame/ui/motion.go` (rect math pure functions + controller state)
- Modify: `internal/frame/ui/ui.go` (Run select gains a 200 ms motion ticker case), `internal/frame/ui/input.go` (`advance`/debut reset motion phase)
- Test: `internal/frame/ui/motion_test.go`

**Approach:**
- Pure function: `driftWindow(t01 float64, img image.Rectangle, focal image.Point, zEnd float64) (x, y, w, h uint32 /*16.16*/)` — start = fitted-image rect (no crop at t=0), end = rect scaled by `1/zEnd` positioned so the focal fraction lands where chosen; everything clamped inside the image rect (R3). `zEnd` const = 1.08 initial value.
- Per-photo params generated at display time: focal point sampled with a centre bias; deterministic given a seed (testable via injected randomness or pure-input function).
- Tick (in Run, under `e.mu`): advance phase when motion gates are open — screen == slideshow, not asleep, Motion != off, overlay-capable, controls hidden. **No transition gate (Decision 10):** the tick shares the `Run` goroutine that blocks inside `startTransition`, so ticks cannot land mid-dissolve, and transition nesting unwinds immediately (its gesture branch presents and returns). Phase per Decisions 4/9: clock-derived with paused-time excluded; `zEnd = clamp(1 + rate × effectiveDuration, 1.04, 1.08)`. No driver `TryLock` per Decision 7. Compute new window; only call the driver when it changed by ≥ ~1/16 px (cheap de-noise).
- Sleep/power: on sleep entry AND wake, reset the window to full and re-arm the phase clock inside the same `e.mu` section as `SetAsleep`'s render — explicit because `SetPower` is a sysfs-only call that never commits; a mid-drift window would otherwise survive power-off and crop the wake frame. `advance()` resets phase; controls-visible pauses; hidden → resumes from current time by wall clock (no snap-back).
- **Window reset sites (W3) / phase sites (W4):** `presentFull`, `advance`, any screen change into or out of the slideshow, Motion=off application, sleep enter/exit (above; inside the same `e.mu` section as `SetAsleep`'s render, explicit because `SetPower` is sysfs-only). W5: a window ≠ full implies every gate is open. W1: `SetSourceWindow` is the sole window writer; W2 lives in U4 (still-photo commits only). Closure rule: every still commit re-applying a stored non-full window follows a W3/W4 reset for the current photo — the post-transition endpoint is always a `presentFull`, so W2 suppression cannot strand a stale window.
- Motion defaults from config (U8 wires values; here: field exists and gates on it).

**Patterns to follow:** `RecentlyActive` gating style; `cfg.Now()` injectability for deterministic tests; pure-function-plus-side-effect separation used in touch/canvas code.

**Test scenarios:**
- Happy path (pure math): t=0 window == image rect exactly; t=1 window == expected 1/1.08 rect at chosen focal anchor; intermediate windows monotonically shrink and stay inside the image rect (property loop over t in [0,1]).
- Covers AE2: portrait photo letterboxed on the panel — image rect derived from fit math; window never covers letterbox area.
- Happy path (engine): fake overlay driver records `SetSourceWindow` calls; ticking Run forward 10 motion intervals while slideshow static produces ≥ 1 call; Motion=off produces none.
- Edge case: advance() during drift → phase resets, next window equals full rect; controls shown → no window commits while visible; auto-hide → drift continues without visible jump.
- Covers AE3: touch activity freeze — `controls=true` + window call count unchanged over subsequent ticks.
- Window hygiene (deepening pass): fake overlay driver records every window value — a full-panel commit issued while a half-res dissolve path is active carries the full window, and after `advance()` mid-drift the next committed window equals the t=0 rect exactly.
- Error path (Decision 11): transient window-set failures back off and retry next tick; N consecutive (default 5) disable motion for the current photo only with one Warn; the next advance re-arms. A single EBUSY never kills the feature. `ErrDriverClosed` at shutdown: no flag, no Warn.
- Covers AE1 salience: a 90 s pause starting at t=20 s of a 120 s slot yields the window for effective t=20 s on resume (phase frozen while paused); no photo clamps at max zoom before its slot ends.
- Clock hygiene: injected clock steps backward mid-photo → window stays inside the image rect, never jumps to end-of-drift crop.
- Perf guard (unit): 10 k drift-window computations with zero allocations in the hot path.

**Verification:** on the Pi the photo drifts while the clock/caption stay pixel-still; `top` shows negligible CPU delta vs motion off.

---

- [ ] U7. **Arrival debuts and the "from Sarah" ribbon**

**Goal:** implement F2 end-to-end: queue from activation, next-slot debut, fading chrome-layer ribbon, caps, sleep behaviour.

**Requirements:** R7–R11; AE4–AE7.

**Dependencies:** U3 (arrival list via extended callback). **U5 is an enhancement, not a prerequisite** (doc-review resequencing): the ribbon draws onto the main canvas whenever no chrome layer exists — Increment 1 ships the arrival moment without waiting for the motion architecture.

**Files:**
- Modify: `internal/frame/ui/ui.go` (queue fields, Reload consumption), `internal/frame/ui/input.go` (`advance` debut targeting), `internal/frame/ui/render.go` (ribbon draw), `internal/frame/ui/settings.go`-adjacent helpers as needed
- Test: `internal/frame/ui/arrivals_test.go`

**Approach:**
- On the extended activate callback (`OnActivate(rev, arrivals)`): cap the in-memory queue at 3 (R9, oldest-first kept), intersect with the loaded collection (deleted between activation and reload → drop), append within the cap when a second wave lands while debuts are pending (memory-only; restart may drop — Decision 5).
- Map each queued hash → uploader display name via the loaded collection (`Uploader`; empty → "new photo" fallback text per R12).
- `advance()` (interval tick or auto-advance): if the debut queue is non-empty, the next advance displays the first queued photo (set idx to its position) instead of idx+1, and arms `ribbon = {text, until = now+8s}`. Natural rotation onto a queued photo (she swiped to it) also consumes + arms the ribbon.
- **Ribbon re-arm on wake / queue persistence:** a ribbon armed just before sleep that would have expired during the sleep window is re-armed for its full duration at the wake debut; the in-memory queue survives browsing excursions (gallery/album trips do not consume debuts); a second activation while the queue is non-empty appends arrivals up to the cap of 3, earliest debut keeps priority.
- Ribbon render (placement fixed at plan level, not deferred): **top-left band** — x = `margin`, y just below the top controls bar's resting edge — clear of the top-right clock pill, the bottom-left FAVOURITE badge, and the caption bar (R8). Z-order: photo → ribbon pill → controls. Fade = scrim alpha only over the last ~2 s; glyph alpha holds ≥ 90 contrast floor (Decision 6).
- **The ribbon owns its render driver:** while `ribbon.until > now`, `Tick` marks the frame dirty unconditionally — with Clock OFF there is otherwise no repaint, and the ribbon would hang at full alpha (past its deadline, over the wrong photo) until the next advance. Guards from Decision 6: draw only while `e.idx == ribbon.idx`; any advance/screen change cancels (debut spent); controls-visible holds the fade clock.
- Queue and ribbon state are memory-only beyond the `arrivals` row (consumed once); frame restart after consumption may drop unshown debuts — accepted (origin scope).
- Ribbon display only while awake (screen gating handles it; during sleep nothing advances — R11 satisfied without extra state).

**Patterns to follow:** `flashStatus` deadline pattern for the ribbon TTL; `e.cfg.Now()` for fade timing; existing `blit`/chrome draw idioms.

**Test scenarios:**
- Covers AE4: activation with 1 arrival → second `advance()` displays it with ribbon "from Sarah"; ribbon gone after 9 s of ticks.
- Covers AE5: 5 arrivals queued → capped queue: three debuts across three advances with ribbons; queue empty afterwards; no further ribbons.
- Covers AE6: first population (no arrivals row written in U3) → no debut; slideshow starts normally.
- Covers AE7: arrivals consumed while asleep — no advance ticks fire asleep (existing shouldAdvance gate); after `SetAsleep(false)`, first advance debuts with ribbon.
- Edge case: arrived photo deleted from the collection before debut → skipped silently, next queued debut proceeds.
- Edge case: photo with empty Uploader → ribbon text "new photo".
- Edge case: ribbon armed 21:59, frame sleeps 22:00, wakes 07:00 → ribbon re-armed for the full duration at the wake debut.
- Edge case: second activation arrives while queue holds 1 → grows to ≤3; first-in keeps debut priority.
- Covers Clock=off: with `ShowClock=false`, fake overlay driver records ≥ 7 `PresentOverlay` calls across an 8 s ribbon and zero ribbon pixels after its deadline.
- Edge case: debut while controls are open → fade held; auto-hide → remaining readable time plays out fully.
- Order fairness: a 5-photo wave from two senders debuts the earliest-uploaded first (R7 via `uploaded_at`).

**Verification:** on the Pi with a real upload from the admin UI, the debut appears at the next slot with a crisp ribbon on the static layer.

---

- [ ] U8. **Motion configuration: settings row + server override**

**Goal:** R5 end-to-end: one frame-side Motion row (calm/off) with the existing staged→held-apply flow, server fleet default, precedence intact.

**Requirements:** R5; supports AE1 (motion on retention).

**Dependencies:** U6 (field it toggles).

**Files:**
- Modify: `internal/frame/ui/settings.go` (Settings field, calm/off choices, cycle), `internal/frame/ui/render.go` (items row + `settingIndex`), `internal/frame/ui/ui.go` (`Config.Motion`), `internal/frame/settings.go` (persist key `settingMotion`, validation), `internal/frame/ui_wiring.go` (`applyUIState` save, default wiring), `internal/frame/health.go` (`motion_active`, `motion_supported`+reason in the heartbeat — Decision 2), `internal/frame/sync/engine.go` (`Config.Motion *string`), `internal/server/devices/config.go` (field + Validate), `internal/server/manifest/manifest.go` (config copy-through), `web/admin/src/views/frames.ts` (three labelled options: follow default / calm / off → nil/calm/off; show motion capability per device)
- **Row index rule (doc review P1):** insert Motion at **index 8 — after Clock, before Wi-Fi** — so `editorSettingNames` (0–5) is untouched and hold-to-type keeps mapping correctly; update **both** range guards `btnSetting+9` → `btnSetting+10` (`input.go` `onHold` + `doButton`) or the new row is unreachable and taps silently do nothing. Below the first fold at 1280×800 — acceptable: the recipient configures nothing; the row exists for the admin.
- Test: extend `internal/frame/settings_test.go`, `internal/frame/e2e_test.go` (settings flow), `internal/server/devices/*` config validation tests, `internal/frame/ui` motion-gate test from U6 gains the setting path

**Approach:**
- Values: `"calm"` and `"off"`; default calm; cycle row (no typed editor needed — editor exclusion consistent with `editorSettingNames`).
- Local setting wins server wins file (existing precedence); tri-state pointer server field mirrors `ShowClock *bool`.
- Server validation rejects anything else (400 at config-set time); frame `applyLocalSettings` rejects bad persisted values without bricking boot (mirror the HH:MM heal lesson: validate + fall back to default with a Warn, never crash-loop).
- Admin UI: single select in the frame config editor.

**Patterns to follow:** Transition row (2 choices) as the template; `ShowClock` pointer precedent; `applyLocalSettings` error-class tests.

**Test scenarios:**
- Happy path: cycle row calm→off→calm; staged value persists only on hold-Apply (existing gate test); after apply, live engine config reflects it immediately (motion stops/starts — reuse U6 gate test).
- Happy path: server config `"motion":"off"` with no local override → frame applies off; local override calm → calm (precedence test).
- Error path: server sends garbage → devices.Validate rejects (400); frame receives via legacy/unknown value → falls back to calm with Warn, does not fail boot (boot-crash regression class from the HH:MM incident gets an explicit test).
- Edge case: `applyLocalSettings` with `motion=bogus` in frame.db → default, warn, no error (boot-safety regression class from the HH:MM incident, explicit test).
- Capability: fake incapable driver → row renders "off (n/a)", tap logs once without state change, heartbeat reports `motion_supported=false` with reason, `motion_active` reflects the effective post-precedence value.

**Verification:** toggling on the Pi (hold-to-apply) changes motion live; admin UI fleet default + per-device override work through the existing config chain.

---

- [ ] U9. **On-device validation, tuning pass, and hardware docs**

**Goal:** prove AE1–AE8 on real hardware, tune the drift curve and ribbon placement, and leave the repo smarter than it was found.

**Requirements:** all AE; R6 (thermal/CPU evidence); success criteria (motion-on retention proxy).

**Dependencies:** U1–U8.

**Files:**
- Modify: `docs/hardware/pi-3b-plus.md` (validation checklist items now actually validated; motion/overlay notes), `docs/hardware/measurements.md` (fill remaining fields touched: commit cadence, plane data, power deltas), `docs/adr/ADR-0006-display-driver.md` (M4 notes addendum: two-plane compositing + motion scope update), `docs/architecture/overview.md` display-pipeline paragraph.

**Approach:**
- **Household trial (owns success criterion 1):** after the AE walkthrough, run the frame in the recipient's home for 7 days; at close the admin records (a) whether motion was noticed unprompted and (b) whether Motion deviated from calm — read from the heartbeat's `motion_active` field (U8), not guessed. Family logistics; scheduled, not assumed.
- Deploy to the Pi; drive the full flow (upload from the admin UI, arrival debut, sleep wake, touch pause) and photograph the panel for the ribbon pass (no on-device snapshot path exists — U5 says so plainly).
- Power caveat: record the board's under-voltage flags alongside measurements; re-test on a solid supply before finalizing "negligible CPU" claims.
- Rollout notes (product lens): senders get no in-app debut confirmation — set the expectation verbally before launch ("upload → her frame shows 'from you' for ~8 s"); a one-line post-upload confirmation is the cheapest follow-up (tracked with the deferred reply-loop plan). Fleet note: count devices before the U2 deploy; the one-time global revision bump is cheap per device but unsized at fleet scale — state whether per-device `sync_interval_sec` phase spread actually staggers it.
- Tune: drift `rate`/focal spread/easing, ribbon timing; **tuning exit criterion: a non-technical household viewer independently reports the picture moves** — AE1's bar governs smoothness, not salience; `rate` may rise toward the zEnd clamp until then. Record chosen constants back into U6 so the ADR reflects reality.
- `docs/hardware/measurements.md` ownership: U1 wrote **Raw spike data**; U9 owns **Validated on device** (5 Hz cadence on a solid supply, thermal deltas, chosen constants), Method lines citing the committed probes under `docs/hardware/probes/`.
- ADR-0006 addendum must record: two-plane compositing replaces the "single primary plane" scope line, AND the rotation invariant — when plane rotation is implemented it applies to **primary and overlay together**.

**Test expectation:** none — validation + documentation unit; any tuning that changes behaviour lands as constant edits with the existing unit tests re-run.

**Verification:** every AE re-checked by a human on the panel; docs updated; ADR-0006 no longer says "single primary plane".

---

## System-Wide Impact

- **Interaction graph:** manifest content hash (revision) shifts once on U2 deploy — affects every device simultaneously (stage + activate, zero downloads); confirm arrival diff (hash-set based) does NOT fire on that upgrade (same hashes → no ribbon).
- **Error propagation:** overlay setup failure degrades to no-motion silently by design (Info log); window-set failures disable motion per-photo, never the slideshow; PresentOverlay errors follow the existing "last image stays on panel" rule.
- **State lifecycle risks:** `arrivals` row consumed-but-crashed edge (delete-after-consume loses pending debuts — accepted); TTL bounds staleness; sync_state writes stay inside the activation transaction.
- **API surface parity:** `display.Driver` interface unchanged for non-DRM drivers; new optional interfaces only. Server API: devices.FrameConfig gains an optional field (back-compat readers unaffected).
- **Integration coverage:** U5's headless-parity rule (existing suite passes unmodified) and U9's live AE walkthrough are the two proofs that cannot come from unit tests alone.
- **Unchanged invariants:** transition path keeps its own present flow (and structurally blocks motion ticks by goroutine identity — Decision 10); flip-event ping-pong and stall-unlock rules untouched; sleep/touch/wake behaviour preserved (the week's hard-won fixes stay green); no on-device snapshot path added or implied; headless/dev rendering and every existing pixel test stay single-canvas.
- **Health/status surface:** heartbeat gains `motion_supported`(+reason) and `motion_active`; `openframe status` and the admin frame view display them — closing the "admin set fleet calm, device physically cannot" voodoo-config gap.

---

## Risks & Dependencies

| Risk | Mitigation |
|------|------------|
| Overlay plane lacks ARGB or blend support on this vc4 build | U1 spike precedes all code; kill-gate documented; arrivals (U3/U7) still ship value without motion |
| Property-only commits at 5 Hz interact with flip-event bookkeeping (EBUSY while flips pending) | Motion commits request no events and skip FB changes; U1 measures the interaction explicitly; U6 throttles on failure |
| Sub-pixel SRC animation shows quantization/stepping | 16.16 precision + 200 ms cadence + ≥1/16 px change filter; tuning is a U9 constant, not an architecture risk |
| Chrome buffer updates tear (single overlay buffer) | Accepted: chrome changes are ~1 Hz text; a torn clock pixel is below perceptual threshold; revisit with ping-pong only if U9 shows artifacts |
| One-time manifest revision bump (uploader/uploaded_at) hits every device at once | Per-device cost is cheap (stage + activate, zero downloads), but fleet size is unrecorded: count devices before rollout; if >~25 on one uplink, stagger via the updater channel or rely on per-device `sync_interval_sec` phase spread (state which actually staggers). Routine caption/favourite edits also bump revisions — the empty-arrival-callback path (no side effects) is the invariant keeping those from disturbing debuts |
| Driver mutex newly serializing paths that previously raced on purpose? | Research confirmed no existing internal locking; all prior safety came from callers — the mutex strictly narrows risk. Coarse section kept whole (Decision 7): the ≤40 ms event wait mutates the same FIFO fields as `commitFB`; motion never joins that wait because event-free commits skip `evq` |
| Sync-loop stall: `OnActivate → Reload` blocks on `e.mu`, which render paths can hold ~2 s (reachability probe) | Pre-existing and bounded — Check simply waits; motion adds a third waiter class to the same chain, stated for awareness |
| Under-voltage test board skews thermal claims | U9 records flags and re-verifies on stable power before closing success criteria |

---

## Documentation / Operational Notes

- `docs/hardware/measurements.md` finally gets filled in (the empty-template finding this week); U1 + U9 own it.
- ADR-0006 gets an addendum (scope change: single-plane → two-plane compositing is a visible architectural delta — worth its own ADR amendment paragraph rather than a new ADR).
- Rollout order: server first (U2) is safe at any time; frames upgrade via the normal updater. Old server + new frame = missing uploader → "new photo" fallback ribbons; new server + old frame = ignored field.
- The threat model should gain a line on the chrome/overlay buffers holding rendered captions (no new data; same as today's framebuffer) — small addition to `docs/security/` at U9 if the reviewer pass agrees.

---

## Sources & References

- **Origin document:** [docs/brainstorms/2026-11-23-living-presentation-requirements.md](../brainstorms/2026-11-23-living-presentation-requirements.md)
- Related code: `internal/frame/display/drm/drm.go` (commitFB, ping-pong, flip events), `internal/frame/sync/engine.go` (maybeActivate/activatePhotos), `internal/server/manifest/manifest.go`, `internal/frame/ui/render.go` (chrome set), `internal/server/photos/photos.go` (uploader join precedent)
- This session's hardware verifications: HVS half-res upscale commit OK on plane 86/crtc 97; 56 planes enumerated; commit cost ~0.25 ms; SRC_X/SRC_Y present in plane property lists (ids 9/10)
- Related commits: `9df33ca` (DRM probe fixes that made all of this testable), `d6f3794` (touch pipeline), `d89fba0` (animation perf groundwork)
