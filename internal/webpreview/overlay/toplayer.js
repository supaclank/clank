// toplayer.js — which top-layer mode the overlay host needs.
//
// A z-index cannot lift the overlay above a guest page's
// dialog.showModal() or popover: the top layer paints above the whole
// normal DOM regardless, so a `dialog::backdrop { backdrop-filter }`
// blurs the overlay along with the page. Being in the top layer is only
// half the fix — order there is promotion order, and showModal() marks
// everything outside its own subtree inert, so a host that merely paints
// on top still can't be clicked. overlay.js owns the DOM calls; this
// module owns the policy so `node --test toplayer_test.mjs` covers it.

export const HOST_LAYER = { popover: 'popover', modal: 'modal' };

// Slack around the overlay's own rects: the pointer escalates the host
// just before it arrives, so a fast move-and-click can't land in the
// frame where the host is still inert.
export const POINTER_GRACE_PX = 24;

// hostLayerFor picks the mode for the current state. Modal is the only
// mode that survives a guest modal's inertness, and it costs the guest
// page its own interactivity — so it is reserved for the moments the
// pointer or the open box says the user is addressing clank.
export const hostLayerFor = ({ hasForeignTopLayer, isBoxVisible, isPointerOverOverlay, isInspecting }) => {
  if (!hasForeignTopLayer) return HOST_LAYER.popover; // popover already outranks the page
  if (isInspecting) return HOST_LAYER.popover; // inspect picks guest nodes by hit-test
  return isBoxVisible || isPointerOverOverlay ? HOST_LAYER.modal : HOST_LAYER.popover;
};

export const TOP_LAYER_ACTION = { none: 'none', switch: 'switch', repromote: 'repromote' };

// A host already in the right mode still has to re-enter the top layer
// once the page promotes something after it — later promotion paints on
// top. foreignEpoch counts promotions the overlay has observed.
export const hostLayerTransition = ({ current, desired, foreignEpoch, appliedEpoch }) => {
  if (current !== desired) return TOP_LAYER_ACTION.switch;
  if (foreignEpoch !== appliedEpoch) return TOP_LAYER_ACTION.repromote;
  return TOP_LAYER_ACTION.none;
};

// isPointNearRects reports whether the pointer is over (or within grace
// of) any of the overlay's own interactive rects.
export const isPointNearRects = (point, rects, grace = POINTER_GRACE_PX) =>
  rects.some((r) =>
    point.x >= r.left - grace && point.x <= r.right + grace &&
    point.y >= r.top - grace && point.y <= r.bottom + grace);
