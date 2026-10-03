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
 */

import { createContext, createElement, useContext, useMemo, type ReactNode } from "react";
import { useQuery } from "./useQuery.ts";
import { useViewer } from "./viewer.ts";

/** The page the count is taken over — the engine's own bound on the read. */
export const INBOX_PAGE = 50;

export interface InboxCounts {
  /** Unread primary notices on the first page; null until the read answers
   *  or where there is nobody to ask for (anonymous, unbound). */
  waiting: number | null;
  /** More exist past this page, so `waiting` is a floor. */
  capped: boolean;
}

/** The one question, for whoever's inbox it is. */
function useInboxRead(handle: string, enabled: boolean): InboxCounts {
  const inbox = useQuery(
    "work_inbox",
    handle ? { handle, limit: INBOX_PAGE, unread: true, primary_only: true } : undefined,
    { enabled: enabled && handle !== "", pollMs: 60_000 },
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
  const counts = useInboxRead(useViewer().handle, true);
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
  const own = handle !== "" && handle === viewer.handle;
  const frame = useInboxCounts();
  const theirs = useInboxRead(handle, enabled && !own);
  return own ? frame : theirs;
}

/** The figure as a badge writes it: a floor carries a "+". */
export function inboxFigure(counts: InboxCounts): string {
  if (counts.waiting === null) return "";
  return counts.capped ? `${counts.waiting}+` : String(counts.waiting);
}
