/**
 * Where "now" is on a waterfall that is still running: a rule at the end of
 * the axis, because a running turn's window ends now. It is state — the turn
 * is live — so it is the one accent on the axis.
 */

import { fraction } from "~/lib/waterfall.ts";

export function NowLine({ now, from, to }: { now: number; from: number; to: number }) {
  if (to <= from) return null;
  return (
    <span
      className="now-line"
      aria-hidden
      style={{ insetInlineStart: `${(fraction(now, from, to) * 100).toFixed(3)}%` }}
    />
  );
}
