/**
 * One span of work on a time axis: a bar from where it began to where it ended.
 *
 * A CREWLET COMPOSITION, not a kit component — the waterfall is this product's
 * own reading of a turn (see `lib/waterfall.ts`), so the kit ships the tokens
 * and this draws with them. It paints NO COLOUR OF ITS OWN: every fill is a
 * token, and the only hues are STATE — running, failed, selected. A phase has
 * no hue; what KIND of work a bar is, its shape says (a model call is solid, a
 * tool call is lighter, a container is a thin rule over what ran under it).
 *
 * A RUNNING SPAN IS STRIPED, and the stripes drift — the one steady-state pulse
 * on this surface, which runs for as long as the span is open and stops when
 * the push that closes it arrives, never on the push itself. For a reader who
 * asked for less motion the stripe is STATIC: drawn, still saying "running",
 * not moving. Held twice on purpose — the stylesheet's own reduced-motion rule
 * (which `styles/motion.test.ts` holds) and the `static` variant chosen here,
 * which is the half a suite can observe.
 */

import type { CSSProperties } from "react";
import { cx } from "@crewlethq/ui";
import { useMediaQuery } from "~/lib/media.ts";
import type { SpanKind } from "~/lib/waterfall.ts";

/** The query a reader's "less motion" setting answers. */
export const REDUCED_MOTION = "(prefers-reduced-motion: reduce)";

export interface SpanBarProps {
  kind: SpanKind;
  /** Where it begins and how much of the axis it takes, as fractions. */
  left: number;
  width: number;
  open?: boolean;
  failed?: boolean;
  selected?: boolean;
}

/** The narrowest a bar is drawn, so an instant call is still a mark. */
const MIN_WIDTH = "2px";

export function SpanBar({ kind, left, width, open, failed, selected }: SpanBarProps) {
  const still = useMediaQuery(REDUCED_MOTION);
  const style: CSSProperties = {
    insetInlineStart: `${(left * 100).toFixed(3)}%`,
    inlineSize: `max(${MIN_WIDTH}, ${(width * 100).toFixed(3)}%)`,
  };
  return (
    <span
      aria-hidden
      data-kind={kind}
      data-motion={open ? (still ? "static" : "drift") : undefined}
      className={cx(
        "span-bar",
        open && "open",
        open && !still && "drifting",
        failed && "failed",
        selected && "selected",
      )}
      style={style}
    />
  );
}
