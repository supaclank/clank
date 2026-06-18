/**
 * clank-preview-runtime — injected into EVERY guest preview project by the
 * clank backend (see inject.go). Embedded into the host binary and written to
 * the guest project root before `expo start` runs.
 *
 * This is the PRIMARY, user-facing half of "no scary error popups in the
 * preview". It runs as a Metro premodule (before the app's entry, on every
 * bundle — inject.go wires metro.config.js to do this), so it's installed
 * before any app code can throw.
 *
 * What it does, in a guest React Native runtime (no-op on web):
 *   1. LogBox.ignoreAllLogs(true) — kills the warning/error TOASTS.
 *   2. ErrorUtils.setGlobalHandler — swallows uncaught FATALS *before* they
 *      reach LogBox's fullscreen error inspector (ignoreAllLogs alone does
 *      NOT cover that), keeps the JS runtime alive so Fast Refresh can
 *      hot-replace the bad module, and forwards a friendly summary to native.
 *   3. global.__clankPreview — a bridge-ready namespace for a future
 *      "visual edits" tool.
 *
 * The clank-mobile native host suppresses the rest (the native redbox, the
 * "Loading from Metro…" banner) and renders a calm "Fixing a glitch…" pill in
 * its floating overlay, driven by the reportPreviewError call below.
 *
 * NOTE: keep this in sync with clank-mobile docs/preview-runtime/clank-preview-runtime.js
 * (the reference copy, coupled to the PreviewLauncher native module contract).
 *
 * Fast-Refresh safety: the install-once guard lives on `global`, which
 * survives HMR — so re-evaluating this module never double-wraps the handler.
 */
(function installClankPreviewRuntime() {
  if (typeof globalThis === 'undefined') return;
  var g = globalThis;

  g.__clankPreview = g.__clankPreview || { version: 1, installed: false };
  if (g.__clankPreview.installed) return; // HMR-safe: install exactly once.
  g.__clankPreview.installed = true;

  var isReactNative =
    typeof navigator !== 'undefined' && navigator.product === 'ReactNative';
  if (!isReactNative) return; // web: namespace only, no-op.

  // Resolve the PreviewLauncher native module lazily. It's an Expo module, so
  // it lives in expo-modules-core's registry (requireNativeModule), NOT on
  // ReactNative.NativeModules — try that first, then fall back to the classic
  // bridge. Absent in a bare RN runtime → this is a no-op.
  function previewLauncher() {
    try {
      var core = require('expo-modules-core');
      if (core && typeof core.requireNativeModule === 'function') {
        try {
          return core.requireNativeModule('PreviewLauncher');
        } catch (e) {
          /* fall through */
        }
      }
      if (core && core.NativeModulesProxy && core.NativeModulesProxy.PreviewLauncher) {
        return core.NativeModulesProxy.PreviewLauncher;
      }
    } catch (e) {
      /* expo-modules-core not present — fall through */
    }
    try {
      var nm = require('react-native').NativeModules;
      if (nm && nm.PreviewLauncher) return nm.PreviewLauncher;
    } catch (e) {
      /* no-op */
    }
    return null;
  }

  function reportError(summary, fatal) {
    var pl = previewLauncher();
    if (pl && pl.reportPreviewError) {
      try {
        pl.reportPreviewError(String(summary == null ? 'error' : summary), !!fatal);
      } catch (e) {
        /* never let telemetry throw */
      }
    }
  }

  // 1) Kill LogBox warning/error toasts. (Notifications only — the fullscreen
  //    inspector is handled by the global handler below; see RN LogBox.js.)
  try {
    var LogBox =
      require('react-native/Libraries/LogBox/LogBox').default ||
      require('react-native').LogBox;
    if (LogBox && LogBox.ignoreAllLogs) LogBox.ignoreAllLogs(true);
  } catch (e) {
    /* LogBox absent (e.g. production) — nothing to silence */
  }

  // 2) Swallow uncaught fatals before they open the fullscreen LogBox.
  var EU = g.ErrorUtils;
  if (EU && typeof EU.setGlobalHandler === 'function') {
    EU.setGlobalHandler(function clankGlobalErrorHandler(error, isFatal) {
      var msg = (error && (error.message || String(error))) || 'unknown error';
      reportError(msg, isFatal);
      // Deliberately do NOT call the previous handler: it re-enters LogBox
      // (the very overlay we're suppressing) and can crash the app. We keep
      // the runtime alive so the next Fast Refresh update can replace the bad
      // module; the native overlay shows the calm "Fixing a glitch…" pill.
    });
  }

  // 3) Bridge-ready hooks for a future "visual edits" tool.
  g.__clankPreview.reportError = function (m) {
    reportError(m, false);
  };
  g.__clankPreview.clearError = function () {
    var pl = previewLauncher();
    if (pl && pl.clearPreviewError) {
      try {
        pl.clearPreviewError();
      } catch (e) {
        /* no-op */
      }
    }
  };
})();
