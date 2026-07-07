# Expo / React Native project

You are working in an existing Expo (React Native) app that the user previews
live on their phone via hot-reload. It is Expo — do not ask which framework to
use and do not re-scaffold; learn the project's conventions from the code that
is already there. Keep the app runnable at all times: the preview reloads on
every save, so work in small, verifiable increments.

Principles that prevent the most damage:

- **Let Expo own versions and native config.** Add packages with
  `npx expo install` (never a raw package manager): native-module versions are
  locked to the Expo SDK, and a mismatch breaks the user's preview build. The
  `ios/` and `android/` trees are generated — configure native behavior through
  `app.json`/config plugins, never hand-edits.
- **Treat per-frame work as a budget.** Smooth means cheap work on the right
  thread every frame. Continuous animation is a cost you spend deliberately,
  not a default. When something janks, attribute before you fix: the JS
  thread, the UI/main thread, and the GPU are different problems with
  different levers.
- **Never draw performance conclusions from a debug build.** Debug inflates
  JS and render cost 2–5×; confirm any "too slow" verdict in a release build
  before re-architecting.
- **Stable identity is leverage.** Memoized components and virtualized lists
  only skip work when unchanged data keeps the same reference. Preserve
  identity before reaching for heavier machinery.
- **Design for the device, not the demo.** Real safe-area insets over
  hardcoded padding; comfortable touch targets; loading, empty, and error
  states on every async surface; reserved space so nothing jumps; both
  platforms checked.
- **If the device stops reflecting your edits, suspect a stale bundle** before
  trusting any measurement or conclusion.

A detailed playbook is installed as the `expo-dev` skill
(`.claude/skills/expo-dev/`): dependency rules, performance mechanisms with
case studies, a profiling toolkit, and a UX polish checklist. Read the
relevant reference there before optimizing performance, debugging jank, or
polishing UI.
