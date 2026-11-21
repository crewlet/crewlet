/**
 * A seat's turns and its calls in flight, as the live overlay states them.
 *
 * A seat runs up to its `sandbox.max_concurrent_runs` turns at once, one per
 * conversation (`livestate.Overlay.Turns`), and each turn has at most one call
 * in flight (`live_calls`, paired with its turn by `turn_id`). Every reader of
 * a seat's turn reads it here, so no screen assumes one.
 *
 * A ROW READ AS ONE TURN reads its FIRST turn ([turnOf]) and that turn's call
 * ([callOf]): at the default cap of one, the seat's only turn — what every
 * one-line summary drew before a seat could run more. A surface that lists
 * running turns lists each one ([eachTurn]), so a seat on two turns is two
 * rows, each a seat row narrowed to its turn, which every one-turn reader then
 * reads unchanged.
 */

import type { AgentRow, LiveCall, LiveTurn } from "~/protocol/index.ts";

/** The turns a seat is on, oldest first; none for a row that names none. */
export function turnsOf(row: AgentRow | null | undefined): readonly LiveTurn[] {
  return row?.turns ?? [];
}

/** The seat's calls in flight, oldest first; none for a row that names none. */
export function callsOf(row: AgentRow | null | undefined): readonly LiveCall[] {
  return row?.live_calls ?? [];
}

/** The turn a row is read as: its first, or null while it is on none. */
export function turnOf(row: AgentRow | null | undefined): LiveTurn | null {
  return turnsOf(row)[0] ?? null;
}

/**
 * The call in flight a row is read as: its [turnOf] turn's — or, while the
 * seat is on no turn with one, a call whose turn it no longer holds (a failed
 * phase's call, kept on screen past its turn's end, or one of no turn at all).
 * Null when there is neither. A call of the seat's OTHER turns is never it.
 */
export function callOf(row: AgentRow | null | undefined): LiveCall | null {
  const calls = callsOf(row);
  const turn = turnOf(row);
  const own = turn ? calls.find((c) => c.turn_id === turn.turn_id) : undefined;
  if (own) return own;
  const held = new Set(turnsOf(row).map((t) => t.turn_id));
  return calls.find((c) => !held.has(c.turn_id)) ?? null;
}

/** The call in flight of turn `turnId` on this seat, or null. */
export function callOfTurn(row: AgentRow | null | undefined, turnId: string): LiveCall | null {
  if (!turnId) return null;
  return callsOf(row).find((c) => c.turn_id === turnId) ?? null;
}

/** The turn `turnId` if this seat is on it, or null. */
export function turnById(row: AgentRow | null | undefined, turnId: string): LiveTurn | null {
  if (!turnId) return null;
  return turnsOf(row).find((t) => t.turn_id === turnId) ?? null;
}

/**
 * One row per turn the seat is on, each the seat's row narrowed to that turn
 * and its call, oldest first — the row itself while it is on one turn or none,
 * so a seat at the default cap reads exactly as it did. A call whose turn the
 * seat no longer holds rides on the first row, where [callOf] reads it.
 */
export function eachTurn(row: AgentRow): AgentRow[] {
  const turns = turnsOf(row);
  if (turns.length <= 1) return [row];
  const held = new Set(turns.map((t) => t.turn_id));
  const orphans = callsOf(row).filter((c) => !held.has(c.turn_id));
  return turns.map((turn, i) => ({
    ...row,
    turns: [turn],
    live_calls: [
      ...callsOf(row).filter((c) => c.turn_id === turn.turn_id),
      ...(i === 0 ? orphans : []),
    ],
  }));
}

/**
 * The seat's row narrowed to turn `turnId` — that turn and its call alone — or
 * null when the seat is on no such turn and holds no call of it. The row a
 * page about one turn reads the seat through.
 */
export function narrowTo(row: AgentRow, turnId: string): AgentRow | null {
  const turn = turnById(row, turnId);
  const call = callOfTurn(row, turnId);
  if (!turn && !call) return null;
  if (turnsOf(row).length <= 1 && callsOf(row).length <= 1) return row;
  return { ...row, turns: turn ? [turn] : [], live_calls: call ? [call] : [] };
}
