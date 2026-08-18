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

test('launcher morph geometry expands from the launcher center into the prompt box', () => {
  assert.deepEqual(
    launcherMorphGeometry(
      { left: 338, top: 678, width: 46, height: 46 },
      { left: 5, top: 415, width: 370, height: 184 },
    ),
    { x: 171, y: 194, scaleX: 46 / 370, scaleY: 0.25 },
  );
});
