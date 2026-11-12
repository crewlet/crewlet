/**
 * Asking Gitlet a question, as a hook — over REST, at this tab's floor
 * on the code log.
 *
 * # Why REST and not the socket
 *
 * The code questions are the query registry's like every other, answered by
 * one implementation on every transport — but the engine answers a socket's
 * questions four at a time and queues the rest on that socket
 * (`MaxInFlightQueries`, internal/api/stream's budget.go; the client sends
 * every question it is asked at once), and the frame's own reads and every
 * screen's first paint wait on those four. A code answer is a
 * page a reader scrolls (a repository listing, a push history) or, once the
 * repository's pages land, a tree, a file or a diff read by object id: answers
 * a reader asks for many of, of a size the socket was never sized to carry
 * beside the projection. So the Gitlet workspace asks them over REST, on the
 * registry's generic form of the question (`GET /query/{what}` — the named
 * routes, `/gitlet/repos…`, are the same answer for a script), and a slow one
 * holds no slot any other screen is waiting on. The reading is `useRest`'s, the
 * one REST loader: an answer belongs to the read that asked and to its key, a
 * refusal replaces what is on screen, and the engine's `Retry-After` is the
 * retry.
 *
 * # What it shares with `useQuery`
 *
 * THE FLOOR, ON EVERY ASK. A write this tab makes to the code log raises its
 * floor there (`protocol/session.ts`), and every ask of a question the engine
 * reads at that floor (`SESSION_QUERIES.code`) names it — `useQuery`'s own
 * `withFloor`, so the rule is written once — and the write asks again, quietly,
 * so the screen that pressed a button is redrawn from an answer that includes
 * the press. The floor is never part of the question's KEY: a key that moved
 * with every write would read as a new question, and a new question shows
 * nothing of the old one until it answers.
 *
 * AND THE SHAPE A SCREEN READS: `data`, `loading`, the refusal's code and what
 * it said, and `refetch` — `useQuery`'s result, so `QueryState` and `usePaged`
 * read a code question as they read any other.
 *
 * # An answer by object id is kept, and never asked again
 *
 * The engine marks an answer read by object id immutable: a strong `ETag` of
 * its own bytes and `Cache-Control: immutable`, and `304` to a request naming
 * the tag. The transport asks with `cache: "no-store"` (`protocol/rest.ts`: a
 * guarded read of something that changes is never served from a cache), so the
 * browser's cache never answers one, and the tab keeps them itself, in two
 * places.
 *
 * THE TAB'S KEPT ANSWERS ([IMMUTABLE_KEPT] of them, the most recently read): a
 * question whose answer the engine marked immutable is answered from there with
 * no request at all, whichever hook or page asks it — a reviewer moving from
 * file A to B and back to A in a diff, a Permalink left and come back to, a
 * page of a patch read again — since `immutable` means its bytes cannot
 * change. Every hook and every later page (`codeRest`) reads and fills the one
 * store. It is the tab's memory and nothing else's: a tab handed to another
 * reader is reloaded with nothing of the last one's kept (`lib/session.ts`),
 * which empties it, and the browser's cache is never asked to keep one.
 *
 * AND EACH HOOK'S TAG, for an answer that carries one and is not kept — one the
 * store let go of while the screen still holds it: read again (a reconnect,
 * the tab coming back), the question names the tag of the answer the hook
 * holds, and a `304` is that answer, with no body sent twice. An answer that
 * carries no tag is read whole every time.
 */

import { useCallback, useEffect, useRef } from "react";
import { restErrorCode, useRest } from "./useRest.ts";
import { withFloor, type QueryResult } from "./useQuery.ts";
import {
  QueryError,
  pathSegment,
  RestError,
  rest,
  type CodeChangeSummary,
  type CodeChangesAnswer,
  type CodeQueryMap,
  type CodeQueryName,
} from "~/protocol/index.ts";
import { session, SessionFloors } from "~/protocol/session.ts";

/**
 * A code question's parameters: a value each, or several for a key the
 * grammar reads whole (`min_position`, one floor per log), sent as the key
 * repeated.
 */
export type CodeParams = Readonly<
  Record<string, string | number | boolean | undefined | readonly string[]>
>;

export interface CodeQueryOptions {
  /** Skip the question entirely — for a screen whose parameter is not chosen yet. */
  enabled?: boolean;
  /**
   * Re-ask every N ms, quietly. Only for an answer something else moves while
   * the screen is open: the code log has no push to the dashboard, so another
   * person's push reaches a screen by this or not at all.
   */
  pollMs?: number;
  /** Ask again when the tab comes back. See `useQuery`'s option of the same name. */
  refetchOnFocus?: boolean;
  /**
   * Ask again when the socket reconnects. Default true; `"unreached"` asks
   * again only a question that never reached the engine. See `useRest`'s
   * option of the same name.
   */
  refetchOnReconnect?: boolean | "unreached";
}

/**
 * The most pages one filled reading of a listing of changes asks, its first
 * included ([CodeChangesOptions.fill]): FOUR, so a screen drawing three rows
 * reads at most the twelve newest rows of its listing's index on each poll —
 * four requests where one answers in the common case.
 *
 * WHAT A PAGE DROPS IS FEW ONCE THE ENGINE SETTLES WHAT ENDED: a change that
 * closes or merges has its stored readiness cleared by the record that ends
 * it, and its asks for review settled by its home's next pass (replicated
 * 0127), so neither `needs=person` nor `needs=review` walks an ended change
 * past that pass. What is left to drop is a change of a repository the reader
 * may not read, one that ended since its home last looked, and one whose
 * readiness, judged at the read, has moved off a person since it was stored:
 * a few rows, not a history. Four pages carries a screen past three times its
 * own rows of those; a reading that still keeps too few answers with `next`,
 * and the screen says where the rest are rather than "none". Every poll of an
 * open screen pays it again, which is what keeps it small.
 */
export const CHANGES_FILL_PAGES = 4;

/** A `code_changes` question's options: a code question's, and the fill. */
export interface CodeChangesOptions extends CodeQueryOptions {
  /**
   * The changes the screen draws: follow `next` until this many are kept or
   * the listing ends, for at most [CHANGES_FILL_PAGES] pages, rather than
   * answer the first page alone.
   *
   * A PAGE IS THE ROWS THE ENGINE READ, NOT THE ROWS IT KEPT (`gitlet.Reader`'s
   * listChanges: `limit` bounds the walk of the listing's index, and a row of
   * a repository the reader may not read, a change no longer live, or — for
   * `needs=person` — a change whose readiness judged at the read no longer
   * waits on a person, is dropped after it), so a page can hold fewer changes
   * than it was asked for, or none, and still carry `next`. A screen that
   * reads one page and says "none" says it of rows it never read.
   *
   * The answer is every change the pages read kept, in the listing's order —
   * possibly more than it asked for, and never cut, since the answer's
   * `next` (the last page's) resumes after the last of them. That `next` is
   * absent only where the listing ENDED, the one answer that says there is
   * nothing past what it holds. Each page is asked with the same parameters
   * and the cursor, at this tab's floor, and the whole reading is one
   * question: a poll, a reconnect or a write from this tab reads it again from
   * the first page.
   */
  fill?: number;
}

/**
 * The query string a code question is asked with — its parameters, and this
 * tab's floor on every log the question reads where it has written one and the
 * caller named no freshness of its own — or "" for none.
 *
 * THE QUERY STRING ALONE, with no `?` and never the path: each request writes
 * its own `/query/<kind>?` where it is sent, so the dev server's forwarding
 * gate (`protocol/proxy.test.ts`) reads the prefix it has to forward at the
 * call, and the segment gate (`protocol/rest.test.ts`) reads the `?` that ends
 * the path before this value.
 */
export function codeSearch(what: CodeQueryName, params: CodeParams = {}): string {
  const asked = withFloor(what, JSON.stringify(params));
  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(asked)) {
    if (value === undefined || value === null) continue;
    // ONE KEY PER VALUE for a key the grammar reads whole: `min_position`
    // names a floor per log, and the engine keeps every one of a repeated key
    // it reads that way (`queries.FromQuery`) and the first of any other.
    if (Array.isArray(value)) {
      for (const one of value) search.append(key, String(one));
    } else {
      search.append(key, String(value));
    }
  }
  return search.toString();
}

/**
 * A refused code question in the vocabulary every question's refusal is read
 * in: a [QueryError] whose message is the code, carrying the refusal and the
 * engine's sentence — so a pager reads a code page's failure as it reads a
 * socket question's. Anything that is not the transport's refusal (the
 * caller's own abort) passes through unchanged.
 */
function questionError(err: unknown): unknown {
  if (!(err instanceof RestError)) return err;
  const code = restErrorCode(err) ?? "query_failed";
  return new QueryError(code, err.refusal, sentenceOf(err));
}

/** The engine's sentence where a reader acts on it: a `bad_params` or a `not_found` refusal. */
function sentenceOf(err: RestError | null): string | null {
  if (!err || (err.status !== 400 && err.status !== 404)) return null;
  return err.detail || null;
}

/**
 * How many immutable answers the tab keeps: the most recently read 64.
 *
 * AN ANSWER IS AT MOST ONE PAGE, `gitlet.MaxGitAnswerBytes` (56 KiB of JSON), so
 * the store holds at most about 3.5 MiB of answers — a tab's memory, beside a
 * screen that already draws what it holds. Sixty-four is the files a reviewer
 * moves between in a large change and the pages of a long patch together, so
 * going back to one they read is answered here; past it, the one read longest
 * ago is let go, and reading it again is one request, as before.
 */
export const IMMUTABLE_KEPT = 64;

/** The tab's immutable answers by question, the most recently read last. */
const kept = new Map<string, unknown>();

/** The question's key: its kind and its parameters, never the floor (see the file's doc). */
const keyOf = (what: CodeQueryName, params: CodeParams = {}): string =>
  `${what}?${JSON.stringify(params)}`;

/** The immutable answer the tab keeps for a question, read as the most recent. */
function keptAnswer(key: string): unknown {
  if (!kept.has(key)) return undefined;
  const body = kept.get(key);
  kept.delete(key);
  kept.set(key, body);
  return body;
}

/** Keep an answer the engine marked immutable, letting go of the one read longest ago past the bound. */
function keep(key: string, body: unknown): void {
  kept.delete(key);
  kept.set(key, body);
  while (kept.size > IMMUTABLE_KEPT) {
    const oldest = kept.keys().next().value;
    if (oldest === undefined) break;
    kept.delete(oldest);
  }
}

/**
 * One ask of a code question, for a page a reader asks for once — a listing's
 * older page — at this tab's floor, refused as a [QueryError]: answered from
 * the tab's kept answers where the engine marked it immutable before.
 *
 * A `query` METHOD, so the engine's reader gate (`clientsource.Calls`) and the
 * dashboard's own ("every read names its question") read the kind written at
 * each call, as they read a socket question's.
 */
export const codeRest = {
  async query<K extends CodeQueryName>(
    what: K,
    params: CodeParams = {},
    signal?: AbortSignal,
  ): Promise<CodeQueryMap[K]> {
    const key = keyOf(what, params);
    const held = keptAnswer(key);
    if (held !== undefined) return held as CodeQueryMap[K];
    try {
      const answer = await rest.request(
        "GET",
        `/query/${pathSegment(what)}?${codeSearch(what, params)}`,
        { signal },
      );
      if (answer.immutable) keep(key, answer.body);
      return answer.body as CodeQueryMap[K];
    } catch (err) {
      throw questionError(err);
    }
  },
};

/**
 * A `code_changes` listing read page after page until `rows` changes are
 * kept, the listing ends or [CHANGES_FILL_PAGES] pages are read
 * ([CodeChangesOptions.fill]): every change kept, the last page's cursor and
 * position. Refused as the transport refuses (`RestError`), so the hook reads
 * a refused page as it reads a refused question.
 */
async function filledChanges(
  params: CodeParams,
  rows: number,
  signal: AbortSignal,
): Promise<CodeChangesAnswer> {
  const changes: CodeChangeSummary[] = [];
  let cursor = "";
  let read = 0;
  let last: CodeChangesAnswer;
  do {
    const asked: CodeParams = cursor ? { ...params, cursor } : params;
    const answer = await rest.request(
      "GET",
      `/query/code_changes?${codeSearch("code_changes", asked)}`,
      { signal },
    );
    last = answer.body as CodeChangesAnswer;
    changes.push(...(last.changes ?? []));
    cursor = last.next ?? "";
    read++;
  } while (cursor !== "" && changes.length < rows && read < CHANGES_FILL_PAGES);
  const filled: CodeChangesAnswer = { ...last };
  delete filled.changes;
  if (changes.length > 0) filled.changes = changes;
  return filled;
}

/** The answer held for a key, and the tag a read again of it names. */
interface Tagged<T> {
  key: string;
  etag: string;
  body: T;
}

/**
 * Ask a code question and keep its answer current: `useQuery`'s result, read
 * one refusal wider — `detail` is the engine's own sentence on a `not_found`
 * as well as on `bad_params`, because which repository it found no
 * declaration of is the one fact a reader can act on.
 */
export function useCodeQuery(
  what: "code_changes",
  params?: CodeParams,
  options?: CodeChangesOptions,
): QueryResult<CodeChangesAnswer>;
export function useCodeQuery<K extends CodeQueryName>(
  what: K,
  params?: CodeParams,
  options?: CodeQueryOptions,
): QueryResult<CodeQueryMap[K]>;
export function useCodeQuery<K extends CodeQueryName>(
  what: K,
  params?: CodeParams,
  options: CodeChangesOptions = {},
): QueryResult<CodeQueryMap[K]> {
  // A FILL IS ONLY EVER A LISTING OF CHANGES': the overloads offer it on
  // `code_changes` alone, the listing a screen draws a few kept rows of.
  const { enabled = true, pollMs, refetchOnFocus, refetchOnReconnect, fill } = options;
  // THE QUESTION IS THE KEY — its kind and its parameters, never the floor
  // (see the file's doc). The parameters are a fresh literal on every render,
  // so their serialisation is what is compared; a fill is part of the
  // question, since it changes what the answer holds.
  const key = enabled ? keyOf(what, params) + (fill ? `#fill=${String(fill)}` : "") : null;
  const paramsRef = useRef(params);
  paramsRef.current = params;
  const tagged = useRef<Tagged<CodeQueryMap[K]> | null>(null);

  const result = useRest<CodeQueryMap[K]>(
    key,
    async (signal) => {
      if (fill) {
        // `code_changes`'s own answer, which the overloads make K's.
        const filled: unknown = await filledChanges(paramsRef.current ?? {}, fill, signal);
        return filled as CodeQueryMap[K];
      }
      const asking = key ?? "";
      // AN ANSWER THE ENGINE MARKED IMMUTABLE is the tab's, and never asked
      // again while it is kept (the file's doc).
      const keptBody = keptAnswer(asking);
      if (keptBody !== undefined) return keptBody as CodeQueryMap[K];
      const held = tagged.current?.key === asking ? tagged.current : null;
      const answer = await rest.request(
        "GET",
        `/query/${pathSegment(what)}?${codeSearch(what, paramsRef.current)}`,
        { signal, ...(held ? { headers: { "If-None-Match": held.etag } } : {}) },
      );
      if (answer.status === 304) {
        // ONLY EVER THE ANSWER THIS READ NAMED: a 304 to a request that named
        // no tag is no answer to anything this hook holds.
        if (held) {
          if (answer.immutable) keep(asking, held.body);
          return held.body;
        }
        throw new RestError(502, {
          error: "unreadable_body",
          detail: "the engine answered 304 to a read that named no tag",
        });
      }
      const body = answer.body as CodeQueryMap[K];
      if (answer.immutable) keep(asking, body);
      tagged.current = answer.etag ? { key: asking, etag: answer.etag, body } : null;
      return body;
    },
    { pollMs, refetchOnFocus, refetchOnReconnect },
  );
  const { reload } = result;

  // A WRITE FROM THIS TAB ASKS AGAIN, quietly, at the floor it raised — which
  // the very next ask reads. Never blanking the screen: what changed is drawn
  // over what was there.
  useEffect(() => {
    if (!enabled) return;
    return session.onWritten((domain, refreshes) => {
      if (SessionFloors.moves(what, domain, refreshes)) void reload({ quiet: true });
    });
  }, [enabled, what, reload]);

  const refetch = useCallback(() => void reload({ quiet: true }), [reload]);
  const detail = sentenceOf(result.error);
  return {
    data: result.data,
    loading: result.loading,
    error: result.code,
    refusal: result.refusal,
    ...(detail ? { detail } : {}),
    refetch,
  };
}
