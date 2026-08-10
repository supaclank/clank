import test from 'node:test';
import assert from 'node:assert/strict';
import {
  scRequest,
  presentStatus,
  actionsFor,
  headerPRFor,
  prConflictWarnFor,
  chipFor,
  diffstatParts,
  currentBranchInfo,
  defaultBaseBranch,
  seedPRTitle,
  friendlyRemoteError,
  mergeInProgressPrompt,
  divergedMergePrompt,
  prConflictsPrompt,
} from './sourcecontrol.js';

const HOSTED = { worktree_id: 'wt one' };
const LOCAL = { local_path: '/home/u/app' };

test('scRequest uses {id}-keyed routes for hosted refs (gateway allowlist shape)', () => {
  assert.deepEqual(scRequest('status', HOSTED), {
    method: 'GET', path: '/worktrees/wt%20one/remote/status', body: null,
  });
  assert.deepEqual(scRequest('push', HOSTED), {
    method: 'POST', path: '/worktrees/wt%20one/remote/push', body: null,
  });
  assert.deepEqual(scRequest('resolve', HOSTED, { strategy: 'merge' }), {
    method: 'POST', path: '/worktrees/wt%20one/remote/resolve', body: { strategy: 'merge' },
  });
  assert.deepEqual(scRequest('create-pr', HOSTED, { title: 't', body: 'b', base: 'main', draft: true }), {
    method: 'POST', path: '/worktrees/wt%20one/pr', body: { title: 't', body: 'b', base: 'main', draft: true },
  });
  assert.deepEqual(scRequest('pr-ready', HOSTED), {
    method: 'POST', path: '/worktrees/wt%20one/pr/ready', body: null,
  });
});

test('scRequest uses GitRef body-addressed routes for local refs', () => {
  assert.deepEqual(scRequest('status', LOCAL), {
    method: 'POST', path: '/worktrees/remote-status', body: { git_ref: LOCAL },
  });
  assert.deepEqual(scRequest('resolve', LOCAL, { strategy: 'abort' }), {
    method: 'POST', path: '/worktrees/remote-resolve', body: { git_ref: LOCAL, strategy: 'abort' },
  });
  assert.deepEqual(scRequest('create-pr', LOCAL, { title: 't', body: '', base: 'main', draft: false }), {
    method: 'POST', path: '/worktrees/create-pr', body: { git_ref: LOCAL, title: 't', body: '', base: 'main', draft: false },
  });
  assert.deepEqual(scRequest('publish', LOCAL, { name: 'app', private: true }), {
    method: 'POST', path: '/worktrees/remote-publish', body: { git_ref: LOCAL, name: 'app', private: true },
  });
});

test('scRequest shared ops are mode-independent', () => {
  assert.deepEqual(scRequest('github-status'), {
    method: 'GET', path: '/credentials/github/status', body: null,
  });
  assert.deepEqual(scRequest('connect-status', null, { flow_id: 'f/1' }), {
    method: 'GET', path: '/credentials/github/connect/status?flow_id=f%2F1', body: null,
  });
  assert.deepEqual(scRequest('branches', LOCAL), {
    method: 'POST', path: '/worktrees/list-branches', body: { git_ref: LOCAL },
  });
});

test('scRequest rejects ambiguous refs and unknown ops', () => {
  assert.throws(() => scRequest('status', {}));
  assert.throws(() => scRequest('status', { worktree_id: 'a', local_path: '/b' }));
  assert.throws(() => scRequest('nope', HOSTED));
});

test('presentStatus maps every remote state (mobile parity)', () => {
  assert.equal(presentStatus({ state: 'synced' }).label, 'Up to date');
  assert.equal(presentStatus({ state: 'unpushed', ahead: 2 }).detail, '2 commits not on the remote.');
  assert.equal(presentStatus({ state: 'unpushed', ahead: 0 }).detail, 'Uncommitted changes to push.');
  assert.equal(presentStatus({ state: 'no_upstream' }).label, 'Not pushed yet');
  assert.equal(presentStatus({ state: 'behind', behind: 3 }).label, '3 behind');
  assert.equal(presentStatus({ state: 'diverged', ahead: 1, behind: 2 }).label, 'Diverged ↑1 ↓2');
  assert.equal(presentStatus({ state: 'conflict' }).tone, 'danger');
});

test('presentStatus keeps unpushed work calm: neutral, not amber', () => {
  assert.equal(presentStatus({ state: 'unpushed', ahead: 1 }).tone, 'neutral');
  assert.equal(presentStatus({ state: 'no_upstream' }).tone, 'neutral');
  assert.equal(presentStatus({ state: 'diverged', ahead: 1, behind: 1 }).tone, 'warn');
});

test('actionsFor drops the PR verb when a PR already exists (header pill covers it)', () => {
  const noPR = actionsFor({ state: 'unpushed' });
  assert.deepEqual(noPR.map((a) => a.id), ['push', 'open-pr']);
  assert.equal(noPR[1].label, 'Create pull request');
  assert.equal(noPR[1].kind, 'primary');
  assert.deepEqual(actionsFor({ state: 'unpushed', pr_number: 7 }), [
    { id: 'push', label: 'Push to remote', kind: 'primary' },
  ]);
});

test('actionsFor covers pull, diverged, conflict, and synced flows', () => {
  assert.deepEqual(actionsFor({ state: 'behind' }).map((a) => a.id), ['pull']);
  const div = actionsFor({ state: 'diverged' });
  assert.deepEqual(div.map((a) => a.id), ['take-remote', 'merge-keep', 'fix-agent']);
  assert.equal(div[0].kind, 'danger');
  assert.equal(div[2].kind, 'primary'); // primary renders last (rightmost)
  assert.deepEqual(actionsFor({ state: 'conflict' }).map((a) => a.id), ['abort-merge', 'fix-agent']);
  assert.deepEqual(actionsFor({ state: 'synced' }).map((a) => a.id), ['open-pr']);
  assert.deepEqual(actionsFor({ state: 'synced', pr_number: 7, pr_draft: true }).map((a) => a.id), ['pr-ready']);
});

test('actionsFor never leaves the refresh button alone: synced+PR gets the merge deep link', () => {
  const merge = actionsFor({ state: 'synced', pr_number: 7 });
  assert.deepEqual(merge, [{ id: 'merge-github', label: 'Merge on GitHub', kind: 'primary', ext: true }]);
  // A base-conflicting PR can't merge — the fix verb takes the slot.
  assert.deepEqual(
    actionsFor({ state: 'synced', pr_number: 7, pr_mergeable: 'conflicting' }).map((a) => a.id),
    ['pr-fix-agent'],
  );
});

test('headerPRFor shows the pill in every state that knows of a PR', () => {
  assert.equal(headerPRFor({ state: 'synced' }), null);
  assert.deepEqual(headerPRFor({ state: 'unpushed', pr_number: 7, pr_url: 'u' }), {
    number: 7, url: 'u', draft: false, conflicting: false,
  });
  assert.equal(headerPRFor({ state: 'synced', pr_number: 7, pr_mergeable: 'conflicting' }).conflicting, true);
  assert.equal(headerPRFor({ state: 'synced', pr_number: 7, pr_draft: true }).draft, true);
});

test('prConflictWarnFor is text-only and fires only on a known base conflict', () => {
  assert.equal(prConflictWarnFor({ state: 'synced', pr_number: 7 }), null);
  const warn = prConflictWarnFor({ state: 'synced', pr_number: 7, pr_mergeable: 'conflicting', pr_base_branch: 'main' });
  assert.deepEqual(warn, { number: 7, baseBranch: 'main' });
});

test('chipFor hides when github is unavailable and prompts connect otherwise', () => {
  assert.equal(chipFor({ gh: { available: false, connected: false } }), null);
  assert.deepEqual(chipFor({ gh: null }).text, '');
  assert.equal(chipFor({ gh: { available: true, connected: false } }).text, 'connect');
  assert.equal(chipFor({ gh: { available: true, connected: true }, statusErrorCode: 'no_origin_remote' }).text, 'publish');
});

test('chipFor works on laptop gh-CLI hosts: connected wins over available', () => {
  // --gh-cli-auth hosts report available:false (no device-flow client
  // id) with connected:true — pushes and PRs work with the borrowed
  // token, so the chip must show.
  const gh = { available: false, connected: true };
  const chip = chipFor({ gh, status: { state: 'unpushed', ahead: 1 }, branchInfo: { name: 'feat' } });
  assert.equal(chip.text, 'Create PR');
});

test('chipFor is the Create PR call to action exactly when there is PR-able work', () => {
  const gh = { available: true, connected: true };
  const chip = chipFor({ gh, status: { state: 'unpushed', ahead: 1, dirty: false }, branchInfo: { name: 'feat', is_default: false } });
  assert.deepEqual([chip.text, chip.tone], ['Create PR', 'accent']);
  // Committed-and-synced but ahead of the default branch still offers a PR.
  const synced = chipFor({ gh, status: { state: 'synced' }, branchInfo: { name: 'feat', commits_ahead: 2 } });
  assert.equal(synced.text, 'Create PR');
  // On the default branch there is no PR to create — sync state instead.
  const onMain = chipFor({ gh, status: { state: 'unpushed', ahead: 1 }, branchInfo: { name: 'main', is_default: true } });
  assert.equal(onMain.text, '↑1');
  assert.equal(chipFor({ gh, status: { state: 'synced' }, branchInfo: { name: 'main', is_default: true } }).text, 'in sync');
});

test('chipFor surfaces PR, conflict, and divergence states', () => {
  const gh = { available: true, connected: true };
  assert.equal(chipFor({ gh, status: { state: 'synced', pr_number: 12 } }).text, 'PR #12');
  assert.equal(chipFor({ gh, status: { state: 'synced', pr_number: 12, pr_mergeable: 'conflicting' } }).tone, 'warn');
  assert.equal(chipFor({ gh, status: { state: 'conflict' } }).text, 'conflicts');
  assert.equal(chipFor({ gh, status: { state: 'diverged', ahead: 1, behind: 2 } }).text, '↑1 ↓2');
  assert.equal(chipFor({ gh, status: { state: 'behind', behind: 4 } }).text, '↓4');
});

test('diffstat, branch info, base seeding, and title seeding', () => {
  assert.deepEqual(diffstatParts({ lines_added: 12, lines_removed: 3 }), { added: '+12', removed: '−3' });
  assert.equal(diffstatParts({ lines_added: 0, lines_removed: 0 }), null);
  assert.equal(diffstatParts(null), null);
  const branches = [
    { name: 'main', is_default: true },
    { name: 'feat', is_current: true, lines_added: 5 },
  ];
  assert.equal(currentBranchInfo(branches, 'feat').lines_added, 5);
  assert.equal(currentBranchInfo(branches, '').name, 'feat');
  assert.equal(defaultBaseBranch(branches, 'feat'), 'main');
  assert.equal(defaultBaseBranch(branches, 'main'), '');
  assert.equal(defaultBaseBranch([], 'feat'), '');
  assert.equal(seedPRTitle('fix-login_flow'), 'fix login flow');
});

test('friendlyRemoteError maps codes and passes unknowns through', () => {
  assert.match(friendlyRemoteError('repo_name_taken', ''), /already have a repository/);
  assert.equal(friendlyRemoteError('weird_code', 'server said no'), 'server said no');
  assert.equal(friendlyRemoteError('', ''), 'Something went wrong.');
});

test('agent hand-off prompts keep publishing manual', () => {
  for (const p of [
    mergeInProgressPrompt(),
    divergedMergePrompt('feat'),
    prConflictsPrompt({ prNumber: 7, branch: 'feat', baseBranch: 'main' }),
    prConflictsPrompt({ prNumber: 7, branch: 'feat' }),
  ]) {
    assert.match(p, /Do not push — I'll push it myself afterwards\.$/);
  }
  assert.match(divergedMergePrompt('feat'), /origin\/feat/);
  assert.match(prConflictsPrompt({ prNumber: 7, branch: 'feat' }), /gh pr view 7/);
});
