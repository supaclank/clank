// Regression tests for the overlay's top-layer policy: which host mode
// (popover vs. modal) is needed, and when the overlay must re-enter the layer.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  HOST_LAYER,
  POINTER_GRACE_PX,
  TOP_LAYER_ACTION,
  hostLayerFor,
  hostLayerTransition,
  isPointNearRects,
} from './toplayer.js';

const state = (over) => ({
  hasForeignTopLayer: false,
  isBoxVisible: false,
  isPointerOverOverlay: false,
  isInspecting: false,
  ...over,
});

test('a page with nothing in the top layer keeps the host non-modal', () => {
  // Popover already outranks every z-index, and staying non-modal is
  // what leaves the guest page clickable.
  assert.equal(hostLayerFor(state({})), HOST_LAYER.popover);
  assert.equal(hostLayerFor(state({ isBoxVisible: true })), HOST_LAYER.popover);
  assert.equal(hostLayerFor(state({ isPointerOverOverlay: true })), HOST_LAYER.popover);
});

test('an open box under a guest modal escalates so the box stays usable', () => {
  // showModal() inerts everything outside its subtree: painting on top
  // is not enough, the host has to be modal itself to receive clicks.
  assert.equal(
    hostLayerFor(state({ hasForeignTopLayer: true, isBoxVisible: true })),
    HOST_LAYER.modal,
  );
});

test('the pointer reaching the launcher escalates while the box is hidden', () => {
  // Without this the launcher paints over the guest modal but swallows
  // no clicks — a dead button.
  assert.equal(
    hostLayerFor(state({ hasForeignTopLayer: true, isPointerOverOverlay: true })),
    HOST_LAYER.modal,
  );
});

test('a hidden box with the pointer elsewhere leaves the guest modal usable', () => {
  assert.equal(hostLayerFor(state({ hasForeignTopLayer: true })), HOST_LAYER.popover);
});

test('inspect never escalates: it picks guest nodes by hit-test', () => {
  // A modal host inerts the guest page, and inert nodes drop out of
  // elementFromPoint — the inspector would select nothing.
  assert.equal(
    hostLayerFor(state({ hasForeignTopLayer: true, isBoxVisible: true, isInspecting: true })),
    HOST_LAYER.popover,
  );
});

test('a mode change is a switch', () => {
  assert.equal(
    hostLayerTransition({
      current: HOST_LAYER.popover, desired: HOST_LAYER.modal, foreignEpoch: 1, appliedEpoch: 1,
    }),
    TOP_LAYER_ACTION.switch,
  );
});

test('a promotion after ours re-promotes even though the mode is unchanged', () => {
  // Top-layer order is promotion order: the guest dialog opened after
  // our popover paints above it until we re-enter the layer.
  assert.equal(
    hostLayerTransition({
      current: HOST_LAYER.popover, desired: HOST_LAYER.popover, foreignEpoch: 2, appliedEpoch: 1,
    }),
    TOP_LAYER_ACTION.repromote,
  );
});

test('a settled host does nothing — re-promotion restarts CSS animations', () => {
  assert.equal(
    hostLayerTransition({
      current: HOST_LAYER.modal, desired: HOST_LAYER.modal, foreignEpoch: 3, appliedEpoch: 3,
    }),
    TOP_LAYER_ACTION.none,
  );
});

const launcher = { left: 100, top: 200, right: 148, bottom: 248 };

test('the pointer inside a rect counts as over the overlay', () => {
  assert.equal(isPointNearRects({ x: 120, y: 220 }, [launcher]), true);
});

test('grace escalates just before the pointer lands, so a fast click cannot race it', () => {
  assert.equal(isPointNearRects({ x: launcher.left - POINTER_GRACE_PX + 1, y: 220 }, [launcher]), true);
  assert.equal(isPointNearRects({ x: launcher.left - POINTER_GRACE_PX - 1, y: 220 }, [launcher]), false);
});

test('no rects means nothing to be over', () => {
  assert.equal(isPointNearRects({ x: 120, y: 220 }, []), false);
});
