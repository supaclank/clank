import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  launcherActivity,
  launcherShortcut,
  shouldShowLauncherCoachmark,
} from './launcher.js';

test('first-run coachmark remains until the launcher is acknowledged', () => {
  assert.equal(shouldShowLauncherCoachmark(false), true);
  assert.equal(shouldShowLauncherCoachmark(true), false);
});

test('shortcut copy is platform-specific but click remains the primary path', () => {
  assert.equal(launcherShortcut(true), '⌘E');
  assert.equal(launcherShortcut(false), 'Ctrl E');
});

test('launcher activity gives thinking and working explicit busy states', () => {
  assert.deepEqual(launcherActivity('thinking', false), {
    state: 'thinking', label: 'Clank is thinking', isBusy: true,
  });
  assert.deepEqual(launcherActivity('working', false), {
    state: 'working', label: 'Clank is working', isBusy: true,
  });
  assert.deepEqual(launcherActivity('working', true), {
    state: 'stopping', label: 'Clank is stopping', isBusy: true,
  });
});

test('launcher activity preserves settled and error feedback', () => {
  assert.equal(launcherActivity('idle', false).label, 'Open Clank');
  assert.equal(launcherActivity('done', false).label, 'Clank finished');
  assert.equal(launcherActivity('error', false).label, 'Clank needs attention');
});
