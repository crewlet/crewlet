/**
 * A history the engine answers a page at a time, followed back past its
 * newest page.
 *
 * The task page draws three — its comments (`work_comments`), the agent turns
 * charged to it (`work_item_turns`) and what changed on it (`work_activity`) —
 * and a seat's profile draws its turns (`turns {seat}`). Each is answered
 * newest first with a cursor to the page before, and each used to be read as
 * its first page and nothing more, so a task past twenty comments or a seat
 * past fifty turns had a history nobody could reach — and a count of the
 * page read as a count of the history.
 *
 * THE SHAPE `usePagedItems` HAS, for its reasons: the newest page is an
 * ordinary `useQuery` (polled, re-asked on reconnect and after this tab's own
 * write, at its read floor); every older page is asked ONCE when the reader
 * asks for it, and held against the question it was asked under. An entry is
 * drawn once, by its id, nearest the top.
 *
 * WHERE THE CURSOR IS is the caller's to say (`cursorOf`), because the
 * answers do not agree on a key: the tracker's histories carry `next_cursor`
 * and the turn list `next`, and a hook that read one of them would page the
 * other as a list with nothing before it.
 */

import { useCallback, useMemo, useState } from "react";
import type { QueryResult } from "~/lib/useQuery.ts";
import { queryErrorCode } from "~/protocol/index.ts";
import type { QueryErrorCode } from "~/contract/errors.ts";

/** No older page held — one constant, so an unpaged list is not a new array each render. */
const NONE: readonly never[] = [];

export interface Paged<T, A> {
  /** Every entry loaded, newest page first, each once. */
  items: T[];
  /** Every page's own answer, newest first — the names and seats each carries. */
  answers: readonly A[];
  loading: boolean;
  error: QueryErrorCode | null;
  /** Whether a page older than every one loaded exists. */
  more: boolean;
  /** Ask for it. A no-op when there is none or one is in flight. */
  older: () => void;
  paging: boolean;
  pageError: QueryErrorCode | null;
  refetch: () => void;
}

/**
 * THE QUESTION IS THE CALLER'S, asked with its kind written out at the call
 * — the newest page through `useQuery("…")` and every older one through
 * `socket.query("…")` — because a kind this hook took as a variable is one
 * the engine's reader gate cannot see (`app/source.test.ts`), and a question
 * no gate can see asked is one the engine may stop answering unnoticed.
 */
export function usePaged<A, T>(
  first: QueryResult<A>,
  /** The newest page's parameters: the older pages belong to this question. */
  params: Record<string, unknown>,
  /** Ask for the page before `cursor`, at this tab's read floor. */
  page: (cursor: string) => Promise<A>,
  pick: (answer: A) => readonly T[],
  idOf: (entry: T) => string,
  /** The cursor to the page before this answer, or "" where there is none. */
  cursorOf: (answer: A) => string,
): Paged<T, A> {
  const question = JSON.stringify(params);
  const [held, setHeld] = useState<{ question: string; pages: readonly A[] } | null>(null);
  const [paging, setPaging] = useState(false);
  const [pageError, setPageError] = useState<QueryErrorCode | null>(null);
  const pages: readonly A[] = held && held.question === question ? held.pages : NONE;
  const last = pages.length > 0 ? pages[pages.length - 1] : first.data;
  const next = last ? cursorOf(last) : "";

  const older = useCallback(async () => {
    if (!next || paging) return;
    setPaging(true);
    setPageError(null);
    try {
      const answer = await page(next);
      setHeld((prev) =>
        prev && prev.question === question
          ? { question, pages: [...prev.pages, answer] }
          : { question, pages: [answer] },
      );
    } catch (err) {
      setPageError(queryErrorCode(err instanceof Error ? err.message : null) ?? "query_failed");
    } finally {
      setPaging(false);
    }
  }, [page, question, next, paging]);

  const items = useMemo(() => {
    const seen = new Set<string>();
    const out: T[] = [];
    for (const answer of [first.data, ...pages]) {
      if (!answer) continue;
      for (const entry of pick(answer)) {
        const id = idOf(entry);
        if (seen.has(id)) continue;
        seen.add(id);
        out.push(entry);
      }
    }
    return out;
  }, [first.data, pages, pick, idOf]);

  const answers = useMemo(() => (first.data ? [first.data, ...pages] : pages), [first.data, pages]);

  return {
    items,
    answers,
    loading: first.loading,
    error: first.error,
    more: next !== "",
    older: () => void older(),
    paging,
    pageError,
    refetch: first.refetch,
  };
}
