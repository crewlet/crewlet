/**
 * Which turn this is on its task — "Turn 3" — from the task's own turn list.
 *
 * `work_item_turns` is the tracker's durable account of the turns charged to
 * a task, newest first, a page at a time, and a turn's ORDINAL is on its row.
 * This walks the pages from the newest until it finds the turn, so a turn
 * deep in a long task's history is still numbered rather than guessed. A turn
 * still running has no row yet — it is recorded when it completes — so it is
 * the next one after the newest: the reading the seat's peek makes.
 *
 * Null where it cannot say: the task could not be read, the walk ended without
 * finding the turn (it was charged elsewhere, or not at all), or the turn is
 * not on a task.
 *
 * THE WALK IS BOUNDED BY THE TURN'S OWN START. The list is newest first by
 * when each turn landed, and a turn lands after it starts — so once a page
 * reaches a row that landed before this turn began, no later page can hold
 * it. Unbounded, a turn charged to ANOTHER task walked every page of this
 * one's history, one query per fifty turns, to render a crumb. A turn whose
 * start nothing stamped has no bound, and is looked for on the newest page
 * alone.
 */

import { useEffect, useState } from "react";
import { useQuery } from "~/lib/useQuery.ts";

/** The most turns one page of the walk asks for — the engine's own cap. */
const PAGE = 50;

export function useTurnOrdinal(
  itemKey: string,
  turnId: string,
  running: boolean,
  /** When the turn began, in ms — the walk's bound; 0 where nothing says. */
  startedAt: number,
): number | null {
  const [cursor, setCursor] = useState("");
  // A NEW TURN OR TASK STARTS THE WALK AGAIN from the newest page.
  useEffect(() => setCursor(""), [itemKey, turnId]);
  const page = useQuery(
    "work_item_turns",
    { id: itemKey, limit: PAGE, ...(cursor ? { cursor } : {}) },
    { enabled: itemKey !== "" && turnId !== "" },
  );
  const rows = page.data?.turns ?? [];
  const found = rows.find((r) => r.turn_id === turnId);
  const next = page.data?.next_cursor ?? "";
  const oldest = rows.length ? Date.parse(rows[rows.length - 1]!.at) : NaN;
  const past = !(startedAt > 0) || (Number.isFinite(oldest) && oldest < startedAt);
  useEffect(() => {
    if (!page.data || found || running || past || !next || next === cursor) return;
    setCursor(next);
  }, [page.data, found, running, past, next, cursor]);

  if (!itemKey || !page.data) return null;
  if (found) return found.ordinal > 0 ? found.ordinal : null;
  if (running && cursor === "") return (rows[0]?.ordinal ?? 0) + 1;
  return null;
}
