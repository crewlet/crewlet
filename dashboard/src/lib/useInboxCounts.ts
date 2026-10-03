/**
 * How many notices are waiting on the viewer — ONE reading, for every surface
 * that says it.
 *
 * The sidebar's Inbox badge, Home's status line and the Inbox's own Notices
 * band all say how much is waiting, and a read per surface was a chance per
 * surface to disagree: each polled on its own minute, so on the Inbox the
 * badge and the band could name two numbers for most of a minute. So the
 * question is asked ONCE, by the frame ([InboxCountsProvider], which
 * `app/Shell.tsx` mounts), and every surface reads that one answer
 * ([useInboxCounts]).
 *
 * # What is counted, and why it is not `unread`
 *
 * `unread` counts every notice on the page, and most of a busy company's
 * notices are things it merely told you: a task you watch moved, a project
 * you own was updated. Nobody answers those, so a badge built on them never
 * reaches zero however diligent the reader is — and a number that cannot go
 * down is read, correctly, as a broken counter. The PRIMARY half is the set a
 * person is on the hook for (a mention, a question, work assigned to them),
 * it is small by construction, and it goes down by answering. The engine
 * states which reasons were applied as primary — defaulted from the person's
 * own record — so this is the company's own split rather than one the client
 * invented.
 *
 * THE ENGINE NARROWS, NOT THE CLIENT. The read asks for unread primary
 * notices only (`unread`, `primary_only`; snoozed ones are excluded by the
 * engine's own default, since a snooze means "not now"), so the page IS the
 * set being counted. It used to be every notice on the first page, filtered
 * here — which undercounted by construction once that page filled with read
 * or informational notices, and drew the result as an exact figure.
 *
 * ONE PAGE, and `capped` says so: past [INBOX_PAGE] the engine hands back a
 * cursor, the count is a floor and is written as one ("50+"), never as a total
 * the engine did not compute.
 *
 * # Whose, and when it is asked again
 *
 * UNDER `owner`, the one name the viewer's own record is kept under — their
 * seat when bound, their login when not — so an UNBOUND reader's badge counts
 * the notices their record actually holds rather than nothing: an unbound
 * principal is an ordinary state (`lib/viewer.ts`), and asking by `handle`
 * left their badge blank while their assistant wrote notices to their login.
 *
 * PUSHED AS WELL AS POLLED. The frame watches the viewer's own record
 * (`app/Shell.tsx`), the engine sends `inbox_changed` when a committed batch
 * moves it, and the read asks again within half a second of one
 * (`refetchOnInboxOf`). The minute's poll stays, because a frame lost to
 * backpressure or a reconnect is never re-sent and a node that cannot decide
 * the watch sends none until it can.
 */

import { createContext, createElement, useContext, useMemo, type ReactNode } from "react";
import { useQuery } from "./useQuery.ts";
import { useViewer } from "./viewer.ts";

/** The page the count is taken over — the engine's own bound on the read. */
export const INBOX_PAGE = 50;

export interface InboxCounts {
  /** Unread primary notices on the first page; null until the read answers
   *  or where there is nobody to ask for (anonymous). */
  waiting: number | null;
  /** More exist past this page, so `waiting` is a floor. */
  capped: boolean;
}

/**
 * The one question, for whoever's inbox it is — the record named `owner`.
 * `watched` is whether this tab's socket watches that record, which only the
 * viewer's own is: a push for any other never reaches the tab.
 */
function useInboxRead(owner: string, enabled: boolean, watched: boolean): InboxCounts {
  const inbox = useQuery(
    "work_inbox",
    owner ? { handle: owner, limit: INBOX_PAGE, unread: true, primary_only: true } : undefined,
    {
      enabled: enabled && owner !== "",
      pollMs: 60_000,
      refetchOnInboxOf: watched ? owner : "",
    },
  );
  return useMemo(() => {
    const data = inbox.data;
    if (!data) return { waiting: null, capped: false };
    return { waiting: data.notices.length, capped: Boolean(data.next_cursor) };
  }, [inbox.data]);
}

const Reading = createContext<InboxCounts | null>(null);

/**
 * Ask once, for everything under it. Inside [ViewerProvider], whose viewer
 * it asks for — a count that asked who the viewer is on its own would be a
 * second standing read of the one fact a tab has about who it is.
 */
export function InboxCountsProvider({ children }: { children: ReactNode }) {
  const counts = useInboxRead(useViewer().owner, true, true);
  return createElement(Reading.Provider, { value: counts }, children);
}

/**
 * The count the frame read.
 *
 * THROWS OUTSIDE ONE rather than asking for itself, as the kit's
 * `useAppShell` does outside a shell: a fallback read here is the per-surface
 * read this module exists to remove, back again the day a surface is mounted
 * somewhere the frame is not — and it would work, so nothing would say so.
 */
export function useInboxCounts(): InboxCounts {
  const counts = useContext(Reading);
  if (counts === null) {
    throw new Error(
      "useInboxCounts() outside an InboxCountsProvider: the frame mounts one (FrameReadings, app/Shell.tsx), and a suite that mounts a screen without the frame mounts one around it",
    );
  }
  return counts;
}

/**
 * The same count for SOMEBODY'S inbox: a person's profile says it for them.
 *
 * THE VIEWER'S OWN IS THE FRAME'S READING, never a second read of it — so the
 * figure on your own profile and the badge in the sidebar are one answer and
 * cannot name two numbers. Anybody else's (an operator reading a colleague's
 * day) is the same question asked of their inbox, which the engine answers
 * only for an operator; `enabled` is the caller's word that this reader may
 * ask. It used to be the length of the person record's `unread` list, which
 * is not a count at all: it holds the EXCEPTIONS below the read mark, notices
 * marked unread again, so a person with fifty waiting read "Unread 0".
 */
export function useInboxCountsOf(handle: string, enabled: boolean): InboxCounts {
  const viewer = useViewer();
  // THE VIEWER'S OWN RECORD under either name it answers to: a bound person's
  // record is kept under their seat, and a profile names them by it.
  const own = handle !== "" && (handle === viewer.owner || handle === viewer.handle);
  const frame = useInboxCounts();
  const theirs = useInboxRead(handle, enabled && !own, false);
  return own ? frame : theirs;
}

/** The figure as a badge writes it: a floor carries a "+". */
export function inboxFigure(counts: InboxCounts): string {
  if (counts.waiting === null) return "";
  return counts.capped ? `${counts.waiting}+` : String(counts.waiting);
}
