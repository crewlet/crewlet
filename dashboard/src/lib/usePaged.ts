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
 * answers do not agree on a key: Worklet's histories carry `next_cursor`
 * and the turn list `next`, and a hook that read one of them would page the
 * other as a list with nothing before it.
 *
 * # The newest page moves, and the join has to move with it
 *
 * A HELD PAGE BEGINS WHERE THE NEWEST PAGE ENDED WHEN IT WAS ASKED FOR, and
 * the newest page does not stay put: every entry that arrives at the top
 * pushes the oldest one off its foot. That entry is then on no page this hook
 * holds — the newest has let go of it and the held page began after it — and
 * a list drawn from the two is A HOLE DRAWN AS CONTINUITY: it reads as the
 * whole history and silently lacks a comment, a change, a turn. Each new
 * arrival made one more.
 *
 * So every held page keeps the cursor it was asked under, and each answer for
 * the newest page is checked against the first of them. The JOIN HOLDS when
 * the newest page's own cursor is that one, or when the two share an entry
 * (two slices of one ordered history that share an entry have nothing
 * between them). When it does not, the missing range is read — from the
 * newest page's cursor, a page at a time, until an answer reaches the held
 * page — and spliced in. Until it has been read, the list stays exactly as
 * it was last drawn, its newest page included, and then moves in one step: a
 * new entry arrives a round trip later for a reader who has paged back, and a
 * gap is never on screen. The read is BOUNDED by the held pages' own extent —
 * a gap longer than everything the reader paged back through is cheaper to
 * page through again than to fill — and a history that ENDS before the held
 * page is one whose older entries have gone, so what the walk read is the
 * rest of it. A gap that cannot be filled, past the bound or because a read
 * failed, DROPS the held pages: the reader pages back again from a list that
 * is whole, which is worse than keeping their place and better than a list
 * that is not.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { QueryResult } from "~/lib/useQuery.ts";
import { queryErrorCode, type LogRefusal, type QueryRefusal } from "~/protocol/index.ts";
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
  /** What the newest page's refusal said beyond its code — see `useQuery`. */
  refusal: QueryRefusal | LogRefusal | null;
  /** Whether a page older than every one loaded exists. */
  more: boolean;
  /** Ask for it. A no-op when there is none or one is in flight. */
  older: () => void;
  paging: boolean;
  /**
   * Why the last older page — or the read that joins the held pages to a
   * newest page that moved — did not arrive: its code alone, until the next
   * one. Both are asked of the question the newest page already answered, so
   * whatever the engine would refuse them on authority it has refused the
   * newest page on first, with the refusal there.
   */
  pageError: QueryErrorCode | null;
  refetch: () => void;
}

/** One older page, and the cursor it was asked under: where it BEGINS. */
interface HeldPage<A> {
  cursor: string;
  answer: A;
}

/** What the reader has paged back through, for one question. */
interface Kept<A> {
  question: string;
  /**
   * The newest page these were last JOINED to. It is what is drawn as the
   * newest while a later answer that does not join is being joined, so the
   * list on screen is always one whose pages meet.
   */
  top: A;
  /** Oldest last, and never empty: no older page held is no [Kept]. */
  pages: readonly HeldPage<A>[];
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
  const [held, setHeld] = useState<Kept<A> | null>(null);
  const [paging, setPaging] = useState(false);
  const [pageError, setPageError] = useState<QueryErrorCode | null>(null);
  const kept = held && held.question === question ? held : null;

  // THE LATEST ASK, read when a read starts rather than depended on: the
  // callers build it inline, and an effect keyed on it would re-run on every
  // render of the screen for no change in what is held.
  const ask = useRef(page);
  ask.current = page;

  /** Whether `answer` reaches the held page beginning at `from` — see the
   *  package doc for why sharing an entry is as good as meeting it. */
  const joins = useCallback(
    (answer: A, from: HeldPage<A>) => {
      if (cursorOf(answer) === from.cursor) return true;
      const ids = new Set(pick(from.answer).map(idOf));
      return pick(answer).some((entry) => ids.has(idOf(entry)));
    },
    [pick, idOf, cursorOf],
  );

  // WHAT IS DRAWN AS THE NEWEST PAGE: the latest answer where it meets what
  // is held, and otherwise the last one that did, until the gap between them
  // has been read.
  const joined = !kept || !first.data || joins(first.data, kept.pages[0]!);
  const newest = joined ? first.data : kept.top;
  const pages: readonly HeldPage<A>[] = kept ? kept.pages : NONE;
  const last = pages.length > 0 ? pages[pages.length - 1]!.answer : newest;
  const next = last ? cursorOf(last) : "";

  // KEEP THE JOIN. One read at a time: an answer that lands while one is out
  // is checked again when it settles, because what it settles is `held`.
  const joining = useRef(false);
  useEffect(() => {
    const answer = first.data;
    if (!answer || !kept || joining.current) return;
    const anchor = kept.pages[0]!;
    if (joins(answer, anchor)) {
      // AGAINST WHAT IS HELD WHEN IT APPLIES, never against this render's
      // copy: an older page can land between this answer's commit and this
      // effect, and a whole state built from `kept` would put back the pages
      // as they were and drop the one the reader just asked for.
      if (kept.top !== answer) {
        setHeld((prev) =>
          prev && prev.question === question && prev.pages[0] === anchor && prev.top !== answer
            ? { ...prev, top: answer }
            : prev,
        );
      }
      return;
    }
    joining.current = true;
    void (async () => {
      const gap: HeldPage<A>[] = [];
      let outcome: "joined" | "ended" | "dropped" = "dropped";
      try {
        let cursor = cursorOf(answer);
        if (!cursor) outcome = "ended";
        // BOUNDED BY WHAT IS HELD: a gap longer than every page the reader
        // went back through is cheaper paged again than filled.
        for (let read = 0; outcome === "dropped" && read < kept.pages.length; read++) {
          const got = await ask.current(cursor);
          gap.push({ cursor, answer: got });
          if (joins(got, anchor)) outcome = "joined";
          else if (!(cursor = cursorOf(got))) outcome = "ended";
        }
      } catch (err) {
        setPageError(queryErrorCode(err instanceof Error ? err.message : null) ?? "query_failed");
      } finally {
        joining.current = false;
      }
      setHeld((prev) => {
        // MOVED ON — another question, or pages that are no longer the ones
        // this read set out to reach: whatever is held now is checked again.
        if (!prev || prev.question !== question || prev.pages[0] !== anchor) return prev;
        if (outcome === "joined") return { question, top: answer, pages: [...gap, ...prev.pages] };
        // THE HISTORY ENDED BEFORE THE HELD PAGES: they hold what has gone,
        // and what the walk read is the whole of the rest.
        if (outcome === "ended" && gap.length > 0) return { question, top: answer, pages: gap };
        return null;
      });
    })();
  }, [first.data, kept, question, joins, cursorOf]);

  const older = useCallback(async () => {
    if (!next || paging) return;
    const from = last;
    // WHETHER IT FOLLOWS THE NEWEST PAGE or a held one, decided when it is
    // asked: nothing held when it lands means two different things.
    const opens = pages.length === 0;
    setPaging(true);
    setPageError(null);
    try {
      const answer = await ask.current(next);
      setHeld((prev) => {
        const kept = prev && prev.question === question ? prev : null;
        // APPENDED ONLY WHERE IT BEGINS: a join that dropped or replaced the
        // pages while this was out leaves a tail this page does not follow —
        // and a page asked after a held one that is gone follows nothing on
        // the list, so it is not held either, not even until the next join.
        if (!kept) {
          return opens && from ? { question, top: from, pages: [{ cursor: next, answer }] } : prev;
        }
        const tail = kept.pages[kept.pages.length - 1]!.answer;
        return cursorOf(tail) === next
          ? { ...kept, pages: [...kept.pages, { cursor: next, answer }] }
          : prev;
      });
    } catch (err) {
      setPageError(queryErrorCode(err instanceof Error ? err.message : null) ?? "query_failed");
    } finally {
      setPaging(false);
    }
  }, [question, next, last, pages, paging, cursorOf]);

  const answers = useMemo(() => {
    const out: A[] = newest ? [newest] : [];
    for (const held of pages) out.push(held.answer);
    return out;
  }, [newest, pages]);

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
    paging,
    pageError,
    refetch: first.refetch,
  };
}
