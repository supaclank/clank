// clank web preview overlay — the browser twin of clank-mobile's
// floating prompt box (modules/preview-launcher's FloatingPromptBox).
//
// Injected into every HTML page by internal/webpreview's proxy. Talks
// to the clank daemon through the same-origin /__clank/api relay using
// the per-run token from window.__CLANK_PREVIEW, so no CORS and no
// credentials beyond the injected config.
//
// Interaction model (mobile parity, hotkeys instead of shake):
//   ⌘E / ⌃E        toggle the prompt box (shake analog)
//   Caps Lock      tap: start dictation, tap again: stop & transcribe
//                  (first use picks local vs Web Speech; ▾ by the mic switches)
//   hold ⇧         the box glides to the cursor (spring), settles on release
//   hold ⌘ / ⌃     momentary element-select; click tags, release exits
//   Esc            leave inspect mode, else hide
//   header tap     expand / collapse the chat view
//   ⛶ (toolbar)    grab a screenshot area — freeze a tab capture, crop
//                  it (mobile ScreenshotCropOverlay parity), and stage
//                  the PNG as an image attachment on the next send
//   paste / +      paste an image into the box (or pick local files
//                  via the + button) to stage it the same way
//
// Element → source resolution prefers deterministic compiler metadata:
// Svelte dev mode stamps every node with __svelte_meta.loc; React ≤18
// exposes fiber._debugSource; otherwise we fall back to the component
// owner chain (React 19) or a plain DOM description. Per the design
// thesis, the agent does the edit — this overlay only has to hand it
// unambiguous context.
import {
  PLAN_TOOL, textFromParts, activeQuestionFromParts, chatFromMessages,
  questionSuppressesPermission, pushPermission, dropPermission,
  customAllowed, toggleSelection, buildAnswers, collectPlanParts, planTextFor,
  buildPreviewContext, composerTextForSend,
  previewGitRef,
} from './chat.js';
import {
  resolvePreset, applyPresetOverrides, configRows, setConfigOverride,
  diffConfigAgainstOptions, effectiveSessionConfig, profileLabel,
  profileSavePayload,
} from './settings.js';

(() => {
  'use strict';
  if (window.__clankOverlay) return;
  window.__clankOverlay = true;

  const CFG = window.__CLANK_PREVIEW || {};
  const TOKEN = CFG.token || '';
  const DONE_LINGER_MS = 8000; // mobile: PreviewOverlayState.DONE_LINGER_MS
  const DEFAULT_PROFILE_STORAGE_KEY = 'clank.defaultPresetByBackend';
  const CONFIG_OPTION_MODE = 'mode';
  // macOS fires CapsLock keydown when the lock engages and keyup when it
  // disengages — one event per physical press, alternating type. Other
  // platforms fire a normal down/up pair per press.
  const IS_MAC = /Mac|iP(hone|ad|od)/.test(navigator.platform);

  // ---------- console error ring (context for "why is this broken") ----
  const recentErrors = [];
  const pushErr = (msg) => {
    const s = String(msg).slice(0, 300);
    recentErrors.push(s);
    if (recentErrors.length > 20) recentErrors.shift();
  };
  const origConsoleError = console.error.bind(console);
  console.error = (...args) => {
    try { pushErr(args.map((a) => (a && a.stack) || String(a)).join(' ')); } catch {}
    origConsoleError(...args);
  };
  window.addEventListener('error', (e) => pushErr(e.message + (e.filename ? ` (${e.filename}:${e.lineno})` : '')));
  window.addEventListener('unhandledrejection', (e) => pushErr('unhandled rejection: ' + ((e.reason && e.reason.message) || e.reason)));

  // ---------- api client -----------------------------------------------
  const api = async (path, opts = {}) => {
    const res = await fetch('/__clank/api' + path, {
      ...opts,
      headers: {
        ...(TOKEN ? { Authorization: 'Bearer ' + TOKEN } : {}),
        ...(opts.body ? { 'Content-Type': 'application/json' } : {}),
        ...(opts.headers || {}),
      },
    });
    if (!res.ok) {
      if (res.status === 401) {
        // The token is per preview run; a restarted `clank preview`
        // invalidates every open page. Without this the failure mode is
        // "everything silently stops working" until a reload.
        toast('preview restarted — reload this page to reconnect');
      }
      const text = await res.text().catch(() => '');
      throw new Error(`${opts.method || 'GET'} ${path}: ${res.status} ${text.slice(0, 200)}`);
    }
    return res;
  };
  const apiJSON = async (path, opts) => {
    const res = await api(path, opts);
    if (res.status === 204) return null;
    return res.json().catch(() => null);
  };

  const readDefaultProfiles = () => {
    try {
      const saved = JSON.parse(localStorage.getItem(DEFAULT_PROFILE_STORAGE_KEY) || '{}');
      return saved && typeof saved === 'object' && !Array.isArray(saved) ? saved : {};
    } catch {
      return {};
    }
  };

  const readDefaultProfileID = (backend) => {
    if (!backend) return '';
    const saved = readDefaultProfiles();
    return typeof saved[backend] === 'string' ? saved[backend] : '';
  };

  const writeDefaultProfileID = (backend, profileID) => {
    const saved = readDefaultProfiles();
    saved[backend] = profileID;
    localStorage.setItem(DEFAULT_PROFILE_STORAGE_KEY, JSON.stringify(saved));
  };

  // ---------- state ------------------------------------------------------
  const store = {
    box: 'hidden', // hidden | prompt | chat
    agent: 'idle', // idle | thinking | working | done | error
    inspect: false,
    crop: false, // screenshot crop layer is up
    chips: [], // [{label, detail, html?, text?, comment?, range?, node?}] — comment = inline comment pinned to the anchor; node dedupes ⌘-selected elements
    images: [], // staged image attachments [{dataURL, mime, filename, label, w, h}]
    msgs: [], // [{role, text}]
    streamText: '', // in-flight assistant text
    perms: [], // pending permission queue [{request_id, tool, description, tool_use_id?}] — head renders
    // Active question card [QST-001]: prompt fields + per-question UI
    // selection state. null when no question awaits an answer.
    question: null, // {request_id, partId, questions, idx, sel: [Set], custom: [string], sending}
    planParts: [], // recent ExitPlanMode plans [{id, plan}] for the review card
    sessionId: sessionStorage.getItem('clank.sessionId') || CFG.session_id || '',
    lastUserMsgId: '',
    voice: 'idle', // idle | recording | transcribing (or 'off' when unavailable)
    engine: CFG.dictation_engine || '', // '' (ask on first use) | 'local' | 'webspeech'
    enginePick: false, // engine picker panel open
    sending: false,
    aborting: false,
    profiles: [], // host-persisted built-in + user agent profiles
    profilesLoaded: false,
    profilesLoading: false,
    profileID: '', // create-time profile; live sessions use pendingConfig
    profileOverrides: {},
    defaultProfileID: readDefaultProfileID(CFG.backend),
    configOptions: null, // agent-advertised knobs; null until probed/fetched
    settingsOpen: false,
    settingsLoading: false,
    profilesError: '',
    configOptionsError: '',
    expandedConfigID: '',
    pendingConfig: {}, // live-session changes staged for the next send
    saveProfileOpen: false,
    saveProfileName: '',
    profileSaving: false,
  };
  // Dictation engines. 'local' streams PCM to the preview process
  // (CFG.voice = that engine exists); 'webspeech' is the browser's own
  // SpeechRecognition, which sends audio to the browser vendor's
  // service — never auto-picked: the user opts in via the picker, and
  // the CLI persists the choice across preview runs.
  const SR = window.SpeechRecognition || window.webkitSpeechRecognition;
  const LOCAL_VOICE = !!CFG.voice;
  if (!LOCAL_VOICE && !SR) store.voice = 'off';
  if (store.sessionId) sessionStorage.setItem('clank.sessionId', store.sessionId);
  let doneTimer = 0;

  const setAgent = (s) => {
    clearTimeout(doneTimer);
    store.agent = s;
    if (s === 'done') doneTimer = setTimeout(() => { store.agent = 'idle'; render(); }, DONE_LINGER_MS);
    render();
  };

  // ---------- react 19: _debugStack → sourcemap --------------------------
  // React 19 removed fiber._debugSource, but dev fibers carry _debugStack:
  // an Error whose first same-origin, non-node_modules frame is the JSX
  // callsite in the SERVED (transformed) module. Vite serves each source
  // file as its own module with an inline sourcemap, so decoding that map
  // turns the frame back into the exact original file:line:col — no build
  // plugin, no config, nothing installed in the user's project.

  // parseDebugStack finds the JSX callsite frame. Handles both stack
  // shapes — Chrome "at Fn (url:L:C)" / "at url:L:C" and Firefox
  // "Fn@url:L:C" — and two URL families:
  //   http(s) same-origin  → Vite-style per-file modules (inline maps)
  //   bundler schemes      → Next/webpack "webpack-internal:///(group)/./app/page.jsx"
  //                          and Turbopack equivalents, resolved via
  //                          Next's own dev endpoint (see resolveNextFrame)
  const parseDebugStack = (stack) => {
    for (const line of String(stack).split('\n')) {
      // \S*? (not [^()]) because bundler URLs contain parens:
      // "webpack-internal:///(app-pages-browser)/./app/page.jsx" — the
      // trailing :line:col)?$ anchor keeps the lazy match honest.
      const m = line.match(/(?:at\s+(?:.*?\s+\()?|@)([a-z+-]+:\/\/\S*?):(\d+):(\d+)\)?$/);
      if (!m) continue;
      const [, raw, ln, col] = m;
      if (raw.includes('/node_modules/') || raw.includes('next/dist')) continue;
      if (/^https?:\/\//.test(raw)) {
        let u;
        try { u = new URL(raw); } catch { continue; }
        if (u.origin !== location.origin) continue;
        if (u.pathname.startsWith('/@')) continue; // vite internals
        // href (with Vite's HMR cache-busting query) is the fetch/cache
        // key; url (query stripped) is what gets displayed.
        return { url: u.origin + u.pathname, href: u.href, line: +ln, column: +col };
      }
      // Non-http bundler scheme: keep the raw specifier for the resolver
      // endpoint, plus a cleaned display path (strip scheme, "(group)/",
      // leading "./").
      const cleaned = raw.replace(/^[a-z+-]+:\/\/\/?/, '').replace(/^\([^)]*\)\//, '').replace(/^\.\//, '');
      return { bundlerFile: raw, file: cleaned, line: +ln, column: +col };
    }
    return null;
  };

  // resolveNextFrame: exact original position for bundler-scheme frames,
  // courtesy of Next's own dev middleware (the same endpoint its error
  // overlay uses — it holds the webpack/turbopack sourcemaps server-side).
  // Field names changed across Next versions (lineNumber/column →
  // line1/column1 in 15.5); send both, accept both.
  const nextFrameCache = new Map();
  const resolveNextFrame = (pos) => {
    const key = `${pos.bundlerFile}:${pos.line}:${pos.column}`;
    let p = nextFrameCache.get(key);
    if (p) return p;
    p = (async () => {
      const res = await fetch('/__nextjs_original-stack-frames', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          frames: [{
            file: pos.bundlerFile, methodName: '<unknown>', arguments: [],
            line1: pos.line, column1: pos.column,
            lineNumber: pos.line, column: pos.column,
          }],
          isServer: false, isEdgeServer: false, isAppDirectory: true,
        }),
      });
      if (!res.ok) return null;
      const arr = await res.json();
      const f = arr && arr[0] && arr[0].status === 'fulfilled' && arr[0].value && arr[0].value.originalStackFrame;
      if (!f || !f.file) return null;
      const line = f.line1 ?? f.lineNumber;
      const column = f.column1 ?? f.column;
      // An unresolved lookup echoes the bundler specifier back — treat
      // that (or a missing line) as failure rather than showing it.
      if (line == null || /^[a-z+-]+:\/\//.test(f.file)) return null;
      return { file: f.file.replace(/^\.\//, ''), line, column: column ?? 1 };
    })().catch(() => null);
    // Evict failed lookups so a transient dev-server hiccup doesn't
    // permanently pin this frame to the approximate location.
    p.then((res) => { if (!res) nextFrameCache.delete(key); });
    nextFrameCache.set(key, p);
    return p;
  };

  // Minimal source-map v3 consumer: decode the VLQ mappings once per
  // module into per-line segment arrays, then binary-ish scan per lookup.
  const B64V = (() => {
    const t = {};
    'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/'.split('').forEach((c, i) => (t[c] = i));
    return t;
  })();
  const decodeMappings = (mappings) => {
    const lines = [];
    let srcIdx = 0, srcLine = 0, srcCol = 0;
    for (const lineStr of mappings.split(';')) {
      const segs = [];
      let genCol = 0;
      for (const segStr of lineStr.split(',')) {
        if (!segStr) continue;
        const vals = [];
        let shift = 0, value = 0;
        for (const ch of segStr) {
          const d = B64V[ch];
          value += (d & 31) << shift;
          if (d & 32) { shift += 5; continue; }
          vals.push(value & 1 ? -(value >>> 1) : value >>> 1);
          shift = 0; value = 0;
        }
        if (vals.length === 0) continue;
        genCol += vals[0];
        if (vals.length >= 4) {
          srcIdx += vals[1]; srcLine += vals[2]; srcCol += vals[3];
          segs.push([genCol, srcIdx, srcLine, srcCol]);
        }
      }
      lines.push(segs);
    }
    return lines;
  };

  // TODO(ai-review): unbounded across HMR edits within a session; add an eviction/size cap if it matters in practice. https://github.com/supaclank/clank/pull/142
  const sourceMapCache = new Map(); // module url → Promise<lookup fn | null>
  const moduleLookup = (url) => {
    let p = sourceMapCache.get(url);
    if (p) return p;
    p = (async () => {
      const res = await fetch(url);
      if (!res.ok) return null;
      const code = await res.text();
      const m = code.match(/\/\/[#@] sourceMappingURL=data:application\/json[^,]*base64,([A-Za-z0-9+/=]+)\s*$/);
      if (!m) return null;
      const bytes = Uint8Array.from(atob(m[1]), (c) => c.charCodeAt(0));
      const map = JSON.parse(new TextDecoder().decode(bytes));
      const lines = decodeMappings(map.mappings);
      return (line, column) => {
        try {
          // Stack positions are 1-based; the map is 0-based.
          const segs = lines[line - 1];
          if (!segs || !segs.length) return null;
          let best = segs[0];
          for (const s of segs) { if (s[0] <= column - 1) best = s; else break; }
          const src = (map.sources && map.sources[best[1]]) || '';
          if (!src) return null;
          // sources are relative to the module's directory.
          const file = new URL(src, url).pathname.replace(/^\//, '');
          return { file, line: best[2] + 1, column: best[3] + 1 };
        } catch {
          return null;
        }
      };
    })().catch(() => null);
    sourceMapCache.set(url, p);
    return p;
  };

  // resolveStackPos: served-module position → original source position.
  const resolveStackPos = async (pos) => {
    const lookup = await moduleLookup(pos.href || pos.url);
    return lookup ? lookup(pos.line, pos.column) : null;
  };

  // React ≤18's fiber._debugSource carries an absolute FS path — and,
  // under Vite's react plugin, line numbers shifted by the injected
  // refresh preamble (Babel stamps positions post-injection). Both are
  // fixed by the same sourcemap pass as React 19: map the FS path back
  // to its module URL (the preview config carries the project root;
  // macOS may report the /private realpath alias) and resolve.
  const fsPathToModuleURL = (fileName) => {
    const root = CFG.local_path || '';
    if (!root) return null;
    for (const r of [root, '/private' + root]) {
      if (fileName.startsWith(r + '/')) return location.origin + fileName.slice(r.length);
    }
    return null;
  };

  // servedPosition → the provisional result shape shared by the React
  // 18 and 19 tiers: exact file (served), approximate line until the
  // async sourcemap resolution upgrades it in place.
  const provisionalSource = (pos, names, el, via) => ({
    file: pos.url.slice(location.origin.length).replace(/^\//, ''),
    line: pos.line,
    column: pos.column,
    approx: true,
    resolve: resolveStackPos(pos),
    via,
    names,
    node: el,
  });

  // ---------- element → source -------------------------------------------
  const resolveSource = (el) => {
    for (let n = el; n && n.nodeType === 1; n = n.parentElement) {
      const m = n.__svelte_meta;
      if (m && m.loc && m.loc.file) {
        return { file: m.loc.file, line: m.loc.line, column: m.loc.column, via: 'svelte', names: [], node: n };
      }
    }
    for (let n = el; n && n.nodeType === 1; n = n.parentElement) {
      // The fiber prop's random suffix is fixed per React instance for the
      // page's lifetime — resolve it once instead of scanning every
      // ancestor's full property list on every mousemove.
      let key = resolveSource.reactFiberKey;
      if (!key || !(key in n)) {
        key = Object.getOwnPropertyNames(n).find((k) => k.startsWith('__reactFiber$'));
        if (key) resolveSource.reactFiberKey = key;
      }
      if (!key) continue;
      let fiber = n[key];
      const stackPos = fiber._debugStack ? parseDebugStack(fiber._debugStack.stack) : null;
      const names = [];
      let src = null;
      while (fiber && names.length < 5) {
        if (!src && fiber._debugSource) src = fiber._debugSource; // React ≤ 18
        const t = fiber.type;
        const nm = typeof t === 'function' ? t.displayName || t.name : t && t.displayName;
        if (nm && !names.includes(nm)) names.push(nm);
        fiber = fiber._debugOwner;
      }
      if (src) {
        // React ≤18: route through the sourcemap when the FS path maps
        // to a served module; fall back to the raw values otherwise.
        const url = fsPathToModuleURL(src.fileName);
        if (url) return provisionalSource({ url, line: src.lineNumber, column: src.columnNumber }, names, el, 'react18');
        return { file: src.fileName, line: src.lineNumber, column: src.columnNumber, via: 'react', names, node: el };
      }
      if (stackPos && stackPos.bundlerFile) {
        // Next.js (webpack/turbopack schemes): provisional shows the
        // cleaned bundler path + transformed line; Next's own resolver
        // upgrades it to the exact original position.
        return {
          file: stackPos.file, line: stackPos.line, column: stackPos.column,
          approx: true, resolve: resolveNextFrame(stackPos),
          via: 'next', names, node: el,
        };
      }
      if (stackPos) {
        // React 19 on Vite: the JSX callsite from _debugStack, same
        // upgrade path through the module's inline sourcemap.
        return provisionalSource(stackPos, names, el, 'react19');
      }
      if (names.length) return { via: 'react', names, node: el };
      break;
    }
    return { via: 'dom', names: [], node: el };
  };

  const domPath = (el) => {
    const parts = [];
    for (let n = el; n && n.nodeType === 1 && parts.length < 4; n = n.parentElement) {
      let p = n.tagName.toLowerCase();
      if (n.id) { parts.unshift(p + '#' + n.id); break; }
      if (n.classList.length) p += '.' + [...n.classList].slice(0, 2).join('.');
      parts.unshift(p);
    }
    return parts.join(' > ');
  };

  const shortHTML = (el) => {
    let h = el.outerHTML || '';
    h = h.replace(/\s+/g, ' ');
    return h.length > 300 ? h.slice(0, 300) + '…' : h;
  };

  const chipFromElement = (el) => {
    const s = resolveSource(el);
    const comps = s.names.length ? ` (components: ${s.names.join(' › ')})` : '';
    const base = s.file ? `${s.file}:${s.line}${s.column ? ':' + s.column : ''}` : domPath(el);
    const label = s.file ? `${s.file.split('/').pop()}:${s.line}` : `<${el.tagName.toLowerCase()}>`;
    const chip = { label, detail: base + comps, html: shortHTML(el) };
    if (s.resolve) {
      // React 19: upgrade the provisional served-module position to the
      // sourcemap-exact one in place; render() repaints the chip row.
      s.resolve.then((orig) => {
        if (!orig) return;
        chip.label = `${orig.file.split('/').pop()}:${orig.line}`;
        chip.detail = `${orig.file}:${orig.line}:${orig.column}${comps}`;
        render();
      });
    }
    return chip;
  };

  // Serialization lives in chat.js (buildPreviewContext) so node --test
  // covers the exact context format the agent receives.
  const buildContext = () => buildPreviewContext({
    chips: store.chips,
    images: store.images.map((s) => s.filename),
    route: location.pathname + location.search,
    viewport: `${innerWidth}x${innerHeight}`,
    errors: recentErrors,
  });

  // ---------- agent profiles / settings ---------------------------------
  // Profiles themselves live on the host (GET /presets). The browser only
  // keeps the per-backend "use by default" pointer, matching mobile's
  // phone-local SecureStore preference; unsent knob edits stay in memory.
  let profilesPromise = null;
  const loadProfiles = async () => {
    if (store.profilesLoaded) return store.profiles;
    if (profilesPromise) return profilesPromise;
    store.profilesLoading = true;
    render();
    profilesPromise = apiJSON('/presets?' + new URLSearchParams({
      backend: CFG.backend,
      hostname: CFG.hostname || 'local',
    })).then((profiles) => {
      if (!Array.isArray(profiles)) throw new Error('profile list was not an array');
      store.profiles = profiles.filter((p) => p && p.backend === CFG.backend && p.config);
      store.profilesLoaded = true;
      if (!store.sessionId) {
        const selected = resolvePreset(store.profiles, CFG.backend, store.profileID || store.defaultProfileID);
        store.profileID = selected ? selected.id : '';
      }
      store.profilesError = '';
      return store.profiles;
    }).catch((err) => {
      store.profilesError = 'Could not load profiles: ' + err.message;
      throw err;
    }).finally(() => {
      store.profilesLoading = false;
      profilesPromise = null;
      render();
    });
    return profilesPromise;
  };

  const configOptionsPath = () => {
    if (!CFG.backend) throw new Error('no backend in the preview config — restart clank preview');
    const ref = previewGitRef(CFG);
    if (!ref) throw new Error('preview config must identify exactly one worktree or local path');
    const q = new URLSearchParams({ backend: CFG.backend });
    if (ref.worktree_id) q.set('git_worktree_id', ref.worktree_id);
    else q.set('git_local_path', ref.local_path);
    return '/config-options?' + q;
  };

  let configOptionsPromise = null;
  let configOptionsScope = '';
  let configOptionsRequestID = 0;
  const loadConfigOptions = async () => {
    const scope = store.sessionId ? `session:${store.sessionId}` : 'create';
    if (configOptionsPromise && configOptionsScope === scope) return configOptionsPromise;
    const requestID = ++configOptionsRequestID;
    configOptionsScope = scope;
    store.settingsLoading = true;
    store.configOptionsError = '';
    render();
    const request = (store.sessionId
      ? apiJSON(`/sessions/${store.sessionId}`).then((info) => {
          if (!info) throw new Error('session settings were unavailable');
          return info.config_options || [];
        })
      : apiJSON(configOptionsPath())
    ).then((options) => {
      if (!Array.isArray(options)) throw new Error('agent settings were not an array');
      if (requestID === configOptionsRequestID) {
        store.configOptions = options;
        if (store.sessionId) {
          store.pendingConfig = diffConfigAgainstOptions(store.pendingConfig, options);
        }
      }
      return options;
    }).catch((err) => {
      if (requestID === configOptionsRequestID) {
        store.configOptionsError = 'Could not load agent settings: ' + err.message;
      }
      throw err;
    }).finally(() => {
      if (configOptionsPromise === request) {
        store.settingsLoading = false;
        configOptionsPromise = null;
        configOptionsScope = '';
        render();
      }
    });
    configOptionsPromise = request;
    return request;
  };

  const selectedCreateProfile = () =>
    resolvePreset(store.profiles, CFG.backend, store.profileID || store.defaultProfileID);

  const openSettings = () => {
    store.settingsOpen = true;
    store.enginePick = false;
    store.expandedConfigID = '';
    render();
    Promise.allSettled([loadProfiles(), loadConfigOptions()]);
  };

  const closeSettings = () => {
    store.settingsOpen = false;
    store.expandedConfigID = '';
    render();
  };

  const openSaveProfile = () => {
    store.saveProfileName = '';
    store.saveProfileOpen = true;
    render();
    setTimeout(() => {
      ui.saveProfileName.value = '';
      ui.saveProfileName.focus({ preventScroll: true });
    }, 0);
  };

  const closeSaveProfile = () => {
    if (store.profileSaving) return;
    store.saveProfileOpen = false;
    store.saveProfileName = '';
    render();
  };

  const saveProfile = async () => {
    if (store.profileSaving) return;
    try {
      const config = store.sessionId
        ? effectiveSessionConfig(store.configOptions, store.pendingConfig)
        : applyPresetOverrides(selectedCreateProfile(), store.profileOverrides);
      const payload = profileSavePayload(store.saveProfileName, CFG.backend, config);
      store.profileSaving = true;
      render();
      const saved = await apiJSON('/presets', {
        method: 'POST',
        body: JSON.stringify(payload),
      });
      if (!saved || !saved.id) throw new Error('profile save returned no id');
      const index = store.profiles.findIndex((profile) => profile.id === saved.id);
      if (index >= 0) store.profiles[index] = saved;
      else store.profiles.push(saved);
      store.profilesLoaded = true;
      store.profilesError = '';
      store.profileID = saved.id;
      if (!store.sessionId) store.profileOverrides = {};
      store.saveProfileOpen = false;
      store.saveProfileName = '';
      toast(`saved profile “${saved.name}”`);
    } catch (err) {
      toast('could not save profile: ' + err.message);
    } finally {
      store.profileSaving = false;
      render();
    }
  };

  // ---------- session ------------------------------------------------------
  const send = async () => {
    // Comment-only submits are real sends: the default instruction rides
    // with the inline comments when the composer is empty.
    const text = composerTextForSend(ui.input.value, store.chips);
    if (!text || store.sending) return;
    hideCommentPopover();
    const full = text + buildContext();
    // Staged images ride as inline data: attachments — resolveAttachments
    // decodes them daemon-side, so no upload service is needed here.
    const attachments = store.images.map((s) => ({ mime: s.mime, filename: s.filename, source: s.dataURL }));
    store.sending = true;
    store.msgs.push({ role: 'user', text });
    store.question = null; // a user message after the tagged part retires it [QST-002]
    store.streamText = '';
    setComposer('');
    store.chips = [];
    store.images = [];
    setAgent('thinking');
    try {
      if (!store.sessionId) {
        // Create-time config is the selected host profile plus explicit
        // knob edits. It still begins from a complete profile — the host
        // rejects a create missing any Default-preset key.
        if (!CFG.backend) throw new Error('no backend in the preview config — restart clank preview');
        const gitRef = previewGitRef(CFG);
        if (!gitRef) throw new Error('preview config must identify exactly one worktree or local path');
        const presetList = await loadProfiles();
        const profile = resolvePreset(presetList, CFG.backend, store.profileID || store.defaultProfileID);
        const createConfig = applyPresetOverrides(profile, store.profileOverrides);
        if (!createConfig) throw new Error(`no agent profile for backend ${CFG.backend} — is the daemon up to date?`);
        const info = await apiJSON('/sessions', {
          method: 'POST',
          body: JSON.stringify({
            backend: CFG.backend,
            hostname: CFG.hostname || 'local',
            git_ref: gitRef,
            prompt: full,
            config: createConfig,
            ...(attachments.length ? { attachments } : {}),
          }),
        });
        store.sessionId = (info && info.id) || '';
        if (!store.sessionId) throw new Error('session create returned no id');
        sessionStorage.setItem('clank.sessionId', store.sessionId);
        if (store.configOptions) {
          store.configOptions = store.configOptions.map((option) =>
            Object.hasOwn(createConfig, option.id)
              ? { ...option, current_value: createConfig[option.id] }
              : option);
        }
        store.profileID = '';
        store.profileOverrides = {};
        subscribe();
        loadConfigOptions().catch(() => {});
      } else {
        const pendingConfig = diffConfigAgainstOptions(store.pendingConfig, store.configOptions);
        await api(`/sessions/${store.sessionId}/message`, {
          method: 'POST',
          body: JSON.stringify({
            text: full,
            ...(Object.keys(pendingConfig).length ? { config: pendingConfig } : {}),
            ...(attachments.length ? { attachments } : {}),
          }),
        });
        if (store.configOptions) {
          store.configOptions = store.configOptions.map((option) =>
            Object.hasOwn(pendingConfig, option.id)
              ? { ...option, current_value: pendingConfig[option.id] }
              : option);
        }
        store.pendingConfig = {};
        store.profileID = '';
      }
    } catch (err) {
      toast('send failed: ' + err.message);
      setAgent('error');
    } finally {
      store.sending = false;
      render();
    }
  };

  const abort = async () => {
    if (!store.sessionId) return;
    store.aborting = true;
    // Abort settles every pending prompt server-side (INV-ABORT-PERM-001)
    // — mirror that locally so no dead card lingers.
    store.perms = [];
    store.question = null;
    render();
    try { await api(`/sessions/${store.sessionId}/abort`, { method: 'POST' }); } catch (err) { toast(err.message); }
    store.aborting = false;
    render();
  };

  const revert = async () => {
    if (!store.sessionId || !store.lastUserMsgId) return;
    try {
      await api(`/sessions/${store.sessionId}/revert`, { method: 'POST', body: JSON.stringify({ message_id: store.lastUserMsgId }) });
      toast('reverted last turn');
    } catch (err) { toast('revert failed: ' + err.message); }
  };

  const replyPermission = async (allow) => {
    const p = store.perms[0];
    if (!p || p.sending || !store.sessionId) return;
    p.sending = true;
    render();
    // Plan review [INV-PERMMODE-EXITPLAN-001]: Approve = allow (the
    // backend exits plan mode); Revise = deny with the notes as the
    // reason so the agent re-plans in plan mode.
    const message = !allow && p.tool === PLAN_TOOL ? ui.notes.value.trim() : '';
    try {
      await api(`/sessions/${store.sessionId}/permissions/${p.request_id}/reply`, {
        method: 'POST',
        body: JSON.stringify({ allow, ...(message ? { message } : {}) }),
      });
    } catch (err) {
      toast('permission reply failed: ' + err.message);
      p.sending = false; // keep the prompt queued so the user can retry
      render();
      return;
    }
    store.perms = store.perms.slice(1); // head answered; the next queued prompt renders
    if (p.tool === PLAN_TOOL) ui.notes.value = '';
    render();
  };

  // answeredQuestions: request ids resolved in this page, so a re-emit
  // of the same tagged part (turn settle, refetch) can't resurrect the
  // card [QST-002].
  const answeredQuestions = new Set();

  // applyQuestion activates a tagged question part [QST-001]. Keeps the
  // in-progress selection when the same prompt re-emits; drops any
  // queued permission the card supersedes [QST-003].
  const applyQuestion = (q) => {
    if (!q || answeredQuestions.has(q.prompt.request_id)) return;
    if (store.question && store.question.request_id === q.prompt.request_id) return;
    const questions = q.prompt.questions || [];
    store.question = {
      request_id: q.prompt.request_id,
      partId: q.partId,
      questions,
      idx: 0,
      sel: questions.map(() => new Set()),
      custom: questions.map(() => ''),
      sending: false,
    };
    store.perms = store.perms.filter((p) => !questionSuppressesPermission(store.question, p));
    if (store.box === 'hidden') store.box = 'prompt';
  };

  const replyQuestion = async (reject) => {
    const q = store.question;
    if (!q || q.sending || !store.sessionId) return;
    q.sending = true;
    render();
    const body = reject ? { reject: true } : { answers: buildAnswers(q.questions, q.sel, q.custom) };
    try {
      await api(`/sessions/${store.sessionId}/questions/${q.request_id}/reply`, { method: 'POST', body: JSON.stringify(body) });
    } catch (err) {
      toast((reject ? 'dismiss' : 'answer') + ' failed: ' + err.message);
      // A failed answer keeps the card for retry; a failed dismissal
      // still dismisses locally so the UI can't lock into a dead prompt.
      if (!reject) { q.sending = false; render(); return; }
    }
    answeredQuestions.add(q.request_id);
    // The gating permission (Claude default/plan mode) resolves through
    // the question reply server-side; drop any local copy.
    store.perms = dropPermission(store.perms, q.request_id);
    store.question = null;
    render();
  };

  // ---------- SSE ----------------------------------------------------------
  const CHAT_CAP = 30; // rolling transcript window (the view shows the last 8)

  // reconcile refetches the transcript on every stream (re)open
  // [INV-RECONCILE-001]: events are at-most-once, so this is what
  // restores chat after a reload and backfills anything emitted while
  // disconnected — including a pending question, whose tag rides the
  // transcript [QST-001]. The refetch is point-in-time and may race a
  // fresher live event; it therefore never *clears* a live question,
  // and the next settle re-syncs the transcript.
  const reconcile = async (sid) => {
    let history;
    try {
      history = await apiJSON(`/sessions/${sid}/messages`);
    } catch { return; }
    if (!Array.isArray(history) || store.sessionId !== sid) return;
    const c = chatFromMessages(history, CHAT_CAP);
    store.msgs = c.msgs;
    if (c.lastUserMsgId) store.lastUserMsgId = c.lastUserMsgId;
    store.planParts = c.planParts;
    applyQuestion(c.question);
    render();
  };

  let sseAbort = null;
  let sseBackoff = 1000;
  const subscribe = () => {
    if (!store.sessionId) return;
    if (sseAbort) sseAbort.abort();
    const ac = new AbortController();
    sseAbort = ac;
    const sid = store.sessionId;
    (async () => {
      try {
        const res = await fetch(`/__clank/api/sessions/${sid}/events`, {
          headers: { Authorization: 'Bearer ' + TOKEN, Accept: 'text/event-stream' },
          signal: ac.signal,
        });
        if (!res.ok || !res.body) throw new Error('events: ' + res.status);
        sseBackoff = 1000;
        // Events are at-most-once with no replay: anything emitted between
        // session create (the agent starts immediately) and this stream
        // opening is gone, and the same applies across reconnects. The
        // transcript refetch recovers settled content; the session
        // snapshot syncs the coarse agent state so the border can't
        // stick on a stale state.
        reconcile(sid);
        fetch(`/__clank/api/sessions/${sid}`, { headers: { Authorization: 'Bearer ' + TOKEN } })
          .then((r) => (r.ok ? r.json() : null))
          .then((info) => {
            if (!info || store.sessionId !== sid) return;
            if (Array.isArray(info.config_options)) {
              store.configOptions = info.config_options;
              store.pendingConfig = diffConfigAgainstOptions(store.pendingConfig, info.config_options);
            }
            if (info.status === 'busy' && (store.agent === 'idle' || store.agent === 'done')) setAgent('thinking');
            else if (info.status === 'idle' && (store.agent === 'thinking' || store.agent === 'working')) setAgent('done');
            else render();
          })
          .catch(() => {});
        const reader = res.body.getReader();
        const dec = new TextDecoder();
        let buf = '';
        for (;;) {
          const { done, value } = await reader.read();
          if (done) break;
          buf += dec.decode(value, { stream: true });
          let idx;
          while ((idx = buf.indexOf('\n\n')) >= 0) {
            handleFrame(buf.slice(0, idx));
            buf = buf.slice(idx + 2);
          }
        }
      } catch (e) {
        if (ac.signal.aborted) return;
      }
      if (ac.signal.aborted || store.sessionId !== sid) return;
      setTimeout(() => { if (store.sessionId === sid && sseAbort === ac) subscribe(); }, sseBackoff);
      sseBackoff = Math.min(sseBackoff * 1.5, 15000);
    })();
  };

  const handleFrame = (frame) => {
    let ev = 'message';
    const datas = [];
    for (const line of frame.split('\n')) {
      if (line.startsWith('event:')) ev = line.slice(6).trim();
      else if (line.startsWith('data:')) datas.push(line.slice(5).trim());
    }
    if (!datas.length) return;
    let d;
    // The data JSON is the clank Event envelope {type, session_id,
    // timestamp, data}; the type-specific payload lives under .data
    // (see internal/agent Event / docs/chat-client-spec 04-event-protocol).
    try { d = JSON.parse(datas.join('\n')).data || {}; } catch { return; }
    switch (ev) {
      case 'status': {
        const s = d.new_status;
        if (s === 'idle') { store.streamText && store.msgs.push({ role: 'assistant', text: store.streamText }); store.streamText = ''; setAgent('done'); }
        else if (s === 'error' || s === 'dead') setAgent('error');
        else if (s === 'busy' && store.agent === 'idle') setAgent('thinking');
        break;
      }
      case 'part': {
        const p = d.part || {};
        if (p.type === 'tool_call') {
          setAgent('working');
          store.planParts = collectPlanParts(store.planParts, [p]);
          if (p.question && (p.question.questions || []).length && p.status !== 'error') {
            applyQuestion({ partId: p.id, prompt: p.question });
          } else if (store.question && p.id !== store.question.partId) {
            store.question = null; // a later tool call means the conversation moved on [QST-002]
          } else if (store.question && p.status === 'error') {
            store.question = null; // deny/abort fallout retires the prompt
          }
          render();
        }
        if (p.type === 'text' && p.text) {
          if (store.question && p.id !== store.question.partId) store.question = null; // moved on [QST-002]
          if (d.is_delta) store.streamText += p.text;
          else store.streamText = p.text;
          render();
        }
        break;
      }
      case 'message': {
        if (d.role === 'user') {
          if (d.id) store.lastUserMsgId = d.id;
          store.question = null; // e.g. a bypass answer sent from another client [QST-002]
        }
        if (d.role === 'assistant') {
          const text = textFromParts(d.parts);
          if (text) { store.msgs.push({ role: 'assistant', text }); store.streamText = ''; }
          store.planParts = collectPlanParts(store.planParts, d.parts);
          // The settled message is authoritative for its own parts: a
          // trailing tag (re)activates the card; a question part that is
          // no longer last means it was answered elsewhere.
          const q = activeQuestionFromParts(d.parts);
          if (q) applyQuestion(q);
          else if (store.question && (d.parts || []).some((p) => p.id === store.question.partId)) store.question = null;
        }
        if (store.msgs.length > CHAT_CAP) store.msgs.splice(0, store.msgs.length - CHAT_CAP);
        render();
        break;
      }
      case 'permission':
        store.perms = pushPermission(store.perms, d, store.question);
        if (store.perms.length && store.box === 'hidden') store.box = 'prompt';
        render();
        break;
      case 'error':
        toast('agent error' + (d && d.message ? ': ' + d.message : ''));
        setAgent('error');
        break;
      case 'revert':
        toast('session reverted');
        break;
      default:
        break; // title / meta / session.* / voice.* — not rendered here
    }
  };

  // ---------- voice: engine choice -----------------------------------------
  // usableEngine gates every dictation start. '' opens the picker: no
  // stored choice yet, or the stored engine isn't available here (e.g.
  // chose local, clank-voice missing this run) — never silently swap a
  // "local only" choice for a cloud service.
  const usableEngine = () => {
    if (store.engine === 'local' && LOCAL_VOICE) return 'local';
    if (store.engine === 'webspeech' && SR) return 'webspeech';
    if (!store.engine && LOCAL_VOICE && !SR) return 'local'; // only the private option exists — nothing to ask
    return '';
  };
  const ENGINE_LABEL = { local: 'fully local', webspeech: 'Web Speech API' };
  let enginePickPending = false; // a talk gesture is waiting on the picker
  const openEnginePick = (pending) => {
    enginePickPending = pending;
    store.enginePick = true;
    store.settingsOpen = false;
    if (store.box === 'hidden') store.box = 'prompt';
    render();
  };
  const closeEnginePick = () => {
    enginePickPending = false;
    store.enginePick = false;
    render();
  };
  const chooseEngine = (eng) => {
    const wasPending = enginePickPending;
    store.engine = eng;
    // Persist for future preview runs. The switch itself is already
    // live — a failed save only costs durability, so keep going.
    fetch('/__clank/voice/engine', {
      method: 'POST',
      headers: { Authorization: 'Bearer ' + TOKEN, 'Content-Type': 'application/json' },
      body: JSON.stringify({ engine: eng }),
    }).then((r) => { if (!r.ok) throw new Error('HTTP ' + r.status); })
      .catch(() => toast('couldn’t save the choice — it applies to this preview only'));
    closeEnginePick();
    if (wasPending) startTalk();
  };

  // ---------- voice: Web Speech API engine ----------------------------------
  // The browser's built-in recognizer, run entirely in-page: no
  // worklet, nothing on the /__clank/voice socket — but audio is
  // processed by the browser vendor's speech service, which is exactly
  // why it's opt-in above.
  let rec = null; // active recognition; non-null marks a webspeech utterance
  const WEBSPEECH_ERRORS = {
    'not-allowed': 'microphone permission denied',
    'service-not-allowed': 'the browser blocked its speech service',
    network: 'speech service unreachable (Web Speech needs network)',
  };
  const startWebSpeech = () => {
    const r = new SR();
    rec = r;
    r.continuous = true; // ⇪ ends the utterance, not the first pause
    r.interimResults = true; // live partials in the composer, local-engine parity
    r.lang = navigator.language || 'en-US';
    let text = '';
    let failure = '';
    r.onresult = (e) => {
      // e.results is cumulative (finals + current interim) — the same
      // utterance-so-far shape the local engine's partials have.
      text = '';
      for (const res of e.results) text += res[0].transcript;
      text = text.trim();
      if ((store.voice === 'recording' || store.voice === 'transcribing') && text) setComposer(withVoiceText(text));
    };
    r.onerror = (e) => {
      // no-speech is just an empty utterance; aborted a deliberate stop.
      if (e.error !== 'no-speech' && e.error !== 'aborted') failure = WEBSPEECH_ERRORS[e.error] || e.error;
    };
    // onend follows rec.stop(), every error, AND service-initiated ends
    // (silence timeout mid-hold) — the single finalize point.
    r.onend = () => {
      if (rec !== r) return; // superseded — a newer utterance owns the state
      rec = null;
      if (store.voice !== 'recording' && store.voice !== 'transcribing') return;
      store.voice = 'idle';
      if (failure) {
        setComposer(voiceBase);
        toast('dictation: ' + failure);
      } else if (text) {
        setComposer(withVoiceText(text));
        // Same post-dictation anchor as the local final: Enter sends.
        if (root.activeElement !== ui.input) ui.box.focus({ preventScroll: true });
      } else {
        setComposer(voiceBase);
        toast('didn’t catch any speech — try again');
      }
      render();
    };
    try {
      r.start();
    } catch (err) {
      rec = null;
      store.voice = 'idle';
      toast('mic: ' + err.message);
      render();
    }
  };

  // ---------- voice (push-to-talk, local engine transport) ------------------
  let vws = null; // dictation WebSocket, opened lazily, reused
  let audio = null; // {ctx, stream, node}
  let voiceBase = ''; // input text at push-to-talk start; partials append after it
  const withVoiceText = (t) => (voiceBase ? voiceBase.replace(/\s+$/, '') + ' ' : '') + t;
  const voiceWS = () =>
    new Promise((resolve, reject) => {
      if (vws && vws.readyState === WebSocket.OPEN) return resolve(vws);
      const proto = location.protocol === 'https:' ? 'wss' : 'ws';
      // TODO(ai-review): move TOKEN off the URL (browser WebSocket has no
      // custom-header API) https://github.com/supaclank/clank/pull/135#discussion_r3571183477
      const w = new WebSocket(`${proto}://${location.host}/__clank/voice?t=${encodeURIComponent(TOKEN)}`);
      w.onopen = () => { vws = w; resolve(w); };
      w.onerror = () => {
        // Classify before giving up: a rejected upgrade after a preview
        // restart is a 401 underneath, and api() will say so in a toast.
        api('/backends').catch(() => {});
        reject(new Error('voice socket failed'));
      };
      w.onmessage = (e) => {
        let m;
        try { m = JSON.parse(e.data); } catch { return; }
        if (m.type === 'partial') {
          // Cumulative utterance-so-far from the engine's VAD segments —
          // preview it live in the composer while the key is still held.
          if ((store.voice === 'recording' || store.voice === 'transcribing') && m.text) {
            setComposer(withVoiceText(m.text));
          }
        } else if (m.type === 'final') {
          if (store.voice === 'transcribing') store.voice = 'idle';
          setComposer(m.text ? withVoiceText(m.text) : voiceBase);
          if (!m.text) toast('didn’t catch any speech — try again');
          // Anchor focus on the container (unless the user is already
          // typing in the composer): Enter still sends via the box's
          // Enter handler, shift-move keeps working, Tab reaches the
          // composer in one press.
          if (root.activeElement !== ui.input) ui.box.focus({ preventScroll: true });
          render();
        } else if (m.type === 'error') {
          store.voice = 'idle';
          setComposer(voiceBase);
          toast('dictation: ' + m.error);
          render();
        }
      };
      w.onclose = () => { if (vws === w) vws = null; };
    });

  // A suspended capture graph rots when the tab is BACKGROUNDED:
  // Chromium/macOS power management guts the capture path while
  // everything still reports healthy — track "live", context resumes
  // to "running" — and the source then delivers pure zeros. Teardown
  // therefore fires on the actual trigger (the tab going hidden; a
  // hidden tab can't be dictated into, so a warm graph there is pure
  // rot risk), not on a guessed timer. The idle timer below is only
  // foreground hygiene: it turns the OS mic indicator off when you
  // haven't dictated in a while. Rebuild costs ~300 ms, no re-prompt.
  const AUDIO_IDLE_TEARDOWN_MS = 60_000;
  let audioIdleReap = 0;
  let utterPeak = 0; // max worklet RMS this utterance; 0.0 = digitally dead capture

  const teardownAudio = () => {
    clearTimeout(audioIdleReap);
    if (!audio) return;
    audio.stream.getTracks().forEach((t) => t.stop());
    audio.ctx.close().catch(() => {});
    audio = null;
  };
  const scheduleAudioReap = () => {
    clearTimeout(audioIdleReap);
    if (document.hidden) teardownAudio(); // already backgrounded — don't let it rot
    else audioIdleReap = setTimeout(teardownAudio, AUDIO_IDLE_TEARDOWN_MS);
  };
  document.addEventListener('visibilitychange', () => {
    // Mid-recording backgrounds are left alone: an ACTIVE graph keeps
    // capturing (that's a deliberate cmd-tab while dictating), and it
    // is torn down at stopTalk via scheduleAudioReap seeing hidden.
    if (document.hidden && store.voice !== 'recording') teardownAudio();
  });
  const buildAudio = async () => {
    const ctx = new AudioContext({ sampleRate: 16000 });
    const stream = await navigator.mediaDevices.getUserMedia({ audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true } });
    await ctx.audioWorklet.addModule('/__clank/worklet.js');
    const srcNode = ctx.createMediaStreamSource(stream);
    const node = new AudioWorkletNode(ctx, 'clank-pcm');
    node.port.onmessage = (e) => {
      if (store.voice === 'recording' && vws && vws.readyState === WebSocket.OPEN) {
        vws.send(e.data.pcm.buffer);
        if (e.data.level > utterPeak) utterPeak = e.data.level;
        ui.micLevel.style.setProperty('--lvl', Math.min(1, e.data.level * 6).toFixed(2));
      }
    };
    srcNode.connect(node);
    const a = { ctx, stream, node };
    // The OS can end the track behind our back (device sleep, another
    // app grabbing the mic). Drop the graph so the next push-to-talk
    // rebuilds instead of streaming silence.
    stream.getTracks().forEach((t) => (t.onended = () => { if (audio === a) teardownAudio(); }));
    return a;
  };
  const audioAlive = () => !!audio && audio.stream.getTracks().some((t) => t.readyState === 'live');

  let capturingSince = 0; // when live audio actually started flowing
  let startingTalk = null; // in-flight startTalk bring-up

  const startTalk = () => {
    if (store.voice !== 'idle') return;
    const eng = usableEngine();
    if (!eng) { openEnginePick(true); return; } // first dictation, or the stored engine isn't available here
    store.voice = 'recording';
    voiceBase = ui.input.value;
    render();
    if (eng === 'webspeech') { startWebSpeech(); return; }
    capturingSince = 0;
    utterPeak = 0;
    clearTimeout(audioIdleReap); // in use — don't reap underneath the utterance
    startingTalk = (async () => {
      await voiceWS();
      // The graph is suspended between utterances; verify it is actually
      // revivable rather than trusting it. A dead track — or a resume()
      // that hangs (Chromium does this when the audio device changed
      // while suspended) — would otherwise record perfect silence and
      // "transcribe nothing" with no visible failure.
      if (!audioAlive()) {
        teardownAudio();
        audio = await buildAudio();
      }
      await Promise.race([audio.ctx.resume(), new Promise((r) => setTimeout(r, 1500))]);
      if (audio.ctx.state !== 'running') {
        teardownAudio();
        audio = await buildAudio();
        await audio.ctx.resume();
      }
      capturingSince = Date.now();
    })().catch((err) => {
      store.voice = 'idle';
      toast('mic: ' + err.message);
      render();
    });
  };

  const stopTalk = async () => {
    if (store.voice !== 'recording') return;
    store.voice = 'transcribing';
    render();
    if (rec) { rec.stop(); return; } // webspeech: its onend finalizes
    // The first utterance on a page pays the capture bring-up (mic
    // permission, worklet load). Ending before any audio flowed would
    // "transcribe" an empty utterance — wait for the bring-up, then
    // require a beat of real capture.
    await startingTalk;
    if (store.voice !== 'transcribing') return; // bring-up failed; already reset
    if (!capturingSince || Date.now() - capturingSince < 150) {
      if (audio) audio.ctx.suspend();
      if (vws && vws.readyState === WebSocket.OPEN) vws.send(JSON.stringify({ type: 'cancel' }));
      store.voice = 'idle';
      toast('mic wasn’t ready — try again');
      scheduleAudioReap();
      render();
      return;
    }
    if (utterPeak <= 0.0005) {
      // Real mics always have a noise floor; an utterance whose PEAK is
      // digital zero means the capture path is dead (rotted suspend),
      // not a quiet room. Rebuild instead of decoding silence.
      if (vws && vws.readyState === WebSocket.OPEN) vws.send(JSON.stringify({ type: 'cancel' }));
      teardownAudio();
      store.voice = 'idle';
      toast('the mic went quiet — reconnected it, just try again');
      render();
      return;
    }
    if (audio) audio.ctx.suspend();
    if (vws && vws.readyState === WebSocket.OPEN) { vws.send(JSON.stringify({ type: 'end' })); }
    else { store.voice = 'idle'; render(); }
    scheduleAudioReap();
  };

  // talkToggle is the ⇪ binding: tap to start, tap to stop. Toggle (not
  // hold) because macOS never reports Caps Lock release — keydown fires
  // when the lock engages and keyup when it disengages, so "held" is
  // undetectable there; a tap cycle behaves identically everywhere.
  const talkToggle = () => {
    if (store.voice === 'off') return;
    if (store.voice === 'recording') { stopTalk(); return; }
    if (store.voice !== 'idle') return; // transcribing — let it finish
    if (store.box === 'hidden') setBox('prompt');
    startTalk();
  };

  // ---------- UI -----------------------------------------------------------
  // Inline SVG icons (lucide-style strokes, currentColor): emoji render
  // differently per OS font; these are identical everywhere.
  const ICONS = {
    // wand pointing top-left: hollow shaft (rotated rounded rect) with a
    // filled tip drawn as a round-capped stroke ALONG THE SAME CENTERLINE
    // whose width equals the shaft's outer width (2·1.5 + stroke 2 = 5) —
    // wall-to-wall by construction, cap curvature matches the corners.
    select: '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M5.06 2.94 21.06 18.94 18.94 21.06 2.94 5.06Z"/><path d="M4.5 4.5 7.5 7.5" stroke-width="5"/><path d="M19 6v4"/><path d="M5 14v4"/><path d="M14 2v2"/><path d="M17 8h4"/><path d="M3 16h4"/><path d="M13 3h2"/></svg>',
    mic: '<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="2" width="6" height="12" rx="3"/><path d="M5 10v1a7 7 0 0 0 14 0v-1"/><path d="M12 18v4"/></svg>',
    // viewfinder corner brackets — mobile PromptIcons.ScreenCapture
    shot: '<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 7V5a2 2 0 0 1 2-2h2"/><path d="M17 3h2a2 2 0 0 1 2 2v2"/><path d="M21 17v2a2 2 0 0 1-2 2h-2"/><path d="M7 21H5a2 2 0 0 1-2-2v-2"/></svg>',
    close: '<svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round"><path d="M6 6l12 12M18 6 6 18"/></svg>',
    plus: '<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M12 5v14"/><path d="M5 12h14"/></svg>',
    chevron: '<svg width="10" height="10" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><path d="m6 9 6 6 6-6"/></svg>',
    settings: '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M4 7h10M18 7h2M4 17h2M10 17h10M14 4v6M6 14v6"/></svg>',
    send: '<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><path d="M12 19V5"/><path d="m5 12 7-7 7 7"/></svg>',
    stop: '<svg width="14" height="14" viewBox="0 0 24 24" fill="currentColor"><rect x="5" y="5" width="14" height="14" rx="3"/></svg>',
    // 2×3 grid, 5px pitch on BOTH axes: adjacent dots equidistant, so
    // any four neighbors form a square and all six a rectangle
    grip: '<svg width="11" height="16" viewBox="0 0 11 16" fill="currentColor"><circle cx="3" cy="3" r="1.5"/><circle cx="8" cy="3" r="1.5"/><circle cx="3" cy="8" r="1.5"/><circle cx="8" cy="8" r="1.5"/><circle cx="3" cy="13" r="1.5"/><circle cx="8" cy="13" r="1.5"/></svg>',
  };

  const host = document.createElement('div');
  host.id = 'clank-overlay-host';
  host.style.cssText = 'position:fixed;inset:0;z-index:2147483646;pointer-events:none;';
  const root = host.attachShadow({ mode: 'open' });
  root.innerHTML = `
<style>
  :host { all: initial; }
  * { box-sizing: border-box; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; }
  .box {
    /* home: horizontally centered, ~one box-height above the bottom
       (mobile FAB parity). Drag/follow positioning lives on the
       standalone \`translate\` property; the entry animation lives on
       \`transform\`. The split is load-bearing: standalone properties
       compose BEFORE transform, so an animated scale on translate's
       side of the chain would multiply the position offset and make
       the box enter along the center→position vector instead of
       vertically (observed). With translate first and the animation
       last, nothing can scale the offset, and transform-origin
       anchors the entry scale to the box's own bottom edge. */
    position: fixed; left: max(16px, calc(50% - 190px)); bottom: 144px; width: 380px; max-width: calc(100vw - 32px);
    background: rgba(255,255,255,.92); color: #1f2937; border-radius: 18px;
    border: 1.5px solid #e5e7eb; box-shadow: 0 12px 40px rgba(0,0,0,.18);
    pointer-events: auto; backdrop-filter: blur(14px); display: none; overflow: hidden;
    transition: border-color .25s ease;
    transform-origin: 50% 100%;
  }
  .box.visible { display: block; animation: boxIn 300ms cubic-bezier(0.26, 1.15, 0.44, 1); }
  .box:focus { outline: none; } /* focus ANCHOR (tabindex=-1), not a tab stop — no ring */
  /* buttons show a ring on KEYBOARD focus only (:focus-visible) — the
     Tab rotation must be visible; the composer's caret speaks for itself */
  .box button:focus-visible { outline: 2px solid #3b82f6; outline-offset: 2px; }
  @keyframes boxIn { from { opacity: 0; transform: translateY(30px) scale(0.96); } }
  @media (prefers-reduced-motion: reduce) { .box.visible { animation: none; } }
  .box.thinking { border-color: #f59e0b; animation: pulse 1.6s ease-in-out infinite; }
  .box.working  { border-color: #3b82f6; }
  .box.done     { border-color: #22c55e; }
  .box.error    { border-color: #ef4444; }
  @keyframes pulse { 50% { border-color: #f59e0b44; } }
  .hd { display:flex; align-items:center; gap:8px; padding:10px 12px 6px; cursor:grab; user-select:none; }
  .hd:active { cursor:grabbing; }
  .dot { width:8px; height:8px; border-radius:50%; background:#6b7280; flex:none; }
  .thinking .dot { background:#f59e0b; } .working .dot { background:#3b82f6; }
  .done .dot { background:#22c55e; } .error .dot { background:#ef4444; }
  .hd .name { font-size:12px; font-weight:600; color:#6b7280; flex:1; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  .hd .st { font-size:11px; color:#9ca3af; }
  /* supaclank pill recipe verbatim (web-app src/app.css tokens +
     landing-page badge classes): brand #FA5573, text brand-muted,
     bg brand-dim, border brand/30 */
  .beta { font-size:10px; font-weight:600; text-transform:uppercase; letter-spacing:.5px;
    color:#e23e5d; background:rgba(250,85,115,.12); border:1px solid rgba(250,85,115,.3);
    padding:2px 8px; border-radius:999px; line-height:1.4;
    cursor:pointer; text-decoration:none; }
  .beta:hover { background:rgba(250,85,115,.22); }
  .grip { color:#9ca3af; display:flex; align-items:center; }
  .grip svg { display:block; pointer-events:none; }
  .chips { display:flex; flex-wrap:wrap; gap:6px; padding:0 12px 4px; }
  .chip { display:inline-flex; align-items:center; gap:6px; background:#f3f4f6; border:1px solid #e5e7eb;
    color:#4338ca; font-size:11px; padding:3px 8px; border-radius:999px; max-width:100%; }
  .chip.editable { cursor:pointer; }
  .chip b { font-weight:600; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  .chip button { all:unset; cursor:pointer; color:#9ca3af; font-size:12px; line-height:1; }
  .chip img { width:18px; height:18px; object-fit:cover; border-radius:4px; }
  .chat { max-height:240px; overflow-y:auto; padding:4px 12px; display:none; }
  .box.expanded .chat { display:block; }
  .m { font-size:12.5px; line-height:1.45; margin:6px 0; white-space:pre-wrap; word-break:break-word; }
  .m.user { color:#2563eb; }
  .m.assistant { color:#374151; }
  .m .who { font-size:10px; text-transform:uppercase; letter-spacing:.6px; color:#9ca3af; display:block; }
  .perm { margin:6px 12px; padding:8px 10px; border:1px solid #f59e0b66; background:#f59e0b14; border-radius:10px; font-size:12px; }
  .perm .t { font-weight:600; margin-bottom:2px; }
  .perm .d { color:#6b7280; margin-bottom:6px; word-break:break-word; }
  .perm button { all:unset; cursor:pointer; font-size:12px; font-weight:600; padding:4px 12px; border-radius:8px; margin-right:6px; }
  .perm .allow { background:#22c55e1f; color:#16a34a; }
  .perm .deny { background:#ef44441f; color:#dc2626; }
  .perm .plan { max-height:140px; overflow-y:auto; white-space:pre-wrap; word-break:break-word; margin:0 0 6px;
    padding:6px 8px; background:#00000008; border:1px solid #e5e7eb; border-radius:8px;
    font-family:inherit; font-size:11.5px; color:#374151; }
  .perm .notes { min-height:0; max-height:80px; margin:0 0 6px; padding:4px 8px; font-size:12px;
    border:1px solid #e5e7eb; border-radius:8px; background:#ffffffa6; }
  .ques { margin:6px 12px; padding:8px 10px; border:1px solid #3b82f666; background:#3b82f60d; border-radius:10px; font-size:12px; }
  .ques .t { font-weight:600; margin-bottom:2px; }
  .ques .qx { color:#374151; margin-bottom:4px; white-space:pre-wrap; word-break:break-word; }
  .ques .qh { color:#6b7280; font-size:11px; margin-bottom:4px; }
  .ques .opt { all:unset; display:block; width:100%; box-sizing:border-box; cursor:pointer;
    padding:6px 8px; border-radius:8px; border:1px solid transparent; }
  .ques .opt:hover { background:#00000008; }
  .ques .opt.cur { border-color:#3b82f6aa; background:#3b82f614; }
  .ques .opt b { display:block; font-weight:600; }
  .ques .opt .d { color:#6b7280; }
  .ques .qother { width:100%; box-sizing:border-box; margin-top:4px; padding:4px 8px; font-size:12px;
    border:1px solid #e5e7eb; border-radius:8px; background:#ffffffa6; outline:0; color:#111827; }
  .ques .qbar { display:flex; align-items:center; gap:6px; margin-top:8px; }
  .ques .qbar .sp { flex:1; }
  .ques .qb { all:unset; cursor:pointer; font-size:12px; font-weight:600; padding:4px 12px; border-radius:8px; }
  .ques .qnext { background:#3b82f6; color:#fff; }
  .ques .qback { background:#00000010; color:#374151; }
  .ques .qdismiss { color:#9ca3af; padding-left:0; }
  .ques .qb[disabled], .ques .opt[disabled] { opacity:.5; cursor:default; }
  textarea { width:100%; resize:none; background:transparent; border:0; outline:0; color:#111827;
    font-size:13px; line-height:1.4; padding:6px 12px; min-height:34px; max-height:120px; }
  textarea::placeholder { color:#9ca3af; }
  .bar { display:flex; align-items:center; gap:4px; padding:6px 8px 8px; }
  .ib { all:unset; cursor:pointer; width:30px; height:30px; border-radius:9px; display:inline-flex;
    align-items:center; justify-content:center; color:#6b7280; font-size:15px; }
  .ib:hover { background:#00000010; color:#111827; }
  .ib.active { background:#3b82f61f; color:#2563eb; }
  .ib[disabled] { opacity:.35; cursor:not-allowed; }
  .ib svg { pointer-events:none; display:block; }
  .sp { flex:1; }
  .mic { position:relative; }
  .mic.rec { color:#dc2626; background:#ef44441f; }
  .mic.rec::after { content:''; position:absolute; inset:-3px; border-radius:12px;
    border:2px solid rgba(220,38,38, calc(.25 + .75 * var(--lvl, 0))); }
  .eng { width:18px; margin-left:-5px; color:#9ca3af; }
  .engpick { margin:6px 12px; padding:8px 10px; border:1px solid #3b82f666; background:#3b82f60d; border-radius:10px; font-size:12px; }
  .engpick .t { font-weight:600; margin-bottom:4px; }
  .engpick .opt { all:unset; display:block; width:100%; box-sizing:border-box; cursor:pointer;
    padding:6px 8px; border-radius:8px; border:1px solid transparent; }
  .engpick .opt:hover:not([disabled]) { background:#00000008; }
  .engpick .opt.cur { border-color:#3b82f6aa; background:#3b82f614; }
  .engpick .opt[disabled] { opacity:.45; cursor:not-allowed; }
  .engpick .opt b { display:block; font-weight:600; }
  .engpick .opt .d { color:#6b7280; }
  .settings { margin:6px 12px; border:1px solid #e5e7eb; background:rgba(255,255,255,.7);
    border-radius:12px; font-size:12px; max-height:300px; overflow-y:auto; }
  .settings-h { position:sticky; top:0; z-index:1; display:flex; align-items:center; gap:8px;
    padding:9px 10px 7px; background:rgba(255,255,255,.96); border-bottom:1px solid #e5e7eb; }
  .settings-h b { flex:1; font-size:13px; }
  .settings-badge { color:#2563eb; background:#3b82f614; border-radius:999px; padding:2px 7px; }
  .settings-badge.custom { color:#b45309; background:#f59e0b14; }
  .settings-done { all:unset; cursor:pointer; color:#2563eb; font-weight:600; padding:3px 2px; }
  .profiles { display:flex; gap:7px; padding:9px 10px; overflow-x:auto; }
  .profile-card { all:unset; cursor:pointer; flex:none; min-width:66px; border:1px solid #e5e7eb;
    border-radius:10px; padding:6px 10px; background:#f9fafb; }
  .profile-card:hover { background:#f3f4f6; }
  .profile-card.cur { border-color:#3b82f6; background:#3b82f60d; color:#2563eb; }
  .profile-card b, .profile-card small { display:block; white-space:nowrap; }
  .profile-card small { color:#9ca3af; font-size:9px; margin-top:1px; }
  .knobs { padding:0 10px 6px; }
  .knob { border-top:1px solid #e5e7eb; }
  .knob-main { all:unset; width:100%; cursor:pointer; display:flex; align-items:center; gap:8px; padding:9px 0; }
  .knob-main[disabled] { cursor:default; }
  .knob-name { flex:1; font-weight:600; }
  .knob-value { color:#2563eb; text-align:right; }
  .knob-value.agent { color:#6b7280; }
  .knob-values { padding:0 0 6px 6px; }
  .knob-group { color:#9ca3af; font-size:9px; font-weight:600; text-transform:uppercase;
    letter-spacing:.5px; padding:4px 0 2px; }
  .knob-option { all:unset; cursor:pointer; width:100%; display:flex; align-items:center; gap:8px;
    padding:6px 4px; border-radius:7px; }
  .knob-option:hover { background:#00000008; }
  .knob-option .copy { flex:1; }
  .knob-option b, .knob-option small { display:block; }
  .knob-option small { color:#6b7280; font-size:10px; }
  .knob-option.cur { color:#2563eb; }
  .settings-state { color:#6b7280; padding:9px 10px; border-top:1px solid #e5e7eb; }
  .settings-state.err { color:#dc2626; }
  .settings-actions { display:flex; justify-content:flex-end; gap:7px; padding:4px 10px 9px; }
  .set-default { all:unset; cursor:pointer; color:#2563eb; font-weight:600; border:1px solid #e5e7eb;
    border-radius:9px; padding:6px 10px; }
  .save-new { all:unset; cursor:pointer; color:#fff; background:#111827; font-weight:600;
    border-radius:9px; padding:6px 10px; }
  .profile { all:unset; cursor:pointer; height:30px; max-width:125px; padding:0 7px; border-radius:9px;
    display:inline-flex; align-items:center; gap:5px; color:#6b7280; font-size:11px; font-weight:600; }
  .profile:hover, .profile.active { background:#00000010; color:#111827; }
  .profile span { overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  .save-profile { position:fixed; inset:0; display:none; align-items:center; justify-content:center;
    padding:20px; pointer-events:auto; background:rgba(0,0,0,.48); }
  .save-profile.show { display:flex; }
  .save-card { width:320px; max-width:100%; background:#fff; color:#1f2937; border:1px solid #e5e7eb;
    border-radius:14px; box-shadow:0 16px 48px rgba(0,0,0,.25); padding:14px; }
  .save-card .t { font-size:14px; font-weight:650; margin-bottom:10px; }
  .save-card input { width:100%; border:1px solid #d1d5db; border-radius:9px; background:#fff;
    color:#111827; outline:0; padding:8px 10px; font-size:13px; }
  .save-card input:focus { border-color:#3b82f6; box-shadow:0 0 0 2px #3b82f622; }
  .save-actions { display:flex; justify-content:flex-end; gap:7px; margin-top:11px; }
  .save-actions button { all:unset; cursor:pointer; font-size:12px; font-weight:600; padding:6px 10px; border-radius:8px; }
  .save-actions .cancel { color:#6b7280; }
  .save-actions .confirm { color:#fff; background:#111827; }
  .save-actions button[disabled] { opacity:.4; cursor:not-allowed; }
  .save-actions button:focus-visible { outline:2px solid #3b82f6; outline-offset:2px; }
  .send { background:#111827; color:#fff; font-weight:700; }
  .send:hover { background:#000; }
  .send.stop { background:#ef4444; color:#fff; }
  .hint { font-size:10px; color:#9ca3af; display:flex; justify-content:center; gap:14px; padding:0 8px 7px; flex-wrap:wrap; }
  .hint span { white-space:nowrap; }
  kbd { font-family:inherit; background:#f3f4f6; border:1px solid #e5e7eb; padding:0 4px; border-radius:4px; color:#6b7280; }
  .hl { position:fixed; pointer-events:none; border:1.5px solid #60a5fa; background:#3b82f61f;
    border-radius:3px; display:none; }
  .hll { position:fixed; pointer-events:none; background:#111318; color:#c7d2fe; font-size:11px;
    padding:2px 7px; border-radius:6px; border:1px solid #3a3b42; display:none; white-space:nowrap; }
  /* inline comment popover — appears at a text selection (or a
     ⌘-selected element) so the instruction lands where you look */
  .cpop { position:fixed; display:none; width:300px; max-width:calc(100vw - 16px); pointer-events:auto;
    background:rgba(255,255,255,.96); border:1.5px solid #e5e7eb; border-radius:12px;
    box-shadow:0 10px 36px rgba(0,0,0,.2); padding:8px 10px 6px; backdrop-filter:blur(14px); }
  .cpop.show { display:block; }
  /* overrides of the shared textarea rule: no inner padding (the popover
     pads), one-row start, grows with the comment like the composer */
  .cpop textarea { font-size:12.5px; padding:0; min-height:18px; max-height:96px; display:block; }
  .cpop .cpop-h { font-size:10px; color:#9ca3af; margin-top:4px; }
  .chip.cmt { background:#fffbeb; border-color:#f59e0bb3; color:#92400e; }
  .chip .ctext { font-weight:400; color:#6b7280; overflow:hidden; text-overflow:ellipsis;
    white-space:nowrap; max-width:150px; }
  .toast { position:fixed; bottom:16px; left:50%; transform:translateX(-50%); background:#111318;
    color:#e8e8ec; border:1px solid #3a3b42; font-size:12px; padding:7px 14px; border-radius:10px;
    pointer-events:none; opacity:0; transition:opacity .2s; max-width:70vw; }
  .toast.show { opacity:1; }
  /* screenshot area grab — clank-mobile ScreenshotCropOverlay theme:
     55% black scrim, 2px dashed #FA5573 outline, pink rounded corner
     handles, white ✕ + pink "Add to context" pill */
  .crop { position:fixed; inset:0; display:none; pointer-events:auto; cursor:crosshair;
    touch-action:none; user-select:none; }
  .crop.on { display:block; }
  .crop canvas { position:absolute; inset:0; width:100%; height:100%; }
  .crop-dim { position:absolute; inset:0; background:rgba(0,0,0,.55); }
  .crop-sel { position:absolute; border:2px dashed #FA5573; box-shadow:0 0 0 100vmax rgba(0,0,0,.55); }
  .crop-sel i { position:absolute; width:10px; height:10px; background:#FA5573; border-radius:2px; }
  .crop-sel .tl { left:-7px; top:-7px; } .crop-sel .tr { right:-7px; top:-7px; }
  .crop-sel .bl { left:-7px; bottom:-7px; } .crop-sel .br { right:-7px; bottom:-7px; }
  .crop-bar { position:absolute; display:none; align-items:center; gap:6px; }
  .crop-x { all:unset; cursor:pointer; width:32px; height:32px; border-radius:50%; background:#fff;
    color:#1f1f23; display:inline-flex; align-items:center; justify-content:center;
    box-shadow:0 2px 8px rgba(0,0,0,.35); }
  .crop-add { all:unset; cursor:pointer; background:#FA5573; color:#fff; font-size:13px; font-weight:500;
    padding:7px 12px; border-radius:999px; box-shadow:0 3px 12px rgba(0,0,0,.35); white-space:nowrap; }
  .crop-x0 { position:absolute; top:12px; right:16px; }
</style>
<div class="box" part="box" tabindex="-1">
  <div class="hd"><span class="dot"></span><span class="name"></span><span class="st"></span><a class="beta" href="https://github.com/supaclank/clank/issues/new?template=bug_report.yml" target="_blank" rel="noopener noreferrer" title="click to report an issue" tabindex="-1">beta</a><span class="grip">${ICONS.grip}</span></div>
  <div class="chips"></div>
  <div class="chat"></div>
  <div class="perm" style="display:none">
    <div class="t"></div><div class="d"></div>
    <pre class="plan" style="display:none"></pre>
    <textarea class="notes" rows="2" placeholder="Revision notes — sent with Request changes" style="display:none"></textarea>
    <button class="allow">Allow</button><button class="deny">Deny</button>
  </div>
  <div class="ques" style="display:none"></div>
  <div class="engpick" style="display:none">
    <div class="t">How should dictation transcribe your voice?</div>
    <button class="opt" data-eng="local"><b>Fully local</b><span class="d"></span></button>
    <button class="opt" data-eng="webspeech"><b>Web Speech API</b><span class="d"></span></button>
  </div>
  <div class="settings" style="display:none"></div>
  <textarea class="compose" rows="1" placeholder="Ask anything…"></textarea>
  <div class="bar">
    <button class="ib att" title="Attach images (or paste into the box)">${ICONS.plus}</button>
    <button class="ib shot" title="Grab a screenshot area">${ICONS.shot}</button>
    <button class="ib sel" title="Select an element (hold ⌘)">${ICONS.select}</button>
    <span class="sp"></span>
    <button class="profile" title="Agent settings">${ICONS.settings}<span>Settings</span></button>
    <button class="ib mic" title="Tap ⇪ to talk (or hold this button)">${ICONS.mic}</button>
    <button class="ib eng" title="Dictation engine">${ICONS.chevron}</button>
    <span class="micLevel" style="display:none"></span>
    <button class="ib send" title="Send (Enter)">${ICONS.send}</button>
  </div>
  <input type="file" class="file" multiple style="display:none">
  <div class="hint"><span><kbd>⇪ caps</kbd> talk</span><span><kbd>⇧</kbd> move</span><span><kbd>⌘</kbd> select</span><span><kbd>⌘E</kbd> toggle</span></div>
</div>
<div class="save-profile">
  <div class="save-card" role="dialog" aria-modal="true" aria-labelledby="clank-save-profile-title">
    <div class="t" id="clank-save-profile-title">Save as new profile</div>
    <input class="save-profile-name" type="text" placeholder="Profile name" autocomplete="off">
    <div class="save-actions"><button class="cancel">Cancel</button><button class="confirm" disabled>Save</button></div>
  </div>
</div>
<div class="hl"></div><div class="hll"></div>
<div class="cpop"><textarea class="cpop-in" rows="1"></textarea><div class="cpop-h"><kbd>Enter</kbd> add · <kbd>Esc</kbd> dismiss</div></div>
<div class="crop">
  <div class="crop-dim"></div>
  <div class="crop-sel"><i class="tl"></i><i class="tr"></i><i class="bl"></i><i class="br"></i></div>
  <div class="crop-bar"><button class="crop-x" title="Cancel">${ICONS.close}</button><button class="crop-add">Add to context</button></div>
  <button class="crop-x crop-x0" title="Cancel (Esc)">${ICONS.close}</button>
</div>
<div class="toast"></div>`;

  const $ = (sel) => root.querySelector(sel);
  const ui = {
    box: $('.box'), name: $('.name'), st: $('.st'), chips: $('.chips'), chat: $('.chat'),
    perm: $('.perm'), permT: $('.perm .t'), permD: $('.perm .d'),
    plan: $('.perm .plan'), notes: $('.perm .notes'),
    permAllow: $('.perm .allow'), permDeny: $('.perm .deny'), ques: $('.ques'),
    input: $('.compose'), sel: $('.sel'), mic: $('.mic'), micLevel: $('.micLevel'),
    eng: $('.eng'), engpick: $('.engpick'), engOpts: [...root.querySelectorAll('.engpick .opt')],
    settings: $('.settings'), profile: $('.profile'), profileLabel: $('.profile span'),
    saveProfile: $('.save-profile'), saveProfileName: $('.save-profile-name'),
    saveProfileCancel: $('.save-actions .cancel'), saveProfileConfirm: $('.save-actions .confirm'),
    send: $('.send'), hl: $('.hl'), hll: $('.hll'), toast: $('.toast'),
    cpop: $('.cpop'), cpopIn: $('.cpop-in'), cpopHint: $('.cpop-h'),
    shot: $('.shot'), att: $('.att'), file: $('.file'),
    crop: $('.crop'), cropDim: $('.crop-dim'), cropSel: $('.crop-sel'),
    cropBar: $('.crop-bar'), cropAdd: $('.crop-add'), cropX: $('.crop-bar .crop-x'), cropX0: $('.crop-x0'),
  };
  ui.name.textContent = CFG.name || 'clank';
  // Tab capture is the capture primitive; without it there is no
  // screenshot feature (same pattern as voice: 'off').
  if (!(navigator.mediaDevices && navigator.mediaDevices.getDisplayMedia)) ui.shot.style.display = 'none';

  let toastTimer = 0;
  const toast = (msg) => {
    ui.toast.textContent = msg;
    ui.toast.classList.add('show');
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => ui.toast.classList.remove('show'), 3500);
  };

  const syncComposerHeight = () => {
    ui.input.style.height = 'auto';
    ui.input.style.height = Math.min(120, ui.input.scrollHeight) + 'px';
  };
  // setComposer is for programmatic text (transcripts, restores): unlike
  // user typing it moves the caret to the end and keeps the tail of a
  // long prompt in view — the browser only auto-scrolls for real keys.
  const setComposer = (text) => {
    ui.input.value = text;
    ui.input.setSelectionRange(text.length, text.length);
    syncComposerHeight();
    ui.input.scrollTop = ui.input.scrollHeight;
  };

  // renderQuestion rebuilds the interactive question card [QST-001]
  // from store.question: one question at a time (Back/Next across a
  // multi-question prompt), clickable options, a free-text "Other"
  // input when allowed, Dismiss = reject. Rebuilt on every render;
  // focus/caret in the Other input survives the rebuild.
  const renderQuestion = () => {
    const q = store.question;
    ui.ques.style.display = q ? '' : 'none';
    if (!q) { ui.ques.replaceChildren(); return; }
    const prevOther = ui.ques.querySelector('.qother');
    const otherFocused = prevOther && root.activeElement === prevOther;
    const caret = otherFocused ? prevOther.selectionStart : 0;

    const question = q.questions[q.idx];
    const last = q.idx === q.questions.length - 1;
    const nextAction = () => {
      if (q.sending) return;
      if (last) replyQuestion(false);
      else { q.idx++; render(); }
    };
    const el = (tag, cls, text) => {
      const n = document.createElement(tag);
      n.className = cls;
      if (text) n.textContent = text;
      return n;
    };

    const frag = document.createDocumentFragment();
    const count = q.questions.length > 1 ? `  (${q.idx + 1}/${q.questions.length})` : '';
    frag.append(el('div', 't', '? ' + (question.header || 'Question') + count));
    frag.append(el('div', 'qx', question.text || ''));
    if (question.multi_select) frag.append(el('div', 'qh', 'select all that apply'));
    (question.options || []).forEach((opt, i) => {
      const b = el('button', 'opt' + (q.sel[q.idx].has(i) ? ' cur' : ''));
      b.disabled = q.sending;
      b.append(el('b', '', opt.label));
      if (opt.description) b.append(el('span', 'd', opt.description));
      b.onclick = () => {
        if (q.sending) return;
        q.sel[q.idx] = toggleSelection(question, q.sel[q.idx], i);
        // Single-select: picking an answer advances to the next question
        // (never auto-submits on the last one).
        if (!question.multi_select && q.sel[q.idx].has(i) && !last) q.idx++;
        render();
      };
      frag.append(b);
    });
    if (customAllowed(question)) {
      const o = el('input', 'qother');
      o.placeholder = 'Other — type your own answer';
      o.value = q.custom[q.idx];
      o.disabled = q.sending;
      o.addEventListener('input', () => { q.custom[q.idx] = o.value; });
      o.addEventListener('keydown', (e) => {
        e.stopPropagation(); // typing must never trigger guest-app or overlay shortcuts
        if (e.key === 'Enter') { e.preventDefault(); nextAction(); }
      });
      frag.append(o);
    }
    const bar = el('div', 'qbar');
    const dismiss = el('button', 'qb qdismiss', 'Dismiss');
    dismiss.disabled = q.sending;
    dismiss.onclick = () => replyQuestion(true);
    bar.append(dismiss, el('span', 'sp'));
    if (q.idx > 0) {
      const back = el('button', 'qb qback', 'Back');
      back.disabled = q.sending;
      back.onclick = () => { if (!q.sending) { q.idx--; render(); } };
      bar.append(back);
    }
    const next = el('button', 'qb qnext', q.sending ? 'Sending…' : last ? 'Answer' : 'Next');
    next.disabled = q.sending;
    next.onclick = nextAction;
    bar.append(next);
    frag.append(bar);

    ui.ques.replaceChildren(frag);
    if (otherFocused) {
      const o = ui.ques.querySelector('.qother');
      if (o) { o.focus(); o.setSelectionRange(caret, caret); }
    }
  };

  const backendLabel = (backend) => ({
    'claude-code': 'Claude Code',
    codex: 'Codex',
    opencode: 'OpenCode',
  })[backend] || backend;

  const settingsChipLabel = () => {
    if (!store.sessionId) {
      return profileLabel(selectedCreateProfile(), store.profileOverrides) || 'Settings';
    }
    const mode = (store.configOptions || []).find((option) =>
      option.category === CONFIG_OPTION_MODE || option.id === CONFIG_OPTION_MODE);
    if (!mode) return Object.keys(store.pendingConfig).length ? 'Settings •' : 'Settings';
    const value = Object.hasOwn(store.pendingConfig, mode.id)
      ? store.pendingConfig[mode.id]
      : mode.current_value;
    const valueName = ((mode.values || []).find((v) => v.value === value) || {}).name || value || 'Settings';
    return valueName + (Object.keys(store.pendingConfig).length ? ' •' : '');
  };

  // renderSettings builds the same two-level editor as mobile's
  // PresetEditorSheet: profile cards first, then expandable agent-owned
  // knobs. All text is assigned through textContent — profile names and
  // adapter descriptions never become markup in the injected page.
  const renderSettings = () => {
    const scrollTop = ui.settings.scrollTop;
    ui.settings.style.display = store.settingsOpen ? '' : 'none';
    if (!store.settingsOpen) {
      ui.settings.replaceChildren();
      return;
    }
    const live = !!store.sessionId;
    const preset = live ? null : selectedCreateProfile();
    const rows = configRows(preset, live ? store.pendingConfig : store.profileOverrides, store.configOptions || []);
    const custom = !live && profileLabel(preset, store.profileOverrides) === 'Custom';
    const badgeText = live
      ? (Object.keys(store.pendingConfig).length ? 'Modified' : '')
      : profileLabel(preset, store.profileOverrides);
    const resolvedDefault = resolvePreset(store.profiles, CFG.backend, store.defaultProfileID);
    const node = (tag, cls, text) => {
      const n = document.createElement(tag);
      n.className = cls;
      if (text !== undefined) n.textContent = text;
      return n;
    };

    const frag = document.createDocumentFragment();
    const header = node('div', 'settings-h');
    header.append(node('b', '', live ? 'Session settings' : `${backendLabel(CFG.backend)} settings`));
    if (badgeText) header.append(node('span', 'settings-badge' + (custom || live ? ' custom' : ''), badgeText));
    const done = node('button', 'settings-done', 'Done');
    done.onclick = closeSettings;
    header.append(done);
    frag.append(header);

    if (store.profiles.length) {
      const profiles = node('div', 'profiles');
      for (const p of store.profiles) {
        const selected = !live && p.id === (preset && preset.id) && !custom;
        const card = node('button', 'profile-card' + (selected ? ' cur' : ''));
        card.append(node('b', '', p.name));
        if (resolvedDefault && p.id === resolvedDefault.id) card.append(node('small', '', 'default'));
        card.onclick = () => {
          store.expandedConfigID = '';
          store.profileID = p.id;
          if (live) store.pendingConfig = diffConfigAgainstOptions({ ...p.config }, store.configOptions);
          else store.profileOverrides = {};
          render();
        };
        profiles.append(card);
      }
      frag.append(profiles);
    }

    if (rows.length) {
      const knobs = node('div', 'knobs');
      for (const row of rows) {
        const wrap = node('div', 'knob');
        const main = node('button', 'knob-main');
        main.disabled = !row.values.length;
        main.append(node('span', 'knob-name', row.name));
        main.append(node('span', 'knob-value' + (row.source === 'agent' ? ' agent' : ''), row.valueName));
        if (row.values.length) {
          main.append(node('span', '', store.expandedConfigID === row.id ? '▴' : '▾'));
          main.onclick = () => {
            store.expandedConfigID = store.expandedConfigID === row.id ? '' : row.id;
            render();
          };
        }
        wrap.append(main);
        if (store.expandedConfigID === row.id) {
          const values = node('div', 'knob-values');
          const groups = new Map();
          for (const value of row.values) {
            const group = value.group || '';
            if (!groups.has(group)) groups.set(group, []);
            groups.get(group).push(value);
          }
          for (const [group, options] of groups) {
            if (group) values.append(node('div', 'knob-group', group));
            for (const value of options) {
              const option = node('button', 'knob-option' + (value.value === row.value ? ' cur' : ''));
              const copy = node('span', 'copy');
              copy.append(node('b', '', value.name || value.value));
              if (value.description) copy.append(node('small', '', value.description));
              option.append(copy);
              if (value.value === row.value) option.append(node('span', '', '✓'));
              option.onclick = () => {
                if (live) {
                  store.pendingConfig = diffConfigAgainstOptions(
                    { ...store.pendingConfig, [row.id]: value.value },
                    store.configOptions,
                  );
                  store.profileID = '';
                } else {
                  store.profileOverrides = setConfigOverride(
                    preset,
                    store.profileOverrides,
                    row.id,
                    value.value,
                  );
                }
                store.expandedConfigID = '';
                render();
              };
              values.append(option);
            }
          }
          wrap.append(values);
        }
        knobs.append(wrap);
      }
      frag.append(knobs);
    }

    if (store.profilesLoading || store.settingsLoading) {
      frag.append(node('div', 'settings-state', 'Loading agent settings…'));
    } else if (store.profilesError || store.configOptionsError) {
      frag.append(node('div', 'settings-state err', store.profilesError || store.configOptionsError));
    } else if (!store.profiles.length && !rows.length) {
      frag.append(node('div', 'settings-state', 'No agent settings are available.'));
    }

    const canSaveAsNew = live ? Object.keys(store.pendingConfig).length > 0 : custom;
    const canSetDefault = !live && !custom && preset &&
      (!resolvedDefault || preset.id !== resolvedDefault.id);
    if (canSaveAsNew || canSetDefault) {
      const actions = node('div', 'settings-actions');
      if (canSetDefault) {
        const makeDefault = node('button', 'set-default', 'Set as default');
        makeDefault.onclick = () => {
          try {
            writeDefaultProfileID(CFG.backend, preset.id);
            store.defaultProfileID = preset.id;
            render();
          } catch (err) {
            toast('could not save the default profile: ' + err.message);
          }
        };
        actions.append(makeDefault);
      }
      if (canSaveAsNew) {
        const saveNew = node('button', 'save-new', 'Save as new profile');
        saveNew.onclick = openSaveProfile;
        actions.append(saveNew);
      }
      frag.append(actions);
    }
    ui.settings.replaceChildren(frag);
    ui.settings.scrollTop = scrollTop;
  };

  const STATUS_TEXT = { idle: '', thinking: 'thinking…', working: 'working…', done: 'done', error: 'error' };
  const render = () => {
    ui.box.classList.toggle('visible', store.box !== 'hidden');
    ui.box.classList.toggle('expanded', store.box === 'chat');
    for (const s of ['thinking', 'working', 'done', 'error']) ui.box.classList.toggle(s, store.agent === s);
    ui.st.textContent = store.aborting ? 'stopping…' : STATUS_TEXT[store.agent] || '';

    ui.chips.innerHTML = '';
    store.chips.forEach((c, i) => {
      const el = document.createElement('span');
      el.className = 'chip editable' + (c.comment ? ' cmt' : '');
      el.title = c.detail + (c.comment ? ' — ' + c.comment : '') + ' (click to edit the comment)';
      el.onclick = () => editChipComment(c, el.getBoundingClientRect());
      const b = document.createElement('b');
      b.textContent = c.label;
      el.append(b);
      if (c.comment) {
        const s = document.createElement('span');
        s.className = 'ctext';
        s.textContent = c.comment;
        el.append(s);
      }
      const x = document.createElement('button');
      x.textContent = '✕';
      x.onclick = (e) => { e.stopPropagation(); store.chips.splice(i, 1); render(); };
      el.append(x);
      ui.chips.appendChild(el);
    });
    syncCommentMarks();
    store.images.forEach((s, i) => {
      const el = document.createElement('span');
      el.className = 'chip';
      el.title = `${s.label} ${s.w}×${s.h}`;
      const img = document.createElement('img');
      img.src = s.dataURL;
      const b = document.createElement('b');
      b.textContent = s.label;
      const x = document.createElement('button');
      x.textContent = '✕';
      x.onclick = () => { store.images.splice(i, 1); render(); };
      el.append(img, b, x);
      ui.chips.appendChild(el);
    });

    const frag = document.createDocumentFragment();
    const rows = [...store.msgs];
    if (store.streamText) rows.push({ role: 'assistant', text: store.streamText });
    rows.slice(-8).forEach((m) => {
      const el = document.createElement('div');
      el.className = 'm ' + m.role;
      const who = document.createElement('span');
      who.className = 'who';
      who.textContent = m.role === 'user' ? 'you' : 'clank';
      el.append(who, document.createTextNode(m.text));
      frag.appendChild(el);
    });
    ui.chat.replaceChildren(frag);
    ui.chat.scrollTop = ui.chat.scrollHeight;

    const perm = store.perms[0] || null;
    ui.perm.style.display = perm ? '' : 'none';
    if (perm) {
      const isPlan = perm.tool === PLAN_TOOL;
      const queued = store.perms.length > 1 ? `  (+${store.perms.length - 1} queued)` : '';
      const plan = isPlan ? planTextFor(store.planParts, perm) : '';
      ui.permT.textContent = (isPlan ? 'Review the plan' : `Allow ${perm.tool || 'tool'}?`) + queued;
      // The plan block replaces the description (which is just the raw
      // input echo for ExitPlanMode); keep the description as fallback
      // when no plan text could be located.
      ui.permD.textContent = plan ? '' : perm.description || '';
      ui.permD.style.display = ui.permD.textContent ? '' : 'none';
      ui.plan.style.display = plan ? '' : 'none';
      ui.plan.textContent = plan;
      ui.notes.style.display = isPlan ? '' : 'none';
      ui.permAllow.textContent = isPlan ? 'Approve' : 'Allow';
      ui.permDeny.textContent = isPlan && ui.notes.value.trim() ? 'Request changes' : 'Deny';
      ui.permAllow.disabled = ui.permDeny.disabled = !!perm.sending;
    }
    renderQuestion();
    renderSettings();
    ui.saveProfile.classList.toggle('show', store.saveProfileOpen);
    ui.saveProfileCancel.disabled = store.profileSaving;
    ui.saveProfileConfirm.disabled = store.profileSaving || !store.saveProfileName.trim();
    ui.saveProfileConfirm.textContent = store.profileSaving ? 'Saving…' : 'Save';

    ui.sel.classList.toggle('active', store.inspect);
    ui.profile.style.display = CFG.backend ? '' : 'none';
    ui.profile.classList.toggle('active', store.settingsOpen);
    ui.profileLabel.textContent = settingsChipLabel();
    ui.mic.style.display = store.voice === 'off' ? 'none' : '';
    ui.mic.classList.toggle('rec', store.voice === 'recording');
    ui.mic.innerHTML = store.voice === 'transcribing' ? '…' : ICONS.mic;
    const eng = usableEngine();
    ui.mic.title = 'Tap ⇪ to talk (or hold this button)' + (eng ? ' — ' + ENGINE_LABEL[eng] : '');
    ui.eng.style.display = store.voice === 'off' ? 'none' : '';
    ui.eng.title = 'Dictation engine' + (eng ? ': ' + ENGINE_LABEL[eng] : '');
    ui.eng.classList.toggle('active', store.enginePick);
    ui.engpick.style.display = store.enginePick ? '' : 'none';
    ui.engOpts.forEach((b) => b.classList.toggle('cur', b.dataset.eng === store.engine));

    const busy = store.agent === 'thinking' || store.agent === 'working';
    ui.send.classList.toggle('stop', busy);
    ui.send.innerHTML = busy ? ICONS.stop : ICONS.send;
    ui.send.title = busy ? 'Stop the agent' : 'Send (Enter)';
  };

  // ---------- box visibility (mobile shake state machine, hotkey-driven) --
  const setBox = (s) => {
    store.box = s;
    if (s === 'hidden') {
      exitInspect();
      enginePickPending = false;
      store.enginePick = false;
      store.settingsOpen = false;
      store.expandedConfigID = '';
      store.saveProfileOpen = false;
      store.saveProfileName = '';
    }
    render();
    // Focus the CONTAINER, not the composer: typing focus on summon
    // fights shift-move (editable targets opt out of it) and isn't
    // wanted anyway. Anchoring focus here parks the tab cursor just
    // before the box's contents, so Tab #1 = composer, then the
    // buttons, then onward into the page — sequential navigation
    // always continues from the focused element.
    if (s !== 'hidden') setTimeout(() => ui.box.focus({ preventScroll: true }), 0);
  };

  // ---------- inspector -----------------------------------------------------
  let hoverEl = null;
  const enterInspect = () => {
    if (store.inspect) return;
    store.inspect = true;
    if (store.box === 'hidden') store.box = 'prompt';
    document.addEventListener('mousemove', onInspectMove, true);
    document.addEventListener('click', onInspectClick, true);
    document.addEventListener('mousedown', squelch, true);
    document.addEventListener('mouseup', squelch, true);
    document.body.style.cursor = 'crosshair';
    render();
  };
  const exitInspect = () => {
    if (!store.inspect) return;
    store.inspect = false;
    hoverEl = null;
    ui.hl.style.display = 'none';
    ui.hll.style.display = 'none';
    document.removeEventListener('mousemove', onInspectMove, true);
    document.removeEventListener('click', onInspectClick, true);
    document.removeEventListener('mousedown', squelch, true);
    document.removeEventListener('mouseup', squelch, true);
    document.body.style.cursor = '';
    render();
  };
  const ours = (t) => t === host || (t && t.getRootNode && t.getRootNode() === root);
  const squelch = (e) => { if (!ours(e.target)) { e.preventDefault(); e.stopPropagation(); } };
  const onInspectMove = (e) => {
    if (ours(e.target)) { ui.hl.style.display = 'none'; ui.hll.style.display = 'none'; hoverEl = null; return; }
    const el = e.target;
    if (!(el instanceof Element)) return;
    hoverEl = el;
    const r = el.getBoundingClientRect();
    Object.assign(ui.hl.style, { display: 'block', left: r.left + 'px', top: r.top + 'px', width: r.width + 'px', height: r.height + 'px' });
    const s = resolveSource(el);
    ui.hll.textContent = s.file ? `${s.file}:${s.line}${s.approx ? '…' : ''}` : s.names.length ? s.names.join(' › ') : domPath(el);
    if (s.resolve) {
      const target = el;
      s.resolve.then((orig) => {
        if (orig && hoverEl === target && store.inspect) ui.hll.textContent = `${orig.file}:${orig.line}`;
      });
    }
    const ly = r.top > 28 ? r.top - 24 : r.bottom + 4;
    Object.assign(ui.hll.style, { display: 'block', left: Math.max(4, r.left) + 'px', top: ly + 'px' });
  };
  const onInspectClick = (e) => {
    if (ours(e.target)) return;
    e.preventDefault();
    e.stopPropagation();
    if (hoverEl) {
      // One chip per element: re-clicking an attached element edits its
      // comment instead of duplicating the chip.
      const existing = store.chips.find((c) => c.node === hoverEl);
      if (existing) {
        toast('already attached — edit its comment');
        editChipComment(existing, hoverEl.getBoundingClientRect());
      } else {
        const chip = chipFromElement(hoverEl);
        chip.node = hoverEl;
        store.chips.push(chip);
        toast('added to context');
        // The chip is attached either way (today's behavior); the popover
        // just offers to pin an inline comment on it. The range keeps the
        // element's text visibly marked after ⌘-release hides the inspect
        // box.
        const range = document.createRange();
        range.selectNodeContents(hoverEl);
        showCommentPopover({ kind: 'element', chip, range }, hoverEl.getBoundingClientRect());
      }
    }
    // Stay in select mode: it ends when the held modifier is released
    // (momentary), or via Esc / the ⌖ button (toggled).
    render();
  };

  // ---------- inline comments -----------------------------------------------
  // Highlight text (or ⌘-select an element) → a comment input appears at
  // the anchor → Enter pins the instruction to it as a comment chip.
  // Several comments then ride ONE submit, each unambiguously anchored —
  // no untangling "which part did you mean" in the composer.
  let commentTarget = null; // {kind:'text', text, range} | {kind:'element', chip}

  // Commented ranges get an in-page mark via the CSS Custom Highlight
  // API — no guest-DOM mutation, so framework hydration can't break.
  // Marks are cosmetic: chips survive a live-reload swap, marks don't.
  // pendingMark repaints the anchor while the popover is up — focusing
  // the popover input deactivates the browser's own selection highlight,
  // which otherwise vanishes right as you start typing.
  const commentMarks = typeof Highlight === 'function' && CSS.highlights ? new Highlight() : null;
  if (commentMarks) CSS.highlights.set('clank-comment', commentMarks);
  const pendingMark = typeof Highlight === 'function' && CSS.highlights ? new Highlight() : null;
  if (pendingMark) CSS.highlights.set('clank-pending', pendingMark);
  let markStyleInjected = false;
  const ensureMarkStyle = () => {
    if (markStyleInjected) return;
    markStyleInjected = true;
    const st = document.createElement('style');
    st.textContent = '::highlight(clank-comment){background:rgba(245,158,11,.35);}' +
      '::highlight(clank-pending){background:var(--clank-pending-bg, rgba(59,130,246,.30));}';
    document.head.appendChild(st);
  };

  // pendingColorFor adopts the page's own ::selection background for the
  // pending mark, so the repaint reads as "your selection is still here"
  // rather than a foreign color. Empty = unstyled, use the fallback.
  const pendingColorFor = (range) => {
    let el = range && range.startContainer;
    if (el && el.nodeType !== 1) el = el.parentElement;
    if (!el) return '';
    try {
      const c = getComputedStyle(el, '::selection').backgroundColor;
      if (c && c !== 'transparent' && !/^rgba\(\d+, \d+, \d+, 0\)$/.test(c)) return c;
    } catch { /* pseudo-element lookup unsupported */ }
    return '';
  };
  const syncCommentMarks = () => {
    if (!commentMarks) return;
    commentMarks.clear();
    // A hidden box hides its marks too: ⌘E away = a clean page.
    if (store.box === 'hidden') return;
    for (const c of store.chips) {
      if (!c.range) continue;
      ensureMarkStyle();
      commentMarks.add(c.range);
    }
  };

  // SELECTION_TEXT_CAP bounds the context block; the agent greps the
  // source for anything past it.
  const SELECTION_TEXT_CAP = 2000;
  const chipFromSelection = (text) => {
    const oneLine = text.replace(/\s+/g, ' ').trim();
    const label = '“' + (oneLine.length > 24 ? oneLine.slice(0, 24) + '…' : oneLine) + '”';
    const clipped = text.length > SELECTION_TEXT_CAP ? text.slice(0, SELECTION_TEXT_CAP) + '…' : text;
    return { label, detail: 'text selection on ' + location.pathname, text: clipped };
  };

  const syncCpopHeight = () => {
    ui.cpopIn.style.height = 'auto';
    ui.cpopIn.style.height = Math.min(96, ui.cpopIn.scrollHeight) + 'px';
  };

  const positionCommentPopover = (rect) => {
    const w = ui.cpop.offsetWidth || 300;
    const left = Math.min(Math.max(rect.left, 8), Math.max(8, innerWidth - w - 8));
    let top = rect.bottom + 8;
    if (top > innerHeight - 72) top = rect.top - 64;
    top = Math.min(Math.max(top, 8), innerHeight - 72);
    Object.assign(ui.cpop.style, { left: left + 'px', top: top + 'px' });
  };

  const showCommentPopover = (target, rect) => {
    commentTarget = target;
    if (pendingMark) {
      pendingMark.clear();
      if (target.range) {
        ensureMarkStyle();
        pendingMark.add(target.range);
        const c = pendingColorFor(target.range);
        if (c) document.documentElement.style.setProperty('--clank-pending-bg', c);
        else document.documentElement.style.removeProperty('--clank-pending-bg');
      }
    }
    ui.cpopIn.value = target.kind === 'edit' ? target.chip.comment || '' : '';
    ui.cpopIn.placeholder = target.kind === 'text' ? 'Comment for the agent…'
      : target.kind === 'edit' ? 'Comment…'
      : 'Comment on this element…';
    ui.cpopHint.innerHTML = target.kind === 'edit'
      ? '<kbd>Enter</kbd> save · <kbd>Esc</kbd> cancel'
      : target.kind === 'text' ? '<kbd>Enter</kbd> add · <kbd>Esc</kbd> dismiss'
      : '<kbd>Enter</kbd> add comment · <kbd>Esc</kbd> keep as plain context';
    ui.cpop.classList.add('show');
    ui.cpop.style.visibility = '';
    syncCpopHeight();
    positionCommentPopover(rect);
    setTimeout(() => {
      ui.cpopIn.focus({ preventScroll: true });
      const n = ui.cpopIn.value.length; // caret at the end for prefilled edits
      ui.cpopIn.setSelectionRange(n, n);
    }, 0);
  };
  const hideCommentPopover = () => {
    if (!commentTarget) return;
    commentTarget = null;
    if (pendingMark) pendingMark.clear();
    ui.cpop.classList.remove('show');
    ui.cpop.style.visibility = '';
  };
  const confirmComment = () => {
    const t = commentTarget;
    if (!t) return;
    const comment = ui.cpopIn.value.trim();
    if (t.kind === 'text') {
      const chip = chipFromSelection(t.text);
      if (comment) chip.comment = comment;
      if (t.range) chip.range = t.range;
      store.chips.push(chip);
      toast(comment ? 'comment added' : 'added to context');
    } else if (t.kind === 'edit') {
      // Saving empty clears the comment; the chip stays plain context.
      if (comment) t.chip.comment = comment;
      else delete t.chip.comment;
      toast(comment ? 'comment updated' : 'comment cleared');
    } else if (comment) {
      t.chip.comment = comment;
      toast('comment added');
    }
    hideCommentPopover();
    if (store.box === 'hidden') store.box = 'prompt';
    render();
  };

  // editChipComment reopens the popover on an existing chip, prefilled.
  // The pending mark re-paints the chip's anchor when it's still live
  // (its stored selection range, or the ⌘-selected element's contents).
  const editChipComment = (c, rect) => {
    let range = c.range || null;
    if (!range && c.node && c.node.isConnected) {
      range = document.createRange();
      range.selectNodeContents(c.node);
    }
    showCommentPopover({ kind: 'edit', chip: c, range }, rect);
  };

  const onSelectionMouseUp = (e) => {
    if (store.inspect || store.crop) return;
    if (ours(realTarget(e))) return;
    // Defer past the browser's own mouseup handling: double-click
    // word-selection only lands after the event.
    setTimeout(() => {
      if (store.inspect || store.crop) return;
      const sel = document.getSelection();
      if (!sel || sel.isCollapsed || !sel.rangeCount) return;
      const n = sel.anchorNode;
      if (n && n.getRootNode && n.getRootNode() === root) return; // composer selections are ours
      const text = sel.toString();
      if (!text.trim()) return;
      const range = sel.getRangeAt(0).cloneRange();
      const rect = range.getBoundingClientRect();
      if (!rect.width && !rect.height) return;
      showCommentPopover({ kind: 'text', text, range }, rect);
    }, 0);
  };
  document.addEventListener('mouseup', onSelectionMouseUp, true);
  // Click-away dismisses. Scrolling does NOT: the popover follows its
  // anchor (below), so you can scroll for context mid-comment.
  document.addEventListener('mousedown', (e) => {
    if (commentTarget && !ui.cpop.contains(realTarget(e))) hideCommentPopover();
  }, true);
  // Follow the anchor on scroll/resize. Only range-anchored popovers
  // move with the page; a chip-opened editor sits on the fixed-position
  // box and stays put. Off-screen anchor = hidden popover, state kept.
  // No rAF hop: scroll events are already frame-coalesced, and rAF
  // pauses entirely in non-rendered documents.
  const onAnchorScroll = () => {
    const t = commentTarget;
    if (!t || !t.range || t.kind === 'edit') return;
    const r = t.range.getBoundingClientRect();
    const off = r.bottom < 0 || r.top > innerHeight;
    ui.cpop.style.visibility = off ? 'hidden' : '';
    if (!off) positionCommentPopover(r);
  };
  window.addEventListener('scroll', onAnchorScroll, true);
  window.addEventListener('resize', onAnchorScroll);
  ui.cpopIn.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      confirmComment();
    } else if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'c' && ui.cpopIn.selectionStart === ui.cpopIn.selectionEnd) {
      // Focus lives here, but the user's mental target is the page
      // highlight — ⌘C with nothing selected inside the input copies
      // the anchor text. A selection inside the input keeps native copy.
      const t = commentTarget;
      const text = t && (t.kind === 'text' ? t.text : t.range ? t.range.toString() : '');
      if (text && navigator.clipboard) {
        e.preventDefault();
        navigator.clipboard.writeText(text).then(() => toast('copied'), () => toast('copy failed'));
      }
    }
    e.stopPropagation(); // typing must never trigger guest-app shortcuts
  });
  ui.cpopIn.addEventListener('input', syncCpopHeight);

  // ---------- image attachments ---------------------------------------------
  // Staged images (screenshot crops, pasted images, picked files) ride
  // the next send as inline data: attachments.
  const IMAGE_MIMES = ['image/png', 'image/jpeg', 'image/webp', 'image/gif']; // pkg/images.AllowedMimes
  const MAX_IMAGE_BYTES = 5 * 1024 * 1024; // daemon-side per-image cap
  const ATTACH_MAX_COUNT = 6; // mobile MAX_ATTACHMENTS
  ui.file.accept = IMAGE_MIMES.join(',');

  // Decode is async, so a synchronous store.images.length check alone lets
  // concurrent stages (multi-select, multi-paste) blow past ATTACH_MAX_COUNT —
  // pendingImageCount reserves a slot for the whole decode.
  let pendingImageCount = 0;

  const stageImageFile = (file) => {
    if (store.images.length + pendingImageCount >= ATTACH_MAX_COUNT) { toast(`max ${ATTACH_MAX_COUNT} images per message`); return; }
    if (!IMAGE_MIMES.includes(file.type)) { toast(`can't attach ${file.type || file.name || 'that'} — images only`); return; }
    if (file.size > MAX_IMAGE_BYTES) { toast(`${file.name || 'image'} is too large (5 MB max)`); return; }
    pendingImageCount++;
    const rd = new FileReader();
    rd.onerror = () => { pendingImageCount--; toast(`couldn't read ${file.name || 'that image'}`); };
    rd.onload = () => {
      const img = new Image();
      img.onerror = () => { pendingImageCount--; toast(`couldn't decode ${file.name || 'that image'}`); };
      img.onload = () => {
        pendingImageCount--;
        const name = file.name || 'pasted-image';
        store.images.push({ dataURL: rd.result, mime: file.type, filename: name, label: name, w: img.naturalWidth, h: img.naturalHeight });
        render();
      };
      img.src = rd.result;
    };
    rd.readAsDataURL(file);
  };

  // ---------- screenshot area grab -----------------------------------------
  // Mobile ScreenshotCropOverlay parity: capture first (frozen bitmap),
  // then crop on top of it. The bitmap is stretched to the viewport
  // (FillBounds), so selection→bitmap mapping is a plain ratio.
  const SHOT_MIN_SIZE = 48; // mobile MIN_SIZE_DP
  const SHOT_HANDLE_HIT = 32; // mobile HANDLE_TOUCH_DP
  const SHOT_MAX_EDGE = 2000; // keep PNGs under the daemon's 5 MiB image cap
  let shotCanvas = null; // frozen tab bitmap being cropped
  let cropSel = null; // selection {x,y,w,h} in viewport px
  let cropDrag = null; // {mode, sx, sy, orig}

  // The browser prompts on EVERY getDisplayMedia call — there is no
  // persistent grant — so the stream from the first Allow is kept warm
  // and re-grabbed from directly. Only the first grab per page prompts;
  // the browser shows its tab-sharing indicator while warm, and ending
  // the share there just means the next grab prompts once again.
  let shotStream = null;
  let shotVideo = null; // stays playing so a warm grab is one drawImage

  const shotAlive = () => !!shotStream && shotStream.getVideoTracks().some((t) => t.readyState === 'live');
  const dropShotStream = () => {
    if (shotStream) shotStream.getTracks().forEach((t) => t.stop());
    shotStream = null;
    shotVideo = null;
  };

  // Wait for a composited frame, but never trust rVFC alone — it can
  // stay silent for an off-DOM video whose frames still draw fine — so
  // a settle timeout unblocks either way (mobile's 48ms analog).
  const awaitFrame = (video, ms) =>
    new Promise((r) => {
      let rvfcId;
      const done = () => {
        clearTimeout(timer);
        if (rvfcId && video.cancelVideoFrameCallback) video.cancelVideoFrameCallback(rvfcId);
        r();
      };
      const timer = setTimeout(done, ms);
      if (video.requestVideoFrameCallback) rvfcId = video.requestVideoFrameCallback(done);
    });

  const grabFrame = async () => {
    await awaitFrame(shotVideo, 300);
    if (!shotVideo.videoWidth) return null;
    const c = document.createElement('canvas');
    c.width = shotVideo.videoWidth;
    c.height = shotVideo.videoHeight;
    c.getContext('2d').drawImage(shotVideo, 0, 0);
    return c;
  };

  // A dead capture path (rotted while backgrounded) delivers pure-black
  // or transparent frames; real page pixels essentially never do.
  const isDeadFrame = (src) => {
    const t = document.createElement('canvas');
    t.width = 8;
    t.height = 8;
    const g = t.getContext('2d');
    g.drawImage(src, 0, 0, 8, 8);
    const d = g.getImageData(0, 0, 8, 8).data;
    for (let i = 0; i < d.length; i += 4) {
      if (d[i + 3] !== 0 && (d[i] || d[i + 1] || d[i + 2])) return false;
    }
    return true;
  };

  const acquireShotStream = async () => {
    // preferCurrentTab: Chromium's picker offers just this tab — the
    // closest the web gets to mobile's window PixelCopy.
    const stream = await navigator.mediaDevices.getDisplayMedia({
      video: true,
      audio: false,
      preferCurrentTab: true,
      selfBrowserSurface: 'include',
    });
    const video = document.createElement('video');
    video.srcObject = stream;
    video.muted = true;
    await video.play();
    // The user can end the share from the browser's own UI at any time.
    stream.getVideoTracks().forEach((t) => (t.onended = () => { if (shotStream === stream) dropShotStream(); }));
    shotStream = stream;
    shotVideo = video;
  };

  const beginShot = async () => {
    if (store.crop) return;
    if (store.images.length >= ATTACH_MAX_COUNT) { toast(`max ${ATTACH_MAX_COUNT} images per message`); return; }
    exitInspect();
    // Keep the overlay out of the shot (mobile hides its overlay view
    // before PixelCopy).
    host.style.visibility = 'hidden';
    try {
      let c = shotAlive() ? await grabFrame() : null;
      if (!c || isDeadFrame(c)) {
        // No warm stream, or it rotted — (re)acquire; this is the only
        // path that shows the browser's share prompt.
        dropShotStream();
        await acquireShotStream();
        c = await grabFrame();
      }
      if (!c) throw new Error('capture produced no frame');
      shotCanvas = c;
    } catch (err) {
      // NotAllowedError = the user dismissed the share picker; not an error.
      if (!err || err.name !== 'NotAllowedError') toast('screenshot failed: ' + (err && err.message));
      dropShotStream();
      shotCanvas = null;
      return;
    } finally {
      host.style.visibility = '';
    }
    store.crop = true;
    cropSel = null;
    cropDrag = null;
    shotCanvas.setAttribute('aria-hidden', 'true');
    ui.crop.prepend(shotCanvas);
    ui.crop.classList.add('on');
    renderCrop();
  };

  const exitCrop = () => {
    if (!store.crop) return;
    store.crop = false;
    cropSel = null;
    cropDrag = null;
    ui.crop.classList.remove('on');
    if (shotCanvas) shotCanvas.remove();
    shotCanvas = null;
  };

  const confirmCrop = () => {
    if (!cropSel || !shotCanvas) return;
    const sx = shotCanvas.width / innerWidth;
    const sy = shotCanvas.height / innerHeight;
    const cw = Math.round(cropSel.w * sx);
    const ch = Math.round(cropSel.h * sy);
    // A zero/non-finite crop (zero-sized viewport, collapsed selection)
    // would silently stage an empty "data:" attachment — refuse instead.
    if (!isFinite(cw) || !isFinite(ch) || cw < 1 || ch < 1) { toast('empty selection — try again'); return; }
    const scale = Math.min(1, SHOT_MAX_EDGE / Math.max(cw, ch));
    const out = document.createElement('canvas');
    out.width = Math.max(1, Math.round(cw * scale));
    out.height = Math.max(1, Math.round(ch * scale));
    out.getContext('2d').drawImage(shotCanvas, cropSel.x * sx, cropSel.y * sy, cw, ch, 0, 0, out.width, out.height);
    store.images.push({ dataURL: out.toDataURL('image/png'), mime: 'image/png', filename: 'screenshot.png', label: 'screenshot', w: out.width, h: out.height });
    exitCrop();
    toast('added to context');
    if (store.box === 'hidden') setBox('prompt');
    else render();
  };

  // The bar's content (icon + fixed "Add to context" label) never changes
  // size within a crop session — measure once and reuse, instead of forcing
  // a layout reflow on offsetWidth/offsetHeight every pointermove.
  let cropBarSize = null;

  const renderCrop = () => {
    const s = cropSel;
    ui.cropDim.style.display = s ? 'none' : '';
    ui.cropX0.style.display = s ? 'none' : '';
    ui.cropSel.style.display = s ? 'block' : 'none';
    ui.cropBar.style.display = s ? 'flex' : 'none';
    if (!s) return;
    Object.assign(ui.cropSel.style, { left: s.x + 'px', top: s.y + 'px', width: s.w + 'px', height: s.h + 'px' });
    // The bar hangs 12px below the selection's bottom-right corner,
    // clamped to 12px screen margins; tucks inside when there's no room
    // below (mobile CropActionBar anchoring).
    if (!cropBarSize) cropBarSize = { w: ui.cropBar.offsetWidth, h: ui.cropBar.offsetHeight };
    const { w: bw, h: bh } = cropBarSize;
    const bx = Math.max(12, Math.min(s.x + s.w - bw, innerWidth - bw - 12));
    let by = s.y + s.h + 12;
    if (by + bh > innerHeight - 12) by = innerHeight - bh - 12;
    Object.assign(ui.cropBar.style, { left: bx + 'px', top: by + 'px' });
  };

  // Corners win, then edge bands, then interior = move, else a new box
  // (mobile hitTest order).
  const cropHit = (x, y) => {
    const s = cropSel;
    if (!s) return 'new';
    const H = SHOT_HANDLE_HIT;
    const nearL = Math.abs(x - s.x) <= H, nearR = Math.abs(x - s.x - s.w) <= H;
    const nearT = Math.abs(y - s.y) <= H, nearB = Math.abs(y - s.y - s.h) <= H;
    if (nearL && nearT) return 'tl';
    if (nearR && nearT) return 'tr';
    if (nearL && nearB) return 'bl';
    if (nearR && nearB) return 'br';
    const inX = x >= s.x && x <= s.x + s.w, inY = y >= s.y && y <= s.y + s.h;
    if (nearL && inY) return 'l';
    if (nearR && inY) return 'r';
    if (nearT && inX) return 't';
    if (nearB && inX) return 'b';
    if (inX && inY) return 'move';
    return 'new';
  };

  // Recompute the selection for the drag-in-progress: move clamps the
  // whole box on-screen; resizes pin the opposite edge and enforce the
  // min size (mobile applyDrag + normalizedRect).
  const cropApply = (x, y) => {
    const { mode, sx, sy, orig: o } = cropDrag;
    if (mode === 'move') {
      cropSel = {
        x: Math.min(Math.max(o.x + x - sx, 0), innerWidth - o.w),
        y: Math.min(Math.max(o.y + y - sy, 0), innerHeight - o.h),
        w: o.w, h: o.h,
      };
      return;
    }
    let l, t, r, b;
    if (mode === 'new') {
      l = Math.min(sx, x); r = Math.max(sx, x);
      t = Math.min(sy, y); b = Math.max(sy, y);
      if (r - l < SHOT_MIN_SIZE) { if (x < sx) l = r - SHOT_MIN_SIZE; else r = l + SHOT_MIN_SIZE; }
      if (b - t < SHOT_MIN_SIZE) { if (y < sy) t = b - SHOT_MIN_SIZE; else b = t + SHOT_MIN_SIZE; }
    } else {
      l = o.x; t = o.y; r = o.x + o.w; b = o.y + o.h;
      const dx = x - sx, dy = y - sy;
      if (/l/.test(mode)) l = Math.min(l + dx, r - SHOT_MIN_SIZE);
      if (/r/.test(mode)) r = Math.max(r + dx, l + SHOT_MIN_SIZE);
      if (/t/.test(mode)) t = Math.min(t + dy, b - SHOT_MIN_SIZE);
      if (/b/.test(mode)) b = Math.max(b + dy, t + SHOT_MIN_SIZE);
    }
    const preL = l, preT = t;
    l = Math.max(0, l); t = Math.max(0, t);
    r = Math.min(innerWidth, r); b = Math.min(innerHeight, b);
    // The viewport clamp above can shrink a resize below SHOT_MIN_SIZE near
    // a screen edge (e.g. dragging the right handle while already close to
    // innerWidth) — pull the edge that wasn't clamped back in to restore it.
    if (r - l < SHOT_MIN_SIZE) { if (l !== preL) r = Math.min(innerWidth, l + SHOT_MIN_SIZE); else l = Math.max(0, r - SHOT_MIN_SIZE); }
    if (b - t < SHOT_MIN_SIZE) { if (t !== preT) b = Math.min(innerHeight, t + SHOT_MIN_SIZE); else t = Math.max(0, b - SHOT_MIN_SIZE); }
    cropSel = { x: l, y: t, w: r - l, h: b - t };
  };

  ui.crop.addEventListener('pointerdown', (e) => {
    if (e.button !== 0) return;
    if (e.target.closest && e.target.closest('button')) return; // bar buttons handle their own clicks
    e.preventDefault();
    const mode = cropHit(e.clientX, e.clientY);
    cropDrag = { mode, sx: e.clientX, sy: e.clientY, orig: cropSel && { ...cropSel } };
    if (mode === 'new') cropSel = null; // the old box dissolves; the drag draws a fresh one
    ui.crop.setPointerCapture(e.pointerId);
    renderCrop();
  });
  ui.crop.addEventListener('pointermove', (e) => {
    if (!cropDrag) return;
    cropApply(e.clientX, e.clientY);
    renderCrop();
  });
  const cropDragEnd = () => { cropDrag = null; };
  ui.crop.addEventListener('pointerup', cropDragEnd);
  ui.crop.addEventListener('pointercancel', cropDragEnd);

  // ---------- drag -----------------------------------------------------------
  (() => {
    const hd = $('.hd');
    let sx = 0, sy = 0, ox = 0, oy = 0, dragging = false;
    const saved = sessionStorage.getItem('clank.boxPos');
    if (saved) { try { const p = JSON.parse(saved); ui.box.style.translate = `${p.x}px ${p.y}px`; ui.box.dataset.x = p.x; ui.box.dataset.y = p.y; } catch {} }
    hd.addEventListener('pointerdown', (e) => {
      if (e.target.closest('.beta')) return; // the pill is a link, not a drag handle
      endFollow(); // manual drag wins over a live shift-follow
      dragging = true;
      sx = e.clientX; sy = e.clientY;
      ox = parseFloat(ui.box.dataset.x || '0'); oy = parseFloat(ui.box.dataset.y || '0');
      hd.setPointerCapture(e.pointerId);
    });
    hd.addEventListener('pointermove', (e) => {
      if (!dragging) return;
      const x = ox + e.clientX - sx, y = oy + e.clientY - sy;
      ui.box.dataset.x = x; ui.box.dataset.y = y;
      ui.box.style.translate = `${x}px ${y}px`;
    });
    hd.addEventListener('pointerup', (e) => {
      if (!dragging) return; // e.g. a click on the pill link — no drag was armed
      dragging = false;
      sessionStorage.setItem('clank.boxPos', JSON.stringify({ x: parseFloat(ui.box.dataset.x || '0'), y: parseFloat(ui.box.dataset.y || '0') }));
      // A drag that never really moved is a tap: toggle the chat view
      // (the old second-keypress cycle, now that ⇪ is a plain toggle).
      if (Math.abs(e.clientX - sx) + Math.abs(e.clientY - sy) < 4) {
        store.box = store.box === 'chat' ? 'prompt' : 'chat';
        render();
      }
    });
  })();

  // ---------- wiring -----------------------------------------------------------
  ui.send.onclick = () => { (store.agent === 'thinking' || store.agent === 'working') ? abort() : send(); };
  ui.profile.onclick = () => (store.settingsOpen ? closeSettings() : openSettings());
  ui.saveProfileCancel.onclick = closeSaveProfile;
  ui.saveProfileConfirm.onclick = saveProfile;
  ui.saveProfile.onclick = (e) => { if (e.target === ui.saveProfile) closeSaveProfile(); };
  ui.saveProfileName.addEventListener('input', () => {
    store.saveProfileName = ui.saveProfileName.value;
    ui.saveProfileConfirm.disabled = store.profileSaving || !store.saveProfileName.trim();
  });
  ui.saveProfileName.addEventListener('keydown', (e) => {
    e.stopPropagation();
    if (e.key === 'Enter' && store.saveProfileName.trim()) {
      e.preventDefault();
      saveProfile();
    }
  });
  ui.sel.onclick = () => (store.inspect ? exitInspect() : enterInspect());
  ui.shot.onclick = beginShot;
  ui.cropAdd.onclick = confirmCrop;
  ui.cropX.onclick = exitCrop;
  ui.cropX0.onclick = exitCrop;
  ui.att.onclick = () => ui.file.click();
  ui.file.onchange = () => { [...ui.file.files].forEach(stageImageFile); ui.file.value = ''; };
  ui.box.addEventListener('paste', (e) => {
    const files = [...((e.clipboardData && e.clipboardData.files) || [])].filter((f) => IMAGE_MIMES.includes(f.type));
    if (!files.length) return; // plain text pastes stay the browser's business
    e.preventDefault();
    files.forEach(stageImageFile);
  });
  ui.mic.addEventListener('pointerdown', (e) => { e.preventDefault(); startTalk(); });
  ui.mic.addEventListener('pointerup', stopTalk);
  ui.mic.addEventListener('pointerleave', stopTalk);
  ui.eng.onclick = () => {
    if (store.voice === 'recording' || store.voice === 'transcribing') return; // not mid-utterance
    if (store.enginePick) closeEnginePick();
    else openEnginePick(false);
  };
  ui.settings.addEventListener('keydown', (e) => e.stopPropagation());
  // Engine picker copy. Availability is fixed for the page's lifetime,
  // and the descriptions double as the consent text — name where the
  // audio goes and what does the transcribing (CFG.voice_engine says
  // which implementation backs the local path).
  const LOCAL_DESC = {
    sherpa: 'transcribed on this machine by the ~670 MB NVIDIA Parakeet v3 model — audio never leaves it',
    exec: 'transcribed on this machine by your CLANK_VOICE_ASR_CMD command — audio never leaves it',
  };
  ui.engOpts.forEach((b) => {
    const local = b.dataset.eng === 'local';
    const avail = local ? LOCAL_VOICE : !!SR;
    b.disabled = !avail;
    let desc;
    if (local) {
      desc = avail
        ? (LOCAL_DESC[CFG.voice_engine] || 'transcribed on this machine — audio never leaves it')
        : 'unavailable — install clank-voice (runs the ~670 MB Parakeet v3 model locally) or set CLANK_VOICE_ASR_CMD, then restart the preview';
    } else {
      desc = avail
        ? 'the browser’s speech service — audio is sent to your browser vendor (e.g. Google in Chrome, Apple in Safari)'
        : 'not supported in this browser';
    }
    b.querySelector('.d').textContent = desc;
    b.onclick = () => { if (!b.disabled) chooseEngine(b.dataset.eng); };
  });
  $('.perm .allow').onclick = () => replyPermission(true);
  $('.perm .deny').onclick = () => replyPermission(false);
  // Typing revision notes must not trigger guest-app shortcuts; render
  // keeps the Deny/Request-changes label in sync with the notes text.
  ui.notes.addEventListener('keydown', (e) => e.stopPropagation());
  ui.notes.addEventListener('input', render);
  ui.input.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); send(); }
    e.stopPropagation(); // typing must never trigger guest-app shortcuts
  });
  // Enter with the container itself focused (the post-summon /
  // post-dictation anchor) fires the primary action, dialog-style.
  // Guarded to the container: buttons handle their own Enter, and the
  // composer's handler above stops propagation.
  ui.box.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && e.target === ui.box && composerTextForSend(ui.input.value, store.chips)) { e.preventDefault(); send(); }
  });
  ui.input.addEventListener('input', syncComposerHeight);

  // ---------- shift-follow: the box glides to the cursor while ⇧ is held
  // Performance shape: mousemove only records the target (input can fire
  // at 1 kHz); one rAF loop does the motion with a single transform write
  // per frame (compositor-only — the guest never relayouts); box/viewport
  // metrics are cached at follow-start (no per-frame layout reads); the
  // loop exists only from ⇧-down until the spring settles after release.
  const REDUCED_MOTION = matchMedia('(prefers-reduced-motion: reduce)');
  let follow = null;

  // Keydown events carry no pointer coordinates, so the press-time jump
  // needs the cursor tracked continuously. Passive, two assignments —
  // nothing else happens outside an active follow.
  let mouseX = 0, mouseY = 0, mouseSeen = false;
  window.addEventListener('mousemove', (e) => { mouseX = e.clientX; mouseY = e.clientY; mouseSeen = true; }, { passive: true, capture: true });

  // The pointer lands 12px INTO the header — deep enough that the
  // grab-cursor hover actually triggers, so a follow hands off straight
  // into click-drag.
  const followTargetFromPointer = (cx, cy) => {
    const left = Math.min(Math.max(cx - follow.w / 2, 8), innerWidth - follow.w - 8);
    const top = Math.min(Math.max(cy - 12, 8), innerHeight - follow.h - 8);
    follow.tx = left - follow.natX;
    follow.ty = top - follow.natY;
  };

  const startFollow = () => {
    if (store.box === 'hidden') return;
    if (follow) {
      // Pressed again while the previous glide is still settling: re-arm
      // the live spring — retarget to the cursor, keep the velocity —
      // instead of swallowing the press until it settles.
      follow.held = true;
      if (mouseSeen) followTargetFromPointer(mouseX, mouseY);
      window.addEventListener('mousemove', onFollowMove, true); // no-op if already attached
      return;
    }
    const r = ui.box.getBoundingClientRect(); // once, outside the loop
    const x = parseFloat(ui.box.dataset.x || '0');
    const y = parseFloat(ui.box.dataset.y || '0');
    follow = {
      // natural (untranslated) top-left, so pointer coords → translate coords
      natX: r.left - x, natY: r.top - y, w: r.width, h: r.height,
      x, y, vx: 0, vy: 0, tx: x, ty: y,
      held: true, lastT: 0, raf: 0,
    };
    if (mouseSeen) followTargetFromPointer(mouseX, mouseY); // jump starts NOW, not at the next mousemove
    window.addEventListener('mousemove', onFollowMove, true);
    follow.raf = requestAnimationFrame(followStep);
  };

  const onFollowMove = (e) => {
    if (!follow || !follow.held) return;
    followTargetFromPointer(e.clientX, e.clientY);
  };

  const followStep = (t) => {
    if (!follow) return;
    const f = follow;
    const dt = Math.min(32, f.lastT ? t - f.lastT : 16) / 1000; // clamp: hitches must not slingshot
    f.lastT = t;
    if (REDUCED_MOTION.matches) {
      f.x = f.tx; f.y = f.ty; f.vx = f.vy = 0;
    } else {
      // Slightly under-damped spring (semi-implicit Euler): the box
      // carries momentum instead of gluing 1:1 to the pointer.
      const K = 170, D = 20;
      f.vx += (K * (f.tx - f.x) - D * f.vx) * dt;
      f.vy += (K * (f.ty - f.y) - D * f.vy) * dt;
      f.x += f.vx * dt;
      f.y += f.vy * dt;
    }
    ui.box.dataset.x = f.x;
    ui.box.dataset.y = f.y;
    ui.box.style.translate = `${f.x}px ${f.y}px`;
    const settled = Math.abs(f.tx - f.x) + Math.abs(f.ty - f.y) < 0.5 && Math.abs(f.vx) + Math.abs(f.vy) < 5;
    if (!f.held && settled) { endFollow(); return; }
    f.raf = requestAnimationFrame(followStep);
  };

  const endFollow = () => {
    if (!follow) return;
    cancelAnimationFrame(follow.raf);
    window.removeEventListener('mousemove', onFollowMove, true);
    follow = null;
    sessionStorage.setItem('clank.boxPos', JSON.stringify({ x: parseFloat(ui.box.dataset.x || '0'), y: parseFloat(ui.box.dataset.y || '0') }));
  };

  // Release: detach the target-updater IMMEDIATELY — otherwise a moving
  // cursor keeps refreshing the target and the box chases forever (the
  // "it never stops following" bug) — then let the momentum play out
  // toward the last target and settle.
  const releaseFollow = () => {
    if (!follow) return;
    follow.held = false;
    window.removeEventListener('mousemove', onFollowMove, true);
  };
  window.addEventListener('blur', endFollow); // ⇧-keyup can be lost to a cmd-tab

  // Keybindings: ⌘E/⌃E toggles the box · ⇪ taps dictation on/off ·
  // hold ⇧ = box follows the cursor · hold ⌘/⌃ = momentary element-
  // select · Esc leaves inspect / hides.
  const realTarget = (e) => (e.composedPath ? e.composedPath()[0] : e.target);
  const isEditable = (el) => !!el && (el.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(el.tagName || ''));

  // Momentary select: entering on the modifier's bare keydown would
  // flash the crosshair on every ⌘C/⌘R/⌘T, so inspect only engages
  // after the modifier has been held alone for a beat — any other key
  // in the window cancels the intent (it was a shortcut, not a hold).
  const MOD_HOLD_MS = 200;
  let modHoldTimer = 0;
  let modInspect = false;
  const cancelModHold = () => { if (modHoldTimer) { clearTimeout(modHoldTimer); modHoldTimer = 0; } };

  window.addEventListener('keydown', (e) => {
    if (store.crop) {
      // The crop layer owns the keyboard: Esc cancels, everything else
      // (including overlay hotkeys) stays off while it's up.
      if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); exitCrop(); }
      return;
    }
    if (e.code === 'CapsLock') {
      if (!e.repeat) talkToggle();
      return; // the OS lock state changes regardless; nothing to prevent
    }
    if (e.key === 'Shift') {
      // Follow the cursor while held — but never while typing (shift is
      // how capitals happen) and never preventDefault (shift-click and
      // shift-selection in the guest must keep working).
      if (!e.repeat && !isEditable(realTarget(e))) startFollow();
      return;
    }
    if (e.key === 'Meta' || e.key === 'Control') {
      if (store.box !== 'hidden' && !store.inspect && !e.repeat && !modHoldTimer) {
        modHoldTimer = setTimeout(() => {
          modHoldTimer = 0;
          modInspect = true;
          enterInspect();
        }, MOD_HOLD_MS);
      }
      return; // never preventDefault a bare modifier — shortcuts must keep working
    }
    cancelModHold(); // some other key while the modifier was held: it's a shortcut

    if (e.code === 'KeyE' && (e.metaKey || e.ctrlKey) && !e.altKey && !e.shiftKey) {
      e.preventDefault(); // ⌘E is the browser's "use selection for find" — expendable
      e.stopPropagation();
      if (!e.repeat) setBox(store.box === 'hidden' ? 'prompt' : 'hidden');
      return;
    }
    if (e.key === 'Escape') {
      if (store.saveProfileOpen) { e.preventDefault(); e.stopPropagation(); closeSaveProfile(); }
      else if (store.settingsOpen) { e.preventDefault(); e.stopPropagation(); closeSettings(); }
      else if (store.enginePick) { e.preventDefault(); e.stopPropagation(); closeEnginePick(); }
      else if (commentTarget) { e.preventDefault(); e.stopPropagation(); hideCommentPopover(); }
      else if (store.inspect) { e.preventDefault(); e.stopPropagation(); modInspect = false; exitInspect(); }
      else if (store.box !== 'hidden') { e.preventDefault(); e.stopPropagation(); setBox('hidden'); }
    }
  }, true);

  window.addEventListener('keyup', (e) => {
    if (e.code === 'CapsLock') {
      // macOS reports the lock-off press as keyup only (no keydown).
      if (IS_MAC) talkToggle();
      return;
    }
    if (e.key === 'Shift') {
      if (!e.shiftKey) releaseFollow(); // both-shifts edge: only the last release ends it
      return;
    }
    if (e.key === 'Meta' || e.key === 'Control') {
      cancelModHold();
      if (modInspect) { modInspect = false; exitInspect(); }
    }
  }, true);

  const mount = () => (document.body ? document.body.appendChild(host) : requestAnimationFrame(mount));
  mount();
  render();
  if (CFG.backend) loadProfiles().catch(() => {});
  if (store.sessionId) subscribe(); // survive full reloads mid-turn
})();
