// Protocol tests for chat.js — the QST/ITOOL/INV edges from
// docs/chat-client-spec (11-interactive-tools, 08-invariants). Run via
// `node --test chat_test.mjs` (TestOverlayChatJS wires this into go test).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  activeQuestionFromParts, chatFromMessages, questionSuppressesPermission,
  pushPermission, dropPermission, customAllowed, toggleSelection,
  buildAnswers, collectPlanParts, planTextFor, textFromParts,
  defaultPresetConfig,
} from './chat.js';

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

// Session create must carry the Default preset's config verbatim — the
// host 400s (config_incomplete) otherwise and never fills values in.
test('defaultPresetConfig: picks builtin-default-<backend>, copies its config', () => {
  const presets = [
    { id: 'builtin-default-claude-code', backend: 'claude-code', config: { mode: 'acceptEdits', model: 'default', effort: 'default' } },
    { id: 'builtin-plan-claude-code', backend: 'claude-code', config: { mode: 'plan', model: 'default', effort: 'default' } },
    { id: 'my-preset', backend: 'claude-code', config: { mode: 'bypassPermissions' } },
  ];
  const cfg = defaultPresetConfig(presets, 'claude-code');
  assert.deepEqual(cfg, { mode: 'acceptEdits', model: 'default', effort: 'default' });
  cfg.mode = 'mutated';
  assert.equal(presets[0].config.mode, 'acceptEdits'); // a copy, not the list's object
});

test('defaultPresetConfig: Plan and user presets never stand in for Default', () => {
  assert.equal(defaultPresetConfig([
    { id: 'builtin-plan-claude-code', backend: 'claude-code', config: { mode: 'plan' } },
    { id: 'my-preset', backend: 'claude-code', config: { mode: 'bypassPermissions' } },
  ], 'claude-code'), null);
});

test('defaultPresetConfig: wrong backend, malformed list, missing backend → null', () => {
  const other = [{ id: 'builtin-default-opencode', backend: 'opencode', config: { mode: 'build' } }];
  assert.equal(defaultPresetConfig(other, 'claude-code'), null);
  // Defense against a spoofed id claiming another backend's default slot.
  assert.equal(defaultPresetConfig([{ id: 'builtin-default-claude-code', backend: 'opencode', config: { mode: 'build' } }], 'claude-code'), null);
  assert.equal(defaultPresetConfig(null, 'claude-code'), null);
  assert.equal(defaultPresetConfig({ error: 'nope' }, 'claude-code'), null);
  assert.equal(defaultPresetConfig([{ id: 'builtin-default-claude-code', backend: 'claude-code' }], 'claude-code'), null); // no config field
  assert.equal(defaultPresetConfig(other, ''), null);
});
