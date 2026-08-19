import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  launcherActivity,
  launcherMorphGeometry,
  launcherShortcut,
  shouldShowLauncherCoachmark,
} from './launcher.js';

test('first-run coachmark remains until the launcher is acknowledged', () => {
  assert.equal(shouldShowLauncherCoachmark(false), true);
  assert.equal(shouldShowLauncherCoachmark(true), false);
});

test('coachmark check rejects a missing launcher_seen value instead of defaulting it', () => {
  assert.throws(() => shouldShowLauncherCoachmark(undefined), TypeError);
  assert.throws(() => shouldShowLauncherCoachmark(null), TypeError);
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
  assert.deepEqual(launcherActivity('idle', false), {
    state: 'idle', label: 'Open Clank', isBusy: false,
  });
  assert.deepEqual(launcherActivity('done', false), {
    state: 'done', label: 'Clank finished', isBusy: false,
  });
  assert.deepEqual(launcherActivity('error', false), {
    state: 'error', label: 'Clank needs attention', isBusy: false,
  });
});

test('launcher activity falls back to the error state for an unknown agent value', () => {
  assert.deepEqual(launcherActivity('bogus', false), {
    state: 'error', label: 'Clank needs attention', isBusy: false,
  });
});

test('launcher morph geometry expands from the launcher center into the prompt box', () => {
  assert.deepEqual(
    launcherMorphGeometry(
      { left: 338, top: 678, width: 46, height: 46 },
      { left: 5, top: 415, width: 370, height: 184 },
    ),
    { x: 171, y: 194, scaleX: 46 / 370, scaleY: 0.25 },
  );
});
