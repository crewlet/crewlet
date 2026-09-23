/**
 * Reading past the first page of a newest-first list, with the cursor the
 * engine's own answer carries.
 *
 * # The first page is frozen while older ones are held
 *
 * Every list this serves is NEWEST FIRST and its first page can be read
 * again — a poll, a reconnect — so a row arriving at the head pushes the live
 * first page's oldest row off its end, into the stretch the older pages were
 * read AFTER, where neither page holds it. So the first page the older ones
 * continue is kept exactly as it was read, the caller stops polling for as
 * long as [OlderPages.frozen] says so, and the screen says it is showing a
 * snapshot, with [OlderPages.backToNewest] beside it. Nothing here asks the
 * engine anything the caller did not: the fetch is the caller's, with its own
 * question and its own parameters.
 *
 * # A page lands only in the walk that asked for it
 *
 * A fetch is in flight across renders, and two things end a walk while it is:
 * a new question (`restart`) and [OlderPages.backToNewest]. An answer that
 * arrived after either and was appended anyway would put the old question's
 * frozen first page, its older rows and its cursor back on the screen under
 * the new one — and freeze it, which stops the new question's polling. So
 * every walk has a GENERATION, each write says which one it is for, and a
 * write for a generation that has ended is dropped.
 *
 * # One implementation
 *
 * Each list's cursor is a different shape (a log position, a
 * `{before_time, before_id}` pair), so the cursor is a type parameter rather
 * than a second copy of the walk per shape.
 */

import { useCallback, useState } from "react";

/** One older page, as the caller's fetch reads it. */
export interface OlderPage<Row, Cursor> {
  rows: Row[];
  /** Where the page after this one starts, or null where this one was the last. */
  next: Cursor | null;
}

export interface OlderPages<Row, Cursor> {
  /** Older pages are held, so the caller must stop re-reading its first page. */
  frozen: boolean;
  paging: boolean;
  /** Why the last older page did not come back, as a query error code. */
  error: string | null;
  /**
   * The rows to draw: the first page — the frozen one while older pages are
   * held, the live one otherwise — then the older pages, each row once.
   *
   * A NEW FUNCTION ONLY WHEN WHAT IS HELD MOVES, so a caller can memoise the
   * rows on it and on its first page and draw the same array between polls.
   */
  rows(first: readonly Row[] | undefined): Row[];
  /**
   * The first page being drawn: the frozen one while older pages are held,
   * `first` otherwise. The SAME ARRAY for as long as the freeze lasts, so a
   * caller can key something on the answer the screen is actually showing.
   */
  head(first: readonly Row[] | undefined): readonly Row[] | undefined;
  /** Whether a page past what is drawn exists, from the last cursor read. */
  more(firstNext: Cursor | null): boolean;
  /** Read the page after what is drawn. */
  loadOlder(
    first: readonly Row[] | undefined,
    firstNext: Cursor | null,
    fetch: (cursor: Cursor) => Promise<OlderPage<Row, Cursor>>,
  ): Promise<void>;
  /** Drop the older pages and go back to the live first page. The caller
   *  re-asks its first page, since polling stopped while they were held. */
  backToNewest(): void;
}

/** One walk: its generation, the frozen first page, and every older page. */
interface Walk<Row, Cursor> {
  generation: number;
  frozen: readonly Row[] | null;
  older: Row[];
  next: Cursor | null;
  paging: boolean;
  error: string | null;
}

function begin<Row, Cursor>(generation: number): Walk<Row, Cursor> {
  return { generation, frozen: null, older: [], next: null, paging: false, error: null };
}

/**
 * The walk, for one question.
 *
 * `restart` is the question's IDENTITY — its parameters, compared with
 * `Object.is`, so an object must be memoised — and a change to it drops every
 * page read under the old one: another question's older pages continue a list
 * that is no longer on the screen. `key` names a row, and must be stable
 * across renders.
 */
export function useOlderPages<Row, Cursor>(
  key: (row: Row) => string,
  restart: unknown,
): OlderPages<Row, Cursor> {
  const [walk, setWalk] = useState<Walk<Row, Cursor>>(() => begin(0));

  // A NEW QUESTION ENDS THE WALK IN THE RENDER THAT SEES IT, rather than in an
  // effect: an effect runs only after that render is committed, and that
  // render drew the old question's pages under the new one. A state update
  // made during render makes React render again before it commits anything.
  const [question, setQuestion] = useState(restart);
  if (!Object.is(question, restart)) {
    setQuestion(restart);
    setWalk((prev) => begin(prev.generation + 1));
  }

  const backToNewest = useCallback(() => setWalk((prev) => begin(prev.generation + 1)), []);

  const { frozen, older } = walk;
  const rows = useCallback(
    (first: readonly Row[] | undefined): Row[] => {
      const seen = new Set<string>();
      return [...(frozen ?? first ?? []), ...older].filter((row) => {
        const id = key(row);
        if (seen.has(id)) return false;
        seen.add(id);
        return true;
      });
    },
    [frozen, older, key],
  );

  const from = (firstNext: Cursor | null): Cursor | null => (frozen ? walk.next : firstNext);

  const loadOlder = async (
    first: readonly Row[] | undefined,
    firstNext: Cursor | null,
    fetch: (cursor: Cursor) => Promise<OlderPage<Row, Cursor>>,
  ): Promise<void> => {
    const cursor = from(firstNext);
    if (cursor === null) return;
    const { generation } = walk;
    const head = frozen ?? first ?? [];
    // Every write names the walk it belongs to — see the package doc.
    const write = (update: (w: Walk<Row, Cursor>) => Walk<Row, Cursor>) =>
      setWalk((prev) => (prev.generation === generation ? update(prev) : prev));
    write((w) => ({ ...w, paging: true, error: null }));
    try {
      const page = await fetch(cursor);
      write((w) => ({
        ...w,
        frozen: head,
        older: [...w.older, ...page.rows],
        next: page.next,
        paging: false,
      }));
    } catch (err) {
      write((w) => ({
        ...w,
        paging: false,
        error: err instanceof Error ? err.message : "query_failed",
      }));
    }
  };

  return {
    frozen: frozen !== null,
    paging: walk.paging,
    error: walk.error,
    rows,
    head: (first) => frozen ?? first,
    more: (firstNext) => from(firstNext) !== null,
    loadOlder,
    backToNewest,
  };
}
