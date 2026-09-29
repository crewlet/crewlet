/**
 * What every list of turns shares: how far back the engine will look for one,
 * and how the work item a turn was on is named on a row.
 *
 * TWO LISTS READ THEM — the company's turns (`routes/live/Turns.tsx`) and one
 * seat's (`routes/agents/seat/Turns.tsx`) — and each used to spell both for
 * itself, so the seat's list asked for the read's default week while calling
 * itself "the newest 50 this seat took", and both drew a keyless item as a
 * full `native:<uuid>` that no row had room for.
 */

import { doingWords } from "./seats.ts";
import type { AgentRow, TurnRow, WorkItemRef } from "~/protocol/index.ts";

/**
 * `store.MaxTurnDays` — the read refuses to look further back than this, and
 * it is the event store's own retention, so it is also as far back as any
 * turn can be read at all.
 */
export const MAX_TURN_DAYS = 30;

/** A uuid, which is what a native task's id is: 36 characters no row has room for. */
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

/**
 * The work item a turn was on, as a row names it — and, for its `title`, the
 * whole identity.
 *
 * THE KEY WHEN THE TURN RECORDED ONE, which is every turn now: a create's wake
 * carries the key the write minted. A turn that recorded none — written by a
 * build from before that, or woken by a trigger that named the item by its
 * identity alone — is still ABOUT an item, so it is named by the item's kind
 * and the head of its id rather than dropped; the full `<backend>:<id>` a
 * search or the event log's `work_item=` takes is on the title, never in the
 * row, where 43 unbreakable characters took the whole cell on a phone and
 * left the summary beside it one letter wide.
 */
export function workItemLabel(ref: WorkItemRef): { text: string; title: string } {
  const identity = `${ref.backend}:${ref.id}`;
  if (ref.key) return { text: ref.key, title: `${ref.key} — the work item this turn was about` };
  const kind = ref.backend === "native" ? "task" : ref.backend;
  const head = UUID.test(ref.id) ? ref.id.slice(0, 8) : ref.id;
  return {
    text: `${kind} ${head}`,
    title: `${identity} — the work item this turn was about; the turn recorded no key for it`,
  };
}

/**
 * What a listed turn that has not ENDED is doing now, from the live overlay —
 * the one source that knows — or null for a turn that is settled, or that no
 * seat is running (it died mid-flight, or the push has not named it yet).
 *
 * THE STORE'S ROW FOR A RUNNING TURN HAS NOTHING SETTLED IN IT: no summary
 * (the plan's is written as the turn completes), no iteration count and a
 * token figure for the phases recorded so far. Drawn as it came, a list said
 * "no summary recorded · 0 iterations · 0 tokens" for a turn the Overview's
 * card named, in the same second, as "Turn 1 on ENG-32 · round 2 of 24". So a
 * row asks the overlay what the seat running it is doing — the words every
 * live strip says (`doingWords`) — and the work item the turn is charged to,
 * which the overlay names before the store does.
 */
export function runningNow(
  turn: TurnRow,
  agents: readonly AgentRow[],
): { words: string; item: WorkItemRef | null } | null {
  if (turn.complete) return null;
  const seat = seatOnTurn(agents, turn.turn_id);
  if (!seat) return null;
  const parked = seat.turn?.stage === "parked";
  return {
    words: `${parked ? "parked" : "running"} — ${doingWords(seat)}`,
    item: seat.live_call?.work_item ?? seat.turn?.work_item ?? null,
  };
}

/**
 * The seat whose live overlay says it is on this turn — its turn record
 * first, then its call in flight — or undefined when no seat is.
 *
 * ONE READING FOR EVERY SURFACE THAT ASKS "is anybody running this turn?",
 * because two readings is how the same turn was "not settled" on its own page
 * and "running" on the turns list beside it: the page asked the overlay and
 * the list assumed that no completion record meant running.
 */
export function seatOnTurn(agents: readonly AgentRow[], turnId: string): AgentRow | undefined {
  return (
    agents.find((a) => a.turn?.turn_id === turnId) ??
    agents.find((a) => a.live_call?.turn_id === turnId)
  );
}

/**
 * A turn that is neither running nor ended: no seat is on it and it published
 * no closing record — its node stopped, or it was abandoned, before it ended.
 * The word and its explanation, drawn identically wherever such a turn is.
 */
export const UNSETTLED = {
  word: "not settled",
  title:
    "no seat is running this turn and it published no closing record — it stopped before it ended",
} as const;

/**
 * What a listed turn with no completion record is: `running` while a seat is
 * on it, `unsettled` when none is and the overlay can see the seats, and
 * `unknown` while it cannot — a view with no live connection has no seats to
 * ask, and calling the turn either would be a guess. "" for a turn that has a
 * record, or is parked (which the store's row says itself).
 */
export type OpenState = "" | "running" | "unsettled" | "unknown";

export function openState(
  turn: Pick<TurnRow, "turn_id" | "complete" | "parked">,
  agents: readonly AgentRow[],
  connected: boolean,
): OpenState {
  if (turn.complete || turn.parked) return "";
  if (seatOnTurn(agents, turn.turn_id)) return "running";
  return connected ? "unsettled" : "unknown";
}
