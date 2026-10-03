/**
 * How much open work is on the viewer — ONE reading, for every surface that
 * says it.
 *
 * The sidebar's My work figure and the Queue tab on the viewer's own day are
 * one number about one person, and they were two numbers about two different
 * things: the sidebar counted the questions put to the reader, the tab counted
 * the work assigned to them, and the page under the tab could be a third list
 * again. A person reading "My work 1", "Queue 2" and five rows had no way to
 * tell which figure was wrong, because none of them was. So the figure a
 * person meets first — the sidebar's — is the Queue's, and it is ASKED ONCE,
 * by the frame ([QueueCountProvider], which `app/Shell.tsx` mounts): the two
 * surfaces read one answer, the way the Inbox badge and the Inbox's own count
 * do (`useInboxCounts.ts`), rather than polling on two clocks and naming two
 * numbers for most of a minute after a change.
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

import { createContext, createElement, useContext, useMemo, type ReactNode } from "react";
import { useQuery } from "./useQuery.ts";
import { useViewer } from "./viewer.ts";
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
 * together, which is how the sidebar and the Queue's own line would come to
 * count different things the day either changed.
 */
export const QUEUE_SCOPE = SCOPE_GROUPS.open;

/** A count read asks the engine for one row: only `total_hint` is read. */
const COUNT_ONLY = 1;

/**
 * The Queue's count, as `work_items` asks it — ONE PLACE for the question the
 * frame asks for the viewer and My work asks for somebody else's day, so the
 * two cannot count different things.
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
 * 30 SECONDS, the interval My work's other readings of a day take. `watched`
 * is whether this tab's socket watches that person's record, which only the
 * viewer's own is: an assignment writes its assignee a notice, so the frame
 * that moves their inbox (`inbox_changed`) asks the count again within half a
 * second, and the poll is the backstop for a frame lost to a reconnect.
 */
export function useQueueCountRead(
  handle: string,
  enabled = true,
  watched = false,
): WorkClaimTotal | undefined {
  const read = useQuery("work_items", handle ? queueCountParams(handle) : undefined, {
    enabled: enabled && handle !== "",
    pollMs: 30_000,
    refetchOnInboxOf: watched ? handle : "",
  });
  return useMemo(() => listTotal(read.data), [read.data]);
}

/**
 * What the frame read, boxed so that a claim nothing has answered yet
 * (`undefined`) is a different value from no frame at all (`null`).
 */
interface QueueCount {
  claim: WorkClaimTotal | undefined;
}

const Reading = createContext<QueueCount | null>(null);

/**
 * Ask once, for everything under it. Inside [ViewerProvider], whose viewer it
 * asks for, for the reason [InboxCountsProvider] is — and under `owner`, the
 * name the viewer's own record is kept under, for the reason it gives: asked
 * by `handle`, an unbound reader's count was withheld for ever, a blank where
 * the engine would have answered.
 */
export function QueueCountProvider({ children }: { children: ReactNode }) {
  const claim = useQueueCountRead(useViewer().owner, true, true);
  const value = useMemo(() => ({ claim }), [claim]);
  return createElement(Reading.Provider, { value }, children);
}

/**
 * The viewer's own Queue count, as the frame read it.
 *
 * THROWS OUTSIDE ONE rather than asking for itself, for the reason
 * `useInboxCounts` does: a fallback read here is the second, separately polled
 * read this module exists to remove, and it would work, so nothing would say
 * so.
 */
export function useOwnQueueCount(): WorkClaimTotal | undefined {
  const reading = useContext(Reading);
  if (reading === null) {
    throw new Error(
      "useOwnQueueCount() outside a QueueCountProvider: the frame mounts one (FrameReadings, app/Shell.tsx), and a suite that mounts a screen without the frame mounts one around it",
    );
  }
  return reading.claim;
}
