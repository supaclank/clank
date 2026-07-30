// Live reload for the filepreview text shell: watch the served file via
// SSE and swap the <pre> in place, so scroll position and the clank
// overlay (a shadow-DOM host on <body>) survive the update. Refetch
// failures fall back to a full reload; EventSource reconnects itself.
(() => {
  'use strict';
  // decode-then-encode: location.pathname is already percent-encoded,
  // and the server decodes the query value exactly once.
  const path = encodeURIComponent(decodeURIComponent(location.pathname));
  const es = new EventSource('/__file/events?path=' + path);
  // Rapid consecutive edits can fire onmessage faster than fetch
  // resolves; a later event's fetch can complete before an earlier
  // one's. Track the newest event and drop a fetch that isn't it, so
  // an in-flight response can never overwrite a newer one already
  // applied.
  let latestEvent = 0;
  es.onmessage = async () => {
    const thisEvent = ++latestEvent;
    try {
      const res = await fetch(location.href, { cache: 'no-store' });
      if (thisEvent !== latestEvent) return;
      if (!res.ok) {
        location.reload();
        return;
      }
      const doc = new DOMParser().parseFromString(await res.text(), 'text/html');
      if (thisEvent !== latestEvent) return;
      const next = doc.querySelector('pre');
      const cur = document.querySelector('pre');
      if (next && cur) {
        cur.replaceWith(next);
      } else {
        location.reload();
      }
    } catch {
      if (thisEvent === latestEvent) location.reload();
    }
  };
})();
