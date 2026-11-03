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
 * THE SHARED PAGER (`lib/usePaged.ts`), not one of this file's own: the next
 * page starts after the last task the first one was answered with, and a task
 * filed or moved into its range pushes that one off the foot of the polled
 * first page — onto neither page, a hole drawn as continuity. The pager joins
 * a moved first page to the pages held before the list is drawn again.
 *
 * # A row is drawn once
 *
 * A keyset cursor resumes after the last row it was minted on, so a task that
 * moved between two asks can appear on both pages. The merge keeps the FIRST
 * occurrence by the task's ID, which is the one nearest the top the reader is
 * looking at — never by its key, which two tasks can hold (`key_collision`):
 * merged on it, the second of the pair was dropped from every list.
 *
 * GROUPED ANSWERS HAVE NO CURSOR (a board's lanes each carry their own count and
 * a link to the rest), so for them this is `useQuery` and nothing more.
 */

import { useClient } from "~/lib/store-hooks.ts";
import { usePaged } from "~/lib/usePaged.ts";
import { useQuery, withFloor } from "~/lib/useQuery.ts";
import type { LogRefusal, QueryRefusal, WorkItemsAnswer, WorkSummary } from "~/protocol/index.ts";
import type { QueryErrorCode } from "~/contract/errors.ts";

export interface PagedItems {
  /** The first page, as the engine answered it — groups, counts, coverage. */
  data: WorkItemsAnswer | null;
  loading: boolean;
  error: QueryErrorCode | null;
  /** What the first page's refusal said beyond its code — see `useQuery`. */
  refusal: QueryRefusal | LogRefusal | null;
  /** Every row loaded, the first page's and every page after it, drawn once. */
  rows: WorkSummary[];
  /** Whether a page after every one loaded exists. */
  more: boolean;
  /** Ask for it. A no-op when there is none or one is in flight. */
  loadMore: () => void;
  /** A page is in flight. */
  paging: boolean;
  /** Why the last "Load more" failed, until the next one. */
  pageError: QueryErrorCode | null;
  refetch: () => void;
}

/** A page's tasks, each task's id, and the cursor to the page after. */
const itemsOf = (a: WorkItemsAnswer) => a.items ?? [];
const itemIdOf = (row: WorkSummary) => row.id;
const itemsAfter = (a: WorkItemsAnswer) => a.next_cursor ?? "";

export function usePagedItems(
  params: Record<string, unknown>,
  options: { pollMs: number },
): PagedItems {
  const { socket } = useClient();
  const first = useQuery("work_items", params, { pollMs: options.pollMs });
  const paged = usePaged(
    first,
    params,
    // AT THIS TAB'S READ FLOOR, like every other ask: a page read from a node
    // behind a write this tab just made would draw the task as it was.
    (cursor) =>
      socket.query("work_items", withFloor("work_items", JSON.stringify({ ...params, cursor }))),
    itemsOf,
    itemIdOf,
    itemsAfter,
  );

  return {
    data: paged.answers[0] ?? null,
    loading: paged.loading,
    error: paged.error,
    refusal: paged.refusal,
    rows: paged.items,
    more: paged.more,
    loadMore: paged.next,
    paging: paged.paging,
    pageError: paged.pageError,
    refetch: paged.refetch,
  };
}
