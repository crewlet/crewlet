/**
 * Reading a person's load against what they can take.
 *
 * The engine answers the numbers — `work_workload` sums the open work per
 * handle and the capacity across every project running a sprint. What is here
 * is the JUDGEMENT a screen makes of them, which is the half that goes wrong:
 * whether somebody is over, by how much, and what to say when nobody declared
 * a capacity at all.
 *
 * PURE FUNCTIONS OVER VALUES, for `lib/timeline.ts`'s reason. Every case worth
 * protecting here is a three-valued one — declared and over, declared and
 * under, not declared — and a rendering test cannot tell the third from a zero
 * without reading a pixel.
 */

import type { WorkloadRow } from "~/protocol/index.ts";
import type { Seat } from "./seats.ts";

/**
 * What a capacity comparison concluded.
 *
 * THREE VALUES, not a boolean, and the third is the one that matters: `unknown`
 * means nobody declared a capacity for this person, which is a fact about the
 * company's configuration rather than about them. Rendered as a zero it would
 * put every person in a company that never set a capacity permanently over,
 * which is the failure that makes the whole screen useless.
 *
 * `unsummable` is its own value for the same reason one level down: this
 * person's projects size in different units, so their capacity could not be
 * added up. "Nobody said" and "it cannot be added up" send a reader to two
 * different places — the first to a sprint policy, the second to two.
 */
export type LoadState = "under" | "over" | "unknown" | "unsummable";

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
  /** What they hold, in the measure their capacity is declared in — so the
   *  two numbers on a row are always about the same thing. */
  held: number;
  /** What they can take, or null when [state] is not `under` or `over`. */
  capacity: number | null;
  /** Held over capacity, 0..1 and beyond. Null when there is nothing to
   *  divide by — never 0, which would draw a full-width empty bar. */
  fraction: number | null;
  /** The row's own copy of the counts, so a renderer never re-derives them. */
  open: number;
  blocked: number;
  overdue: number;
  unscheduled: number;
  /** How many projects the capacity came from, so a reader can tell a whole
   *  week's number from part of one. */
  from: number;
}

/**
 * How far over capacity counts as an emergency rather than a stretch.
 *
 * 1.25: a quarter over is the overflow a fortnight absorbs — an estimate that
 * slipped, one task pulled forward — and a caution is the right weight for it.
 * Past that the plan is wrong rather than tight, and the row says so in the
 * critical tone. The number is a judgement rather than a measurement, which is
 * why it is named here rather than written into a comparison: a company that
 * wants a different one changes a constant, not an expression.
 */
export const OverCapacityAlarm = 1.25;

/** What a row holds in the measure its capacity is stated in. */
function heldIn(row: WorkloadRow): number {
  return row.capacity_measure === "estimate" ? row.estimate_min : row.points;
}

/** One engine row as the screen reads it. */
export function loadOf(row: WorkloadRow, seat?: Seat): Load {
  const held = heldIn(row);
  const capacity = typeof row.capacity === "number" ? row.capacity : null;
  let state: LoadState = "unknown";
  if (row.mixed_measures) state = "unsummable";
  else if (capacity !== null) state = held > capacity ? "over" : "under";
  return {
    handle: row.handle,
    seat,
    state,
    held,
    capacity,
    // A CAPACITY OF ZERO IS NOT A DIVISOR. Somebody declared to have no
    // capacity at all is over by any amount of work, and a division would
    // answer Infinity — which renders as a bar of unbounded width.
    fraction: capacity !== null && capacity > 0 ? held / capacity : null,
    open: row.open,
    blocked: row.blocked,
    overdue: row.overdue,
    unscheduled: row.unscheduled,
    from: row.capacity_from ?? 0,
  };
}

/** The tone a load wears. */
export function loadTone(load: Load): LoadTone {
  switch (load.state) {
    case "over":
      // PAST THE ALARM IT IS CRITICAL, and a capacity of zero with work
      // against it is too: there is no fraction to compare, and the plan
      // says this person should be holding nothing.
      if (load.fraction === null || load.fraction >= OverCapacityAlarm) return "critical";
      return "caution";
    case "under":
      return "positive";
    default:
      // NEUTRAL, never caution. An undeclared capacity is not a warning
      // about the person — nothing is known about whether they are
      // overloaded, and colouring it would be a claim.
      return "neutral";
  }
}

/**
 * What the capacity column says.
 *
 * A SENTENCE FOR AN ABSENCE, never a zero and never a punctuation mark. The
 * two unknown states get different words because they are different problems:
 * nobody set a capacity, or two projects set one in units that do not add.
 *
 * It was an em dash, which a screen reader reads as "dash" or skips entirely —
 * so the cell with the least to say said nothing at all, and a reader could
 * not tell it from a capacity of zero. The WORDS are the value here; the
 * [EmptyValue] mark beside them is the column that holds a number.
 */
export function capacityText(load: Load): string {
  switch (load.state) {
    case "unknown":
      return "not set";
    case "unsummable":
      return "mixed units";
    default:
      return String(load.capacity);
  }
}

/** What a load means, as a sentence for a tooltip or a screen reader. */
export function loadSentence(load: Load): string {
  const unit = load.open === 1 ? "item" : "items";
  const what = `${load.open} open ${unit}`;
  switch (load.state) {
    case "unknown":
      return `${what}. No sprint policy declares a capacity for ${load.handle}, so there is nothing to compare this against.`;
    case "unsummable":
      return `${what}. ${load.handle} works in projects that size in different units, so their capacities cannot be added up.`;
    case "over":
      return `${what}, ${load.held} against a capacity of ${load.capacity}${load.from > 1 ? ` across ${load.from} projects` : ""} — over.`;
    default:
      return `${what}, ${load.held} against a capacity of ${load.capacity}${load.from > 1 ? ` across ${load.from} projects` : ""}.`;
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
 * holding nothing after them, by name. Never re-sorted by fraction: a person
 * one point over a capacity of two would outrank somebody holding forty
 * against a capacity nobody declared, which is not the question.
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
