/**
 * Asking the code host a question, as a hook — over REST, at this tab's floor
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
 * beside the projection. So the Code workspace asks them over REST, on the
 * registry's generic form of the question (`GET /query/{what}` — the named
 * routes, `/code/repos…`, are the same answer for a script), and a slow one
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
 * # An answer by object id is held by its tag
 *
 * The engine marks an answer read by object id immutable: a strong `ETag` of
 * its own bytes and `Cache-Control: immutable`, and `304` to a request naming
 * the tag. The transport asks with `cache: "no-store"` (`protocol/rest.ts`: a
 * guarded read of something that changes is never served from a cache), so the
 * tag is kept here instead: a question read again — a reconnect, the tab coming
 * back — names the tag of the answer it holds, and a `304` is that answer, with
 * no body sent twice. An answer that carries no tag is read whole every time.
 */

import { useCallback, useEffect, useRef } from "react";
import { restErrorCode, useRest } from "./useRest.ts";
import { withFloor, type QueryResult } from "./useQuery.ts";
import {
  QueryError,
  pathSegment,
  RestError,
  rest,
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
  /** Ask again when the socket reconnects. Default true. */
  refetchOnReconnect?: boolean;
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
 * One ask of a code question, for a page a reader asks for once — a listing's
 * older page — at this tab's floor, refused as a [QueryError].
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
    try {
      const answer = await rest.request(
        "GET",
        `/query/${pathSegment(what)}?${codeSearch(what, params)}`,
        { signal },
      );
      return answer.body as CodeQueryMap[K];
    } catch (err) {
      throw questionError(err);
    }
  },
};

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
export function useCodeQuery<K extends CodeQueryName>(
  what: K,
  params?: CodeParams,
  options: CodeQueryOptions = {},
): QueryResult<CodeQueryMap[K]> {
  const { enabled = true, pollMs, refetchOnFocus, refetchOnReconnect } = options;
  // THE QUESTION IS THE KEY — its kind and its parameters, never the floor
  // (see the file's doc). The parameters are a fresh literal on every render,
  // so their serialisation is what is compared.
  const key = enabled ? `${what}?${JSON.stringify(params ?? {})}` : null;
  const paramsRef = useRef(params);
  paramsRef.current = params;
  const tagged = useRef<Tagged<CodeQueryMap[K]> | null>(null);

  const result = useRest<CodeQueryMap[K]>(
    key,
    async (signal) => {
      const asking = key ?? "";
      const held = tagged.current?.key === asking ? tagged.current : null;
      const answer = await rest.request(
        "GET",
        `/query/${pathSegment(what)}?${codeSearch(what, paramsRef.current)}`,
        { signal, ...(held ? { headers: { "If-None-Match": held.etag } } : {}) },
      );
      if (answer.status === 304) {
        // ONLY EVER THE ANSWER THIS READ NAMED: a 304 to a request that named
        // no tag is no answer to anything this hook holds.
        if (held) return held.body;
        throw new RestError(502, {
          error: "unreadable_body",
          detail: "the engine answered 304 to a read that named no tag",
        });
      }
      const body = answer.body as CodeQueryMap[K];
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
