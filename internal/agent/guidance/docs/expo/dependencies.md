# Dependencies — install the Expo way

**Always add packages with `npx expo install <package>` — never `npm install`,
`yarn add`, `pnpm add`, or `npx install`.** Expo pins each package to the version
compatible with the project's Expo SDK. Installing with a raw package manager
pulls the latest version, which routinely mismatches the native runtime and
**breaks the preview build** (red-screen native errors, incompatible native
modules). `expo install` resolves the SDK-compatible version instead.

- **Fix an already-mismatched tree:** `npx expo install --fix` realigns every
  dependency to the current SDK.
- **Check compatibility before adding a library.** Prefer packages with Expo
  support; many community native modules need a config plugin or a dev client to
  work in the preview.
- **Native configuration goes through config plugins, not hand-edits.** Set
  native options in `app.json` / `app.config.*` (or a local config plugin). Don't
  hand-edit `ios/` or `android/` — Continuous Native Generation (prebuild)
  regenerates them and your edits are lost.
- **Never manually bump `react`, `react-native`, or `expo`** to chase a feature.
  Their versions are locked together by the SDK; move the SDK as a unit instead.
- Pure-JS libraries with no native code can technically use any package manager,
  but when in doubt use `expo install` — it installs non-native packages normally
  and gets the native ones right.
