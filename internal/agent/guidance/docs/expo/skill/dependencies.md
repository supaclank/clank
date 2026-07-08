# Dependencies — versions, native modules, and the preview

## Install the Expo way

**Always add packages with `npx expo install <package>` — never `npm
install`, `yarn add`, `pnpm add`, or `npx install`.** Expo pins each package
to the version compatible with the project's Expo SDK. Installing with a raw
package manager pulls the latest version, which routinely mismatches the
native runtime and breaks the preview build (red-screen native errors,
incompatible native modules). `expo install` resolves the SDK-compatible
version instead.

- **Fix an already-mismatched tree:** `npx expo install --fix` realigns every
  dependency to the current SDK.
- **Never manually bump `react`, `react-native`, or `expo`** to chase a
  feature. Their versions are locked together by the SDK; move the SDK as a
  unit instead.
- Pure-JS libraries with no native code can technically use any package
  manager, but when in doubt use `expo install` — it installs non-native
  packages normally and gets the native ones right.

## Native modules and the preview — a decision, not a rule

The user previews through an app that works like Expo Go: **JS changes
hot-reload; native changes don't load over the wire.** A new native module,
or native configuration (`app.json` config plugins, anything under
`ios/`/`android/`), only takes effect after a development build — which the
user must trigger and download. Many common native modules are already
bundled in the preview app and work immediately.

How to reason about it:

- **Default to what works in the preview.** For most needs there is a JS-only
  or Expo-Go-compatible community library — the RN ecosystem has been solving
  problems under this constraint for years. Greenfield apps especially should
  err toward not requiring new native modules.
- **But don't treat native modules as forbidden.** Sometimes a native module
  is the right or only tool (a capability gap, a real performance need). Use
  your judgment — the user may not be technical, and a stream of dependency
  questions is its own UX bug. When you do adopt one, tell them in plain
  language what it buys and that it won't show up in the preview until they
  build a development build; if they've given guidance about native modules,
  follow it. And check what the workflow supports first: some preview
  environments don't offer development builds at all, and there a new native
  module can't run, period.
- **Native config edits share the same constraint.** Configure native
  behavior through `app.json` / config plugins — never hand-edit `ios/` or
  `android/`, which Continuous Native Generation regenerates — and remember
  the change is inert in the preview until a development build happens.
