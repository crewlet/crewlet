/**
 * Asking the engine one of its RANKED SEARCHES — `work_search` and
 * `knowledge` — as a hook, and the one way a screen asks either.
 *
 * # Why a hook of its own
 *
 * The engine refuses a phrase past its search bound (`SEARCH_QUERY_MAX`) as
 * `bad_params`, and a refused search that a screen drew as its results read as
 * a company with nothing written down. The palette, the work search and the
 * knowledge screen each checked the bound by hand before asking; the two
 * pickers that ask `work_search` too — handing a task over, linking one — did
 * not, so a pasted description was sent, refused, and shown as "No task
 * matches", or, typed past the bound, the list went on showing the hits of the
 * shorter term before it. A rule each caller has to remember is one a new
 * picker forgets, so the rule is the hook: it sends nothing past the bound,
 * says why (`tooLong`), and `app/source.test.ts` fails on any ranked search
 * asked through anything else.
 *
 * NOTHING ON HAND OUTLIVES A PHRASE TOO LONG TO SEND: the query is disabled,
 * and a disabled `useQuery` holds no answer, so a list can never go on showing
 * the hits of a shorter term under one it never asked.
 */

import type { QueryMap } from "~/protocol/index.ts";
import { searchTooLong } from "./search.ts";
import { useQuery, type QueryOptions, type QueryResult } from "./useQuery.ts";

/** The engine's ranked searches: both refuse a phrase past `SEARCH_QUERY_MAX`. */
export type SearchQueryName = "work_search" | "knowledge";

export interface SearchQueryResult<T> extends QueryResult<T> {
  /**
   * Why the phrase was not sent — it is past the engine's bound — or null.
   * When it is set nothing was asked and `data` is null; a screen draws it in
   * place of its results.
   */
  tooLong: string | null;
}

/**
 * `what`, asked for `params` exactly as `useQuery` asks it — unless the phrase
 * (`params.q`) is past the engine's bound, when nothing is sent and `tooLong`
 * says why. `options.enabled` keeps its meaning on top of that.
 */
export function useSearchQuery<K extends SearchQueryName>(
  what: K,
  params: { q: string } & Record<string, unknown>,
  options: QueryOptions = {},
): SearchQueryResult<QueryMap[K]> {
  const tooLong = searchTooLong(params.q);
  const result = useQuery(what, params, {
    ...options,
    enabled: (options.enabled ?? true) && tooLong === null,
  });
  return { ...result, tooLong };
}
