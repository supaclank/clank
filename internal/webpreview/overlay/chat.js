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

// planTextFor returns the plan text an ExitPlanMode permission prompt
// gates: matched by tool_use id, falling back to the most recent plan
// when the backend couldn't attribute one (mirrors the TUI golden ref).
export const planTextFor = (planParts, perm) => {
  for (let i = planParts.length - 1; i >= 0; i--) {
    if (!perm.tool_use_id || planParts[i].id === perm.tool_use_id) return planParts[i].plan;
  }
  return planParts.length ? planParts[planParts.length - 1].plan : '';
};
