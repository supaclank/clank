// sourcecontrol.js — pure source-control logic for the preview overlay
// (github connection, remote sync state, PR creation). No DOM, no
// network: overlay.js owns those, this module owns request shapes and
// state presentation so `node --test sourcecontrol_test.mjs` covers the
// decision tables directly. Mirrors clank-mobile's SourceControlSheet.

// scRequest builds the wire call for a source-control operation. Refs
// with a worktree_id use the {id}-keyed REST routes (the only ones the
// hosted preview gateway allowlists, path-scoped to the route's
// worktree); local_path refs use the GitRef body-addressed routes.
// Throws on an op it doesn't know — no fallbacks.
export const scRequest = (op, gitRef, extra) => {
  const flowID = extra && extra.flow_id;
  switch (op) {
    case 'github-status':
      return { method: 'GET', path: '/credentials/github/status', body: null };
    case 'connect-start':
      return { method: 'POST', path: '/credentials/github/connect/start', body: null };
    case 'connect-status':
      return { method: 'GET', path: '/credentials/github/connect/status?flow_id=' + encodeURIComponent(flowID || ''), body: null };
    case 'connect-cancel':
      return { method: 'POST', path: '/credentials/github/connect/cancel?flow_id=' + encodeURIComponent(flowID || ''), body: null };
    case 'branches':
      return { method: 'POST', path: '/worktrees/list-branches', body: { git_ref: gitRef } };
  }
  if (!gitRef || !!gitRef.worktree_id === !!gitRef.local_path) {
    throw new Error('sourcecontrol: git ref must set exactly one of worktree_id/local_path');
  }
  if (gitRef.worktree_id) {
    const base = '/worktrees/' + encodeURIComponent(gitRef.worktree_id);
    switch (op) {
      case 'status': return { method: 'GET', path: base + '/remote/status', body: null };
      case 'push': return { method: 'POST', path: base + '/remote/push', body: null };
      case 'pull': return { method: 'POST', path: base + '/remote/pull', body: null };
      case 'resolve': return { method: 'POST', path: base + '/remote/resolve', body: { strategy: extra.strategy } };
      case 'publish': return { method: 'POST', path: base + '/remote/publish', body: extra };
      case 'create-pr': return { method: 'POST', path: base + '/pr', body: extra };
      case 'pr-ready': return { method: 'POST', path: base + '/pr/ready', body: null };
    }
  } else {
    switch (op) {
      case 'status': return { method: 'POST', path: '/worktrees/remote-status', body: { git_ref: gitRef } };
      case 'push': return { method: 'POST', path: '/worktrees/remote-push', body: { git_ref: gitRef } };
      case 'pull': return { method: 'POST', path: '/worktrees/remote-pull', body: { git_ref: gitRef } };
      case 'resolve': return { method: 'POST', path: '/worktrees/remote-resolve', body: { git_ref: gitRef, strategy: extra.strategy } };
      case 'publish': return { method: 'POST', path: '/worktrees/remote-publish', body: { git_ref: gitRef, ...extra } };
      case 'create-pr': return { method: 'POST', path: '/worktrees/create-pr', body: { git_ref: gitRef, ...extra } };
      case 'pr-ready': return { method: 'POST', path: '/worktrees/pr-ready', body: { git_ref: gitRef } };
    }
  }
  throw new Error('sourcecontrol: unknown op ' + op);
};

// presentStatus maps a RemoteStatusResult to the status card. tone:
// ok | neutral | warn | accent | danger. Unpushed work is deliberately
// neutral (dark heading, blue icon): it is the everyday state, not a
// warning — amber means diverged, red means conflict, nothing else.
export const presentStatus = (st) => {
  switch (st.state) {
    case 'synced':
      return { icon: 'ok', tone: 'ok', label: 'Up to date', detail: 'Your branch matches the remote.' };
    case 'unpushed':
      return {
        icon: 'up', tone: 'neutral', label: 'Unpushed work',
        detail: st.ahead > 0
          ? `${st.ahead} commit${st.ahead === 1 ? '' : 's'} not on the remote.`
          : 'Uncommitted changes to push.',
      };
    case 'no_upstream':
      return { icon: 'cloud', tone: 'neutral', label: 'Not pushed yet', detail: "This branch isn't on the remote." };
    case 'behind':
      return { icon: 'down', tone: 'accent', label: `${st.behind} behind`, detail: 'The remote has newer commits you can pull.' };
    case 'diverged':
      return { icon: 'diverged', tone: 'warn', label: `Diverged ↑${st.ahead} ↓${st.behind}`, detail: 'Both sides advanced — reconcile them.' };
    case 'conflict':
      return { icon: 'conflict', tone: 'danger', label: 'Merge conflict', detail: 'A merge is in progress with conflicts.' };
    default:
      return { icon: 'conflict', tone: 'warn', label: st.state || 'unknown', detail: '' };
  }
};

// actionsFor returns the action row for a status: {id, label, kind,
// ext?} with kind primary | secondary | danger; ext marks a control
// that leaves the page. The row renders left-to-right after the
// refresh button, primary last. Every state carries at least one verb
// so the refresh button never sits alone — for a synced, ready PR the
// verb IS the GitHub merge deep link (merging stays on GitHub: their
// button inherits branch protection, merge methods, and queues).
export const actionsFor = (st) => {
  switch (st.state) {
    case 'unpushed':
    case 'no_upstream':
      if (st.pr_number) {
        return [{ id: 'push', label: 'Push to remote', kind: 'primary' }];
      }
      return [
        { id: 'push', label: 'Push to remote', kind: 'secondary' },
        { id: 'open-pr', label: 'Create pull request', kind: 'primary' },
      ];
    case 'behind':
      return [{ id: 'pull', label: 'Pull (fast-forward)', kind: 'primary' }];
    case 'diverged':
      // Destructive last: this list renders as a vertical ⋯ menu.
      return [
        { id: 'merge-keep', label: 'Keep mine & merge', kind: 'secondary' },
        { id: 'take-remote', label: 'Discard mine', kind: 'danger' },
        { id: 'fix-agent', label: '✦ Fix with agent', kind: 'primary' },
      ];
    case 'conflict':
      return [
        { id: 'abort-merge', label: 'Abort merge', kind: 'secondary' },
        { id: 'fix-agent', label: '✦ Resolve with agent', kind: 'primary' },
      ];
    case 'synced':
      if (!st.pr_number) return [{ id: 'open-pr', label: 'Create pull request', kind: 'primary' }];
      if (st.pr_draft) return [{ id: 'pr-ready', label: 'Mark ready for review', kind: 'primary' }];
      if (st.pr_mergeable === 'conflicting') {
        return [{ id: 'pr-fix-agent', label: '✦ Fix with agent', kind: 'primary' }];
      }
      return [{ id: 'merge-github', label: 'Merge on GitHub', kind: 'primary', ext: true }];
    default:
      return [];
  }
};

// actionLayout splits an action list for the card's button row: two or
// fewer verbs render side by side with full labels; three or more
// collapse to the primary plus a ⋯ overflow menu (a vertical list, so
// the collapsed verbs keep their full labels no matter how many).
export const actionLayout = (actions) => {
  if (actions.length <= 2) return { buttons: actions, overflow: [] };
  return {
    buttons: actions.filter((a) => a.kind === 'primary'),
    overflow: actions.filter((a) => a.kind !== 'primary'),
  };
};

// headerPRFor returns the header PR pill model — shown in EVERY state
// the host knows of an open PR, so the deep link is always one click
// away. null when no PR is known (the branch name stands alone).
export const headerPRFor = (st) =>
  st.pr_number
    ? {
        number: st.pr_number,
        url: st.pr_url,
        draft: !!st.pr_draft,
        conflicting: st.pr_mergeable === 'conflicting',
      }
    : null;

// prConflictWarnFor returns the base-conflict warning (text only —
// the fix verb lives in the action row via actionsFor, and only in
// the synced state: any other state has a local step to finish first).
export const prConflictWarnFor = (st) =>
  st.pr_number && st.pr_mergeable === 'conflicting'
    ? { number: st.pr_number, baseBranch: st.pr_base_branch || '' }
    : null;

// chipFor computes the header pill: {text, tone, title} or null to
// hide it entirely (host has no GitHub integration). tone: muted |
// accent | warn | danger | neutral.
//
// connected wins over available: a laptop host borrowing the gh CLI
// login reports available:false (no device-flow client id) with
// connected:true, and that state pushes and opens PRs just fine.
export const chipFor = ({ gh, status, statusErrorCode, branchInfo }) => {
  if (gh && gh.available === false && !gh.connected) return null;
  if (!gh) return { text: '', tone: 'muted', title: 'source control' };
  if (!gh.connected) return { text: 'connect', tone: 'muted', title: 'Connect GitHub to ship your changes' };
  if (statusErrorCode === 'no_origin_remote') {
    return { text: 'publish', tone: 'accent', title: 'Publish this project to GitHub' };
  }
  if (statusErrorCode) return { text: 'git', tone: 'muted', title: 'source control' };
  if (!status) return { text: '', tone: 'muted', title: 'source control' };
  switch (status.state) {
    case 'conflict':
      return { text: 'conflicts', tone: 'danger', title: 'a merge is in progress with conflicts' };
    case 'diverged':
      return { text: `↑${status.ahead} ↓${status.behind}`, tone: 'warn', title: 'local and remote have diverged' };
    case 'behind':
      return { text: `↓${status.behind}`, tone: 'accent', title: 'the remote has newer commits' };
  }
  if (status.pr_number) {
    return {
      text: `PR #${status.pr_number}`,
      tone: status.pr_mergeable === 'conflicting' ? 'warn' : 'neutral',
      title: status.pr_mergeable === 'conflicting' ? 'pull request has conflicts with its base' : 'view pull request',
    };
  }
  const hasWork = status.state !== 'synced' || status.dirty;
  const branchDiff = branchInfo &&
    ((branchInfo.lines_added || 0) + (branchInfo.lines_removed || 0) + (branchInfo.commits_ahead || 0)) > 0;
  if (!(branchInfo && branchInfo.is_default) && (hasWork || branchDiff)) {
    return { text: 'Create PR', tone: 'accent', title: 'open a pull request with your changes' };
  }
  if (hasWork) {
    return { text: status.ahead > 0 ? `↑${status.ahead}` : 'unpushed', tone: 'warn', title: 'work not on the remote yet' };
  }
  return { text: 'in sync', tone: 'muted', title: 'your branch matches the remote' };
};

// diffstatParts returns {added: '+A', removed: '−R'} for a BranchInfo,
// or null when there is no line delta to show. Split so the renderer
// can color the halves green/red.
export const diffstatParts = (branchInfo) => {
  if (!branchInfo) return null;
  const a = branchInfo.lines_added || 0;
  const r = branchInfo.lines_removed || 0;
  if (a + r === 0) return null;
  return { added: `+${a}`, removed: `−${r}` };
};

// currentBranchInfo picks the list-branches entry for the status'
// branch, falling back to the checked-out entry when the status (and
// so the branch name) isn't known yet.
export const currentBranchInfo = (branches, branchName) => {
  const list = branches || [];
  return list.find((b) => branchName && b.name === branchName) ||
    list.find((b) => b.is_current) || null;
};

// defaultBaseBranch seeds the create-PR base field: the repo's default
// branch, unless that IS the current branch (no self-PRs). '' when
// unknown — the field stays required, never silently 'main'.
export const defaultBaseBranch = (branches, currentBranch) => {
  const def = (branches || []).find((b) => b.is_default);
  if (!def || def.name === currentBranch) return '';
  return def.name;
};

// seedPRTitle humanizes a branch name into a starting title.
export const seedPRTitle = (branch) => String(branch || '').replace(/[-_/]+/g, ' ').trim();

// friendlyRemoteError maps the host's machine codes to user-facing
// copy. Unknown codes surface the server message verbatim.
export const friendlyRemoteError = (code, message) => {
  switch (code) {
    case 'github_not_connected': return 'GitHub is not connected yet.';
    case 'github_unavailable': return 'GitHub integration is not enabled on this Clank instance.';
    case 'no_origin_remote': return 'This project is not on GitHub yet.';
    case 'remote_diverged': return 'The remote has new commits — reconcile before pushing.';
    case 'worktree_dirty': return 'Uncommitted changes block this pull.';
    case 'not_fast_forward': return 'The remote moved — refresh and reconcile.';
    case 'nothing_to_push': return 'Nothing to ship yet — the branch is up to date with its base.';
    case 'base_branch_not_found': return 'That base branch does not exist on the remote.';
    case 'no_common_ancestor': return 'The remote shares no history with this project — origin may point at the wrong repo.';
    case 'push_denied': return 'GitHub rejected the push — check your access to the repository.';
    case 'github_repo_not_accessible': return 'The repository is not accessible with the connected account.';
    case 'github_token_invalid': return 'The GitHub connection expired — reconnect and try again.';
    case 'repo_name_taken': return 'You already have a repository with that name. Try another.';
    case 'invalid_repo_name': return 'That repository name is not valid.';
    case 'already_published': return 'This project is already on GitHub — push instead.';
    case 'no_open_pr': return 'No open pull request for this branch anymore.';
    default: return message || 'Something went wrong.';
  }
};

// ---------- agent hand-off prompts ----------------------------------
// Ports of clank-mobile's agentFixPrompts. Publishing stays manual
// (product decision) — every prompt ends with do-not-push, and the
// panel's unpushed → Push flow takes over after the agent commits.

const DO_NOT_PUSH = "Do not push — I'll push it myself afterwards.";

// mergeInProgressPrompt: conflict markers are on disk; finish THAT merge.
export const mergeInProgressPrompt = () =>
  'A git merge is in progress in this worktree with conflicts. Open each ' +
  'conflicted file, reconcile the <<<<<<< / ======= / >>>>>>> sections so ' +
  "both sides' intent is preserved, remove the markers, and stage the " +
  'resolved files. Then finish the merge with `git commit --no-edit`. ' +
  DO_NOT_PUSH;

// divergedMergePrompt: local and origin/<branch> both advanced; the
// agent performs the merge itself.
export const divergedMergePrompt = (branch) =>
  `This branch (\`${branch}\`) has diverged from its remote: both the ` +
  `local branch and \`origin/${branch}\` have new commits. Fetch, then ` +
  `merge \`origin/${branch}\` into the local branch, resolving any ` +
  "conflicts so both sides' intent is preserved, and commit the merge. " +
  DO_NOT_PUSH;

// prConflictsPrompt: the branch's open PR conflicts with its base, so
// GitHub can't merge it and CI won't run. baseBranch may be absent —
// the agent then discovers the base from the PR itself.
export const prConflictsPrompt = ({ prNumber, branch, baseBranch }) => {
  const mergeStep = baseBranch
    ? `Fetch, then merge \`origin/${baseBranch}\` into \`${branch}\``
    : `Find PR #${prNumber}'s base branch (e.g. \`gh pr view ${prNumber} --json baseRefName --jq .baseRefName\`), ` +
      `fetch it, then merge its remote counterpart (e.g. \`origin/<base_branch>\`) into \`${branch}\``;
  return (
    `Pull request #${prNumber} for this branch (\`${branch}\`) has merge ` +
    `conflicts with its base branch${baseBranch ? ` \`${baseBranch}\`` : ''}, so it ` +
    `can't be merged and CI won't run. ${mergeStep}, resolving the ` +
    "conflicts so both sides' intent is preserved, and commit the merge " +
    `so the pull request becomes mergeable again. ${DO_NOT_PUSH}`
  );
};
