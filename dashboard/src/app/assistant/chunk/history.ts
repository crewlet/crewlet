/**
 * The reader's conversations with Ask Crewlet, as its page lists them: newest
 * first, filed under the day they were last used, a page at a time, found by
 * their title.
 *
 * THE ENGINE'S LIST, READ AGAIN WHEN THE OPEN CONVERSATION MOVES. A question
 * asked here makes a conversation, an answer that ends moves it to the top,
 * and its title is written by the engine a moment after the first answer, on
 * the stream the open conversation reads. Each of those is a change of the
 * store this page draws (its id, its title, the turn that ended), so the list
 * is read again on each, and never on a timer.
 */

import { useCallback, useEffect, useRef, useState } from "react";
import { call } from "~/protocol/assistant.ts";
import { civilAt, readerDay } from "~/lib/format.ts";
import type { AssistantConversationList } from "~/contract/assistant.ts";

/** One conversation as the list draws it. */
export type HistoryRow = AssistantConversationList["conversations"][number];

/**
 * How many conversations a page of the list reads: the engine's largest page
 * (`GET /assistant/conversations` takes 1..50), so most readers' thirty days
 * are one read.
 */
export const HISTORY_PAGE = 50;

/** The longest title search the engine takes, in characters. */
export const HISTORY_QUERY_MAX = 100;

/** The day groups, newest first, in the words the list heads them with. */
export const HISTORY_GROUPS = ["Today", "Yesterday", "Previous 7 days", "Older"] as const;
export type HistoryGroup = (typeof HISTORY_GROUPS)[number];

const DAY_MS = 86_400_000;

/** The group a conversation last used at `at` is filed under, on the reader's calendar. */
export function groupOf(at: string, now: number): HistoryGroup {
  const used = civilAt(readerDay(Date.parse(at)));
  const today = civilAt(readerDay(now));
  if (used === null || today === null) return "Older";
  const days = Math.round((today - used) / DAY_MS);
  if (days <= 0) return "Today";
  if (days === 1) return "Yesterday";
  if (days <= 7) return "Previous 7 days";
  return "Older";
}

/** The rows under each group that holds any, in the list's order. */
export function grouped(
  rows: readonly HistoryRow[],
  now: number,
): { group: HistoryGroup; rows: HistoryRow[] }[] {
  const by = new Map<HistoryGroup, HistoryRow[]>();
  for (const row of rows) {
    const group = groupOf(row.last_activity_at, now);
    by.set(group, [...(by.get(group) ?? []), row]);
  }
  return HISTORY_GROUPS.flatMap((group) => {
    const held = by.get(group);
    return held ? [{ group, rows: held }] : [];
  });
}

/** What the list holds, and how to read more of it. */
export interface History {
  rows: readonly HistoryRow[] | null;
  /** Why the last read failed, or null. */
  failed: string | null;
  /** Whether older conversations are left to read. */
  more: boolean;
  loading: boolean;
  /** Read the next page. */
  older: () => void;
  /** Read the list again from its start. */
  reload: () => void;
  /** Take a conversation out of the list at once, before the engine is read again. */
  drop: (id: string) => void;
}

/**
 * The reader's conversations matching `query` (all of them for an empty
 * one), read again whenever `moved` changes.
 */
export function useHistory(query: string, moved: string): History {
  const [rows, setRows] = useState<HistoryRow[] | null>(null);
  const [cursor, setCursor] = useState<string | null>(null);
  const [failed, setFailed] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [asked, setAsked] = useState(0);
  // A LATER READ WINS: an answer for an earlier query or an earlier move is
  // dropped rather than drawn over the one asked since.
  const generation = useRef(0);

  const read = useCallback(
    async (from: string | null) => {
      const mine = ++generation.current;
      setLoading(true);
      try {
        const answer = await call("GET /assistant/conversations", null, {
          query: {
            limit: HISTORY_PAGE,
            cursor: from ?? undefined,
            q: query.trim() === "" ? undefined : query.trim(),
          },
        });
        if (mine !== generation.current) return;
        setRows((held) =>
          from === null ? answer.conversations : [...(held ?? []), ...answer.conversations],
        );
        setCursor(answer.next_cursor ?? null);
        setFailed(null);
      } catch {
        if (mine !== generation.current) return;
        setFailed("Your conversations could not be read. Try again in a moment.");
      } finally {
        if (mine === generation.current) setLoading(false);
      }
    },
    [query],
  );

  useEffect(() => {
    void read(null);
  }, [read, moved, asked]);

  return {
    rows,
    failed,
    more: cursor !== null,
    loading,
    older: () => {
      if (cursor !== null && !loading) void read(cursor);
    },
    reload: () => setAsked((n) => n + 1),
    drop: (id) => setRows((held) => held && held.filter((row) => row.id !== id)),
  };
}
