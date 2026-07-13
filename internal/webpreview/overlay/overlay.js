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
//   hold ⇧         the box glides to the cursor (spring), settles on release
//   hold ⌘ / ⌃     momentary element-select; click tags, release exits
//   Esc            leave inspect mode, else hide
//   header tap     expand / collapse the chat view
//
// Element → source resolution prefers deterministic compiler metadata:
// Svelte dev mode stamps every node with __svelte_meta.loc; React ≤18
// exposes fiber._debugSource; otherwise we fall back to the component
// owner chain (React 19) or a plain DOM description. Per the design
// thesis, the agent does the edit — this overlay only has to hand it
// unambiguous context.
(() => {
  'use strict';
  if (window.__clankOverlay) return;
  window.__clankOverlay = true;

  const CFG = window.__CLANK_PREVIEW || {};
  const TOKEN = CFG.token || '';
  const DONE_LINGER_MS = 8000; // mobile: PreviewOverlayState.DONE_LINGER_MS
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
        Authorization: 'Bearer ' + TOKEN,
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

  // ---------- state ------------------------------------------------------
  const store = {
    box: 'hidden', // hidden | prompt | chat
    agent: 'idle', // idle | thinking | working | done | error
    inspect: false,
    chips: [], // [{label, detail, html, names}]
    msgs: [], // [{role, text}]
    streamText: '', // in-flight assistant text
    permission: null, // {request_id, tool, description}
    sessionId: sessionStorage.getItem('clank.sessionId') || CFG.session_id || '',
    lastUserMsgId: '',
    voice: 'idle', // idle | recording | transcribing (or 'off' when unavailable)
    sending: false,
    aborting: false,
  };
  if (!CFG.voice) store.voice = 'off';
  if (store.sessionId) sessionStorage.setItem('clank.sessionId', store.sessionId);
  let doneTimer = 0;

  const setAgent = (s) => {
    clearTimeout(doneTimer);
    store.agent = s;
    if (s === 'done') doneTimer = setTimeout(() => { store.agent = 'idle'; render(); }, DONE_LINGER_MS);
    render();
  };

  // ---------- element → source -------------------------------------------
  const resolveSource = (el) => {
    for (let n = el; n && n.nodeType === 1; n = n.parentElement) {
      const m = n.__svelte_meta;
      if (m && m.loc && m.loc.file) {
        return { file: m.loc.file, line: m.loc.line, column: m.loc.column, via: 'svelte', names: [], node: n };
      }
    }
    for (let n = el; n && n.nodeType === 1; n = n.parentElement) {
      const key = Object.getOwnPropertyNames(n).find((k) => k.startsWith('__reactFiber$'));
      if (!key) continue;
      let fiber = n[key];
      const names = [];
      let src = null;
      while (fiber && names.length < 5) {
        if (!src && fiber._debugSource) src = fiber._debugSource; // React ≤ 18
        const t = fiber.type;
        const nm = typeof t === 'function' ? t.displayName || t.name : t && t.displayName;
        if (nm && !names.includes(nm)) names.push(nm);
        fiber = fiber._debugOwner;
      }
      if (src) return { file: src.fileName, line: src.lineNumber, column: src.columnNumber, via: 'react', names, node: el };
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
    const base = s.file ? `${s.file}:${s.line}${s.column ? ':' + s.column : ''}` : domPath(el);
    const label = s.file ? `${s.file.split('/').pop()}:${s.line}` : `<${el.tagName.toLowerCase()}>`;
    return {
      label,
      detail: base + (s.names.length ? ` (components: ${s.names.join(' › ')})` : ''),
      html: shortHTML(el),
    };
  };

  const buildContext = () => {
    if (!store.chips.length && !recentErrors.length) return '';
    const lines = ['', '', '--- clank preview context (auto-attached by the web overlay) ---'];
    if (store.chips.length) {
      lines.push('Selected elements:');
      store.chips.forEach((c, i) => {
        lines.push(`${i + 1}. ${c.detail}`);
        lines.push(`   html: ${c.html}`);
      });
    }
    lines.push(`Route: ${location.pathname}${location.search}`);
    lines.push(`Viewport: ${innerWidth}x${innerHeight}`);
    if (recentErrors.length) {
      lines.push('Recent console errors:');
      recentErrors.slice(-3).forEach((e) => lines.push('- ' + e));
    }
    lines.push('--- end context ---');
    return lines.join('\n');
  };

  // ---------- session ------------------------------------------------------
  const send = async () => {
    const text = ui.input.value.trim();
    if (!text || store.sending) return;
    const full = text + buildContext();
    store.sending = true;
    store.msgs.push({ role: 'user', text });
    store.streamText = '';
    setComposer('');
    store.chips = [];
    setAgent('thinking');
    try {
      if (!store.sessionId) {
        const info = await apiJSON('/sessions', {
          method: 'POST',
          body: JSON.stringify({
            backend: CFG.backend || undefined,
            hostname: CFG.hostname || 'local',
            git_ref: { local_path: CFG.local_path },
            prompt: full,
          }),
        });
        store.sessionId = (info && info.id) || '';
        if (!store.sessionId) throw new Error('session create returned no id');
        sessionStorage.setItem('clank.sessionId', store.sessionId);
        subscribe();
      } else {
        await api(`/sessions/${store.sessionId}/message`, { method: 'POST', body: JSON.stringify({ text: full }) });
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
    const p = store.permission;
    if (!p || !store.sessionId) return;
    store.permission = null;
    render();
    try {
      await api(`/sessions/${store.sessionId}/permissions/${p.request_id}/reply`, { method: 'POST', body: JSON.stringify({ allow }) });
    } catch (err) { toast('permission reply failed: ' + err.message); }
  };

  // ---------- SSE ----------------------------------------------------------
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
        // opening is gone, and the same applies across reconnects. Sync the
        // coarse agent state from the session snapshot so the border can't
        // stick on a stale state; parts/messages catch up on the live stream.
        fetch(`/__clank/api/sessions/${sid}`, { headers: { Authorization: 'Bearer ' + TOKEN } })
          .then((r) => (r.ok ? r.json() : null))
          .then((info) => {
            if (!info || store.sessionId !== sid) return;
            if (info.status === 'busy' && (store.agent === 'idle' || store.agent === 'done')) setAgent('thinking');
            else if (info.status === 'idle' && (store.agent === 'thinking' || store.agent === 'working')) setAgent('done');
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
        if (p.type === 'tool_call') setAgent('working');
        if (p.type === 'text' && p.text) {
          if (d.is_delta) store.streamText += p.text;
          else store.streamText = p.text;
          render();
        }
        break;
      }
      case 'message': {
        if (d.role === 'user' && d.id) store.lastUserMsgId = d.id;
        if (d.role === 'assistant') {
          const text = (d.parts || []).filter((p) => p.type === 'text').map((p) => p.text).join('');
          if (text) { store.msgs.push({ role: 'assistant', text }); store.streamText = ''; }
        }
        if (store.msgs.length > 30) store.msgs.splice(0, store.msgs.length - 30);
        render();
        break;
      }
      case 'permission':
        store.permission = d;
        if (store.box === 'hidden') store.box = 'prompt';
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

  // ---------- voice (push-to-talk) ----------------------------------------
  let vws = null; // dictation WebSocket, opened lazily, reused
  let audio = null; // {ctx, stream, node}
  let voiceBase = ''; // input text at push-to-talk start; partials append after it
  const withVoiceText = (t) => (voiceBase ? voiceBase.replace(/\s+$/, '') + ' ' : '') + t;
  const voiceWS = () =>
    new Promise((resolve, reject) => {
      if (vws && vws.readyState === WebSocket.OPEN) return resolve(vws);
      const proto = location.protocol === 'https:' ? 'wss' : 'ws';
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
          ui.input.focus();
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
    store.voice = 'recording';
    voiceBase = ui.input.value;
    capturingSince = 0;
    utterPeak = 0;
    clearTimeout(audioIdleReap); // in use — don't reap underneath the utterance
    render();
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
  const host = document.createElement('div');
  host.id = 'clank-overlay-host';
  host.style.cssText = 'position:fixed;inset:0;z-index:2147483646;pointer-events:none;';
  const root = host.attachShadow({ mode: 'open' });
  root.innerHTML = `
<style>
  :host { all: initial; }
  * { box-sizing: border-box; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif; }
  .box {
    position: fixed; right: 24px; bottom: 24px; width: 380px; max-width: calc(100vw - 32px);
    background: rgba(21,22,26,.94); color: #e8e8ec; border-radius: 18px;
    border: 1.5px solid #3a3b42; box-shadow: 0 12px 40px rgba(0,0,0,.45);
    pointer-events: auto; backdrop-filter: blur(14px); display: none; overflow: hidden;
    transition: border-color .25s ease;
  }
  .box.visible { display: block; }
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
  .hd .name { font-size:12px; font-weight:600; color:#9ca3af; flex:1; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  .hd .st { font-size:11px; color:#6b7280; }
  .grip { color:#4b5563; font-size:10px; letter-spacing:2px; }
  .chips { display:flex; flex-wrap:wrap; gap:6px; padding:0 12px 4px; }
  .chip { display:inline-flex; align-items:center; gap:6px; background:#26272e; border:1px solid #3a3b42;
    color:#c7d2fe; font-size:11px; padding:3px 8px; border-radius:999px; max-width:100%; }
  .chip b { font-weight:600; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  .chip button { all:unset; cursor:pointer; color:#8b8d98; font-size:12px; line-height:1; }
  .chat { max-height:240px; overflow-y:auto; padding:4px 12px; display:none; }
  .box.expanded .chat { display:block; }
  .m { font-size:12.5px; line-height:1.45; margin:6px 0; white-space:pre-wrap; word-break:break-word; }
  .m.user { color:#93c5fd; }
  .m.assistant { color:#d1d5db; }
  .m .who { font-size:10px; text-transform:uppercase; letter-spacing:.6px; color:#6b7280; display:block; }
  .perm { margin:6px 12px; padding:8px 10px; border:1px solid #f59e0b66; background:#f59e0b14; border-radius:10px; font-size:12px; }
  .perm .t { font-weight:600; margin-bottom:2px; }
  .perm .d { color:#9ca3af; margin-bottom:6px; word-break:break-word; }
  .perm button { all:unset; cursor:pointer; font-size:12px; font-weight:600; padding:4px 12px; border-radius:8px; margin-right:6px; }
  .perm .allow { background:#22c55e22; color:#4ade80; }
  .perm .deny { background:#ef444422; color:#f87171; }
  textarea { width:100%; resize:none; background:transparent; border:0; outline:0; color:#e8e8ec;
    font-size:13px; line-height:1.4; padding:6px 12px; min-height:34px; max-height:120px; }
  textarea::placeholder { color:#6b7280; }
  .bar { display:flex; align-items:center; gap:4px; padding:6px 8px 8px; }
  .ib { all:unset; cursor:pointer; width:30px; height:30px; border-radius:9px; display:inline-flex;
    align-items:center; justify-content:center; color:#9ca3af; font-size:15px; }
  .ib:hover { background:#ffffff14; color:#e8e8ec; }
  .ib.active { background:#3b82f622; color:#93c5fd; }
  .ib[disabled] { opacity:.35; cursor:not-allowed; }
  .mic { position:relative; }
  .mic.rec { color:#f87171; background:#ef444422; }
  .mic.rec::after { content:''; position:absolute; inset:-3px; border-radius:12px;
    border:2px solid rgba(248,113,113, calc(.25 + .75 * var(--lvl, 0))); }
  .send { margin-left:auto; background:#e8e8ec; color:#111; font-weight:700; }
  .send:hover { background:#fff; color:#000; }
  .send.stop { background:#ef4444; color:#fff; }
  .hint { font-size:10px; color:#4b5563; text-align:center; padding:0 0 7px; }
  kbd { font-family:inherit; background:#26272e; padding:0 4px; border-radius:4px; }
  .hl { position:fixed; pointer-events:none; border:1.5px solid #60a5fa; background:#3b82f61f;
    border-radius:3px; display:none; }
  .hll { position:fixed; pointer-events:none; background:#111318; color:#c7d2fe; font-size:11px;
    padding:2px 7px; border-radius:6px; border:1px solid #3a3b42; display:none; white-space:nowrap; }
  .toast { position:fixed; bottom:16px; left:50%; transform:translateX(-50%); background:#111318;
    color:#e8e8ec; border:1px solid #3a3b42; font-size:12px; padding:7px 14px; border-radius:10px;
    pointer-events:none; opacity:0; transition:opacity .2s; max-width:70vw; }
  .toast.show { opacity:1; }
</style>
<div class="box" part="box">
  <div class="hd"><span class="dot"></span><span class="name"></span><span class="st"></span><span class="grip">⋮⋮</span></div>
  <div class="chips"></div>
  <div class="chat"></div>
  <div class="perm" style="display:none">
    <div class="t"></div><div class="d"></div>
    <button class="allow">Allow</button><button class="deny">Deny</button>
  </div>
  <textarea rows="1" placeholder="Ask anything…"></textarea>
  <div class="bar">
    <button class="ib sel" title="Select an element (Alt+S)">⌖</button>
    <button class="ib mic" title="Tap ⇪ to talk (or hold this button)">🎙</button>
    <span class="micLevel" style="display:none"></span>
    <button class="ib send" title="Send (Enter)">↑</button>
  </div>
  <div class="hint"><kbd>⌘E</kbd> toggle · <kbd>⇪</kbd> talk · hold <kbd>⌘</kbd> select · hold <kbd>⇧</kbd> move · <kbd>Esc</kbd> hide</div>
</div>
<div class="hl"></div><div class="hll"></div><div class="toast"></div>`;

  const $ = (sel) => root.querySelector(sel);
  const ui = {
    box: $('.box'), name: $('.name'), st: $('.st'), chips: $('.chips'), chat: $('.chat'),
    perm: $('.perm'), permT: $('.perm .t'), permD: $('.perm .d'),
    input: $('textarea'), sel: $('.sel'), mic: $('.mic'), micLevel: $('.micLevel'),
    send: $('.send'), hl: $('.hl'), hll: $('.hll'), toast: $('.toast'),
  };
  ui.name.textContent = CFG.name || 'clank';

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

  const STATUS_TEXT = { idle: '', thinking: 'thinking…', working: 'working…', done: 'done', error: 'error' };
  const render = () => {
    ui.box.classList.toggle('visible', store.box !== 'hidden');
    ui.box.classList.toggle('expanded', store.box === 'chat');
    for (const s of ['thinking', 'working', 'done', 'error']) ui.box.classList.toggle(s, store.agent === s);
    ui.st.textContent = store.aborting ? 'stopping…' : STATUS_TEXT[store.agent] || '';

    ui.chips.innerHTML = '';
    store.chips.forEach((c, i) => {
      const el = document.createElement('span');
      el.className = 'chip';
      el.title = c.detail;
      const b = document.createElement('b');
      b.textContent = c.label;
      const x = document.createElement('button');
      x.textContent = '✕';
      x.onclick = () => { store.chips.splice(i, 1); render(); };
      el.append(b, x);
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

    ui.perm.style.display = store.permission ? '' : 'none';
    if (store.permission) {
      ui.permT.textContent = `Allow ${store.permission.tool || 'tool'}?`;
      ui.permD.textContent = store.permission.description || '';
    }

    ui.sel.classList.toggle('active', store.inspect);
    ui.mic.style.display = store.voice === 'off' ? 'none' : '';
    ui.mic.classList.toggle('rec', store.voice === 'recording');
    ui.mic.textContent = store.voice === 'transcribing' ? '…' : '🎙';

    const busy = store.agent === 'thinking' || store.agent === 'working';
    ui.send.classList.toggle('stop', busy);
    ui.send.textContent = busy ? '◼' : '↑';
    ui.send.title = busy ? 'Stop the agent' : 'Send (Enter)';
  };

  // ---------- box visibility (mobile shake state machine, hotkey-driven) --
  const setBox = (s) => {
    store.box = s;
    if (s === 'hidden') exitInspect();
    render();
    if (s !== 'hidden') setTimeout(() => ui.input.focus(), 0);
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
    ui.hll.textContent = s.file ? `${s.file}:${s.line}` : s.names.length ? s.names.join(' › ') : domPath(el);
    const ly = r.top > 28 ? r.top - 24 : r.bottom + 4;
    Object.assign(ui.hll.style, { display: 'block', left: Math.max(4, r.left) + 'px', top: ly + 'px' });
  };
  const onInspectClick = (e) => {
    if (ours(e.target)) return;
    e.preventDefault();
    e.stopPropagation();
    if (hoverEl) {
      store.chips.push(chipFromElement(hoverEl));
      toast('added to context');
    }
    // Stay in select mode: it ends when the held modifier is released
    // (momentary), or via Esc / the ⌖ button (toggled).
    render();
  };

  // ---------- drag -----------------------------------------------------------
  (() => {
    const hd = $('.hd');
    let sx = 0, sy = 0, ox = 0, oy = 0, dragging = false;
    const saved = sessionStorage.getItem('clank.boxPos');
    if (saved) { try { const p = JSON.parse(saved); ui.box.style.transform = `translate(${p.x}px, ${p.y}px)`; ui.box.dataset.x = p.x; ui.box.dataset.y = p.y; } catch {} }
    hd.addEventListener('pointerdown', (e) => {
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
      ui.box.style.transform = `translate(${x}px, ${y}px)`;
    });
    hd.addEventListener('pointerup', (e) => {
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
  ui.sel.onclick = () => (store.inspect ? exitInspect() : enterInspect());
  ui.mic.addEventListener('pointerdown', (e) => { e.preventDefault(); startTalk(); });
  ui.mic.addEventListener('pointerup', stopTalk);
  ui.mic.addEventListener('pointerleave', stopTalk);
  $('.perm .allow').onclick = () => replyPermission(true);
  $('.perm .deny').onclick = () => replyPermission(false);
  ui.input.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); send(); }
    e.stopPropagation(); // typing must never trigger guest-app shortcuts
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

  // The box TOP lands at the cursor (nudged 2px in), so the pointer sits
  // on the header — the drag handle — and follow hands off to click-drag.
  const followTargetFromPointer = (cx, cy) => {
    const left = Math.min(Math.max(cx - follow.w / 2, 8), innerWidth - follow.w - 8);
    const top = Math.min(Math.max(cy - 2, 8), innerHeight - follow.h - 8);
    follow.tx = left - follow.natX;
    follow.ty = top - follow.natY;
  };

  const startFollow = () => {
    if (follow || store.box === 'hidden') return;
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
    ui.box.style.transform = `translate(${f.x}px, ${f.y}px)`;
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
      if (store.inspect) { e.preventDefault(); e.stopPropagation(); modInspect = false; exitInspect(); }
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
  if (store.sessionId) subscribe(); // survive full reloads mid-turn
})();
