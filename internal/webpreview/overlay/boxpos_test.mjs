// Regression tests for the resize-rescue rule: a drag may park the
// prompt box off-screen, but a viewport resize must never strand it
// there (overlay.js re-clamps on every resize with this math).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { clampTranslateToViewport, BOX_EDGE_MARGIN } from './boxpos.js';

// Home geometry mirrors the .box CSS: left = max(16, 50vw - 190),
// bottom-anchored 144px above the viewport floor.
const homeLeft = (vw) => Math.max(16, vw / 2 - 190);
const homeTop = (vh, h) => vh - 144 - h;

const size = { width: 380, height: 200 };
const wide = { width: 1400, height: 900 };
const wideNatural = { left: homeLeft(wide.width), top: homeTop(wide.height, size.height) };

test('a translate that fits is returned unchanged', () => {
  const t = { x: 100, y: -50 };
  assert.deepEqual(clampTranslateToViewport(t, wideNatural, size, wide), t);
});

test('shrinking the viewport pulls a right-overflowing box back in', () => {
  // Dragged far right on a wide window, then the window shrinks to
  // mobile width: the box (now at max-width 100vw-32) must end flush
  // with the right margin, not off-screen.
  const mobile = { width: 375, height: 667 };
  const mobileSize = { width: mobile.width - 32, height: size.height };
  const natural = { left: 16, top: homeTop(mobile.height, size.height) };
  const clamped = clampTranslateToViewport({ x: 800, y: 0 }, natural, mobileSize, mobile);
  assert.deepEqual(clamped, {
    x: mobile.width - BOX_EDGE_MARGIN - mobileSize.width - natural.left,
    y: 0,
  });
  // Resulting visual right edge sits exactly one margin inside the viewport.
  assert.equal(natural.left + clamped.x + mobileSize.width, mobile.width - BOX_EDGE_MARGIN);
});

test('a left-overflowing box lands on the left margin', () => {
  const clamped = clampTranslateToViewport({ x: -2000, y: 0 }, wideNatural, size, wide);
  assert.equal(wideNatural.left + clamped.x, BOX_EDGE_MARGIN);
  assert.equal(clamped.y, 0);
});

test('vertical overflow clamps to the top and bottom margins', () => {
  const below = clampTranslateToViewport({ x: 0, y: 4000 }, wideNatural, size, wide);
  assert.equal(wideNatural.top + below.y + size.height, wide.height - BOX_EDGE_MARGIN);
  const above = clampTranslateToViewport({ x: 0, y: -4000 }, wideNatural, size, wide);
  assert.equal(wideNatural.top + above.y, BOX_EDGE_MARGIN);
});

test('viewport smaller than the box: the top-left (header) edge wins', () => {
  const tiny = { width: 300, height: 150 };
  const natural = { left: 16, top: 20 };
  const clamped = clampTranslateToViewport({ x: 50, y: 50 }, natural, size, tiny);
  // Left and top edges pinned to the margin; right/bottom may overflow.
  assert.equal(natural.left + clamped.x, BOX_EDGE_MARGIN);
  assert.equal(natural.top + clamped.y, BOX_EDGE_MARGIN);
});

test('growing the viewport back restores the original intent', () => {
  // The clamp must always be computed FROM the user's intended
  // translate, never from a previously clamped value: a shrink
  // displaces the box, the grow puts it right back.
  const intent = { x: -400, y: 0 }; // parked bottom-left on a wide window
  const mobile = { width: 375, height: 667 };
  const mobileSize = { width: mobile.width - 32, height: size.height };
  const mobileNatural = { left: 16, top: homeTop(mobile.height, size.height) };
  const displaced = clampTranslateToViewport(intent, mobileNatural, mobileSize, mobile);
  assert.notDeepEqual(displaced, intent);
  assert.deepEqual(clampTranslateToViewport(intent, wideNatural, size, wide), intent);
});

test('a custom margin overrides the default', () => {
  const clamped = clampTranslateToViewport({ x: -2000, y: 0 }, wideNatural, size, wide, 0);
  assert.equal(wideNatural.left + clamped.x, 0);
});
