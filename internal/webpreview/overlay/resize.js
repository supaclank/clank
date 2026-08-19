// resize.js — pure resize math for the preview overlay box. The top
// edge (chat view only) sets the chat log's height — the box is
// bottom-anchored, so it grows upward; the side edges (either view)
// set the box width; the top corners (chat view) drag both axes at
// once, OS-window style. The collapsed prompt view's height and the
// composer keep their defaults — the composer only autosizes with
// typed content. No DOM: overlay.js owns pointer events and styles,
// so `node --test resize_test.mjs` covers the arithmetic directly.

// BOX_EDGE_MARGIN (px) keeps a dragged box edge inside the viewport.
export const BOX_EDGE_MARGIN = 8;

// BOX_DEFAULT_WIDTH (px) is the unresized box width. overlay.js
// interpolates it into the box CSS so the stylesheet and the drag
// math can't drift.
export const BOX_DEFAULT_WIDTH = 380;

// CHAT_DEFAULT_MAX (px) is the expanded chat log's unresized height.
// The log always renders at exactly this plus the user's extra — a
// content-fit log would snap ~240px on the first drag pixel when the
// transcript is short. overlay.js interpolates it into the box CSS so
// the stylesheet and the drag math can't drift.
export const CHAT_DEFAULT_MAX = 240;

// CHAT_ROW_PX approximates one transcript row; it converts the log's
// height into how many trailing messages are worth rendering.
export const CHAT_ROW_PX = 20;

// chatRowCap returns the transcript window for a log stretched by
// `extra`: the unresized cap works out to the longstanding 12 rows.
export const chatRowCap = (extra) => Math.round((CHAT_DEFAULT_MAX + extra) / CHAT_ROW_PX);

// boxExtraFromDrag maps an edge drag to a proposed extra size: the
// edge follows the pointer, growing toward smaller coordinates (up for
// the top edge, left for the west edge — the east edge swaps the
// positional args).
export const boxExtraFromDrag = (startExtra, startPos, pos) => startExtra + (startPos - pos);

// clampBoxExtra bounds a proposed extra size: never negative (the
// default size is the floor), never past the room between the dragged
// edge and the viewport.
export const clampBoxExtra = (extra, room) =>
  Math.min(Math.max(0, Math.round(extra)), Math.max(0, Math.round(room)));
