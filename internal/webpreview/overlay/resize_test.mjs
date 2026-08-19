import { test } from 'node:test';
import assert from 'node:assert/strict';
import { boxExtraFromDrag, clampBoxExtra, chatRowCap } from './resize.js';

test('boxExtraFromDrag: the top edge follows the pointer', () => {
  assert.equal(boxExtraFromDrag(0, 300, 200), 100); // up = grow
  assert.equal(boxExtraFromDrag(100, 300, 350), 50); // down = shrink
  assert.equal(boxExtraFromDrag(40, 300, 300), 40); // no move = no change
});

test('clampBoxExtra: the default size is the floor', () => {
  assert.equal(clampBoxExtra(-25, 500), 0);
  assert.equal(clampBoxExtra(0, 500), 0);
  assert.equal(clampBoxExtra(60, 500), 60);
});

test('clampBoxExtra: the room above the box is the ceiling', () => {
  assert.equal(clampBoxExtra(600, 480), 480);
  // A box already shoved past the viewport top has no room: the drag
  // can only hold or shrink, never grow further off-screen.
  assert.equal(clampBoxExtra(120, -40), 0);
});

test('clampBoxExtra: Infinity room sanitizes without a ceiling (restore path)', () => {
  assert.equal(clampBoxExtra(500, Infinity), 500);
  assert.equal(clampBoxExtra(-3, Infinity), 0);
});

test('clampBoxExtra: rounds fractional pointer math to whole pixels', () => {
  assert.equal(clampBoxExtra(59.5, 100.4), 60);
  assert.equal(clampBoxExtra(150, 99.6), 100);
});

test('chatRowCap: unresized keeps the longstanding 12-row window', () => {
  assert.equal(chatRowCap(0), 12);
});

test('chatRowCap: a stretched log renders proportionally more history', () => {
  assert.equal(chatRowCap(240), 24);
  assert.equal(chatRowCap(100), 17); // 340/20
});
