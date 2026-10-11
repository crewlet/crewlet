/**
 * How much open work is on a person — ONE question, for every surface that
 * says it.
 *
 * The Queue tab's figure and the list under it were once two numbers about
 * two different things — a count of the questions put to the reader beside a
 * list of the work assigned to them — and a person reading "1" over five rows
 * had no way to tell which was wrong, because neither was. So there is one
 * question ([queueCountParams]), asked by the screen that draws the figure:
 * My work, on anybody's day.
 *
 * NOT A FRAME READING ANY MORE. The person sidebar's My work row carries no
 * figure — a count beside a row on every screen is a mark a reader learns to
 * skip, and what is on a person is their Queue's tab — so a reading the frame
 * held for every screen would poll for a number nothing outside My work draws.
 *
 * # What is counted
 *
 * The work ASSIGNED to the person that is still to do — the tracker's open
 * groups, `not_started` and `active` — and EVERY TASK ON ITS OWN
 * (`subtasks=separate`), which is how the Queue's list draws them: in the
 * grammar's default a root this person holds brings its whole subtree along
 * unfiltered, so a count taken that way took in sub-tasks held by somebody
 * else, and finished ones, that the list never drew.
 *
 * The questions put to the person are NOT in it. They have a section of their
 * own on My work (Asked of me), with its own count, and each one reached the
 * person as an Inbox notice, which is the badge that asks to be cleared.
 *
 * THE ENGINE COUNTS, ONE ROW IS READ. `total_hint` is counted over the whole
 * matching set whatever the page is, so the read asks for a single row — the
 * count used to ride a fifty-row page, with every card's facts, every poll.
 */

import { useMemo } from "react";
import { useQuery } from "./useQuery.ts";
import { SCOPE_GROUPS } from "./work.ts";
import type { WorkClaimTotal, WorkItemsAnswer } from "~/protocol/index.ts";

/**
 * The status groups the Queue's count is taken over: unfinished work.
 *
 * THE LIST'S OWN OPEN SEGMENT, by reference — the groups the Queue's "N in
 * Open" line is counted over — and deliberately NOT the segment the reader
 * has pressed now: the figure says how much is on this person, and a reader
 * who presses Closed to check something finished has not emptied their day.
 * It was a second literal of the same two groups, with nothing tying the two
 * together, which is how the tab and the Queue's own line would come to count
 * different things the day either changed.
 */
export const QUEUE_SCOPE = SCOPE_GROUPS.open;

/** A count read asks the engine for one row: only `total_hint` is read. */
const COUNT_ONLY = 1;

/**
 * The Queue's count, as `work_items` asks it — ONE PLACE for the question, so
 * the tab on one person's day and on another's cannot count different things.
 */
export function queueCountParams(handle: string): Record<string, unknown> {
  return {
    container: "workspace",
    assignee: handle,
    status_group: QUEUE_SCOPE,
    subtasks: "separate",
    limit: COUNT_ONLY,
  };
}

/** A tracker list's own count, as a claim — or undefined until it answers. */
export function listTotal(answer: WorkItemsAnswer | null): WorkClaimTotal | undefined {
  return answer ? { total: answer.total_hint, capped: answer.total_capped } : undefined;
}

/**
 * The standing read for one person's queue count.
 *
 * 30 SECONDS, the interval My work's other readings of a day take: a count
 * that moves when somebody assigns work is read, not watched, and it has no
 * push behind it.
 */
export function useQueueCountRead(handle: string, enabled = true): WorkClaimTotal | undefined {
  const read = useQuery("work_items", handle ? queueCountParams(handle) : undefined, {
    enabled: enabled && handle !== "",
    pollMs: 30_000,
  });
  return useMemo(() => listTotal(read.data), [read.data]);
}
