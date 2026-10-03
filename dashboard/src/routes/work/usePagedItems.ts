/**
 * `work_items`, followed past its first page.
 *
 * THE LIST STOPPED AT A HUNDRED. The first page was the whole of what the
 * screen asked for — `limit: 100` and nothing after it — so the hundred-and-
 * first task in a project was unreachable from its own list: the foot said more
 * existed and offered no way to them. The answer has always carried a
 * `next_cursor`; this is the reader of it.
 *
 * # The first page polls and the rest are held
 *
 * The first page is an ordinary `useQuery`: polled, re-asked on reconnect and
 * after a write from this tab. The pages after it are asked ONCE, on "Load
 * more", and held against the question they were asked under — a filter
 * change is a different list, so they are dropped rather than appended to the
 * new one. A held page is not re-polled: re-asking N cursors every twenty
 * seconds would multiply the list's cost by its length, and the first page is
 * where a change lands for a reader looking at the top.
 *
 * # A row is drawn once
 *
 * A keyset cursor resumes after the last row it was minted on, so a task that
 * moved between two asks can appear on both pages. The merge keeps the FIRST
 * occurrence, which is the one nearest the top the reader is looking at — BY
 * ITS ID, which no two tasks share: a key two tasks hold (`key_collision`) is
 * one value for two rows, and a merge keyed on it dropped the second task from
 * the list as though it were the first one seen again.
 *
 * GROUPED ANSWERS HAVE NO CURSOR (a board's lanes each carry their own count and
 * a link to the rest), so for them this is `useQuery` and nothing more.
 */

import { useCallback, useMemo, useState } from "react";
import { useClient } from "~/lib/store-hooks.ts";
import { useQuery, withFloor } from "~/lib/useQuery.ts";
import {
  queryFailure,
  type LogRefusal,
  type QueryFailure,
  type QueryRefusal,
  type WorkItemsAnswer,
  type WorkSummary,
} from "~/protocol/index.ts";
import type { QueryErrorCode } from "~/contract/errors.ts";

export interface PagedItems {
  /** The first page, as the engine answered it — groups, counts, coverage. */
  data: WorkItemsAnswer | null;
  loading: boolean;
  error: QueryErrorCode | null;
  /** Why the first page was refused, beside `error` — see `QueryState`. */
  refusal: QueryRefusal | LogRefusal | null;
  /** Every row loaded, the first page's and every page after it, drawn once. */
  rows: WorkSummary[];
  /** Where the next page starts, or "" when the list is whole. */
  next: string;
  /** Ask for the next page. A no-op when there is none or one is in flight. */
  more: () => void;
  /** A page is in flight. */
  paging: boolean;
  /**
   * Why the last "Load more" failed, until the next one — WHOLE, its refusal
   * included ([queryFailure]), so a page the state log refused is not told to
   * try again.
   */
  pageFailure: QueryFailure | null;
  refetch: () => void;
}

/** Rows from several pages, each task once by its id, first occurrence kept. */
export function mergePages(pages: readonly (readonly WorkSummary[])[]): WorkSummary[] {
  const seen = new Set<string>();
  const out: WorkSummary[] = [];
  for (const page of pages) {
    for (const row of page) {
      if (seen.has(row.id)) continue;
      seen.add(row.id);
      out.push(row);
    }
  }
  return out;
}

const NO_PAGES: WorkItemsAnswer[] = [];

export function usePagedItems(
  params: Record<string, unknown>,
  options: { pollMs: number },
): PagedItems {
  const { socket } = useClient();
  const first = useQuery("work_items", params, { pollMs: options.pollMs });
  // THE QUESTION THE HELD PAGES BELONG TO. Its serialisation, because the
  // params object is rebuilt by the screen and only its content is the list.
  const question = JSON.stringify(params);
  const [held, setHeld] = useState<{ question: string; pages: WorkItemsAnswer[] } | null>(null);
  const [paging, setPaging] = useState(false);
  const [pageFailure, setPageFailure] = useState<QueryFailure | null>(null);
  const pages = held && held.question === question ? held.pages : NO_PAGES;
  const last = pages.length > 0 ? pages[pages.length - 1] : first.data;
  const next = last?.next_cursor ?? "";

  const more = useCallback(async () => {
    if (!next || paging) return;
    setPaging(true);
    setPageFailure(null);
    try {
      // AT THIS TAB'S READ FLOOR, like every other ask: a page read from a
      // node behind a write this tab just made would draw the task as it was.
      const page = await socket.query(
        "work_items",
        withFloor("work_items", JSON.stringify({ ...params, cursor: next })),
      );
      setHeld((prev) =>
        prev && prev.question === question
          ? { question, pages: [...prev.pages, page] }
          : { question, pages: [page] },
      );
    } catch (err) {
      setPageFailure(queryFailure(err));
    } finally {
      setPaging(false);
    }
  }, [socket, params, question, next, paging]);

  const rows = useMemo(
    () => mergePages([first.data?.items ?? [], ...pages.map((p) => p.items ?? [])]),
    [first.data, pages],
  );

  return {
    data: first.data,
    loading: first.loading,
    error: first.error,
    refusal: first.refusal,
    rows,
    next,
    more: () => void more(),
    paging,
    pageFailure,
    refetch: first.refetch,
  };
}
