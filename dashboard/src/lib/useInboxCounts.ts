/**
 * How much is waiting on the viewer — ONE reading, for every surface that
 * says it.
 *
 * The sidebar's Inbox badge, Home's status line and a person's own profile all
 * say how much waits, and a read per surface was a chance per surface to
 * disagree. So the question is asked ONCE, by the frame
 * ([InboxCountsProvider], which `app/Shell.tsx` mounts), and every surface
 * reads that one answer ([useInboxCounts]).
 *
 * # What is counted: what needs the person to act, and nothing else
 *
 * It is `lib/decisions.ts`' [needsYouCount]: the engine's `total` of what
 * `decisions` puts to them (the open asks addressed to them, the runs parked
 * on them, and the runs they may answer whose delegates escalated them or let
 * them pass their deadline), the budget-stopped seats they can act on, and the
 * conditions a person decides that they have not put off.
 *
 * IT USED TO BE THE UNREAD PRIMARY NOTICES, and the primary split is "what
 * leads your updates" (`worklet.DefaultPrimaryReasons`): a mention, a comment
 * on a task you are assigned or reported, an answer to your question. Nobody
 * acts on those, so a badge built on them counted comments and mentions as
 * work waiting, and on a busy company never reached zero. They stay listed in
 * the Inbox's Updates, readable and markable as read; they drive no figure.
 * A run that a reader may answer but that waits on somebody else is listed too,
 * in its own group, and counted by nothing until it waits on them.
 *
 * ONE FIGURE WHATEVER THE COMPANY RUNS: a company without Worklet has no asks
 * but has parked runs, stopped seats and conditions, and is counted the same
 * way rather than by a second rule.
 *
 * `capped` says the engine's count of asks reached its ceiling, so the figure
 * is a floor and written as one ("50+").
 *
 * # Whose, and when it is asked again
 *
 * The viewer's own record: `decisions` asked with no name answers the caller's
 * own (their seat when bound, their login when not). Re-asked on the frame's
 * `inbox_changed` push for that record (an ask put to them lands as a notice)
 * and on a poll, which also catches a run that parked or was answered.
 */

import { createContext, createElement, useContext, type ReactNode } from "react";
import { useQuery } from "./useQuery.ts";
import { useNeedsYouCount, type NeedsYouCount } from "./decisions.ts";
import { useViewer } from "./viewer.ts";

/** The page the Inbox's lists are read in — the engine's own bound on a read. */
export const INBOX_PAGE = 50;

/** How often what waits on the viewer is asked again without a push. */
const NEEDS_POLL_MS = 30_000;

export type InboxCounts = NeedsYouCount;

const Reading = createContext<InboxCounts | null>(null);

/**
 * Ask once, for everything under it. Inside [ViewerProvider], whose viewer
 * it asks for — a count that asked who the viewer is on its own would be a
 * second standing read of the one fact a tab has about who it is.
 */
export function InboxCountsProvider({ children }: { children: ReactNode }) {
  const viewer = useViewer();
  const counts = useNeedsYouCount(viewer.owner !== "", NEEDS_POLL_MS, viewer.owner);
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
 * The same count for SOMEBODY'S day: a person's profile says it for them.
 *
 * THE VIEWER'S OWN IS THE FRAME'S READING, never a second read of it — so the
 * figure on your own profile and the badge in the sidebar are one answer and
 * cannot name two numbers. Anybody else's (a lead or an admin reading a
 * colleague's day) is the engine's `total` of what `decisions` puts to them,
 * which it answers only to them and their lead; `enabled` is the caller's
 * word that this reader may ask.
 */
export function useInboxCountsOf(handle: string, enabled: boolean): InboxCounts {
  const viewer = useViewer();
  // THE VIEWER'S OWN RECORD under either name it answers to: a bound person's
  // record is kept under their seat, and a profile names them by it.
  const own = handle !== "" && (handle === viewer.owner || handle === viewer.handle);
  const frame = useInboxCounts();
  const theirs = useQuery(
    "decisions",
    { handle },
    {
      enabled: enabled && !own && handle !== "",
      pollMs: 60_000,
    },
  );
  if (own) return frame;
  const data = theirs.data;
  return data ? { waiting: data.total, capped: data.capped } : { waiting: null, capped: false };
}

/**
 * Where the reader's Needs you holds the standing alarms (`lib/alarms.ts`'
 * alarmPlace): the frame's reading, never a second read of it, and null
 * outside the frame or for a reader told no notice, where the engine panel
 * then says nothing about the Inbox.
 */
export function useAlarmPlaces(): { needs: readonly string[]; putOff: readonly string[] } | null {
  return useContext(Reading)?.alarms ?? null;
}

/** The figure as a badge writes it: a floor carries a "+". */
export function inboxFigure(counts: InboxCounts): string {
  if (counts.waiting === null) return "";
  return counts.capped ? `${counts.waiting}+` : String(counts.waiting);
}
