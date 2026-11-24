/**
 * Who said a line, told apart by its kind as well as its stored name.
 *
 * THE ENGINE'S OWN LINE (`system`) IS NO SEAT'S. The engine writes it under a
 * name of its own, which the org chart refuses a seat; a line is still read by
 * its name and its kind together, so a stored name alone never makes the
 * engine's line a reader's own, nor two lines one speaker's.
 */

import type { ChatAuthorKind } from "~/protocol/index.ts";

/** A line as it names its speaker. */
export interface Said {
  author?: string;
  author_kind?: ChatAuthorKind;
}

/** Whether the seat `handle` said `line`. */
export function saidBy(line: Said, handle: string): boolean {
  return line.author === handle && line.author_kind !== "system";
}

/** Whether two lines are one speaker's: one name, said by the engine in both or by a seat in both. */
export function sameSpeaker(a: Said, b: Said): boolean {
  return a.author === b.author && (a.author_kind === "system") === (b.author_kind === "system");
}
