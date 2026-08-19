// Frame-coalesced rendering.
//
// The engine can emit several envelopes per second per agent while a
// turn runs — a progress round, its phase event, a token rollup, a
// health tick. Rendering once per envelope means rendering many times
// per frame, and the browser only paints once per frame anyway: the
// extra renders were pure cost, and the churn they caused is what the
// screen was showing.
//
// Everything that wants to re-render asks here instead. Requests for
// the same target inside one frame collapse into a single call, so a
// burst of ten envelopes paints exactly once.

const pending = new Map();
let frame = 0;

/**
 * Run `fn` on the next animation frame, at most once per `key`.
 * A later request for the same key within the same frame replaces the
 * earlier one — the newest state is the one worth drawing.
 */
export function schedule(key, fn) {
  pending.set(key, fn);
  if (frame) return;
  frame = requestAnimationFrame(flush);
}

function flush() {
  frame = 0;
  const due = [...pending.values()];
  pending.clear();
  for (const fn of due) {
    try {
      fn();
    } catch (err) {
      // One view throwing must not stop the rest of the frame from
      // painting, or a single bad record freezes the whole dashboard.
      console.error("render failed", err);
    }
  }
}

/** Drop any pending work for `key` (a view being unmounted). */
export function cancel(key) {
  pending.delete(key);
}
