# React Native performance — gotchas & principles

A **running document** for building smooth Expo / React Native apps. The user
previews the app live on their phone, so performance is directly user-visible:
smooth (≈60fps) is the bar and visible jank is a high-priority bug.

**How to read this:** these are heuristics and tradeoffs, not laws. Almost every
perf decision is case-by-case — the goal is to understand the *mechanism* so you
can make the call for your situation, then let the profiler decide. Where this
doc states something as a rule, assume there's a context where the opposite is
right. The case studies are the ground truth, the principles are the
generalization.

---

## First principles

1. **Continuous rendering is a cost you should spend deliberately.** A GPU only
   produces a frame when something invalidates; any *ongoing* animation pins it
   to ~60fps for as long as it runs (battery, heat, less headroom for other
   work). That's a fine price for active feedback or a hero moment — and a waste
   for a permanent idle loop nobody asked for. The question is never "animations
   are bad," it's "is this frame-every-16ms earning its keep right now?" When the
   answer is no, make it finite, gate it on visibility, or stop it when
   off-screen/backgrounded/done. (The idle-render test below tells you if a
   screen is quietly looping.)

2. **Per-frame work is fine — if each frame is cheap and on the right thread.**
   Things that legitimately move every frame (keyboard tracking, scroll-linked
   effects, gestures) can be perfectly smooth at 60fps. What makes them janky is
   usually not the motion, it's *what you do per frame*: a cheap GPU transform
   (translate/scale/opacity) on the UI thread is fine; a relayout, a full
   recomposition, or a hop to the JS thread every frame is not. Move the per-frame
   update down to a transform, off the JS bridge.

3. **Keep animated state off the JS bridge — but know which thread you bought.**
   Reanimated worklets (`useAnimatedStyle`) and native-driven `Animated`
   (`useNativeDriver`) run on the UI thread, so a dropped JS frame doesn't stutter
   the animation. **But the UI thread *is* the platform main thread** — it also
   runs native view mounting, text measurement/layout, and GC. So you've escaped a
   busy *JS* thread, not a busy *main* thread. If a worklet-driven animation still
   janks, the contention is on the UI thread itself (a big list mounting, a
   `TextInput` re-shaping, per-frame view-prop churn, GC) — and moving work *off
   JS* (memo, state isolation, background worklet runtimes) won't touch it. (An
   *infinite* one still renders every frame — that's principle #1, not a
   contradiction.)

4. **Stable identity is leverage.** Preserving object/reference identity for
   unchanged data lets memoized components and virtualized lists *bail out* of
   work. This is often higher-impact than swapping components or libraries.

5. **Measure, isolate, verify.** Change one variable per measurement, and
   confirm the change actually took effect (not a stale bundle or a no-op) before
   trusting the result. Debug builds inflate jank ~2–5× (no R8, dev-mode
   rendering, JS over Metro) — compare *relative* improvement and confirm wins
   survive a release build.

---

## Worked example: keyboard avoidance

Smoothly following the keyboard at 60fps is absolutely doable — plenty of apps do
it. The decision is *what the content needs* and *how you move it*:

- **If content should track the keyboard** (text inputs, footers, a composer
  that rides up with it): drive a **UI-thread translation that follows the inset
  each frame**. On Android that's a `WindowInsetsAnimation` callback applying a
  translation; in RN, libraries like `react-native-keyboard-controller` expose
  worklet keyboard handlers for exactly this; in Compose, animate a
  `graphicsLayer`/offset or use the inset-consuming modifiers. Cheap per frame →
  smooth tracking.
- **If content only needs to clear the keyboard** (e.g. a floating box that just
  must not be covered, with no need to visually ride the keyboard): animating
  **once to the settled target** is simpler and does zero per-frame work.
- **The anti-pattern** (the floating-box bug below): reading the *animating* inset
  inside the layout/composition path, so the framework recomputes layout and
  re-runs effects every frame of the slide. That's the jank — and it's true
  whether you're tracking or clearing.

So: pick tracking vs. clearing based on the UX, then make the per-frame cost a
transform, not a relayout. Both can be 60fps.

---

## Animations

- **Infinite vs. finite vs. gated** is the main lever (principle #1). An infinite
  loop is right for ongoing status (a live waveform, a working spinner) and wrong
  as a permanent ambient effect; in between, a finite or visibility-gated
  animation often gives the same perceived affordance for a fraction of the cost.
  It's a UX/cost tradeoff, not a rule — decide per element.
- **A worklet animation's smoothness is orthogonal to React re-renders.** It runs
  off shared values on the UI thread, so the owning component can re-render zero or
  a hundred times a second and the motion is identical. Chasing re-renders (memo,
  state colocation) is worth doing for *render cost*, but it is **not** the lever
  for animation *jank* — the lever is UI-thread availability. Prove it with a
  render-count log before optimizing the wrong thing (one investigation showed a
  janky waveform was already memoized and never re-rendered per frame; the jitter
  was pure UI-thread contention).
- **For a heavy or continuous animation, collapse N animated Views into one Skia
  draw.** N animated `View`s push N view-prop updates through Reanimated's mapper
  onto the UI thread *every frame*; a single `<Canvas>` + one path is one draw.
  That's *why* a native (Compose/Skia) equivalent stays smooth under load — it does
  almost no per-frame main-thread work, not because it sits on a faster thread.
  When an RN animation janks only *under contention* (a list mounting, text
  streaming beside it), this headroom is usually the fix — match the native
  approach rather than concluding "RN can't." (40 animated bars → one Skia path
  took a waveform from "jitters under load" to "buttery.")
- **No per-frame allocation in worklets.** A 60fps worklet that allocates — new
  arrays, per-item Skia `Rect`/`RRect` objects, a fresh transform array each frame
  — feeds the UI-runtime GC, which then pauses ~every couple of seconds: a subtle
  but *regular* single-frame skip. Build Skia paths with raw `moveTo`/`lineTo`
  (numbers, no objects), preallocate + mutate, and signal change with a
  counter/clock rather than a fresh object. (One ~2s skip was ~1000 short-lived
  Skia objects/sec from building bars as `RRect`s; raw path commands removed it.)
- **`useEffect`-started animations can silently fail to start on a cold/fast
  mount** (the animation runtime may not be ready when the screen mounts
  immediately on a cold launch). If an entrance animation must be reliable, don't
  hang it on first-frame timing.
- **Reanimated + Fast Refresh:** an animation-start effect keyed on a stable
  shared value won't re-run on Fast Refresh, so an already-running animation
  keeps going and your edit appears to do nothing. **Cold-reload to test
  animation changes.**
- **Reanimated lint:** eslint-config-expo flags `.value` writes as forbidden
  mutations — a false positive for worklets/props; suppress per-line, don't
  contort the code.

---

## Lists (large / virtualized)

Ordered by how broadly the advice holds:

**Broadly true, highest leverage:**
- **Preserve item identity.** Recreating objects for unchanged rows
  (`items.map(i => ({ ...i }))`) defeats memoization; reuse the same reference
  when a row's content hasn't changed. This is usually the single biggest win.
- **Minimize churn on updates** — a "show more" that re-keys or rebuilds every
  item can cost hundreds of ms on a large list, independent of list component.
- **`React.memo` on a row is necessary but not sufficient** — the list still
  walks and reconciles children before a row can bail; bail-out depends on
  stable props/refs, not just the memo boundary.

**Real but version/implementation-dependent (profile to confirm for your RN
version):**
- RN's `VirtualizedList` (under both `FlatList` and `SectionList`) exposes a
  `strictMode` prop that stabilizes cell-wrapper props so memoized cells can
  actually skip re-render. It measurably helped one large inbox; treat it as "try
  it and measure," not a guaranteed switch.
- `SectionList`'s nested shape (`sections:[{data:[…]}]`) tends to lose
  referential identity more easily on parent updates; flattening to a single
  `FlatList` (rendering headers as item types) removes that layer so unchanged
  cells can bail. A real refactor with a real payoff for big lists — and overkill
  for small ones.

**A tuning tradeoff, not a fixed recommendation:**
- `windowSize` / mounted window: larger = smoother scroll and fewer blank cells,
  at the cost of memory and per-update work; smaller = leaner and cheaper but
  more blank-during-fast-scroll. Tune to the device + list, don't cargo-cult a
  number. **Two caveats that bite:** (1) "larger is cheap" assumes *light* rows —
  if rows render markdown/rich content, a large window eagerly *mounts* most of the
  history on open (a multi-hundred-ms main-thread burst that can freeze a concurrent
  animation); `windowSize=31` does exactly this. (2) When a growing element (a
  compose box) reflows the list, that reflow's cost *scales with the mounted
  window* — so a smaller window can be what keeps the reflow cheap enough to stay
  smooth, removing the need for hacks like freezing the inset. ~5 screens is often a
  good landing: no blank-stop on normal scroll, no open-freeze, cheap reflow.

---

## Navigation & transitions

`@react-navigation/native-stack` (what expo-router's `<Stack>` uses) runs the slide
on the **UI thread**, so it can't stutter from JS — *but it only starts the
transition after React commits the destination screen.* A heavy mount (e.g. a
markdown list) therefore reads as **dead air between the tap and the slide**, not as
a janky slide. So a "slow transition" is almost always a slow *commit* — profile the
commit (see toolkit), don't reach for the animation config.

- **Keep the first commit cheap; defer below-the-fold by one frame.** Render the
  shell (header, composer, a placeholder) first and mount the heavy list on the next
  frame, so the slide starts immediately and the content fills in one follow-up
  commit. Use **`requestAnimationFrame`, not `InteractionManager.runAfterInteractions`**:
  the latter waits for the *whole* transition to finish (content lands ~300ms late);
  rAF lands it ~a frame in, during the slide.
- **…but render enough to cover the viewport in that commit.** Deferring + too small
  an `initialNumToRender` gives a two-step "a few rows, then the rest" flash. Size the
  initial batch to fill the screen so it paints in one shot.
- **Cache-first the destination.** A detail screen usually re-fetches what the list
  already holds. Seed the detail query from the list cache (React Query `initialData`
  keyed off the list query) and render what you already know (title, the opening
  content) immediately; let the fetch reconcile in the background. This decouples
  *perceived* nav speed from network latency — the screen is never blank on a
  round-trip.
- **Predictive back (Android):** set `android:enableOnBackInvokedCallback="false"` via a
  config plugin (`android/` is CNG). React Navigation does not yet fully support
  Android's predictive back gesture API — opting in (`true`) while the navigator can't
  drive the seekable transition leaves the system back gesture misbehaving. Flip to
  `true` the day react-native-screens + React Navigation ship predictive-back support.

---

## Profiling toolkit (Android, on-device)

- **Frame stats:** `adb shell dumpsys gfxinfo <pkg> reset`, drive the
  interaction, read back Janky %, 50/90/95/99th percentiles, and GPU vs. total.
  **Low GPU time + high frame time ⇒ CPU/UI-thread bound** (the recomposition/JS
  signature, not graphics).
- **Idle-render test:** `gfxinfo … reset`, leave the screen *untouched* ~10s,
  read *Total frames rendered*. **~0 = healthy** (render-on-demand); a steady
  ~60/s means a continuous loop is running — decide if it's earning it
  (principle #1).
- **Animation detector:** two screenshots ~400ms apart; byte-identical ⇒ nothing
  is animating (handy to tell "fixed" from "never started").
- **Isolate + verify:** one variable per run; confirm it took effect before
  believing the number.

**Diagnosing input/interaction latency (taps that feel delayed before *anything* happens):**
- **Instrument the boundaries, `__DEV__`-only.** A tap→first-commit timer (around
  `router.push` and a destination `useLayoutEffect`) localizes "dead air" to the JS
  commit; per-request timing in the API client localizes it to the network. These
  **vanish in a release build** (dev-gated) — expected, not a bug.
- **gfxinfo says it outright:** `High input latency` + `Slow UI thread` + a trivial GPU
  percentile (~3ms) ⇒ CPU/UI-thread bound. Input is waiting on a busy thread, not the GPU.
- **Measure the endpoint the UI actually waits on, not a health check.** A `/ping` is a
  no-op (and may 401); the latency that matters lives in the auth'd endpoints that gate
  a screen.
- **Beware measurement artifacts.** A wall-clock timer around `await` captures whatever
  blocked the JS thread before the continuation ran — an in-memory wait can read as
  ~130ms purely because the thread was mid-commit. The number is real; the
  *attribution* may not be.

---

## Debug-build overhead — real, but not a catch-all

Debug runs JS/CPU work **~2–5× slower** than release. It's a near-constant *multiplier*
on JavaScript + main-thread work, not a per-feature gremlin — which makes it tempting to
wave away every slow thing with "it's just debug." **Don't.** That reflex buries real bugs.
The multiplier explains only *some* symptoms, and there's a one-step test for which.

**Where it comes from** (≈ order of impact for render/commit work):
1. **Dev-mode React** — per-component prop/hook/key validation, dev fiber bookkeeping,
   richer errors, StrictMode double-invokes. Scales with component count, so big commits pay
   most. Production React strips it all.
2. **Unminified dev bundle + no Hermes AOT bytecode** — slower to parse *and* execute;
   release ships optimized, precompiled `.hbc`.
3. **`__DEV__` instrumentation** — RN invariants, LogBox, and every `console.*` shipped
   *synchronously* to Metro (per-event logs are surprisingly costly).
4. **Debuggable native build, no R8** — Kotlin/Java modules + the bridge/JSI run
   un-shrunk/un-optimized; ART stays debuggable. Hits native-module calls + cold start.
5. **New Arch + React Compiler dev checks** — extra Fabric/compiler validation per render.
   Plus the attached dev tooling (HMR, inspector).

**Most noticeable** (debug is a reasonable first suspect):
- **Large React commits** — entering a component-heavy screen, a big list's initial render,
  mounting markdown/rich content. (One chat-enter commit: ~165ms release → ~460ms debug.)
- **Cold start / first screen** — unoptimized bundle parse + no AOT + un-optimized native.
- **Frequent re-renders** — streaming/SSE updates, rapid `setState`; dev React re-validates
  every pass.
- **Chatty logging** — anything that `console.*`s per event/frame.

**NOT the answer** (do not blame debug):
- **Native-thread animations** — Reanimated worklets + native-stack transitions run on the
  UI thread; their *speed* is ~identical debug vs release.
- **Network latency** — the wire doesn't care which build you're on.
- **GPU-bound work** — debug doesn't slow the GPU.
- **Library gaps / logic bugs** — identical in both builds.

**The discriminator — the debug→release ratio.** Measure the *same* interaction in release:
- **~2–5× faster in release** ⇒ it was debug overhead. Move on; don't "optimize" it.
- **~unchanged (ratio ≈ 1×)** ⇒ NOT debug. It's real — library, network, GPU, logic, or
  genuinely expensive code — and needs a real fix.

The rule: **never conclude "it's just debug" without a release measurement showing the
multiplier. No ratio, no verdict.**

---

## Dev-loop debugging (not app perf, but it will save you hours)

- **If on-device behavior stops tracking your edits, suspect a stale bundle**
  before you trust any measurement. Causes: a JS syntax error (Metro keeps serving
  the last good bundle and returns a `TransformError` for the entry), a crashed
  transform watcher (e.g. NativeWind/Tailwind), or a flaky device↔Metro connection
  (wrong host/port, not on the same network, or an `adb reverse` that dropped).
  **Verify the served bundle directly:** `curl` the entry bundle and `grep` for a
  unique marker you just added (a small JSON `TransformError` body means it isn't
  compiling). This single check saves long detours where "measurements" were of a
  frozen bundle.
- **Cold-reload, not Fast Refresh, to test animation changes** (see above).

---

## Case studies

> The concrete ground truth. Keep each to: symptom → root cause → fix → result.

### Floating prompt-box keyboard jank
- **Symptom:** opening the keyboard with a floating box visible was consistently
  laggy.
- **Root cause:** the box read the *animating* `WindowInsets.ime` inset inside
  `BoxWithConstraints` composition, so every frame of the ~300ms slide recomputed
  layout and re-fired two coroutine effects (a `snapTo` + a spring), ~18
  cancel/relaunch cycles per open.
- **Fix:** this box only needs to *clear* the keyboard (not ride it), so it now
  reads the *settled* `WindowInsets.imeAnimationTarget`, keeps resting `bounds`
  independent of the inset, and lifts/restores with one spring (remembering the
  pre-keyboard position so dismiss returns it home). For content that needed to
  *track* the keyboard you'd have driven a per-frame UI-thread translation instead.
- **Result:** 95th-pct frame time 73ms → 44ms, jank 12.9% → 7.3% (debug build);
  visibly smooth.

### "Shake to open" hint heat
- **Symptom:** the device warmed up just sitting on a tutorial page.
- **Root cause:** a hint glyph ran an infinite opacity pulse → a permanent 60fps
  render loop the whole time the page was open. (Isolation: overlay attached but
  pulse not running = **0** idle frames; ~587 frames/10s only with the pulse
  animating.) The continuous cost wasn't earning much here.
- **Fix:** make the glyph static (the label already conveys the affordance). A
  finite or gated pulse would also have worked; the call was that the effect
  wasn't worth any continuous cost on a screen you sit and read.
- **Result:** idle render → 0 frames.

### Inbox scroll / "show more" stall
- **Symptom:** ~335ms stall on "show more"; janky inbox scroll.
- **Root cause:** large `SectionList` updates revisited mounted cells during
  reconciliation even though rows were memoized.
- **Fix:** flat `FlatList` + `VirtualizedList` `strictMode` + stable item refs
  (reuse wrappers when content is unchanged) so unchanged cells bail; kept a
  large mounted window to avoid blank-scroll (a deliberate memory/smoothness
  tradeoff).
- **Result:** stall eliminated.

### "The whole app feels laggy" — a debug build + commit-gated nav
- **Symptom:** every interaction (open a screen, expand a card, back to the list)
  felt like several hundred ms of dead air "before anything happens." Suspected, in
  turn: network, a global JS-thread hog, a stray Skia import, the drawer animation.
- **Ruled out by measurement (don't re-chase these):** `refetchOnWindowFocus` is
  **inert in RN** with no AppState `focusManager` wiring, so it never fired; a Skia
  dep with **no import** anywhere can't run; a `/ping` was a no-op 401
  (unrepresentative); and **exit fired zero network yet was still delayed** — which
  proved the exit lag was JS, not the wire.
- **Root cause:** two things. (1) The screen blocked render on its data fetch and
  committed a heavy markdown list synchronously, and native-stack only starts the
  slide *after* that commit → ~300ms of dead air before the animation. (2) It was
  all being judged on a **debug build**, which inflated the commit 2–5×.
- **Fix:** cache-first seed the screen from the list (`initialData`) + show the known
  content instantly; defer the heavy list by one rAF frame (cheap first commit →
  slide starts immediately), sized to fill the viewport; then **measure release**.
- **Result:** debug tap→commit ~300ms → ~165ms after the defer; **release** gfxinfo
  99th-pct frame 105ms → 36ms, missed-vsync 11 → 0. The "RN is too slow, go native"
  hypothesis was a debug-build artifact. **Lesson: confirm on a release build before
  drawing any architectural conclusion.**

### Dictation waveform jitter
- **Symptom:** an RN mic waveform (Reanimated) stuttered for the first seconds of
  recording and while a transcript streamed in; a native Compose waveform —
  *same* audio engine — was perfectly smooth. (Debug + release both, so not a
  debug artifact.)
- **What it was NOT (ruled out by measurement — don't re-chase):** React
  re-renders. Render-count probes (`__DEV__` `console.log` at the top of each
  component) proved the waveform was memoized and **never re-rendered per frame**,
  and the list didn't re-render during dictation. State-isolating the streaming
  transcript into its own leaf changed the jitter **not at all**. That's the tell:
  a Reanimated animation's smoothness is *orthogonal* to re-renders — it animates
  on the UI thread off shared values.
- **Root cause:** UI-thread (= main-thread) contention, two sources. (1) The
  waveform pushed ~40 animated-`View` updates through Reanimated's mapper every
  frame — enough that a concurrent `TextInput` re-shape (streaming transcript) or
  a list mount tipped the 16ms budget. (2) Opening a screen eagerly mounted ~the
  whole markdown history (`windowSize=31` × non-cheap rows). Reanimated can't
  escape either — the UI thread *is* the main thread.
- **Fix:** draw the waveform as **one Skia path in one `<Canvas>`** (scroll = a
  cheap `<Group>` transform), so ~40 view updates/frame become a single draw and
  it has headroom to ride through main-thread bursts the way the native box does.
  Then a subtle, *regular* ~2s single-frame skip remained — periodic UI-runtime GC
  from building each bar as `Skia.RRectXY(...)` (~1000 throwaway objects/sec);
  rebuilding the path with raw `moveTo`/`lineTo` (zero per-bar allocation) removed
  it. Finally tuned `windowSize` to ~5 screens so screen-open doesn't mount the
  whole history.
- **Result:** "buttery smooth," waveform + anchored bottom + comfortable scroll
  together.
- **Lessons:** (1) Reanimated escapes a busy *JS* thread, not a busy *main* thread.
  (2) Animation smoothness ≠ re-render count — measure the UI thread, don't chase
  memo/isolation for jank. (3) Collapse many animated Views into one Skia draw to
  match native smoothness under contention. (4) Treat allocation in a 60fps worklet
  as the enemy — it surfaces as periodic GC frame-skips. (5) **Change one variable
  at a time** — reverting two things together invites a wrong conclusion about which
  was load-bearing.
