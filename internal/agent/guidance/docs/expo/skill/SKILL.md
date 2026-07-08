---
name: expo-dev
description: Expo / React Native development playbook. Use when adding or upgrading dependencies (especially anything native), debugging performance or jank, working with animations or large lists, or polishing UI/UX (layout, keyboard, safe areas, touch targets). Contains mechanisms and case studies, not just rules.
---

# Expo development playbook

Read the reference that matches your task before acting. The case studies are
the ground truth, the principles are the generalization — where a reference
states something as a rule, assume there's a context where the opposite is
right.

- [dependencies.md](dependencies.md) — how dependencies interact with the
  Expo SDK and the live preview environment, and how to reason about native
  modules vs JS-only alternatives.
- [performance.md](performance.md) — mechanisms for smooth rendering:
  which thread does what, animations, lists, debug-build overhead, dev-loop
  debugging, and the case studies behind them.
- [ux.md](ux.md) — the UX checklist: icons, touch targets, safe areas,
  keyboard, layout stability, text, platform differences, accessibility.
