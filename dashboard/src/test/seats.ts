/**
 * Seats on a store's roster, the way a running tab gets them.
 *
 * THE ROSTER SAYS WHICH SEATS EXIST: the snapshot and the `seats` push carry
 * it, and an `agents` push is merged onto the row of the same HANDLE and
 * dropped for a handle the roster does not list (`Store.applyAgents`). So a case
 * that wants a seat drawn puts it on the roster first and pushes what it is
 * doing after — a push alone puts no seat on screen.
 */

import type { Store } from "~/protocol/store.ts";
import type { AgentRow } from "~/protocol/types.ts";

/** A row a case names: the seat's name, and whatever else it needs. */
export type SeatFixture = Partial<AgentRow> & { role: string };

/**
 * A roster row: its handle the case's own, or — where it names none — the name
 * lowered with every run of other characters a hyphen, which is how the
 * suites' org fixtures spell their seats' handles; and its `id` the handle, as
 * the engine's roster writes it.
 */
export function seatRow(row: SeatFixture): AgentRow {
  const handle = row.handle ?? row.role.toLowerCase().replace(/[^a-z0-9]+/g, "-");
  return { id: handle, ...row, handle } as AgentRow;
}

/** Puts these seats on the store's roster, replacing what it held. */
export function rosterOf(store: Store, rows: readonly SeatFixture[]): void {
  store.applySeats(rows.map(seatRow));
}
