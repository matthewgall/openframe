# ADR-0013: Destructive actions require confirmation

**Status:** accepted (user-confirmed rule, admin UI audit)

## Context

A stray tap on the Frames page *Rotate* (device credential) rotated a
live credential with no confirmation. The frame's current credential
died immediately and the new one was shown only once — a misclick
orphaned the frame until someone physically reached the device to
re-pair it. Cheap to misclick, expensive to undo: the cost was paid
out-of-band (a person, a device, a lost secret).

## Decision

The rule, stated by the project owner and applied to every
interface (admin SPA today; frame touchscreen already follows it via
hold-to-apply):

> **If undoing an action requires a device, a person, or the only
> copy of a secret — confirm it.**

Baseline audit of the admin SPA (2026-11-15):

| Action | Undo path | Confirmation |
|---|---|---|
| Rotate device credential | device (re-pair) + show-once secret | **added** |
| Revoke user sessions | person (user re-authenticates) | **added** |
| Revoke device | none (terminal) | already |
| Delete photo / batch | person (restore) — but in-UI, so borderline | already |
| Delete album | in-UI restore | already |
| Reset user password | person + show-once secret | already |
| Deactivate user | in-UI | already |
| Set album cover, remove-from-album, unassign frame, restore photo/album | two clicks in the same UI | deliberately none — confirmation would tax routine management |

Implementation notes:

- `confirmAction()` in `web/admin/src/components.ts` resolves before
  closing (an earlier bug resolved `false` before `true`).
- Confirmation copy must state the consequence, not just the action
  ("its current credential stops working immediately … shown only
  once"), so the user can judge the specific risk.
- First-time *issue credential* is ungated: it breaks nothing.
- The frame UI's hold-to-apply is the same rule in a different
  gesture (no cursor, no modal — hold is the confirm).

## Consequences

- New destructive endpoints/UI in code review: check the rule before
  shipping. An action that passes "undo needs a device, a person, or
  the only copy of a secret" must gate on confirmation.
- Confirmation fatigue is the opposing failure mode: actions
  reversible in two clicks inside the same UI stay ungated.
- The frame UI must apply the same rule to any future on-screen
  action (hold-protect or equivalent), not just settings apply.
