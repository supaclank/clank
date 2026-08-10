/**
 * clank-preview-runtime — injected into EVERY guest preview project by the
 * clank backend. The backend embeds this file into the host binary and writes
 * it next to the Metro shim before `expo start` runs; the shim appends
 * `require(<this file>)` to react-native's InitializeCore during the Babel
 * transform, so it runs right after InitializeCore on every bundle — before
 * any app code can throw.
 *
 * SYNC MANDATE — this file exists in two repos and MUST stay byte-identical:
 *   - supaclank/clank         internal/host/preview/preview-runtime.js (DEPLOYED)
 *   - supaclank/clank-mobile  docs/preview-runtime/clank-preview-runtime.js
 *     (reference copy, coupled to the PreviewLauncher native-module contract,
 *     covered by clank-preview-runtime.test.js)
 * The 2026-08 preview-bricking bug was exactly this drift: the boot-race
 * hardening (clank-mobile PR #98) landed in the reference copy but never
 * shipped in the backend. Diff against the other repo before changing either.
 *
 * This is the PRIMARY, user-facing half of "no scary error popups in the
 * preview". What it does, in a guest React Native runtime (no-op on web):
 *   1. LogBox.ignoreAllLogs(true) — kills the warning/error TOASTS.
 *   2. ErrorUtils.setGlobalHandler — swallows uncaught FATALS *before* they
 *      reach LogBox's fullscreen error inspector (ignoreAllLogs alone does
 *      NOT cover that), keeps the JS runtime alive so Fast Refresh can
 *      hot-replace the bad module, and forwards a friendly summary to native.
 *      Installed unconditionally; the clank/not-clank decision happens at
 *      error time (see the boot-race notes inline). Non-clank hosts get the
 *      previous (stock) handler.
 *   3. global.__clankPreview — a bridge-ready namespace for a future
 *      "visual edits" tool.
 *
 * Boot-race hardening: `globalThis.expo` can be MISSING while the bundle
 * evaluates — expo-modules-core's JSI install can lose the race against the
 * initial bundle evaluation (both are JS-thread tasks; whichever was enqueued
 * first wins), and it silently no-ops outright if the guest ReactContext is
 * torn down mid-boot (e.g. the preview window was closed while bundling).
 * This file must then (a) never import expo-modules-core — its EventEmitter
 * module reads `globalThis.expo.EventEmitter` at import time, so importing it
 * both throws AND permanently poisons Metro's module cache (every later
 * import, including the app's own, replays the same error) — and (b) still
 * swallow the resulting app-side fatal, which would otherwise take RN
 * bridgeless's "[runtime not ready]" path and either brick the preview or
 * SIGABRT the entire host process. Reports raised before the native module is
 * reachable are queued and flushed on a bounded retry timer.
 *
 * Boot-race SELF-HEAL: when a fatal was swallowed while `globalThis.expo` was
 * missing and expo then comes up moments later (the JSI install landed right
 * after the failed evaluation — the common "opened a preview and it
 * white-screened" case), the app's entry has already failed and nothing will
 * re-evaluate it. Nothing is wrong with the guest's code, so the "Fix it"
 * pill could only send the agent in circles. Instead: retract the queued
 * boot-race report and reload the guest JS ONCE via RN-core DevSettings (no
 * expo dependency). The fresh evaluation starts with expo installed, so the
 * app just boots. One-shot per evaluation, gated on expo actually having come
 * up (if the install never lands, e.g. torn-down context, we never reload —
 * reports stay queued and the native overlay's restart affordances take
 * over), and cancelled by a successful render (clearError).
 *
 * The clank-mobile native host suppresses the rest (the native redbox, the
 * "Loading from Metro…" banner) and renders a calm "Fixing a glitch…" pill in
 * its floating overlay, driven by the reportPreviewError call below.
 *
 * Why no imports of app code: this must be dependency-free and side-effect
 * isolated so it's safe to inject into an arbitrary project's bundle.
 *
 * Fast-Refresh safety: the install-once guard lives on `global`, which
 * survives HMR — so re-evaluating this module never double-wraps the handler.
 */
(function installClankPreviewRuntime() {
  var g =
    typeof globalThis !== 'undefined'
      ? globalThis
      : typeof global !== 'undefined'
        ? global
        : null;
  if (!g) return;

  // Set up the namespace first so even the web/no-op path exposes it.
  g.__clankPreview = g.__clankPreview || { version: 2, installed: false };
  if (g.__clankPreview.installed) return; // HMR-safe: install exactly once.
  g.__clankPreview.installed = true;

  // Smoke-test beacon: proves the shim actually injected + ran this module in
  // the guest bundle (independent of the RN/native checks below). If this line
  // shows in logcat (ReactNativeJS), the whole NODE_OPTIONS→Metro injection
  // works — and `expo=missing` is the boot-race tell. Cheap to keep.
  try {
    console.log(
      '[clank-preview] runtime v2 evaluated; require=' +
        (typeof require === 'function') +
        ' navigator=' +
        (typeof navigator !== 'undefined' && navigator.product) +
        ' expo=' +
        (g.expo ? 'installed' : 'missing'),
    );
  } catch (e) {}

  // React Native sets navigator.product === 'ReactNative'. On web there's no
  // native bridge and no LogBox redbox to suppress here, so bail after the
  // namespace is in place.
  var isReactNative =
    typeof navigator !== 'undefined' && navigator.product === 'ReactNative';
  if (!isReactNative) return;

  // BOOT-RACE GUARD: is expo's native side actually installed in THIS runtime?
  // See the header — the JSI install (global.expo) can land after the bundle
  // has already evaluated, or never (torn-down ReactContext: expo-modules-core's
  // MainRuntime.install() bails without logging when the context or its JS
  // context holder is already gone — the tell is "✅ Constants were exported"
  // with no "✅ JSI interop was installed").
  function expoReady() {
    return !!(g.expo && g.expo.EventEmitter);
  }

  // Drop THIS module's own "Deep imports from the 'react-native' package are
  // deprecated" warnings before they reach the guest's LogBox as a toast
  // badge. Mechanism matters: babel-preset-expo's warn-on-deep-rn-imports
  // plugin (vendored from RN 0.80+) does NOT warn at require time — it
  // statically collects deep imports and APPENDS `console.warn("Deep imports
  // … deprecated ('<dep>'). Source: <this file> <line>:<col>")` statements to
  // the END of the transformed module body. They fire when this module
  // finishes evaluating, so no swap scoped around the require sites can catch
  // them (a previous attempt did exactly that and the badge survived).
  //
  // Instead: a permanent pass-through console.warn filter that drops ONLY
  // deprecation lines whose baked-in `Source:` is this very file. The app's
  // own deep-import warnings carry their own filename and pass through
  // untouched, as does every other warn. Ordering works in our favor:
  // LogBox's console patch installed during InitializeCore — BEFORE this
  // premodule ran — so our filter wraps it (we're outer), and dropped lines
  // never reach LogBox's badge in any client. Install-once via a marker on
  // the function, so a premodule re-evaluation (whose appended warns fire
  // even under the IIFE's install-once guard) still hits an active filter
  // without double-wrapping.
  try {
    if (!(console.warn && console.warn.__clankDeepImportFiltered)) {
      var prevWarn = console.warn;
      var filteredWarn = function () {
        try {
          var m = arguments[0];
          if (
            typeof m === 'string' &&
            m.indexOf("Deep imports from the 'react-native' package are deprecated") === 0 &&
            m.indexOf('clank-preview-runtime') !== -1
          ) {
            return; // self-inflicted premodule noise — not the app's warning
          }
        } catch (e) {}
        return prevWarn.apply(console, arguments);
      };
      filteredWarn.__clankDeepImportFiltered = true;
      console.warn = filteredWarn;
    }
  } catch (e) {
    /* frozen console — the two deprecation lines surface, nothing worse */
  }

  // Resolve the PreviewLauncher native module lazily. It's an Expo module, so
  // it lives in expo-modules-core's registry (requireNativeModule), NOT on
  // ReactNative.NativeModules — try that first, then fall back to the classic
  // bridge. Absent in a bare RN runtime → this is a no-op.
  //
  // NEVER touch expo-modules-core before expoReady(): its EventEmitter module
  // reads `globalThis.expo.EventEmitter` at import time, so importing it
  // during the boot race above both throws AND permanently poisons Metro's
  // module cache — every later import (including the app's own) replays the
  // same error. Skipping the import here keeps the module loadable for
  // whoever imports it once expo is up, and later calls of ours retry.
  function previewLauncher() {
    if (expoReady()) {
      try {
        var core = require('expo-modules-core');
        if (core && typeof core.requireNativeModule === 'function') {
          // requireNativeModule throws if the module isn't registered.
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
    }
    try {
      var nm = require('react-native').NativeModules;
      if (nm && nm.PreviewLauncher) return nm.PreviewLauncher;
    } catch (e) {
      /* no-op */
    }
    return null;
  }

  // Reports raised before PreviewLauncher is resolvable (the boot race) are
  // queued and re-tried on a short timer, so the "Fixing a glitch…" pill still
  // fires once expo comes up instead of the report being dropped. Bounded on
  // both axes: a dead runtime must not leak a queue or tick forever.
  var pendingReports = [];
  var flushTimer = null;
  var flushAttempts = 0;

  function scheduleFlush() {
    if (flushTimer || flushAttempts >= 20 || typeof setTimeout !== 'function') return;
    if (!pendingReports.length) return; // nothing to retry — don't burn a background timer
    flushTimer = setTimeout(function () {
      flushTimer = null;
      // A pending self-heal owns these reports: they describe the very
      // condition the reload fixes, so don't surface them (a ghost pill)
      // while the heal can still land. If the heal gives up, bootHealDone
      // flips and delivery resumes here. Don't spend retry budget on these
      // gated cycles — they never attempt delivery, so counting them can
      // exhaust the budget before the heal even finishes, permanently
      // stranding a report that becomes deliverable the moment it's done.
      if (bootHealPending()) {
        scheduleFlush();
        return;
      }
      flushAttempts++;
      var pl = previewLauncher();
      if (!pl || !pl.reportPreviewError) {
        scheduleFlush();
        return;
      }
      // Host resolved late → the eager detection at install time was a false
      // negative; apply the clank-only suppression it skipped.
      onHostResolvedLate();
      while (pendingReports.length) {
        var r = pendingReports.shift();
        try {
          pl.reportPreviewError(r.m, r.f);
        } catch (e) {
          // Bridge call failed even though the module resolved — requeue and
          // retry later instead of dropping (queue-never-silenced guarantee).
          pendingReports.unshift(r);
          break;
        }
      }
      scheduleFlush();
    }, 500);
  }

  function reportError(summary, fatal) {
    var msg = String(summary == null ? 'error' : summary);
    var pl = previewLauncher();
    if (!pl || !pl.reportPreviewError || bootHealPending()) {
      if (pendingReports.length < 20) pendingReports.push({ m: msg, f: !!fatal });
      scheduleFlush();
      return;
    }
    try {
      pl.reportPreviewError(msg, !!fatal);
    } catch (e) {
      // Same requeue-never-drop guarantee as the queued path above.
      if (pendingReports.length < 20) pendingReports.push({ m: msg, f: !!fatal });
      scheduleFlush();
    }
  }

  function clearError() {
    pendingReports.length = 0; // healthy again — stale queued errors are noise
    if (flushTimer && typeof clearTimeout === 'function') {
      clearTimeout(flushTimer);
      flushTimer = null;
    }
    flushAttempts = 0; // give the next unrelated error its own full retry budget
    cancelBootHeal(); // something rendered — the runtime is healthy, no reload needed
    var pl = previewLauncher();
    if (pl && pl.clearPreviewError) {
      try {
        pl.clearPreviewError();
      } catch (e) {
        /* no-op */
      }
    }
  }

  // --- Boot-race self-heal (see header) ------------------------------------
  // Armed when a fatal is swallowed while expo isn't installed. Polls for the
  // late JSI install on a bounded timer; when expo comes up, retracts the
  // queued boot-race report and reloads the guest JS once so the fresh
  // evaluation starts with expo present. If expo never comes up (torn-down
  // context), the poll expires and normal (queued) reporting resumes.
  var bootFatalSeen = false; // a fatal was swallowed while !expoReady()
  var bootHealDone = false; // heal question settled: reloaded, gave up, or expired
  var bootHealTimer = null;
  var bootHealTicks = 0;

  function bootHealPending() {
    return bootFatalSeen && !bootHealDone;
  }

  function noteBootRaceFatal() {
    bootFatalSeen = true;
    scheduleBootHeal();
  }

  function scheduleBootHeal() {
    if (!bootHealPending() || bootHealTimer) return;
    if (bootHealTicks >= 40 || typeof setTimeout !== 'function') {
      bootHealDone = true; // give up — unblock queued-report delivery
      return;
    }
    bootHealTimer = setTimeout(function () {
      bootHealTimer = null;
      bootHealTicks++;
      if (!expoReady()) {
        scheduleBootHeal();
        return;
      }
      attemptBootHeal();
    }, 250);
  }

  function attemptBootHeal() {
    if (bootHealDone) return;
    bootHealDone = true;
    try {
      // Only heal inside the clank host (launcher resolvable now that expo is
      // up). Elsewhere — Expo Go, plain `expo start` — leave stock behavior.
      var pl = previewLauncher();
      var DevSettings = null;
      try {
        DevSettings = require('react-native').DevSettings;
      } catch (e) {}
      if (!pl || !DevSettings || typeof DevSettings.reload !== 'function') return;
      // The queued boot-race reports describe the very condition this reload
      // fixes — retract them (and any pill already shown) so the agent isn't
      // sent chasing an infra hiccup that no code change can fix.
      pendingReports.length = 0;
      if (pl.clearPreviewError) {
        try {
          pl.clearPreviewError();
        } catch (e) {}
      }
      try {
        console.log(
          '[clank-preview] boot-race self-heal: expo installed after a boot fatal — reloading guest JS',
        );
      } catch (e) {}
      DevSettings.reload('clank preview boot-race self-heal');
    } catch (e) {
      /* reload unavailable — queued reports flow through the flush path */
    }
  }

  function cancelBootHeal() {
    bootFatalSeen = false;
    if (bootHealTimer && typeof clearTimeout === 'function') {
      clearTimeout(bootHealTimer);
      bootHealTimer = null;
    }
  }

  // --- Rich error formatting for the agent ---------------------------------
  // A LogBoxLog carries far more than the message: the code frame (syntax
  // errors), the React component stack (file:line of each component), and the
  // JS stack. We assemble those into one report so "Fix it" gives the agent the
  // same context a developer sees in the Expo error screen. Locations are the
  // un-symbolicated bundle positions (symbolication is async), but the file +
  // message + approximate line are enough to pinpoint it.
  //
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
  // During the boot race this is a false negative (expo isn't queryable yet),
  // so treat it as provisional: re-detect at error/flush time and apply the
  // clank-only suppression late via onHostResolvedLate().
  var clankHost = previewLauncher();
  console.log(
    '[clank-preview] host=' +
      (clankHost ? 'clank-mobile' : expoReady() ? 'other' : 'unknown (expo not installed yet)'),
  );

  // DETECT + report every error LogBox sees — fullscreen, syntax, AND the soft
  // toast/Fast-Refresh errors the native error surface can't see (those keep the
  // app alive and never present a surface). This DRIVES the "Fixing a glitch…"
  // pill, so it runs UNCONDITIONALLY: reportError resolves the native module
  // lazily (and queues + retries without it), so the pill never depends on
  // whether the module was resolvable this early. LogBoxData.observe hands us
  // the full log set on each change → the error (with its message, for "Fix
  // it") plus a clean CLEAR when the logs empty (a successful Fast Refresh).
  // Dedup on change.
  try {
    // Deep import — internal-only module, no public export. Its build-time
    // deprecation warning is dropped by the console.warn filter above.
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

  // SUPPRESS LogBox toasts ONLY inside the clank-mobile host, so a normal
  // Expo Go client (or plain `expo start`) keeps stock error behavior. When
  // host detection was a boot-race false negative, this is applied late by
  // onHostResolvedLate() (from the report-flush timer or the error handler).
  var suppressionInstalled = false;
  function installSuppression() {
    if (suppressionInstalled) return;
    suppressionInstalled = true;
    // Kill LogBox warning/error toasts. (Notifications only — the fullscreen
    // inspector is handled by the global handler below; see RN LogBox.js.)
    try {
      // Public root export first — it has existed since RN 0.63, so the deep
      // path is a legacy fallback only. (The deep literal is still collected
      // at build time and warns regardless of the branch taken — the
      // console.warn filter above covers it.)
      var LogBox =
        require('react-native').LogBox ||
        require('react-native/Libraries/LogBox/LogBox').default;
      if (LogBox && LogBox.ignoreAllLogs) LogBox.ignoreAllLogs(true);
    } catch (e) {
      /* LogBox absent (e.g. production) — nothing to silence */
    }
    // ignoreAllLogs only filters logs ADDED from now on. Anything logged
    // BEFORE suppression installed — app warnings during the boot-race window
    // before host detection resolved late — is already in LogBoxData and would
    // keep a toast badge up forever, un-tappable to boot (the host's nop
    // surface delegate suppresses the LogBox inspector). Purge it. This goes
    // through the wrapped clear() from the detection section above, so a
    // pending error pill for a pre-suppression log is retracted too — correct:
    // if the error is still real, the next render re-adds it and observe
    // re-reports (the same clear-then-re-add contract Fast Refresh uses).
    try {
      if (LogBoxData && typeof LogBoxData.clear === 'function') LogBoxData.clear();
    } catch (e) {
      /* detection section didn't get LogBoxData — nothing to purge */
    }
  }

  function onHostResolvedLate() {
    if (!clankHost) clankHost = previewLauncher();
    if (clankHost) installSuppression();
  }

  if (clankHost) installSuppression();

  // Swallow uncaught fatals before they open the fullscreen LogBox — or worse.
  // Installed UNCONDITIONALLY (not just when clankHost resolved): during the
  // boot race, host detection is a false negative, and a fatal that reaches
  // RN's default handler while the bridgeless runtime is still initializing
  // takes the "[runtime not ready]" path — with dev support disengaged (the
  // preview window closed) ExceptionsManagerModule.reportException THROWS, the
  // exception crosses JNI uncaught, and std::terminate kills the ENTIRE host
  // process. The clank/not-clank decision moves to error time, when it can
  // actually be answered.
  var EU = g.ErrorUtils;
  if (EU && typeof EU.setGlobalHandler === 'function') {
    var prevHandler =
      typeof EU.getGlobalHandler === 'function' ? EU.getGlobalHandler() : null;
    EU.setGlobalHandler(function clankGlobalErrorHandler(error, isFatal) {
      var msg = (error && (error.message || String(error))) || 'unknown error';
      onHostResolvedLate(); // detection may have been a boot-race false negative
      if (clankHost) {
        reportError(msg, isFatal);
        // Deliberately do NOT call the previous handler: it re-enters LogBox
        // (the very overlay we're suppressing) and can crash the app. We keep
        // the runtime alive so the next Fast Refresh update can replace the bad
        // module; the native overlay shows the calm "Fixing a glitch…" pill.
        return;
      }
      if (!expoReady()) {
        // Broken boot: global.expo never installed, so "are we inside clank?"
        // is unanswerable — and letting the error through is what bricks the
        // preview or aborts the process (see above). Swallow + queue, and arm
        // the self-heal: if expo comes up moments later, one reload boots the
        // app cleanly and the queued report is retracted. A healthy Expo Go
        // always has global.expo before app code runs, so it never lands
        // here; the worst case outside clank is a blank screen instead of a
        // crash.
        noteBootRaceFatal();
        reportError(msg, isFatal);
        return;
      }
      // Healthy expo runtime and genuinely no PreviewLauncher → not the clank
      // host (Expo Go / plain `expo start`): stock error behavior.
      if (typeof prevHandler === 'function') prevHandler(error, isFatal);
    });
  }

  // Bridge-ready hooks for a future "visual edits" tool. No behavior today
  // beyond the error funnel — just a stable, namespaced surface to build on.
  g.__clankPreview.reportError = function (m) {
    reportError(m, false);
  };
  g.__clankPreview.clearError = clearError;
})();
