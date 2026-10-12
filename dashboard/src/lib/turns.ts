/**
 * What every list of turns shares: how far back the engine will look for one,
 * how the work item a turn was on is named on a row, and where a link that
 * WATCHES a running turn lands.
 *
 * TWO LISTS READ THEM — the company's turns (`routes/live/Turns.tsx`) and one
 * seat's (`routes/agents/seat/Turns.tsx`) — and each used to spell both for
 * itself, so the seat's list asked for the read's default week while calling
 * itself "the newest 50 this seat took", and both drew a keyless item as a
 * full `native:<uuid>` that no row had room for.
 */

import { pathOf } from "~/app/frame/objects.ts";
import { href } from "~/app/router.tsx";
import { doingWords, RUNNING_ROWS, workingLongestFirst } from "./seats.ts";
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
 * THE KEY WHEN THE TURN RECORDED ONE, which is nearly every turn: a create's
 * wake carries the key the write minted. A turn that recorded none — woken by
 * a trigger that named the item by its identity alone — is still ABOUT an
 * item, so it is named by the item's kind
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
 * The turn a seat's live overlay says it is on — its turn record first, then
 * its call in flight, the order [seatOnTurn] reads them in — or "" while it
 * names none: a seat the engine calls working can be a frame or two ahead of
 * the turn id, and a link to `#/live/turns/` with no id is a link to nothing.
 */
export function turnIdOf(row: AgentRow | null | undefined): string {
  return row?.turn?.turn_id || row?.live_call?.turn_id || "";
}

/**
 * The tab a watch link opens a running turn on: the TRANSCRIPT, where the
 * phase it is on is drawn open with its rounds arriving (`routes/live/Turn.tsx`
 * opens a live phase's card as it mounts). Every other link to a turn lands on
 * the Timeline, the page's default, because a reader following a turn from a
 * list or a record is asking what it did and how long each part took. A
 * reader who pressed "Watch live" is asking what it is doing NOW, and on the
 * Timeline that is one drifting bar, with its words a tab and a press away.
 *
 * A PARKED TURN IS WATCHED HERE TOO. Its executor launched a detached coding
 * run and suspended, so no phase is in flight — yet the engine still calls the
 * seat working, because the run is the work. The Transcript draws that run's
 * live output above the phases, links the run's own page, and opens the
 * executor's card it parked in. Sending a parked turn's watch link to the
 * run's page instead would have left the other half dead: a turn that parks
 * while a reader is already watching this tab is the same page with nothing
 * live on it, and no link is followed to fix that.
 *
 * `routes/live/TurnScreen.test.tsx` holds this to a tab the page has.
 */
export const WATCH_TAB = "transcript";

/**
 * Where a link whose job is to WATCH a running turn goes: the turn's own page
 * (`pathOf`, the one map of an object to its route), on [WATCH_TAB]. For a
 * caller that navigates (`nav.to(path, query)`) or draws a row the kit links
 * itself; [watchHref] is the same address as an `href`.
 *
 * NOT THE ROWS OF A MONITOR. Home's Live now and Live › Now running list
 * running turns as rows whose link is the turn's TRACE — the default tab —
 * because a monitor's row is a way into the record of the turn, the same
 * address a settled turn's row has. A watch link is a control that says what
 * it is for: "Watch live", the sidebar's Running group, `g r`, ⌘K's
 * Running now.
 */
export function watchLink(turnId: string): { path: string[]; query: Record<string, string> } {
  return { path: pathOf({ kind: "turn", id: turnId }), query: { tab: WATCH_TAB } };
}

/** [watchLink] as an `href`. */
export function watchHref(turnId: string): string {
  const { path, query } = watchLink(turnId);
  return href(path, query);
}

/**
 * The SHORT list of running turns — Home's Live now card and the sidebar's
 * Running group — and how many working seats it leaves to Live › Now running.
 *
 * ONE DERIVATION FOR BOTH, because each spelled its own and they named
 * different seats: the sidebar dropped a working seat whose turn has no id
 * before it took [RUNNING_ROWS], Home took the first four as they came, and a
 * seat working only through a coding run no turn record names yet led Home's
 * card while the sidebar beside it listed the four after it.
 *
 * THE FILTER COMES BEFORE THE CAP. Every row in either list is a way into its
 * turn, and a seat whose turn has published no id yet has nowhere to go — a
 * link to `#/live/turns/` with no id is a link to nothing — so it is not a
 * row, and it does not take one of the four places either. It is still
 * COUNTED: `working` is every working seat, for a list's figure, and `more`
 * is what the short list leaves out, id-less seats included, which Live ›
 * Now running lists one row each.
 */
export function runningShortList(agents: readonly AgentRow[]): {
  working: AgentRow[];
  shown: AgentRow[];
  more: number;
} {
  const working = workingLongestFirst(agents);
  const shown = working.filter((row) => turnIdOf(row) !== "").slice(0, RUNNING_ROWS);
  return { working, shown, more: working.length - shown.length };
}

/**
 * Where "go to the running turn" goes (`g r`): the one running turn's watch
 * link when exactly one seat is working and its turn has an id, and Live ›
 * Now running — every running turn, one row each — otherwise. With two
 * running there is no "the" turn to pick for the reader, and with none the
 * screen that says so is the one that would list it.
 */
export function runningTarget(agents: readonly AgentRow[]): {
  path: string[];
  query: Record<string, string>;
} {
  const working = workingLongestFirst(agents);
  const only = working.length === 1 ? turnIdOf(working[0]) : "";
  return only ? watchLink(only) : { path: ["live"], query: {} };
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
