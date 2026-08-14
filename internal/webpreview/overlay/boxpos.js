// boxpos.js — pure geometry for the prompt box's drag offset. The box
// sits at its CSS home position plus a user-drag translate; a drag may
// park it off-screen on purpose, but a viewport resize must never
// strand it there. overlay.js owns the DOM reads/writes; this module
// owns the math so `node --test boxpos_test.mjs` covers the edges.

// Air kept between the clamped box and every viewport edge (matches
// the shift-follow margin).
export const BOX_EDGE_MARGIN = 8;

// lo wins on conflict: when the viewport is too small for both edges,
// the top-left edge stays visible — that's where the drag header is,
// so the box remains grabbable.
const clampAxis = (v, lo, hi) => Math.max(Math.min(v, hi), lo);

// clampTranslateToViewport returns the {x, y} translate that keeps the
// box fully inside the viewport (unchanged if it already fits).
// natural is the untranslated layout position (offsetLeft/offsetTop),
// which the caller must read AFTER the resize — the CSS home position
// itself moves with the viewport.
export const clampTranslateToViewport = (translate, natural, size, viewport, margin = BOX_EDGE_MARGIN) => ({
  x: clampAxis(translate.x, margin - natural.left, viewport.width - margin - size.width - natural.left),
  y: clampAxis(translate.y, margin - natural.top, viewport.height - margin - size.height - natural.top),
});
