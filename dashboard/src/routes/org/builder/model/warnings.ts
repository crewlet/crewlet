/**
 * What the engine will run the company with, as the one list a person reads.
 *
 * ONE LIST, TWO PLACES. The banner over the chart and a save's review both
 * draw [warningLines] of the same check, so the two cannot count differently.
 * They used to be read apart from one answer: the banner counted only the
 * warnings no node is about, and the review every warning, so the chart said
 * "1 warning to know about" over a review that listed nine.
 *
 * SAID ONCE FOR EVERY SEAT IT IS ABOUT. The engine raises some advisories once
 * per seat (seven agents learning on the model they work on are seven
 * warnings), each in words for a person that are the same for every seat, with
 * `seat` naming which. A line is every warning that says the same words with
 * the same link, in the order the engine first said each, and it says who it
 * is about: the seat's name for one, how many agents (or seats) for several.
 * A line is one thing to know however many warnings it says, so a count of
 * them is a count of lines.
 *
 * NAMED FROM THE SAME ANSWER. A seat is named by the derivation that came with
 * the warnings, never by the draft as it stands now, for the reason
 * `problems.ts` places a warning against the document that was sent.
 */

import { plural } from "~/lib/format.ts";
import type { Derived } from "~/protocol/index.ts";
import type { PlacedProblem, ProblemLink } from "./problems.ts";
import type { BuilderState } from "./reducer.ts";

/** One thing to know about the company the engine will run. */
export interface WarningLine {
  /** Who it is about: a seat's name, or how many agents or seats; null where it names no seat. */
  readonly who: string | null;
  /** What it risks and what to do: the engine's words for a person where it wrote them. */
  readonly words: string;
  /** Where it is changed, when the builder does not change it. */
  readonly link: ProblemLink | null;
}

/** The warnings among `placed`, one line for each thing they say, in the engine's order. */
export function warningLines(
  placed: readonly PlacedProblem[],
  derived: Derived | null,
): WarningLine[] {
  const groups = new Map<
    string,
    { words: string; link: ProblemLink | null; seats: string[]; unseated: boolean }
  >();
  for (const p of placed) {
    if (p.severity !== "warning") continue;
    const key = `${p.link ?? ""}\u0000${p.message}`;
    let group = groups.get(key);
    if (group === undefined) {
      group = { words: p.message, link: p.link, seats: [], unseated: false };
      groups.set(key, group);
    }
    const seat = p.source.seat;
    if (!seat) group.unseated = true;
    else if (!group.seats.includes(seat)) group.seats.push(seat);
  }
  const seats = new Map((derived?.seats ?? []).map((s) => [s.handle, s]));
  return [...groups.values()].map(({ words, link, seats: handles, unseated }) => ({
    who: unseated ? null : whoOf(handles, seats),
    words,
    link,
  }));
}

/**
 * The lines of the check that answered the draft as it stands, and none while
 * the last answer was about an older draft: what the banner and the review
 * both draw.
 */
export function draftWarningLines(state: BuilderState): WarningLine[] {
  if (state.check.generation !== state.generation) return [];
  return warningLines(state.check.problems.all, state.check.derived);
}

/** The seats a line is about, as its reader names them. */
function whoOf(
  handles: readonly string[],
  seats: ReadonlyMap<string, { name: string; kind: string }>,
): string | null {
  if (handles.length === 0) return null;
  if (handles.length === 1) return seats.get(handles[0]!)?.name || handles[0]!;
  const agents = handles.every((h) => seats.get(h)?.kind === "agent");
  return plural(handles.length, agents ? "agent" : "seat");
}
