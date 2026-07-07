---
name: expo-dev
description: Expo / React Native development playbook. Use when adding or upgrading dependencies, debugging performance, jank, or "the app feels slow", profiling on-device, working with animations or large lists, or polishing UI/UX (layout, keyboard, safe areas, touch targets). Contains the mechanisms, a profiling toolkit, and case studies.
---

# Expo development playbook

Distilled lessons first; each links to a reference with the mechanism, the
profiling recipes, and the case studies. Read the reference that matches your
task before acting — the details there exist because each one has broken a
real app.

## The lessons

- **Version alignment is the #1 environment breaker.** The Expo SDK locks
  `react`, `react-native`, and every native module together; any version
  change outside `npx expo install` risks a native/JS mismatch that breaks
  the build. Realign a drifted tree with `npx expo install --fix`.
  → [dependencies.md](dependencies.md)
- **Jank has exactly three homes** — the JS thread, the UI/main thread, and
  the GPU — and the fix depends entirely on which one you're in. Reanimated
  escapes a busy *JS* thread, not a busy *main* thread; animation smoothness
  is orthogonal to React re-render counts. Measure first, change one variable
  at a time, and never conclude from a debug build without the debug→release
  ratio. → [performance.md](performance.md)
- **Anything rendering every frame must earn it.** An idle screen should
  render ~0 frames. Many animated views can collapse into one Skia draw;
  allocation inside a 60fps worklet surfaces as periodic GC frame-skips.
- **Lists pay off only with stable identity.** Virtualization and memo bail
  out on reference equality, not intent. The mounted window is a tradeoff:
  bigger scrolls smoother but mounts more up front and makes reflows cost
  more.
- **A "slow transition" is almost always a slow commit.** The stack starts
  its slide only after the destination screen commits — render a cheap shell
  first, defer heavy content by a frame, and seed from cache instead of
  blocking on a fetch.
- **Polish is a checklist, not taste.** Insets from the safe-area API, ≥44pt
  touch targets, keyboard never covering the focused input, all three async
  states (loading/empty/error), reserved space so nothing jumps, both
  platforms checked. → [ux.md](ux.md)
- **Distrust the dev loop before distrusting your code.** When on-device
  behavior stops tracking edits, verify the served bundle; cold-reload (not
  Fast Refresh) to test animation changes.

## References

- [dependencies.md](dependencies.md) — the install rules and why they exist
- [performance.md](performance.md) — mechanisms, profiling toolkit (gfxinfo,
  idle-render test), debug-build discriminator, and the case studies
- [ux.md](ux.md) — the UX checklist: icons, touch targets, safe areas,
  keyboard, layout stability, text, platform differences, accessibility
