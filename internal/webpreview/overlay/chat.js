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

// chatFromMessages projects a Messages() refetch into overlay chat
// state: rolling transcript, last user message id (revert target), the
// pending question (only a trailing assistant message can carry one),
// and any plan texts for ExitPlanMode permission prompts.
export const chatFromMessages = (messages, cap) => {
  const msgs = [];
  let lastUserMsgId = '';
  let planParts = [];
  for (const m of messages || []) {
    const text = textFromParts(m.parts) || m.content || '';
    if (text) msgs.push({ role: m.role, text });
    if (m.role === 'user' && m.id) lastUserMsgId = m.id;
    planParts = collectPlanParts(planParts, m.parts);
  }
  const last = (messages || [])[(messages || []).length - 1];
  const question = last && last.role === 'assistant' ? activeQuestionFromParts(last.parts) : null;
  return { msgs: msgs.slice(-cap), lastUserMsgId, question, planParts };
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

// Mirrors internal/agent/presets BuiltinDefaultPrefix — the id contract
// a session create's Default-preset lookup must match on both clients.
export const BUILTIN_DEFAULT_PREFIX = 'builtin-default-';

// defaultPresetConfig picks the backend's built-in Default ("Build")
// preset from a GET /presets list and returns a copy of its config —
// the create-time bundle a session create must carry verbatim (the host
// 400s on missing keys and never fills values in). null when absent: the
// caller fails the send loudly instead of creating a config-less session.
export const defaultPresetConfig = (presetList, backend) => {
  if (!Array.isArray(presetList) || !backend) return null;
  const p = presetList.find((x) => x && x.backend === backend && x.id === BUILTIN_DEFAULT_PREFIX + backend);
  return p && p.config ? { ...p.config } : null;
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
