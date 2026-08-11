// resize.js — pure height-resize math for the preview overlay box.
// Dragging the box's top edge (chat view only) raises the chat log's
// height cap; the box is bottom-anchored, so it grows upward. The
// collapsed prompt view and the composer keep their default sizes —
// the composer only autosizes with typed content. No DOM: overlay.js
// owns pointer events and styles, so `node --test resize_test.mjs`
// covers the arithmetic directly.

// BOX_TOP_MARGIN (px) keeps a resized box from growing under the
// viewport's top edge.
export const BOX_TOP_MARGIN = 8;

// CHAT_DEFAULT_MAX (px) is the unresized chat log's height cap.
// overlay.js interpolates it into the box CSS so the stylesheet and
// the drag math can't drift.
export const CHAT_DEFAULT_MAX = 240;

// CHAT_ROW_PX approximates one transcript row; it converts the log's
// height into how many trailing messages are worth rendering.
export const CHAT_ROW_PX = 20;

// chatRowCap returns the transcript window for a log stretched by
// `extra`: the unresized cap works out to the longstanding 12 rows.
export const chatRowCap = (extra) => Math.round((CHAT_DEFAULT_MAX + extra) / CHAT_ROW_PX);

// boxExtraFromDrag maps a top-edge drag to a proposed extra height:
// the edge follows the pointer, so moving up (clientY < startY) grows.
export const boxExtraFromDrag = (startExtra, startY, clientY) => startExtra + (startY - clientY);

// clampBoxExtra bounds a proposed extra height: never negative (the
// default size is the floor), never past the room left above the box.
export const clampBoxExtra = (extra, room) =>
  Math.min(Math.max(0, Math.round(extra)), Math.max(0, Math.round(room)));
