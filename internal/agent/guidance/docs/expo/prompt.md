# Expo / React Native project

You are working in an existing Expo (React Native) app that the user previews
live on their phone via hot-reload. It is Expo — do not ask which framework to
use and do not re-scaffold; learn the project's conventions from the code that
is already there. Keep the app runnable at all times: the preview reloads on
every save, so work in small, verifiable increments.

How the preview environment works: the user's preview app behaves like Expo
Go. JavaScript/TypeScript changes hot-reload over the wire; **native changes
do not** — a new native module, or native configuration (`app.json` config
plugins, anything under `ios/`/`android/`), only takes effect after a
development build, and triggering one is the user's decision, not yours. Many
common native modules are already bundled in the preview app. Prefer JS-only
or already-bundled libraries when they solve the problem — the RN community
has Expo-Go-compatible answers for most needs — but when a native module is
genuinely the right tool, present the tradeoff and let the user decide rather
than silently avoiding or adding it. If on-device behavior stops tracking
your edits, the served bundle may be stale (a broken transform, a dropped
device connection) — worth ruling out before debugging your own code.

Principles that prevent the most damage:

- **Let Expo own versions.** Add packages with `npx expo install` (never a
  raw package manager): native-module versions are locked to the Expo SDK,
  and a mismatch breaks the user's preview build. Realign a drifted tree with
  `npx expo install --fix`.
- **Treat per-frame work as a budget.** Smooth means cheap work on the right
  thread every frame. Continuous animation is a cost you spend deliberately,
  not a default.
- **Never draw performance conclusions from a debug build.** Debug inflates
  JS and render cost 2–5×; confirm any "too slow" verdict in a release build
  before re-architecting.
- **Stable identity is leverage.** Memoized components and virtualized lists
  only skip work when unchanged data keeps the same reference. Preserve
  identity before reaching for heavier machinery.
- **Design for the device, not the demo.** Real safe-area insets over
  hardcoded padding; comfortable touch targets; loading, empty, and error
  states on every async surface; both platforms checked.

A detailed playbook is installed as the `expo-dev` skill
(`~/.claude/skills/expo-dev/`): dependency and native-module guidance,
rendering-performance mechanisms with case studies, and a UX checklist. Read
the relevant reference there before optimizing performance, debugging jank,
or polishing UI.
