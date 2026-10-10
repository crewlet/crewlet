/**
 * Which turn this is on its task — "Turn 3" — from the turn's own story.
 *
 * `turn_story` names the work a session is charged to with the task's own
 * count of its turns (`work_item.ordinal`): the tracker places a session once
 * a segment of it is recorded, and a session still running is the task's
 * next one, which the engine reads off the same count. So the number on a
 * turn's crumb, on a seat's state card and on the task's Activity tab is ONE
 * reading, made where the count lives, by the turn's id.
 *
 * It replaced a walk of the task's turn list from its newest page until the
 * turn turned up, which each of the three readers repeated in its own way —
 * one paging back through a long task's history to render a crumb, two
 * guessing "the newest plus one" from a single page.
 *
 * Null where it cannot say: the turn is not on a task, its story says it is on
 * ANOTHER one (the reader's key is stale), the task does not count turns (a
 * vendor's issue), or the answer has not landed. A session that is not a story
 * is answered "not found", which is null too — and no session charged to a task
 * is one of those.
 */

import { useQuery } from "~/lib/useQuery.ts";

export function useTurnOrdinal(itemKey: string, turnId: string): number | null {
  const story = useQuery(
    "turn_story",
    { turn_id: turnId },
    { enabled: itemKey !== "" && turnId !== "" },
  );
  if (!itemKey || !turnId) return null;
  const item = story.data?.story?.work_item;
  if (!item || item.key !== itemKey) return null;
  return item.ordinal && item.ordinal > 0 ? item.ordinal : null;
}
