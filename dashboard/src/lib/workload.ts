/**
 * Reading a person's load.
 *
 * The engine answers the numbers — `work_workload` sums the open work per
 * handle. What is here is the JUDGEMENT a screen makes of them: how heavy a
 * queue is relative to the rest of the company, and what to say about one that
 * is entirely blocked.
 *
 * PURE FUNCTIONS OVER VALUES, for `lib/timeline.ts`'s reason. Every case worth
 * protecting here is one a rendering test cannot tell from a zero without
 * reading a pixel — a queue of nothing, a queue that is all blocked, and the
 * seat that holds nothing at all.
 */

import type { WorkloadRow } from "~/protocol/index.ts";
import type { Seat } from "./seats.ts";

/**
 * What a queue's shape concluded.
 *
 * THREE VALUES, not a boolean. `idle` is its own value rather than an open
 * count of zero because "nobody has given this seat anything" is a fact about
 * the company rather than about them, and it is the answer "is anybody free"
 * is looking for. `stuck` is every open item blocked — a seat that looks busy
 * and can move none of it, which reads identically to a healthy queue until
 * it is said.
 */
export type LoadState = "idle" | "stuck" | "working";

/** The load column's own tone, in the tracker's shared vocabulary. */
export type LoadTone = "neutral" | "positive" | "caution" | "critical";

/** One row of the workload screen, as it is drawn. */
export interface Load {
  handle: string;
  /** The seat this handle belongs to, when the roster has one. A handle with
   *  no seat is somebody who has left with work still assigned — which is
   *  worth seeing, so it is a row rather than a filter. */
  seat?: Seat;
  state: LoadState;
  /** What they hold, in both measures a company may size in — carried rather
   *  than one chosen, because which one is used differs by team. */
  points: number;
  estimateMin: number;
  /** The row's own copy of the counts, so a renderer never re-derives them. */
  open: number;
  blocked: number;
  overdue: number;
  unscheduled: number;
}

/**
 * How many open items make a queue heavy enough to say so.
 *
 * TWELVE: two working weeks at roughly an item a day, which is the point past
 * which a queue stops being a list somebody is working through and starts
 * being one nothing at the bottom of will be reached. The number is a
 * judgement rather than a measurement, which is why it is named here rather
 * than written into a comparison: a company that wants a different one changes
 * a constant, not an expression.
 */
export const HeavyQueue = 12;

/** One engine row as the screen reads it. */
export function loadOf(row: WorkloadRow, seat?: Seat): Load {
  let state: LoadState = "working";
  if (row.open === 0) state = "idle";
  // EVERY OPEN ITEM BLOCKED, not merely some. A queue with one blocker in it
  // is an ordinary queue; one where nothing can move is a person waiting.
  else if (row.blocked >= row.open) state = "stuck";
  return {
    handle: row.handle,
    seat,
    state,
    points: row.points,
    estimateMin: row.estimate_min,
    open: row.open,
    blocked: row.blocked,
    overdue: row.overdue,
    unscheduled: row.unscheduled,
  };
}

/** The tone a load wears. */
export function loadTone(load: Load): LoadTone {
  switch (load.state) {
    case "stuck":
      // CRITICAL RATHER THAN CAUTION: a seat holding work it can move none of
      // is the one row on this screen somebody has to act on.
      return "critical";
    case "idle":
      // NEUTRAL, never positive. Holding nothing is not an achievement and it
      // is not a warning — it is the answer to a different question.
      return "neutral";
    default:
      return load.open >= HeavyQueue ? "caution" : "positive";
  }
}

/**
 * How wide the load bar is drawn, 0..1, against the heaviest queue on screen.
 *
 * RELATIVE TO THE COMPANY rather than to an absolute ceiling, because there is
 * no number that is "full": a queue of thirty is heavy in one company and a
 * quiet week in another. NULL when nobody holds anything, because a bar is a
 * comparison and there is nothing to compare against — never 0, which would
 * draw a full-width empty track on every row.
 */
export function loadFraction(load: Load, heaviest: number): number | null {
  if (heaviest <= 0) return null;
  return Math.min(1, load.open / heaviest);
}

/** What a load means, as a sentence for a tooltip or a screen reader. */
export function loadSentence(load: Load): string {
  const unit = load.open === 1 ? "item" : "items";
  const what = `${load.open} open ${unit}`;
  switch (load.state) {
    case "idle":
      return `${load.handle} holds no open work.`;
    case "stuck":
      return `${what}, and every one of them is waiting on something else — ${load.handle} can move none of it.`;
    default:
      return `${what}${load.blocked > 0 ? `, ${load.blocked} blocked` : ""}${
        load.overdue > 0 ? `, ${load.overdue} overdue` : ""
      }.`;
  }
}

/**
 * The rows a screen draws, seats attached and the people with nothing kept.
 *
 * A SEAT WITH NO WORK IS STILL A ROW. The engine answers who HOLDS something,
 * which is the expensive half; "who is idle" is the other half of the same
 * question and the roster already has it. A workload screen that listed only
 * the busy would answer "is anybody free" with silence.
 *
 * Ordered by the engine's own ordering — heaviest first — with the people
 * holding nothing after them, by name.
 */
export function loadRows(rows: WorkloadRow[], seats: Seat[]): Load[] {
  const byHandle = new Map(seats.map((seat) => [seat.handle, seat]));
  const held = rows.map((row) => loadOf(row, byHandle.get(row.handle)));
  const carrying = new Set(held.map((load) => load.handle));
  const idle = seats
    .filter((seat) => !carrying.has(seat.handle))
    .sort((a, b) => a.handle.localeCompare(b.handle))
    .map((seat) =>
      loadOf(
        {
          handle: seat.handle,
          open: 0,
          points: 0,
          estimate_min: 0,
          blocked: 0,
          overdue: 0,
          unscheduled: 0,
        },
        seat,
      ),
    );
  return [...held, ...idle];
}
