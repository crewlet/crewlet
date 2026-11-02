/**
 * A history the engine answers a page at a time, followed past its first
 * page.
 *
 * The task page draws three — its comments (`work_comments`), the agent turns
 * charged to it (`work_item_turns`) and what changed on it (`work_activity`) —
 * a seat's profile draws its turns (`turns {seat}`), chat draws a person's
 * Activity (`chat_activity`) and a thread (`chat_thread`), Live its turns and
 * recent phases, the Inbox its lists, Artifacts a folder, and the Code
 * workspace its repositories, changes and pushes and a repository's refs,
 * history, tree, files and diffs (`code_*`). Each is answered a page at a time
 * with a cursor to the page after: newest first for a history, in the order the
 * engine keeps for a listing (a repository's name, a path), forwards from its
 * root for a thread. Each used to be read as its first page and nothing more,
 * so a task past twenty comments or a seat past fifty turns had a history
 * nobody could reach — and a count of the page read as a count of the history.
 *
 * THE SHAPE `usePaged` HAS, for its reasons: the first page is an ordinary
 * `useQuery` or `useCodeQuery` (polled, re-asked on reconnect and after this
 * tab's own write, at its read floor) and it STAYS LIVE while the reader pages
 * on — Chatlet's feeds are read for what arrives at their top — so the join
 * below, not a first page held still, is what keeps the list whole; every page
 * after it is asked when the reader asks for it, and belongs to the reading of
 * the question it was asked under. An entry is drawn once, by its id, on the
 * first page that carries it.
 *
 * A NEW QUESTION DISCARDS WHAT THE LAST ONE HELD. Kept under the old question
 * instead, its pages came back the moment the reader returned to it — and for
 * that moment the first page is the other question's answer, which `useQuery`
 * shows until the new one lands, so the join read the returning question's
 * pages from a cursor another question minted. A reader who comes back starts
 * again from the first page, and a page still on its way for a reading that
 * ended is dropped when it lands, whatever question is on screen by then — the
 * same one again included.
 *
 * WHERE THE CURSOR IS is the caller's to say (`cursorOf`), because the
 * answers do not agree on a key: Worklet's histories carry `next_cursor`,
 * the turn list and the code listings `next`, and a hook that read one of them
 * would page the others as a list with nothing after it.
 *
 * A PAGE THAT FAILS IS ASKED AGAIN BY THE SAME PRESS: it keeps what is held
 * and the cursor it was asked from, so [Paged.next] reads it again. Its code
 * is `pageError`, which a caller says BESIDE the control that asks it
 * (`components/common.tsx`'s `PageFailed`), never in its place: in its place
 * it left the reader nothing to press, and nothing to do but reload.
 *
 * # Every page has to meet the next one
 *
 * A HELD PAGE BEGINS WHERE THE PAGE BEFORE IT ENDED WHEN IT WAS ASKED FOR —
 * that is the cursor it was asked under, and it never moves — but the page
 * before it does not stay put. A newest page loses its oldest entry off its
 * foot to every arrival at its top; a page read again after entries became
 * visible inside its range (a private room joined, and the mentions said in
 * it with it) holds as many entries as before over a shorter range. Either
 * way an entry is then on no page this hook holds, and a list drawn from the
 * two is A HOLE DRAWN AS CONTINUITY: it reads as the whole history and
 * silently lacks a comment, a mention, a turn.
 *
 * So WHENEVER A PAGE'S ANSWER CHANGES, ITS FOOT IS CHECKED against the page
 * after it — the first page's on every answer, a held page's on every re-read.
 * A held page's head is the cursor it was asked under, which a re-read does
 * not move, so the foot is the only end that can. The JOIN HOLDS when the
 * page's own cursor is the one the next page was asked under, or when the two
 * share an entry (two slices of one ordered history that share an entry have
 * nothing between them). When it does not, the missing range is read — from
 * the page's cursor, a page at a time, until an answer reaches the next page
 * — and spliced in. Until it has been read the list stays exactly as it was
 * last drawn, the moved page included, and then moves in one step: a gap is
 * never on screen. The read is BOUNDED by the pages after the gap — a gap
 * longer than everything the reader paged through below it is cheaper to page
 * through again than to fill — and a history that ENDS before the next page
 * is one whose later entries have gone, so what the walk read is the rest of
 * it. A gap that cannot be filled within the bound DROPS the pages after it:
 * the reader pages on again from a list that is whole, which is worse than
 * keeping their place and better than a list that is not. A gap under the
 * first page whose read FAILED drops them too, and says why; one under a held
 * page that was being read again leaves that page as it last was, which still
 * meets the next.
 *
 * # A held page is read once — unless its caller says it moved
 *
 * An entry can change after its page was read — a chat mention's message
 * edited or erased, a reply in a thread taken down — and so can which entries
 * a reader may see at all. A page read once draws all of that as it was for as
 * long as the screen is open. A caller whose entries change knows which pages
 * a change reached, from its own live signal, and asks for them again with
 * [Paged.refresh]: the first page through the caller's own question, each held
 * page under the cursor it was first asked with — put in place only once its
 * foot meets the page after it, as above, because a re-read can ADD entries to
 * a page's range as well as take them away. Chatlet's mention feed and a thread
 * ask; the task page's histories and a seat's turns do not.
 *
 * # One change to what is held at a time
 *
 * Joining a moved first page, reading held pages again and paging on all
 * change which pages are held, and each is decided against what is held — so
 * they run ONE AT A TIME, in the order they were asked, each reading what is
 * held when its turn comes and making every read it needs before it applies.
 * Run side by side, each was right alone and two of them lost work: a join
 * that set out to reach a first held page which a re-read then replaced threw
 * its answer away, and the new entry it had read stayed off screen; and a page
 * asked from the old foot of a page a re-read had moved was dropped, so the
 * press did nothing. In one line: the next page always begins where what is
 * held ends NOW, and a join always reaches the page that is there.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { QueryResult } from "~/lib/useQuery.ts";
import { queryErrorCode, type LogRefusal, type QueryRefusal } from "~/protocol/index.ts";
import type { QueryErrorCode } from "~/contract/errors.ts";

/** No page held after the first — one constant, so an unpaged list is not a new array each render. */
const NONE: readonly never[] = [];

/** Where a page sits among those drawn: what a caller deciding which pages a
 *  change reached may need besides the page's own entries. */
export interface PagePlace {
  /** The first page — the caller's own question. */
  first: boolean;
  /** The last page drawn: the one the next page would follow, and so the one
   *  an entry arriving at the far end of a forward-read history lands on. */
  last: boolean;
}

/** Which pages a change reached, asked of each page drawn. */
export type PagesReached<A> = (answer: A, place: PagePlace) => boolean;

export interface Paged<T, A> {
  /** Every entry loaded, first page first, each once. */
  items: T[];
  /** Every page's own answer, first page first — the names and seats each carries. */
  answers: readonly A[];
  loading: boolean;
  error: QueryErrorCode | null;
  /** What the first page's refusal said beyond its code — see `useQuery`. */
  refusal: QueryRefusal | LogRefusal | null;
  /** Whether a page after every one loaded exists. */
  more: boolean;
  /** Ask for it. A no-op when there is none or one is on its way. */
  next: () => void;
  paging: boolean;
  /**
   * Why the last page asked for — or a read that joins the held pages to a
   * first page that moved — did not arrive: its code alone, until the next
   * one. Each is asked of the question the first page already answered, so
   * whatever the engine would refuse them on authority it has refused the
   * first page on first, with the refusal there.
   */
  pageError: QueryErrorCode | null;
  refetch: () => void;
  /**
   * Ask every page `reached` accepts — every one, without it — again: the
   * first through the caller's own question ([refetch]), each held page under
   * the cursor it was first asked with, put in place once its foot meets the
   * page after it. Decided when the re-read's turn comes, of the pages held
   * then. A held page that could not be read again keeps what it last said.
   */
  refresh: (reached?: PagesReached<A>) => void;
}

/** One page after the first, and the cursor it was asked under: where it BEGINS. */
interface HeldPage<A> {
  cursor: string;
  answer: A;
}

/** What the reader has paged through, for one reading of one question. */
interface Kept<A> {
  /** The reading these belong to: the question, and which time it was asked. */
  era: string;
  /**
   * The first page these were last JOINED to. It is what is drawn as the
   * first while a later answer that does not join is being joined, so the
   * list on screen is always one whose pages meet.
   */
  top: A;
  /** In the order they follow one another, and never empty: no page held is
   *  no [Kept]. */
  pages: readonly HeldPage<A>[];
}

/** How the caller's answers are read: its entries, an entry's id, and the
 *  cursor to the page after. */
interface Reading<A, T> {
  pick: (answer: A) => readonly T[];
  idOf: (entry: T) => string;
  cursorOf: (answer: A) => string;
}

/** Whether `answer` reaches the held page `next` — see the package doc for
 *  why sharing an entry is as good as meeting it. */
function meets<A, T>(answer: A, next: HeldPage<A>, reading: Reading<A, T>): boolean {
  if (reading.cursorOf(answer) === next.cursor) return true;
  const ids = new Set(reading.pick(next.answer).map(reading.idOf));
  return reading.pick(answer).some((entry) => ids.has(reading.idOf(entry)));
}

/** What a read of the range between a page and the next one came to. */
interface Gap<A> {
  /** `met`: the last page read reaches the next one. `ended`: the history
   *  ends before it. `unfilled`: the bound ran out, or a read failed. */
  outcome: "met" | "ended" | "unfilled";
  /** Every page read, in order. */
  pages: HeldPage<A>[];
  /** Why a read failed, when one did. */
  error: QueryErrorCode | null;
}

const errorOf = (err: unknown): QueryErrorCode =>
  queryErrorCode(err instanceof Error ? err.message : null) ?? "query_failed";

/**
 * THE QUESTION IS THE CALLER'S, asked with its kind written out at the call
 * — the first page through `useQuery("…")` or `useCodeQuery("…")`, every
 * later one through `socket.query("…")` or `codeRest.query("…")` — because a
 * kind this hook took as a variable is one the engine's reader gate cannot see
 * (`app/source.test.ts`), and a question no gate can see asked is one the
 * engine may stop answering unnoticed.
 */
export function usePaged<A, T>(
  first: QueryResult<A>,
  /** The first page's parameters: the later pages belong to this question. */
  params: Record<string, unknown>,
  /** Ask for the page after `cursor`, at this tab's read floor. */
  page: (cursor: string) => Promise<A>,
  pick: (answer: A) => readonly T[],
  idOf: (entry: T) => string,
  /** The cursor to the page after this answer, or "" where there is none. */
  cursorOf: (answer: A) => string,
): Paged<T, A> {
  const question = JSON.stringify(params);
  // WHICH READING OF THE QUESTION THIS IS: a new question starts the next one,
  // so what the last held is never drawn, joined or paged on again — not even
  // when the reader comes back to it (the package doc's first part).
  const readings = useRef({ question, count: 0 });
  if (readings.current.question !== question) {
    readings.current = { question, count: readings.current.count + 1 };
  }
  const era = `${readings.current.count}:${question}`;
  const [held, setHeld] = useState<Kept<A> | null>(null);
  const [paging, setPaging] = useState(false);
  // A FAILURE BELONGS TO ITS READING too: a new question is not told that the
  // last one's page could not be read.
  const [failed, setFailed] = useState<{ era: string; code: QueryErrorCode } | null>(null);
  const pageError = failed?.era === era ? failed.code : null;
  const kept = held && held.era === era ? held : null;

  // THE LATEST OF WHAT THE CALLER HANDS IN, read when a change runs rather
  // than depended on: the callers build them inline, and a change queued
  // under one render runs under a later one.
  const ask = useRef(page);
  ask.current = page;
  const reading = useRef<Reading<A, T>>({ pick, idOf, cursorOf });
  reading.current = { pick, idOf, cursorOf };
  const asked = useRef(era);
  asked.current = era;
  const latest = useRef(first);
  latest.current = first;

  // WHAT IS HELD NOW, written the moment a change applies, so the next change
  // reads it rather than a render's copy: a change built from `kept` put back
  // whatever had landed between that render and itself.
  const book = useRef<Kept<A> | null>(null);
  const commit = useCallback((next: Kept<A> | null) => {
    book.current = next;
    setHeld(next);
  }, []);

  // ONE CHANGE AT A TIME, in the order asked (the package doc's last part).
  const line = useRef<Promise<void>>(Promise.resolve());
  const serially = useCallback((change: () => Promise<void>) => {
    const run = line.current.then(change);
    // A CHANGE THAT THREW is a bug — every read one makes is caught — and it
    // is said where every uncaught error is said, rather than ending the line
    // for every change queued behind it.
    line.current = run.catch((err: unknown) => {
      setTimeout(() => {
        throw err;
      });
    });
  }, []);

  // THE CHANGES, each run on the line and nowhere else. They read only refs,
  // so the copy made at the first render is every render's.
  const changes = useMemo(() => {
    /** What is held for the reading `q`, while `q` is still the one asked. */
    const holding = (q: string) =>
      asked.current === q && book.current?.era === q ? book.current : null;

    /** The page a render draws first, from what is held now. */
    const drawnFirst = (kept: Kept<A> | null): A | null => {
      const data = latest.current.data;
      if (!kept || !data) return data;
      return meets(data, kept.pages[0]!, reading.current) ? data : kept.top;
    };

    /** Read the range between `from` and the held page `to`: at most `bound`
     *  pages of it. */
    const bridge = async (from: A, to: HeldPage<A>, bound: number): Promise<Gap<A>> => {
      const gap: HeldPage<A>[] = [];
      let cursor = reading.current.cursorOf(from);
      try {
        for (let read = 0; cursor && read < bound; read++) {
          const answer = await ask.current(cursor);
          gap.push({ cursor, answer });
          if (meets(answer, to, reading.current))
            return { outcome: "met", pages: gap, error: null };
          cursor = reading.current.cursorOf(answer);
        }
      } catch (err) {
        return { outcome: "unfilled", pages: gap, error: errorOf(err) };
      }
      return { outcome: cursor ? "unfilled" : "ended", pages: gap, error: null };
    };

    /** JOIN THE FIRST PAGE: draw the latest answer, with the gap under it
     *  read first where it no longer meets the first held page. */
    const join = async (q: string) => {
      const kept = holding(q);
      const answer = latest.current.data;
      if (!kept || !answer || answer === kept.top) return;
      const anchor = kept.pages[0]!;
      if (meets(answer, anchor, reading.current)) {
        commit({ ...kept, top: answer });
        return;
      }
      // BOUNDED BY EVERYTHING HELD, all of which is below this gap.
      const gap = await bridge(answer, anchor, kept.pages.length);
      if (holding(q) !== kept) return;
      if (gap.error) setFailed({ era: q, code: gap.error });
      if (gap.outcome === "met") {
        commit({ era: q, top: answer, pages: [...gap.pages, ...kept.pages] });
      } else if (gap.outcome === "ended" && gap.pages.length > 0) {
        // THE HISTORY ENDED BEFORE THE HELD PAGES: they hold what has gone,
        // and what the walk read is the whole of the rest.
        commit({ era: q, top: answer, pages: gap.pages });
      } else {
        commit(null);
      }
    };

    /** READ AGAIN every page `reached` accepts, and put each in place. */
    const reread = async (q: string, reached: PagesReached<A>) => {
      if (asked.current !== q) return;
      const kept = holding(q);
      const top = drawnFirst(kept);
      if (top !== null && reached(top, { first: true, last: !kept })) {
        latest.current.refetch();
      }
      if (!kept) return;
      const chosen = kept.pages.filter((held, i) =>
        reached(held.answer, { first: false, last: i === kept.pages.length - 1 }),
      );
      if (chosen.length === 0) return;
      // ASKED SIDE BY SIDE and applied below against one state: the reads
      // are independent, and what they change is not.
      const fresh = await Promise.all(
        chosen.map((held) =>
          ask.current(held.cursor).then(
            (answer) => answer,
            // A PAGE THAT COULD NOT BE READ AGAIN keeps what it last said:
            // the next change that reaches it asks again.
            () => null,
          ),
        ),
      );
      if (holding(q) !== kept) return;
      let pages = kept.pages;
      // FROM THE FOOT UP, so the page a foot is checked against has already
      // been read again wherever it was asked for too — and what a page's
      // own change does is below it, so every page above is still where it
      // was found.
      for (let k = chosen.length - 1; k >= 0; k--) {
        const answer = fresh[k];
        if (!answer) continue;
        const { cursor } = chosen[k]!;
        const at = pages.findIndex((held) => held.cursor === cursor);
        const placed: HeldPage<A> = { cursor, answer };
        const above = pages.slice(0, at);
        const after = pages[at + 1];
        if (!after || meets(answer, after, reading.current)) {
          pages = [...above, placed, ...pages.slice(at + 1)];
          continue;
        }
        // THE PAGE NOW ENDS SHORT OF THE NEXT ONE — entries became visible
        // in its range — so what it let go of is read before it is drawn,
        // bounded by the pages after it: they are what a gap left unfilled
        // drops.
        const gap = await bridge(answer, after, pages.length - at - 1);
        if (holding(q) !== kept) return;
        if (gap.error) continue;
        if (gap.outcome === "met") pages = [...above, placed, ...gap.pages, ...pages.slice(at + 1)];
        else if (gap.outcome === "ended") pages = [...above, placed, ...gap.pages];
        else pages = [...above, placed];
      }
      if (pages !== kept.pages) commit({ ...kept, pages });
    };

    /** PAGE ON from where what is held ends now. */
    const onward = async (q: string) => {
      if (asked.current !== q) return;
      const kept = holding(q);
      const tail = kept ? kept.pages[kept.pages.length - 1]!.answer : latest.current.data;
      const cursor = tail ? reading.current.cursorOf(tail) : "";
      if (!tail || !cursor) return;
      const answer = await ask.current(cursor);
      if (asked.current !== q || holding(q) !== kept) return;
      commit(
        kept
          ? { ...kept, pages: [...kept.pages, { cursor, answer }] }
          : { era: q, top: tail, pages: [{ cursor, answer }] },
      );
    };

    return { join, reread, onward };
  }, [commit]);

  // WHAT IS DRAWN AS THE FIRST PAGE: the latest answer where it meets what
  // is held, and otherwise the last one that did, until the gap between them
  // has been read.
  const joined =
    !kept || !first.data || meets(first.data, kept.pages[0]!, { pick, idOf, cursorOf });
  const newest = joined ? first.data : kept.top;
  const pages: readonly HeldPage<A>[] = kept ? kept.pages : NONE;
  const last = pages.length > 0 ? pages[pages.length - 1]!.answer : newest;
  const following = last ? cursorOf(last) : "";

  // KEEP THE JOIN, once however many answers arrive while one is waiting
  // for its turn: the join reads the latest answer when its turn comes.
  const joinQueued = useRef(false);
  useEffect(() => {
    if (!kept || !first.data || first.data === kept.top || joinQueued.current) return;
    joinQueued.current = true;
    serially(async () => {
      joinQueued.current = false;
      await changes.join(era);
    });
  }, [first.data, kept, era, serially, changes]);

  const pagingNow = useRef(false);
  const next = useCallback(() => {
    if (!following || pagingNow.current) return;
    pagingNow.current = true;
    setPaging(true);
    setFailed(null);
    serially(async () => {
      try {
        await changes.onward(era);
      } catch (err) {
        setFailed({ era, code: errorOf(err) });
      } finally {
        pagingNow.current = false;
        setPaging(false);
      }
    });
  }, [following, era, serially, changes]);

  // ONE RE-READ WAITING AT A TIME: a second asked while the first waits for
  // its turn widens it, rather than queueing another read of the same pages.
  // One asked once the first has started waits for it, because the first
  // may have read before the change the second was asked for.
  const rereadQueued = useRef<{ era: string; reached: PagesReached<A>[] } | null>(null);
  const refresh = useCallback(
    (reached?: PagesReached<A>) => {
      const accept: PagesReached<A> = reached ?? (() => true);
      const q = asked.current;
      const waiting = rereadQueued.current;
      if (waiting && waiting.era === q) {
        waiting.reached.push(accept);
        return;
      }
      const wanted = { era: q, reached: [accept] };
      rereadQueued.current = wanted;
      serially(async () => {
        if (rereadQueued.current === wanted) rereadQueued.current = null;
        await changes.reread(q, (answer, place) =>
          wanted.reached.some((one) => one(answer, place)),
        );
      });
    },
    [serially, changes],
  );

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
    more: following !== "",
    next,
    paging,
    pageError,
    refetch: first.refetch,
    refresh,
  };
}
