// chat.js — pure chat-protocol logic for the preview overlay
// (docs/chat-client-spec: interactive questions [QST-001..003], plan
// review, permission queueing, transcript reconcile). No DOM, no
// network: overlay.js owns those, this module owns the decisions, so
// `node --test chat_test.mjs` covers the protocol edges directly.

// Claude's plan-approval tool; its permission prompt renders as a plan
// review card instead of a generic allow/deny (internal/agent
// ClaudeToolExitPlanMode).
export const PLAN_TOOL = 'ExitPlanMode';

// textFromParts joins a message's text parts (assistant transcript text).
export const textFromParts = (parts) =>
  (parts || []).filter((p) => p.type === 'text' && p.text).map((p) => p.text).join('');

// activeQuestionFromParts returns {partId, prompt} for a question tool
// part still awaiting an answer, or null. Answerability is positional
// [QST-002]: the tagged part must be the last content — paired tool
// results, reasoning, and empty text placeholders don't count as the
// conversation moving on; anything else after it does. status "error"
// (deny/abort fallout) retires it; "completed" does NOT (a bypass-mode
// question auto-runs to completed while still awaiting the answer).
export const activeQuestionFromParts = (parts) => {
  const list = parts || [];
  for (let i = list.length - 1; i >= 0; i--) {
    const p = list[i];
    if (p.type === 'tool_result' || p.type === 'thinking') continue;
    if (p.type === 'text' && !(p.text || '').trim()) continue;
    if (p.type !== 'tool_call' || !p.question || !(p.question.questions || []).length) return null;
    if (p.status === 'error') return null;
    return { partId: p.id, prompt: p.question };
  }
  return null;
};

const TOOL_STATUS_RANK = { pending: 0, running: 1, completed: 2, error: 2, canceled: 2 };

const advancedToolStatus = (current, incoming) => {
  if (!incoming) return current || 'pending';
  if (!current) return incoming;
  return (TOOL_STATUS_RANK[incoming] ?? 0) >= (TOOL_STATUS_RANK[current] ?? 0) ? incoming : current;
};

const capped = (rows, cap) => rows.length > cap ? rows.slice(-cap) : rows;

// upsertTranscriptPart applies one live or settled part to the flat transcript.
// Tool calls/results merge by id across message roles; text/thinking honor delta
// vs snapshot semantics. The returned array never mutates the caller's state.
export const upsertTranscriptPart = (rows, part, role, isDelta, cap) => {
  if (!part || !part.type) return rows;
  const id = part.id || '';
  if (part.type === 'text' || part.type === 'thinking') {
    if (!part.text) return rows;
    const kind = part.type === 'text' ? 'text' : 'thinking';
    const idx = id ? rows.findIndex((row) => row.kind === kind && row.id === id) : -1;
    const prior = idx >= 0 ? rows[idx] : null;
    const incomingText = isDelta && prior ? prior.text + part.text : part.text;
    const text = prior && !isDelta && incomingText.length < prior.text.length ? prior.text : incomingText;
    const next = kind === 'text'
      ? { kind, id, role, text }
      : { kind, id, text };
    if (idx < 0) return capped(rows.concat(next), cap);
    const out = rows.slice();
    out[idx] = next;
    return out;
  }
  if (part.type !== 'tool_call' && part.type !== 'tool_result') return rows;
  const idx = id ? rows.findIndex((row) => row.kind === 'tool' && row.id === id) : -1;
  const prior = idx >= 0 ? rows[idx] : null;
  const next = {
    kind: 'tool',
    id,
    tool: part.tool || (prior && prior.tool) || 'tool',
    status: advancedToolStatus(prior && prior.status, part.status),
    input: part.input !== undefined ? part.input : prior ? prior.input : undefined,
    output: part.output !== undefined ? part.output : prior ? prior.output : undefined,
  };
  if (idx < 0) return capped(rows.concat(next), cap);
  const out = rows.slice();
  out[idx] = next;
  return out;
};

// createStreamPartTracker synthesizes a stable id for a run of id-less
// streaming parts of the same type, so consecutive delta chunks merge into
// one row instead of each becoming its own [upsertTranscriptPart]. Call
// boundary() on any status or message event that settles the turn, so a
// later id-less stream starts a fresh id instead of reusing a stale one
// left open by a turn that never sent a final non-delta chunk.
export const createStreamPartTracker = () => {
  let seq = 0;
  let open = { type: null, id: '' };
  return {
    resolve(rawPart, isDelta) {
      if (rawPart.id) return rawPart;
      if (open.type !== rawPart.type) open = { type: rawPart.type, id: `stream:${rawPart.type}:${++seq}` };
      const p = { ...rawPart, id: open.id };
      if (!isDelta) open = { type: null, id: '' };
      return p;
    },
    boundary() { open = { type: null, id: '' }; },
  };
};

export const toolSummary = (tool) => {
  const input = tool && tool.input;
  if (input && typeof input === 'object') {
    for (const key of ['filePath', 'file_path', 'path', 'file', 'command', 'description']) {
      if (typeof input[key] === 'string') return input[key];
    }
  }
  return tool && typeof tool.text === 'string' ? tool.text.slice(0, 100) : '';
};

// chatFromMessages projects a Messages() refetch into ordered transcript rows.
// A user-role tool-result carrier contributes no user bubble: its result merges
// into the earlier assistant tool card by part id [DATA-022].
// TODO(ai-review): O(n^2) on reconnect (each part folds through an O(n) upsertTranscriptPart) — needs an ID-indexed merge for long-lived sessions. https://github.com/supaclank/clank/pull/263#discussion_r3808254503
export const chatFromMessages = (messages, cap) => {
  let msgs = [];
  let lastUserMsgId = '';
  let planParts = [];
  for (const [messageIndex, m] of (messages || []).entries()) {
    const parts = m.parts || [];
    let hasTextPart = false;
    for (const [partIndex, rawPart] of parts.entries()) {
      if (rawPart.type === 'text' && rawPart.text) hasTextPart = true;
      if (!['text', 'thinking', 'tool_call', 'tool_result'].includes(rawPart.type)) continue;
      const part = rawPart.id ? rawPart : {
        ...rawPart,
        id: `${m.id || `message-${messageIndex}`}:${rawPart.type}:${partIndex}`,
      };
      msgs = upsertTranscriptPart(msgs, part, m.role, false, Number.MAX_SAFE_INTEGER);
    }
    const fallbackText = !hasTextPart && m.content ? m.content : '';
    if (fallbackText) {
      msgs = upsertTranscriptPart(msgs, {
        id: `${m.id || `message-${messageIndex}`}:content`, type: 'text', text: fallbackText,
      }, m.role, false, Number.MAX_SAFE_INTEGER);
    }
    if (m.role === 'user' && m.id && (hasTextPart || fallbackText)) lastUserMsgId = m.id;
    planParts = collectPlanParts(planParts, parts);
  }
  const last = (messages || [])[(messages || []).length - 1];
  const question = last && last.role === 'assistant' ? activeQuestionFromParts(last.parts) : null;
  return { msgs: capped(msgs, cap), lastUserMsgId, question, planParts };
};

// questionSuppressesPermission reports whether a permission prompt is
// superseded by the question card for the same request [QST-003] —
// Claude's gated modes emit both; the reply endpoint resolves the
// parked prompt server-side.
export const questionSuppressesPermission = (question, perm) =>
  !!question &&
  (perm.request_id === question.request_id ||
    (!!perm.tool_use_id && perm.tool_use_id === question.partId));

// pushPermission queues a permission prompt: duplicates (same
// request_id) and prompts the active question supersedes are dropped,
// everything else appends in arrival order.
export const pushPermission = (perms, perm, question) => {
  if (!perm || !perm.request_id) return perms;
  if (questionSuppressesPermission(question, perm)) return perms;
  if (perms.some((p) => p.request_id === perm.request_id)) return perms;
  return perms.concat(perm);
};

// dropPermission removes a queued prompt by request id without replying
// (its question card was answered; the backend resolves the parked
// prompt through the question reply).
export const dropPermission = (perms, requestID) =>
  perms.filter((p) => p.request_id !== requestID);

// customAllowed reports whether a question accepts a free-text answer.
// Tri-state on the wire: absent means the provider didn't say, treated
// as allowed (the universal default).
export const customAllowed = (q) => q.allow_custom !== false;

// toggleSelection returns the new selected-index set after choosing
// idx: multi-select toggles; single-select replaces (picking the same
// option again clears it).
export const toggleSelection = (question, sel, idx) => {
  const next = new Set(sel);
  if (next.has(idx)) {
    next.delete(idx);
    return next;
  }
  if (!question.multi_select) next.clear();
  next.add(idx);
  return next;
};

// buildAnswers converts UI selection state into the reply payload: one
// answer per question, in order; an all-empty answer delegates that
// question back to the agent [QST-001].
export const buildAnswers = (questions, sel, custom) =>
  questions.map((q, i) => {
    const selected = (q.options || [])
      .map((o, idx) => ((sel[i] || new Set()).has(idx) ? o.label : ''))
      .filter(Boolean);
    const answer = {};
    if (selected.length) answer.selected = selected;
    const c = (custom[i] || '').trim();
    if (c) answer.custom = c;
    return answer;
  });

// collectPlanParts folds any ExitPlanMode tool calls from parts into
// the plan list (newest last, re-emits replace by part id, capped — a
// permission prompt only ever references a recent plan).
export const collectPlanParts = (planParts, parts) => {
  let out = planParts;
  for (const p of parts || []) {
    if (p.type !== 'tool_call' || p.tool !== PLAN_TOOL) continue;
    const plan = p.input && typeof p.input.plan === 'string' ? p.input.plan : '';
    if (!plan) continue;
    out = out.filter((e) => e.id !== p.id).concat({ id: p.id, plan });
    if (out.length > 4) out = out.slice(-4);
  }
  return out;
};

// previewGitRef builds the one explicit repo identity a headless session
// create carries. Both-or-neither is invalid rather than silently preferred.
export const previewGitRef = (config) => {
  const worktreeID = config && config.worktree_id;
  const localPath = config && config.local_path;
  if (!!worktreeID === !!localPath) return null;
  return worktreeID ? { worktree_id: worktreeID } : { local_path: localPath };
};

// initialSessionId picks the session the overlay boots into. A config
// session id the tab hasn't adopted yet wins over sessionStorage — that
// is `clank preview --attach` addressing a possibly-stale tab. Once
// adopted (adoptedCfgSessionId matches), the tab's own choice rules
// again, so switching or creating a session survives reloads within
// the same preview run.
export const initialSessionId = (cfgSessionId, storedSessionId, adoptedCfgSessionId) => {
  if (cfgSessionId && cfgSessionId !== adoptedCfgSessionId) return cfgSessionId;
  return storedSessionId || cfgSessionId || '';
};

// planTextFor returns the plan text an ExitPlanMode permission prompt
// gates: matched by tool_use id, falling back to the most recent plan
// when the backend couldn't attribute one (mirrors the TUI golden ref).
export const planTextFor = (planParts, perm) => {
  for (let i = planParts.length - 1; i >= 0; i--) {
    if (!perm.tool_use_id || planParts[i].id === perm.tool_use_id) return planParts[i].plan;
  }
  return planParts.length ? planParts[planParts.length - 1].plan : '';
};

// ---------- send-time context serialization --------------------------
// Chips are the overlay's attached context: {label, detail, html?,
// text?, comment?}. `text` anchors a chip to a page text selection,
// `html` to a ⌘-selected element, and `comment` turns either into an
// inline comment — an instruction pinned to that anchor.

// COMMENTS_DEFAULT_PROMPT is the message when the user submits inline
// comments with an empty composer — the comments are the instructions.
export const COMMENTS_DEFAULT_PROMPT = 'Address the attached inline comments.';

// hasComments reports whether any chip carries an inline comment.
export const hasComments = (chips) => (chips || []).some((c) => c && c.comment);

// composerTextForSend returns the message text a send should carry: the
// typed text, or the default instruction when only inline comments ride
// along. Empty means there is nothing to send.
export const composerTextForSend = (typed, chips) => {
  const t = (typed || '').trim();
  if (t) return t;
  return hasComments(chips) ? COMMENTS_DEFAULT_PROMPT : '';
};

// chipContextLines serializes one chip: its anchor (element source /
// dom path, or the quoted text selection), then the user's comment.
const chipContextLines = (c, i) => {
  const lines = [`${i + 1}. ${c.detail}`];
  if (c.text) {
    lines.push('   selected text:');
    for (const l of String(c.text).split('\n')) lines.push('   > ' + l);
  }
  if (c.html) lines.push(`   html: ${c.html}`);
  if (c.comment) {
    const parts = String(c.comment).split('\n');
    lines.push(`   comment: ${parts[0]}`);
    for (const l of parts.slice(1)) lines.push('   ' + l);
  }
  return lines;
};

// buildPreviewContext renders the auto-attached context block appended
// to every send: inline comments first (each pinned to its anchor, the
// agent addresses them in order), then plain attached context, then
// page facts. Empty when there is nothing to attach. images is the
// staged attachments' filenames.
export const buildPreviewContext = ({ chips = [], images = [], route = '', viewport = '', errors = [] }) => {
  if (!chips.length && !images.length && !errors.length) return '';
  const comments = chips.filter((c) => c.comment);
  const plain = chips.filter((c) => !c.comment);
  const lines = ['', '', '--- clank preview context (auto-attached by the web overlay) ---'];
  if (comments.length) {
    lines.push('Inline comments — address each one:');
    comments.forEach((c, i) => lines.push(...chipContextLines(c, i)));
  }
  if (plain.length) {
    lines.push('Attached context:');
    plain.forEach((c, i) => lines.push(...chipContextLines(c, i)));
  }
  if (images.length) {
    const grabNote = images.includes('screenshot.png') ? ' (screenshot.png = an area grab of the page as currently rendered)' : '';
    lines.push(`Attached images: ${images.join(', ')}${grabNote}.`);
  }
  if (route) lines.push(`Route: ${route}`);
  if (viewport) lines.push(`Viewport: ${viewport}`);
  if (errors.length) {
    lines.push('Recent console errors:');
    errors.slice(-3).forEach((e) => lines.push('- ' + e));
  }
  lines.push('--- end context ---');
  return lines.join('\n');
};
