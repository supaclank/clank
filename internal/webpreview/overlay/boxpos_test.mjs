// Regression tests for the resize-rescue rule: a drag may park the
// prompt box off-screen, but a viewport resize must never strand it
// there (overlay.js re-clamps on every resize with this math).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  BOX_EDGE_MARGIN,
  clampTranslateToViewport,
  followTranslateTarget,
  parseStoredBoxIntent,
  resizeOwesClamp,
} from './boxpos.js';

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

test('parseStoredBoxIntent: valid finite coordinates round-trip', () => {
  assert.deepEqual(parseStoredBoxIntent(JSON.stringify({ x: -40, y: 12 })), { x: -40, y: 12 });
});

test('parseStoredBoxIntent: missing key, malformed JSON, or non-finite values are rejected', () => {
  assert.equal(parseStoredBoxIntent(null), null);
  assert.equal(parseStoredBoxIntent(''), null);
  assert.equal(parseStoredBoxIntent('not json'), null);
  assert.equal(parseStoredBoxIntent(JSON.stringify({})), null);
  assert.equal(parseStoredBoxIntent(JSON.stringify({ x: 'abc', y: 0 })), null);
  assert.equal(parseStoredBoxIntent(JSON.stringify({ x: NaN, y: 0 })), null);
  assert.equal(parseStoredBoxIntent(JSON.stringify({ x: Infinity, y: 0 })), null);
});

test('resizeOwesClamp: hidden box or a bogus 0×0 viewport defers to next summon', () => {
  assert.equal(resizeOwesClamp({ innerWidth: 0, innerHeight: 900, isHidden: false }), true);
  assert.equal(resizeOwesClamp({ innerWidth: 1400, innerHeight: 0, isHidden: false }), true);
  assert.equal(resizeOwesClamp({ innerWidth: 1400, innerHeight: 900, isHidden: true }), true);
  assert.equal(resizeOwesClamp({ innerWidth: 1400, innerHeight: 900, isHidden: false }), false);
});

// FOLLOW_POINTER_HEADER_INSET mirrors overlay.js's private constant: the
// pointer lands 12px into the header from its top edge.
const FOLLOW_POINTER_HEADER_INSET = 12;

test('shift-follow lands the header under the pointer regardless of expand state', () => {
  // Collapsed/expanded natural.top differ by the 240px the chat panel adds —
  // that's the bottom-anchored box's own live geometry, not something this
  // formula needs to correct for separately.
  const pointer = { x: 720, y: 520 };
  const viewport = { width: 1440, height: 1200 }; // tall enough that neither case clamps
  const collapsedNatural = { left: 530, top: 596 };
  const expandedNatural = { left: 530, top: 356 };
  const collapsed = followTranslateTarget({
    pointer,
    natural: collapsedNatural,
    size: { width: 380, height: 160 },
    viewport,
  });
  const expanded = followTranslateTarget({
    pointer,
    natural: expandedNatural,
    size: { width: 380, height: 400 },
    viewport,
  });

  assert.equal(collapsedNatural.top + collapsed.y, pointer.y - FOLLOW_POINTER_HEADER_INSET);
  assert.equal(expandedNatural.top + expanded.y, pointer.y - FOLLOW_POINTER_HEADER_INSET);
});

test('shift-follow clamps the header to the top margin near the viewport edge in expanded mode', () => {
  const pointer = { x: 720, y: 5 }; // near the top of the viewport
  const viewport = { width: 1440, height: 900 };
  const natural = { left: 530, top: 356 };
  const target = followTranslateTarget({
    pointer,
    natural,
    size: { width: 380, height: 400 },
    viewport,
  });
  assert.equal(natural.top + target.y, BOX_EDGE_MARGIN);
});
