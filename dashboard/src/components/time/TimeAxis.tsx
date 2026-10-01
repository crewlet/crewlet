/**
 * The ruler above a waterfall: offsets from the turn's start, on a 1-2-5 step.
 *
 * Offsets rather than wall-clock times, because the question a waterfall
 * answers is "how long into the turn", and a clock reading repeats the same
 * minute across every mark of a short turn. The wall clock of the start is the
 * header's.
 *
 * HOW MANY MARKS is a property of the ruler's WIDTH, not of the turn: a fixed
 * six printed "0 10 s20 s30 s4050s s" over a phone's 125px track. So the axis
 * measures itself and asks `ticks` for as many marks as fit one label each.
 */

import { useLayoutEffect, useRef, useState } from "react";
import { tickLabel, ticks } from "~/lib/waterfall.ts";

/**
 * The room one mark needs: the widest label a 1-2-5 step prints ("1m 30s",
 * "500 ms") is seven mono characters at the caption size, about 48px, plus a
 * gap a reader sees as one. Measured against the kit's mono caption.
 */
export const AXIS_MARK_PX = 64;

/**
 * How many steps a ruler `width` px wide has room for. Never fewer than two:
 * with one, a step as long as the turn put its only mark past the end and the
 * ruler read "0" and nothing else.
 */
export function marksFor(width: number): number {
  return Math.max(2, Math.floor(width / AXIS_MARK_PX));
}

/**
 * Where a mark anchors on its instant. The origin starts there and a mark at
 * the far end ends there, so neither hangs past the track; every other mark is
 * centred, including the last one when it falls short of the end — anchoring
 * THAT one to its right edge drew "40 s" a label's width before forty seconds.
 */
function anchor(t: number, length: number): "start" | "centre" | "end" {
  if (t === 0) return "start";
  return length - t < length * 0.04 ? "end" : "centre";
}

export function TimeAxis({ from, to }: { from: number; to: number }) {
  const ref = useRef<HTMLDivElement>(null);
  // Before the first measurement (and under jsdom, which lays nothing out)
  // the ruler assumes the laptop width it is designed at.
  const [most, setMost] = useState(6);
  useLayoutEffect(() => {
    const el = ref.current;
    if (!el || typeof ResizeObserver === "undefined") return;
    const measure = () => {
      const width = el.getBoundingClientRect().width;
      if (width > 0) setMost(marksFor(width));
    };
    measure();
    const watch = new ResizeObserver(measure);
    watch.observe(el);
    return () => watch.disconnect();
  }, []);
  const length = Math.max(0, to - from);
  const { step, marks } = ticks(length, most);
  return (
    <div className="time-axis" aria-hidden ref={ref}>
      {marks.map((t) => (
        <span
          key={t}
          className="time-axis-mark"
          data-anchor={anchor(t, length)}
          style={{ insetInlineStart: `${length ? (t / length) * 100 : 0}%` }}
        >
          {tickLabel(t, step)}
        </span>
      ))}
    </div>
  );
}
