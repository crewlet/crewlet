/**
 * The company's seats, as a field offers them.
 *
 * TWO GESTURES NAME PEOPLE — the founding membership of a room, and who is in
 * a direct conversation — and they must offer the same set in the same order,
 * or the same colleague is called two different things one dialog apart.
 *
 * A HANDLE IS WHAT TRAVELS, never a display name: the engine's membership and
 * participant sets are handles, and a seat's handle is DERIVED by the engine
 * (a slug of the role name, which this client deliberately does not
 * re-implement — see `lib/seats.ts`). A seat the projection reports no handle
 * for is therefore not offered at all: naming it would mean inventing the one
 * value the engine owns.
 *
 * AGENTS ARE PEOPLE HERE. A company running this engine talks to its seats,
 * and a picker that offered only the humans would make the product's whole
 * premise unreachable from the screen a person starts a conversation on. They
 * are grouped rather than mixed, because who you are addressing — a colleague
 * or a seat that will run a turn about it — is the one thing worth knowing
 * before you press send.
 */

import type { Seat } from "~/lib/seats.ts";

/** One seat, in the shape `TagsInput` offers options in. */
export interface PersonOption {
  value: string;
  label: string;
  description: string;
  group: string;
}

/**
 * Everybody this viewer can name, minus the viewer.
 *
 * THE AUTHOR IS NEVER IN THE LIST. A direct conversation's participants are
 * completed with the caller server-side, and the creator of a room is always
 * in the room they made — so offering somebody their own name is offering a
 * value that changes nothing, and in the direct case it is the one shape of
 * "naming a seat" this surface refuses on principle.
 */
export function peopleOptions(seats: readonly Seat[], viewer: string): PersonOption[] {
  const rows = seats
    .filter((seat) => seat.handle !== "" && seat.handle !== viewer)
    .map((seat) => ({
      value: seat.handle,
      label: seat.name || seat.handle,
      description: seat.handle,
      group: seat.kind === "human" ? "People" : "Agents",
    }));
  // PEOPLE FIRST, THEN BY NAME. `TagsInput` lists groups in first-seen order,
  // so the order of this array IS the order of the headings; and a roster
  // somebody scans is alphabetical, where the document's own order is an
  // implementation detail of how the org chart happens to be written.
  const first = (group: string) => (group === "People" ? 0 : 1);
  rows.sort((a, b) => first(a.group) - first(b.group) || a.label.localeCompare(b.label));
  return rows;
}

/** The handles, as the write path takes them: trimmed, deduplicated, ordered. */
export function handlesOf(chosen: readonly string[]): string[] {
  return [...new Set(chosen.map((handle) => handle.trim()).filter(Boolean))];
}
