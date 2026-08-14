// Protocol tests for chat.js — the QST/ITOOL/INV edges from
// docs/chat-client-spec (11-interactive-tools, 08-invariants). Run via
// `node --test chat_test.mjs` (TestOverlayChatJS wires this into go test).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  activeQuestionFromParts, chatFromMessages, questionSuppressesPermission,
  pushPermission, dropPermission, customAllowed, toggleSelection,
  buildAnswers, collectPlanParts, planTextFor, textFromParts,
  buildPreviewContext, composerTextForSend,
  previewGitRef, initialSessionId,
  COMMENTS_DEFAULT_PROMPT,
} from './chat.js';

test('previewGitRef: cloud and local previews use their explicit identity', () => {
  assert.deepEqual(previewGitRef({ worktree_id: 'wt-123' }), { worktree_id: 'wt-123' });
  assert.deepEqual(previewGitRef({ local_path: '/repo' }), { local_path: '/repo' });
  assert.equal(previewGitRef({}), null);
  assert.equal(previewGitRef({ worktree_id: 'wt-123', local_path: '/repo' }), null);
});

const prompt = (id) => ({
  request_id: id,
  questions: [{ text: 'Which auth?', header: 'Auth', options: [{ label: 'JWT' }, { label: 'Session' }] }],
});
const qPart = (partId, requestId, extra) => ({
  id: partId, type: 'tool_call', tool: 'AskUserQuestion', status: 'completed',
  question: prompt(requestId), ...extra,
});

test('activeQuestionFromParts: trailing tagged part is answerable', () => {
  const q = activeQuestionFromParts([{ id: 't1', type: 'text', text: 'hi' }, qPart('p1', 'q-p1')]);
  assert.equal(q.partId, 'p1');
  assert.equal(q.prompt.request_id, 'q-p1');
});

test('activeQuestionFromParts: text after the tag retires it (QST-002)', () => {
  assert.equal(activeQuestionFromParts([qPart('p1', 'q-p1'), { id: 't1', type: 'text', text: 'moving on' }]), null);
});

test('activeQuestionFromParts: a later tool call retires it (QST-002)', () => {
  assert.equal(activeQuestionFromParts([qPart('p1', 'q-p1'), { id: 'p2', type: 'tool_call', tool: 'Bash' }]), null);
});

test('activeQuestionFromParts: paired tool_result, thinking, empty text do not retire', () => {
  const q = activeQuestionFromParts([
    qPart('p1', 'q-p1'),
    { id: 'p1', type: 'tool_result', output: 'ok' },
    { id: 'th', type: 'thinking', text: 'hmm' },
    { id: 't2', type: 'text', text: '  ' },
  ]);
  assert.equal(q.partId, 'p1');
});

test('activeQuestionFromParts: error status retires; completed does not', () => {
  assert.equal(activeQuestionFromParts([qPart('p1', 'q-p1', { status: 'error' })]), null);
  assert.notEqual(activeQuestionFromParts([qPart('p1', 'q-p1', { status: 'completed' })]), null);
});

test('activeQuestionFromParts: empty questions or no tag → null', () => {
  assert.equal(activeQuestionFromParts([{ id: 'p1', type: 'tool_call', tool: 'Bash' }]), null);
  assert.equal(activeQuestionFromParts([{ id: 'p1', type: 'tool_call', question: { request_id: 'x', questions: [] } }]), null);
  assert.equal(activeQuestionFromParts([]), null);
});

test('chatFromMessages: transcript, revert target, trailing question', () => {
  const c = chatFromMessages([
    { id: 'u1', role: 'user', content: 'do the thing' },
    { id: 'a1', role: 'assistant', parts: [{ id: 't', type: 'text', text: 'which way?' }, qPart('p1', 'q-p1')] },
  ], 30);
  assert.deepEqual(c.msgs, [{ role: 'user', text: 'do the thing' }, { role: 'assistant', text: 'which way?' }]);
  assert.equal(c.lastUserMsgId, 'u1');
  assert.equal(c.question.partId, 'p1');
});

test('chatFromMessages: a user message after the tag means no pending question (QST-002)', () => {
  const c = chatFromMessages([
    { id: 'a1', role: 'assistant', parts: [qPart('p1', 'q-p1')] },
    { id: 'u2', role: 'user', content: 'never mind' },
  ], 30);
  assert.equal(c.question, null);
});

test('chatFromMessages: caps the transcript window', () => {
  const many = Array.from({ length: 40 }, (_, i) => ({ id: `u${i}`, role: 'user', content: `m${i}` }));
  const c = chatFromMessages(many, 30);
  assert.equal(c.msgs.length, 30);
  assert.equal(c.msgs[0].text, 'm10');
  assert.equal(c.lastUserMsgId, 'u39');
});

test('question suppresses its gating permission by request id and tool_use id (QST-003)', () => {
  const q = { request_id: 'r1', partId: 'p1' };
  assert.ok(questionSuppressesPermission(q, { request_id: 'r1', tool: 'AskUserQuestion' }));
  assert.ok(questionSuppressesPermission(q, { request_id: 'other', tool_use_id: 'p1' }));
  assert.ok(!questionSuppressesPermission(q, { request_id: 'other', tool_use_id: 'p2' }));
  assert.ok(!questionSuppressesPermission(null, { request_id: 'r1' }));
});

test('pushPermission queues without overwriting; dedupes; suppression drops (INV-PERM-SINGLEFLIGHT-001)', () => {
  let perms = pushPermission([], { request_id: 'r1', tool: 'Bash' }, null);
  perms = pushPermission(perms, { request_id: 'r2', tool: 'Edit' }, null);
  assert.deepEqual(perms.map((p) => p.request_id), ['r1', 'r2']); // the first prompt survives the second
  perms = pushPermission(perms, { request_id: 'r1', tool: 'Bash' }, null);
  assert.equal(perms.length, 2);
  perms = pushPermission(perms, { request_id: 'r3', tool_use_id: 'p1' }, { request_id: 'rq', partId: 'p1' });
  assert.equal(perms.length, 2);
  assert.deepEqual(dropPermission(perms, 'r1').map((p) => p.request_id), ['r2']);
});

test('customAllowed: tri-state, absent means allowed', () => {
  assert.ok(customAllowed({}));
  assert.ok(customAllowed({ allow_custom: true }));
  assert.ok(!customAllowed({ allow_custom: false }));
});

test('toggleSelection: single-select replaces, re-pick clears; multi toggles', () => {
  const single = { multi_select: false };
  let sel = toggleSelection(single, new Set(), 0);
  assert.deepEqual([...sel], [0]);
  sel = toggleSelection(single, sel, 1);
  assert.deepEqual([...sel], [1]); // replaced, not added
  sel = toggleSelection(single, sel, 1);
  assert.deepEqual([...sel], []); // re-pick clears

  const multi = { multi_select: true };
  sel = toggleSelection(multi, new Set([0]), 1);
  assert.deepEqual([...sel].sort(), [0, 1]);
  sel = toggleSelection(multi, sel, 0);
  assert.deepEqual([...sel], [1]);
});

test('buildAnswers: labels in option order, custom text, delegation stays empty (QST-001)', () => {
  const questions = [
    { text: 'q1', options: [{ label: 'A' }, { label: 'B' }, { label: 'C' }] },
    { text: 'q2', options: [{ label: 'X' }] },
    { text: 'q3', options: [{ label: 'Y' }] },
  ];
  const answers = buildAnswers(questions, [new Set([2, 0]), new Set(), new Set()], ['', '  custom  ', '']);
  assert.deepEqual(answers, [{ selected: ['A', 'C'] }, { custom: 'custom' }, {}]);
});

test('plan parts: collected from tool calls, re-emits replace, capped', () => {
  let plans = collectPlanParts([], [{ id: 'p1', type: 'tool_call', tool: 'ExitPlanMode', input: { plan: 'v1' } }]);
  plans = collectPlanParts(plans, [{ id: 'p1', type: 'tool_call', tool: 'ExitPlanMode', input: { plan: 'v2' } }]);
  assert.deepEqual(plans, [{ id: 'p1', plan: 'v2' }]);
  plans = collectPlanParts(plans, [
    { id: 'x', type: 'tool_call', tool: 'Bash', input: { plan: 'not a plan tool' } },
    { id: 'p2', type: 'tool_call', tool: 'ExitPlanMode', input: {} }, // no plan text — skipped
  ]);
  assert.equal(plans.length, 1);
  for (let i = 3; i <= 8; i++) plans = collectPlanParts(plans, [{ id: `p${i}`, type: 'tool_call', tool: 'ExitPlanMode', input: { plan: `v${i}` } }]);
  assert.equal(plans.length, 4);
});

test('planTextFor: tool_use id match wins, newest as fallback', () => {
  const plans = [{ id: 'p1', plan: 'old' }, { id: 'p2', plan: 'new' }];
  assert.equal(planTextFor(plans, { tool_use_id: 'p1' }), 'old');
  assert.equal(planTextFor(plans, { tool_use_id: '' }), 'new');
  assert.equal(planTextFor(plans, { tool_use_id: 'unknown' }), 'new');
  assert.equal(planTextFor([], { tool_use_id: 'p1' }), '');
});

test('textFromParts joins text parts only', () => {
  assert.equal(textFromParts([{ type: 'text', text: 'a' }, { type: 'tool_call' }, { type: 'text', text: 'b' }]), 'ab');
  assert.equal(textFromParts(undefined), '');
});

// ---------- send-time context serialization --------------------------

test('buildPreviewContext: comments numbered with anchors, plain chips separate', () => {
  const ctx = buildPreviewContext({
    chips: [
      { label: '“Getting sta…”', detail: 'text selection on /README.md', text: 'Getting started', comment: 'make this friendlier' },
      { label: 'app.jsx:12', detail: 'src/app.jsx:12:3', html: '<h1>Hi</h1>', comment: 'wrong copy' },
      { label: '<p>', detail: 'div > p', html: '<p>x</p>' },
    ],
    route: '/README.md',
    viewport: '800x600',
  });
  assert.match(ctx, /Inline comments — address each one:/);
  assert.match(ctx, /1\. text selection on \/README\.md/);
  assert.match(ctx, /> Getting started/);
  assert.match(ctx, /comment: make this friendlier/);
  assert.match(ctx, /2\. src\/app\.jsx:12:3/);
  assert.match(ctx, /html: <h1>Hi<\/h1>/);
  assert.match(ctx, /comment: wrong copy/);
  // The commentless chip lands under its own heading, not the comments.
  assert.match(ctx, /Attached context:\n1\. div > p/);
  assert.match(ctx, /Route: \/README\.md/);
  assert.match(ctx, /Viewport: 800x600/);
});

test('buildPreviewContext: multi-line selections quote every line', () => {
  const ctx = buildPreviewContext({
    chips: [{ label: 'x', detail: 'text selection on /a.txt', text: 'line one\nline two', comment: 'tighten' }],
  });
  assert.match(ctx, /   > line one\n   > line two/);
});

test('buildPreviewContext: multi-line comments (Shift+Enter) keep their lines indented', () => {
  const ctx = buildPreviewContext({
    chips: [{ label: 'x', detail: 'div > p', html: '<p>x</p>', comment: 'first line\nsecond line' }],
  });
  assert.match(ctx, /   comment: first line\n   second line/);
});

test('buildPreviewContext: nothing attached yields the empty string', () => {
  assert.equal(buildPreviewContext({}), '');
  assert.equal(buildPreviewContext({ chips: [], images: [], errors: [], route: '/x', viewport: '1x1' }), '');
});

test('buildPreviewContext: images and errors keep their notes', () => {
  const ctx = buildPreviewContext({
    images: ['screenshot.png', 'logo.png'],
    errors: ['e1', 'e2', 'e3', 'e4'],
  });
  assert.match(ctx, /Attached images: screenshot\.png, logo\.png \(screenshot\.png = an area grab/);
  // Only the most recent three errors ride along.
  assert.doesNotMatch(ctx, /- e1/);
  assert.match(ctx, /- e2\n- e3\n- e4/);
});

test('composerTextForSend: typed text wins; comments alone get the default; plain chips do not', () => {
  assert.equal(composerTextForSend('fix it', [{ comment: 'x' }]), 'fix it');
  assert.equal(composerTextForSend('  ', [{ comment: 'x' }]), COMMENTS_DEFAULT_PROMPT);
  assert.equal(composerTextForSend('', [{ label: 'plain' }]), '');
  assert.equal(composerTextForSend('', []), '');
});

// --attach regression: a stale tab's sessionStorage must not silently
// swallow the session id the CLI injected via config.
test('initialSessionId: an unadopted config session id beats a stale stored one', () => {
  assert.equal(initialSessionId('cfg1', 'stale', ''), 'cfg1');
  assert.equal(initialSessionId('cfg2', 'stale', 'cfg1'), 'cfg2');
});

test('initialSessionId: an adopted config id defers to the tab\'s own choice', () => {
  // Same preview run, user switched sessions in the overlay, reload.
  assert.equal(initialSessionId('cfg1', 'user-picked', 'cfg1'), 'user-picked');
  // Adopted but nothing stored (storage cleared): fall back to the config id.
  assert.equal(initialSessionId('cfg1', '', 'cfg1'), 'cfg1');
});

test('initialSessionId: no config id keeps the pre---attach behavior', () => {
  assert.equal(initialSessionId('', 'stored', ''), 'stored');
  assert.equal(initialSessionId('', '', ''), '');
});
