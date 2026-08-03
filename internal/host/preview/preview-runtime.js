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
  var g = typeof globalThis !== 'undefined' ? globalThis : (typeof global !== 'undefined' ? global : null);
  if (!g) return;

  g.__clankPreview = g.__clankPreview || { version: 1, installed: false };
  if (g.__clankPreview.installed) return; // HMR-safe: install exactly once.
  g.__clankPreview.installed = true;

  // Smoke-test beacon: proves the shim actually injected + ran this module in
  // the guest bundle (independent of the RN/native checks below). If this line
  // shows in logcat (ReactNativeJS), the whole NODE_OPTIONS→Metro injection
  // works. Cheap to keep; remove once the mechanism is trusted.
  try {
    console.log(
      '[clank-preview] runtime evaluated; require=' +
        (typeof require === 'function') +
        ' navigator=' +
        (typeof navigator !== 'undefined' && navigator.product),
    );
  } catch (e) {}

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

  function clearError() {
    var pl = previewLauncher();
    if (pl && pl.clearPreviewError) {
      try {
        pl.clearPreviewError();
      } catch (e) {
        /* no-op */
      }
    }
  }

  // --- Rich error formatting for the agent ---------------------------------
  // A LogBoxLog carries far more than the message: the code frame (syntax
  // errors), the React component stack (file:line of each component), and the
  // JS stack. We assemble those into one report so "Fix it" gives the agent the
  // same context a developer sees in the Expo error screen. Locations are the
  // un-symbolicated bundle positions (symbolication is async), but the file +
  // message + approximate line are enough to pinpoint it. Capped to 4000 chars.
  // NOTE on ANSI: Babel highlights the code frame with ANSI color codes
  // (ESC[..m). We deliberately KEEP them in the reported text — the
  // clank-mobile host parses them to render a syntax-highlighted code frame
  // (and strips them for the agent message). So do NOT strip here.

  function clankCleanFile(fileName) {
    if (!fileName) return '';
    return String(fileName)
      .replace(/^https?:\/\/[^/]+\//, '') // strip protocol + host
      .replace(/[?#].*$/, '') // strip query/hash
      .replace(/\.bundle.*$/, '') // strip .bundle + trailing //…
      .replace(/\/+$/, '');
  }

  // Render one component-stack frame, or null if it's pure noise (no name AND
  // no file — some RN/React builds emit a stack of nameless, file-less frames
  // that add nothing over the code frame).
  function clankFrame(f) {
    if (!f) return null;
    var name = f.content || '';
    var file = clankCleanFile(f.fileName);
    if ((!name || name === '<anonymous>') && !file) return null;
    var line = '  ' + (name || '<anonymous>');
    if (file) {
      line += ' (' + file;
      if (f.location && f.location.row != null) {
        line += ':' + f.location.row;
        if (f.location.column != null) line += ':' + f.location.column;
      }
      line += ')';
    }
    return line;
  }

  function formatLogBoxError(log) {
    var parts = [];
    parts.push(
      String(log.message && log.message.content != null ? log.message.content : 'error'),
    );
    if (log.codeFrame && log.codeFrame.content) {
      var head = clankCleanFile(log.codeFrame.fileName);
      if (head && log.codeFrame.location && log.codeFrame.location.row != null) {
        head += ':' + log.codeFrame.location.row;
      }
      parts.push('\n' + (head ? head + '\n' : '') + String(log.codeFrame.content));
    }
    // Component stack (React errors), keeping only frames with a name or file.
    var cframes = [];
    var cs = log.componentStack;
    if (cs && cs.length) {
      for (var i = 0; i < cs.length && cframes.length < 16; i++) {
        var fr = clankFrame(cs[i]);
        if (fr) cframes.push(fr);
      }
    }
    if (cframes.length) {
      parts.push('\nComponent stack:\n' + cframes.join('\n'));
    } else {
      // No useful component stack — fall back to the JS stack.
      var st = log.stack;
      if (st && st.length) {
        var slines = [];
        for (var j = 0; j < st.length && slines.length < 16; j++) {
          var sf = st[j];
          var sfile = clankCleanFile(sf.file);
          slines.push(
            '  ' +
              (sf.methodName || '<fn>') +
              (sfile ? ' (' + sfile + (sf.lineNumber != null ? ':' + sf.lineNumber : '') + ')' : ''),
          );
        }
        if (slines.length) parts.push('\nStack:\n' + slines.join('\n'));
      }
    }
    // Generous safety bound only — guards against a pathological multi-MB
    // minified stack flooding the agent; real errors are a few KB after the
    // noise filtering above.
    return parts.join('\n').slice(0, 16000);
  }

  // Detect whether we're running INSIDE the clank-mobile host (the
  // PreviewLauncher native module is present). A normal Expo Go client — or
  // someone running plain `expo start` without our wrapper — won't have it.
  var clankHost = previewLauncher();
  console.log('[clank-preview] host=' + (clankHost ? 'clank-mobile' : 'other'));

  // SUPPRESS the dev error UI ONLY inside the clank-mobile host, so a normal
  // Expo Go client (or plain `expo start`) keeps stock error behavior. The
  // DETECTION/reporting below (LogBoxData.observe) runs regardless — it drives
  // the pill and no-ops without the native module, so the pill never depends on
  // whether the module was resolvable this early.
  if (clankHost) {
    // Kill LogBox warning/error toasts. (Notifications only — the fullscreen
    // inspector is handled by the global handler below; see RN LogBox.js.)
    try {
      var LogBox =
        require('react-native/Libraries/LogBox/LogBox').default ||
        require('react-native').LogBox;
      if (LogBox && LogBox.ignoreAllLogs) LogBox.ignoreAllLogs(true);
    } catch (e) {
      /* LogBox absent (e.g. production) — nothing to silence */
    }

    // Swallow uncaught fatals before they open the fullscreen LogBox.
    var EU = g.ErrorUtils;
    if (EU && typeof EU.setGlobalHandler === 'function') {
      EU.setGlobalHandler(function clankGlobalErrorHandler(error, isFatal) {
        var msg = (error && (error.message || String(error))) || 'unknown error';
        // TODO(ai-review): sanitize/truncate msg before sending to native (raw messages can include module paths).
        // https://github.com/supaclank/clank/pull/65#discussion_r3439529642
        reportError(msg, isFatal);
        if (error) console.log('[clank preview]', error.stack || error.message || error);
        // Deliberately do NOT call the previous handler: it re-enters LogBox
        // (the very overlay we're suppressing) and can crash the app. We keep
        // the runtime alive so the next Fast Refresh update can replace the bad
        // module; the native overlay shows the calm "Fixing a glitch…" pill.
      });
    }
  }

  // 3) Catch EVERY error LogBox sees — fullscreen, syntax, AND the soft
  //    toast/Fast-Refresh errors the native error surface can't see (those
  //    keep the app alive and never present a surface). LogBoxData.observe
  //    hands us the full log set on each change, so we get both the error (with
  //    its message, for the pill's "Fix it" button) and a clean CLEAR when the
  //    logs empty (a successful Fast Refresh). This is what makes the pill fire
  //    for agent-introduced HMR errors. Dedup so we only report on change.
  try {
    var LogBoxData = require('react-native/Libraries/LogBox/Data/LogBoxData');
    if (LogBoxData && typeof LogBoxData.observe === 'function') {
      var lastReport = null; // null = healthy; string = last reported message
      LogBoxData.observe(function (state) {
        try {
          var logs = state && state.logs;
          var latest = null;
          if (logs && typeof logs.forEach === 'function') {
            logs.forEach(function (log) {
              var lvl = log && log.level;
              if (lvl === 'error' || lvl === 'fatal' || lvl === 'syntax') {
                latest = log; // keep the most recent error-level log
              }
            });
          }
          if (latest) {
            // Rich report: message + code frame + component stack + JS stack,
            // so "Fix it" hands the agent the same context the Expo error
            // screen shows — not just the one-line title.
            var m = formatLogBoxError(latest);
            if (m !== lastReport) {
              lastReport = m;
              // Diagnostic: confirms the LogBox subscription fired and what it
              // extracted (logcat ReactNativeJS). Remove once trusted.
              try {
                console.log('[clank-preview] report ' + latest.level + ': ' + m.slice(0, 140));
              } catch (e) {}
              reportError(m, latest.level !== 'error');
            }
          } else if (lastReport !== null) {
            lastReport = null;
            try {
              console.log('[clank-preview] clear');
            } catch (e) {}
            clearError();
          }
        } catch (e) {
          /* never let the observer throw */
        }
      });

      // RN clears ALL LogBox logs on every Fast Refresh update (HMRClient →
      // LogBox.clearAllLogs → LogBoxData.clear). But clear() does NOT notify
      // logs-observers (unlike clearWarnings/handleUpdate), so the observe above
      // never sees the empty set on recovery — the pill would stay forever even
      // though HMR fixed the error. Wrap clear() so a hot refresh clears our
      // error too; if the guest is still broken, the next render re-adds the log
      // and observe re-reports. This mirrors RN's own clear-then-re-add.
      try {
        if (typeof LogBoxData.clear === 'function' && !LogBoxData.__clankClearWrapped) {
          var origClear = LogBoxData.clear;
          LogBoxData.clear = function () {
            var r = origClear.apply(this, arguments);
            try {
              if (lastReport !== null) {
                lastReport = null;
                console.log('[clank-preview] clear (hot refresh)');
                clearError();
              }
            } catch (e) {}
            return r;
          };
          LogBoxData.__clankClearWrapped = true;
        }
      } catch (e) {}
    }
  } catch (e) {
    /* LogBoxData unavailable — fall back to the native surface + ErrorUtils */
  }

  // 4) Bridge-ready hooks for a future "visual edits" tool.
  g.__clankPreview.reportError = function (m) {
    reportError(m, false);
  };
  g.__clankPreview.clearError = clearError;
})();
