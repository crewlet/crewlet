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
import { itemPath, type ItemRef } from "~/lib/work.ts";
import type { WorkItemRef } from "~/protocol/index.ts";

/** The most turns one page of the walk asks for — the engine's own cap. */
const PAGE = 50;

/**
 * The spelling a turn's task is ASKED for by — `work_item`, `work_item_turns`
 * — and opened by until a read of it has said more ([taskPath]): its ID where
 * it is the engine's own task, else its key.
 *
 * NEVER A NATIVE TASK'S KEY. Two tasks can hold one key, and the engine
 * answers a key with the task that claimed it first (`lib/work.ts`'s
 * `itemAddress`). A turn's work item carries no `key_collision` — that is the
 * tracker's derived column, not part of what a turn is charged to — so a turn
 * on the task that did NOT claim its key, asked for by the key, was numbered,
 * titled and opened as the other task. The id is the task's identity
 * (`types.WorkItem`), which no collision moves. A vendor's item, or one whose
 * turn recorded no id, keeps its key: a vendor's id is the vendor's, which the
 * engine's own tracker does not hold.
 */
export function taskAsked(task: WorkItemRef | null | undefined): string {
  if (!task) return "";
  return task.backend === "native" && task.id ? task.id : task.key;
}

/**
 * Whether an answer — the id and key of the task it is about — is about THIS
 * turn's task, compared on the spelling it was asked by ([taskAsked]). An
 * answer still held for the task the seat was on a moment ago is not: while a
 * seat moves between tasks, its number and its title would name the wrong one.
 */
export function answersFor(
  task: WorkItemRef | null | undefined,
  answered: { id: string; key: string } | null | undefined,
): boolean {
  if (!task || !answered) return false;
  return task.backend === "native" && task.id ? answered.id === task.id : answered.key === task.key;
}

/**
 * Where a turn's task opens: through `itemAddress` once a read of the task has
 * answered — the answer carries the `key_collision` the turn's reference does
 * not, so an unshared key opens by the key a reader types — and otherwise by
 * [taskAsked], which for a native task is its id and so opens the right task
 * whichever of two claimed its key.
 */
export function taskPath(task: WorkItemRef, answered: ItemRef | null): string[] {
  return answered ? itemPath(answered) : ["work", taskAsked(task)];
}

export function useTurnOrdinal(
  task: WorkItemRef | null | undefined,
  turnId: string,
  running: boolean,
  /** When the turn began, in ms — the walk's bound; 0 where nothing says. */
  startedAt: number,
): number | null {
  const asked = taskAsked(task);
  const [cursor, setCursor] = useState("");
  // A NEW TURN OR TASK STARTS THE WALK AGAIN from the newest page.
  useEffect(() => setCursor(""), [asked, turnId]);
  const page = useQuery(
    "work_item_turns",
    { id: asked, limit: PAGE, ...(cursor ? { cursor } : {}) },
    { enabled: asked !== "" && turnId !== "" },
  );
  // ONLY THIS TASK'S PAGE: one held for the task asked a moment ago would
  // number this turn against somebody else's history.
  const mine = !!page.data && answersFor(task, { id: page.data.item, key: page.data.key });
  const rows = (mine ? page.data?.turns : undefined) ?? [];
  const found = rows.find((r) => r.turn_id === turnId);
  const next = (mine ? page.data?.next_cursor : undefined) ?? "";
  const oldest = rows.length ? Date.parse(rows[rows.length - 1]!.at) : NaN;
  const past = !(startedAt > 0) || (Number.isFinite(oldest) && oldest < startedAt);
  useEffect(() => {
    if (!mine || found || running || past || !next || next === cursor) return;
    setCursor(next);
  }, [mine, found, running, past, next, cursor]);

  if (!asked || !mine) return null;
  if (found) return found.ordinal > 0 ? found.ordinal : null;
  if (running && cursor === "") return (rows[0]?.ordinal ?? 0) + 1;
  return null;
}
