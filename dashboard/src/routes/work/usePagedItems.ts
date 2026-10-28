/**
 * `work_items`, followed past its first page.
 *
 * THE LIST STOPPED AT A HUNDRED. The first page was the whole of what the
 * screen asked for — `limit: 100` and nothing after it — so the hundred-and-
 * first task in a project was unreachable from its own list: the foot said more
 * existed and offered no way to them. The answer has always carried a
 * `next_cursor`; this is the reader of it.
 *
 * # The first page polls until "Load more", and is then held
 *
 * The first page is an ordinary `useQuery`: polled, re-asked on reconnect and
 * after a write from this tab. The pages after it are asked ONCE, on "Load
 * more", and a filter change is a different list, so they are dropped rather
 * than appended to the new one. A held page is not re-polled: re-asking N
 * cursors every twenty seconds would multiply the list's cost by its length.
 *
 * So from "Load more" the first page is HELD too (`lib/usePaged.ts`): the next
 * page starts after the last task the first one was answered with, and a task
 * filed or moved into its range would push that one out of both pages when the
 * first was read again on its poll. A write from this tab still reads it again,
 * and the pages loaded are then read again from it, so an edit made on any of
 * them is drawn.
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
 * a link to the rest), so for them this is `useQuery` and nothing more: nothing
 * is ever held.
 */

import { useClient } from "~/lib/store-hooks.ts";
import { usePageHold, usePaged } from "~/lib/usePaged.ts";
import { useQuery, withFloor } from "~/lib/useQuery.ts";
import type { LogRefusal, QueryRefusal, WorkItemsAnswer, WorkSummary } from "~/protocol/index.ts";
import type { QueryErrorCode } from "~/contract/errors.ts";

export interface PagedItems {
  /**
   * The first page, as the engine answered it — groups, counts, coverage: the
   * one held, while one is, so the counts are those of the rows drawn.
   */
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
  /** Whether the first page is held, so the list is one reading: see the file's doc. */
  held: boolean;
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
  // THE QUESTION THE PAGES BELONG TO. Its serialisation, because the params
  // object is rebuilt by the screen and only its content is the list.
  const hold = usePageHold<WorkItemsAnswer>(params);
  const first = useQuery("work_items", params, { pollMs: options.pollMs, hold: hold.holding });
  const paged = usePaged(
    hold,
    first,
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
    loadMore: paged.older,
    paging: paged.paging,
    held: paged.held,
    pageError: paged.pageError,
    refetch: paged.refetch,
  };
}
