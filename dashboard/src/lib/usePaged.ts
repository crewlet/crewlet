/**
 * A history the engine answers a page at a time, followed back past its
 * newest page — and held still while it is.
 *
 * The task page draws three — its comments (`work_comments`), the agent turns
 * charged to it (`work_item_turns`) and what changed on it (`work_activity`) —
 * a seat's profile draws its turns (`turns {seat}`), and the Code workspace
 * its repositories (`code_repos`) and a repository's pushes (`code_pushes`).
 * Each is answered a page at a time with a cursor to the page after, and each
 * used to be read as its first page and nothing more, so a task past twenty
 * comments or a seat past fifty turns had a history nobody could reach — and a
 * count of the page read as a count of the history.
 *
 * # Two hooks, because the newest page is asked between them
 *
 * `usePageHold` keeps what the pager holds, and says whether the newest page
 * is held; the caller asks the newest page with that as its `hold` (`useQuery`
 * and `useCodeQuery` take it); `usePaged` follows the answer back and merges
 * the pages. THE QUESTION IS THE CALLER'S, asked with its kind written out at
 * the call — the newest page through `useQuery("…")` or `useCodeQuery("…")`,
 * every older one through `socket.query("…")` or `codeRest.query("…")` —
 * because a kind this hook took as a variable is one the engine's reader gate
 * cannot see (`app/source.test.ts`), and a question no gate can see asked is
 * one the engine may stop answering unnoticed.
 *
 * # Held from the press of "older"
 *
 * An older page is asked from the cursor the page before it was answered
 * with, minted from its last row. Read again under newer rows, that page
 * pushes its last rows out — and the older page starts after them, so they
 * belong to neither: a silent hole in the middle of the list. So from the
 * moment the reader asks for an older page, the newest page is HELD — nothing
 * asks it again on its own, no poll, no reconnect, no return of the tab — and
 * the pages are one reading of the list until its question changes. Every
 * page after the first is asked ONCE, by the reader, and never polled:
 * re-asking every held cursor on each tick would multiply the list's cost by
 * its length.
 *
 * A NEW QUESTION DISCARDS what the last one held. Hidden under the old
 * question instead, its pages came back the moment the reader returned to
 * it, under a newest page read since — the same hole, reached by a filter.
 *
 * # A newest page that moves anyway reads the held pages again
 *
 * The reader's own write still asks the newest page again, as does their
 * refetch and an answer already on its way when they paged back: the screen
 * they acted on is redrawn from an answer that includes the act. Under held
 * pages that newest page continues none of them, so the held pages are read
 * again FROM IT, in order and as many as were held, and swapped in together;
 * until they are, the reading the reader was shown stays. A read again that
 * fails lets the hold go — the newest page is drawn live and the older ones
 * dropped — rather than keep pages that no longer continue what is above
 * them, and an older page that fails with nothing older held does the same.
 *
 * # A page that fails is asked again by the same press
 *
 * An older page that fails keeps what is held — the pages under it still
 * continue the newest one — and the cursor it was asked from, so `older`
 * asks that page again. Its code is `pageError`, which a caller says BESIDE
 * the control that asks it (`components/common.tsx`'s `PageFailed`), never
 * in its place: in its place it left the reader nothing to press, and under a
 * list held still, nothing to do but reload.
 *
 * # Where the cursor is
 *
 * The caller's to say (`cursorOf`), because the answers do not agree on a
 * key: the tracker's histories carry `next_cursor`, the turn list and the code
 * listings `next`, and a hook that read one of them would page the others as
 * a list with nothing after it. An entry is drawn once, by its id, nearest the
 * top.
 */

import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type Dispatch,
  type SetStateAction,
} from "react";
import type { QueryResult } from "~/lib/useQuery.ts";
import { queryErrorCode, type LogRefusal, type QueryRefusal } from "~/protocol/index.ts";
import type { QueryErrorCode } from "~/contract/errors.ts";

/** No older page held — one constant, so an unpaged list is not a new array each render. */
const NONE: readonly never[] = [];

/** What a pager holds for one question. */
export interface Held<A> {
  /** The question it belongs to: its parameters, serialised. */
  question: string;
  /**
   * Which reading of the list this is. A new question starts the next one,
   * and a page asked under an earlier reading is dropped when it lands,
   * whatever question is on screen by then — the same one again included.
   */
  reading: number;
  /** The newest page the held ones continue from, from the press of "older"; null while none is held. */
  head: A | null;
  /** The older pages, newest first. */
  pages: readonly A[];
  /** An older page, or a read again of the held ones, is on its way. */
  paging: boolean;
  /** Why the last of those did not arrive — its code alone. */
  error: QueryErrorCode | null;
}

function unheld<A>(question: string, reading: number): Held<A> {
  return { question, reading, head: null, pages: NONE, paging: false, error: null };
}

/** What `usePageHold` keeps, for the newest page's question and for `usePaged`. */
export interface PageHold<A> {
  /**
   * Whether the newest page is held: an older page is held or on its way.
   * The newest page's question takes it as its `hold`.
   */
  holding: boolean;
  /** The state itself — `usePaged`'s alone to read and write. */
  held: Held<A>;
  set: Dispatch<SetStateAction<Held<A>>>;
}

/**
 * What the pager holds for the question `params` asks, before its newest page
 * is asked — see the file's doc.
 */
export function usePageHold<A>(params: Record<string, unknown>): PageHold<A> {
  const question = JSON.stringify(params);
  const [held, set] = useState<Held<A>>(() => unheld(question, 0));
  if (held.question !== question) {
    // A NEW QUESTION DISCARDS WHAT THE LAST ONE HELD, in the render that asks
    // it: its newest page is asked live, and nothing of the last reading is
    // left to come back.
    const next = unheld<A>(question, held.reading + 1);
    set(next);
    return { holding: false, held: next, set };
  }
  return { holding: held.paging || held.head !== null, held, set };
}

export interface Paged<T, A> {
  /** Every entry loaded, newest page first, each once. */
  items: T[];
  /** Every page's own answer, newest first — the names and seats each carries. */
  answers: readonly A[];
  loading: boolean;
  error: QueryErrorCode | null;
  /** What the newest page's refusal said beyond its code — see `useQuery`. */
  refusal: QueryRefusal | LogRefusal | null;
  /** Whether a page older than every one loaded exists. */
  more: boolean;
  /** Ask for it. A no-op when there is none or a page is on its way. */
  older: () => void;
  /** An older page, or a read again of the held ones, is on its way. */
  paging: boolean;
  /** Whether the newest page is held still, so the list is one reading: see the file's doc. */
  held: boolean;
  /**
   * Why the last older page, or read again, did not arrive — its code alone.
   * An older page is asked of the question the newest page already answered,
   * so whatever the engine would refuse it on authority it has refused the
   * newest page on first, with the refusal there.
   */
  pageError: QueryErrorCode | null;
  refetch: () => void;
}

/** A failed page's code, in the vocabulary every question's refusal is read in. */
function codeOf(err: unknown): QueryErrorCode {
  return queryErrorCode(err instanceof Error ? err.message : null) ?? "query_failed";
}

/**
 * Follow `first` — the newest page, asked with `hold.holding` as its hold —
 * back a page at a time, and merge what is loaded.
 */
export function usePaged<A, T>(
  hold: PageHold<A>,
  first: QueryResult<A>,
  /** Ask for the page after `cursor`, at this tab's read floor. */
  page: (cursor: string) => Promise<A>,
  pick: (answer: A) => readonly T[],
  idOf: (entry: T) => string,
  /** The cursor to the page after this answer, or "" where there is none. */
  cursorOf: (answer: A) => string,
): Paged<T, A> {
  const { held, set } = hold;
  // THE NEWEST PAGE THE READER IS SHOWN: the one held, while one is.
  const newest = held.head ?? first.data;
  const last = held.pages.length > 0 ? held.pages[held.pages.length - 1]! : newest;
  const next = last ? cursorOf(last) : "";

  // THE CALLER'S READERS AS THEY ARE NOW: fresh closures every render, so a
  // read under way finishes with the latest — and what may still write: a
  // reading a new question ended, or a screen that has gone, asks no more
  // pages for it.
  const reader = useRef({ page, cursorOf });
  reader.current = { page, cursorOf };
  const reading = useRef(held.reading);
  reading.current = held.reading;
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);

  const older = useCallback(async () => {
    if (held.paging || !newest || !next) return;
    const mine = held.reading;
    // HELD FROM THE PRESS, not from the answer: a poll tick or a reconnect
    // landing while this page is on its way would read the newest page
    // again under the cursor it is being asked from.
    set((prev) =>
      prev.reading === mine ? { ...prev, head: newest, paging: true, error: null } : prev,
    );
    try {
      const answer = await reader.current.page(next);
      set((prev) =>
        prev.reading === mine ? { ...prev, pages: [...prev.pages, answer], paging: false } : prev,
      );
    } catch (err) {
      const error = codeOf(err);
      set((prev) =>
        prev.reading !== mine
          ? prev
          : prev.pages.length > 0
            ? { ...prev, paging: false, error }
            : // NOTHING OLDER HELD, nothing to hold still for: the newest
              // page is live again.
              { ...prev, head: null, paging: false, error },
      );
    }
  }, [held.paging, held.reading, newest, next, set]);

  // THE NEWEST PAGE MOVED UNDER HELD PAGES — the reader's write or refetch,
  // or an answer on its way when they paged back — so they are read again
  // from it: see the file's doc.
  const moved =
    held.head !== null && !held.paging && first.data !== null && first.data !== held.head;
  const count = held.pages.length;
  useEffect(() => {
    if (!moved || first.data === null) return;
    const from = first.data;
    const mine = held.reading;
    set((prev) => (prev.reading === mine ? { ...prev, paging: true, error: null } : prev));
    void (async () => {
      const again: A[] = [];
      let cursor = reader.current.cursorOf(from);
      try {
        while (again.length < count && cursor !== "") {
          const answer = await reader.current.page(cursor);
          if (!mounted.current || reading.current !== mine) return;
          again.push(answer);
          cursor = reader.current.cursorOf(answer);
        }
        set((prev) =>
          prev.reading !== mine
            ? prev
            : again.length > 0
              ? { ...prev, head: from, pages: again, paging: false }
              : // NO PAGE AFTER THE NEWEST ANY MORE: nothing to hold.
                { ...prev, head: null, pages: NONE, paging: false },
        );
      } catch (err) {
        const error = codeOf(err);
        set((prev) =>
          prev.reading === mine ? { ...prev, head: null, pages: NONE, paging: false, error } : prev,
        );
      }
    })();
  }, [moved, first.data, held.reading, count, set]);

  const answers = useMemo(
    () => (newest ? [newest, ...held.pages] : held.pages),
    [newest, held.pages],
  );

  const items = useMemo(() => {
    const seen = new Set<string>();
    const out: T[] = [];
    for (const answer of answers) {
      for (const entry of pick(answer)) {
        const id = idOf(entry);
        if (seen.has(id)) continue;
        seen.add(id);
        out.push(entry);
      }
    }
    return out;
  }, [answers, pick, idOf]);

  return {
    items,
    answers,
    loading: first.loading,
    error: first.error,
    refusal: first.refusal,
    more: next !== "",
    older: () => void older(),
    paging: held.paging,
    held: held.head !== null,
    pageError: held.error,
    refetch: first.refetch,
  };
}
