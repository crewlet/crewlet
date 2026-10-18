/**
 * Asking the engine one of its RANKED SEARCHES — `work_search`, `knowledge`
 * and `chat_search` — as a hook, and the one way a screen asks any of them.
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
 * shorter term before it. The chat screen's search and its room finder made
 * the same mistake a third time, the moment chat took the same bound. A rule
 * each caller has to remember is one a new picker forgets, so the rule is the
 * hook: it sends nothing past the bound, says why (`tooLong`), and
 * `app/source.test.ts` fails on any ranked search asked through anything else.
 *
 * NOTHING ON HAND OUTLIVES A PHRASE TOO LONG TO SEND: the query is disabled,
 * and a disabled `useQuery` holds no answer, so a list can never go on showing
 * the hits of a shorter term under one it never asked.
 *
 * # Each search is its app's
 *
 * `work_search` searches Worklet, `knowledge` the knowledge base and
 * `chat_search` the company's own rooms, so each is asked only of a reader
 * holding that app ([SEARCH_APP]): the engine refuses the first and the last
 * without Worklet or Chatlet and answers the second with nothing searched
 * (`no_access`) without Doclet. Not asked, the hook says why (`withheld`), in
 * the words every lock uses, so a list draws "Search needs Doclet access"
 * rather than "nothing matched".
 */

import type { QueryMap } from "~/protocol/index.ts";
import { CHATLET, DOCLET, WORKLET, holds, needsWords, type App } from "./access.ts";
import { searchTooLong } from "./search.ts";
import { useQuery, type QueryOptions, type QueryResult } from "./useQuery.ts";
import { useViewer } from "./viewer.ts";

/** The engine's ranked searches: each refuses a phrase past `SEARCH_QUERY_MAX`. */
export type SearchQueryName = "work_search" | "knowledge" | "chat_search";

/** The app each ranked search searches, which a reader must hold to ask it. */
export const SEARCH_APP: Record<SearchQueryName, App> = {
  work_search: WORKLET,
  knowledge: DOCLET,
  chat_search: CHATLET,
};

/**
 * The `backend` a `knowledge` answer names when Doclet answered it. The engine
 * stores it on every search record, so it keeps this spelling; a screen
 * compares against it and never shows it.
 */
export const DOCLET_SEARCH_BACKEND = "native";

export interface SearchQueryResult<T> extends QueryResult<T> {
  /**
   * Why the phrase was not sent — it is past the engine's bound — or null.
   * When it is set nothing was asked and `data` is null; a screen draws it in
   * place of its results.
   */
  tooLong: string | null;
  /**
   * Why the search was not asked because the reader does not hold its app —
   * "Search needs Doclet access. An admin can give it to you.", or null.
   * Decided once the viewer has answered; null while it has not.
   */
  withheld: string | null;
}

/**
 * `what`, asked for `params` exactly as `useQuery` asks it — unless the phrase
 * (`params.q`) is past the engine's bound, when nothing is sent and `tooLong`
 * says why, or the reader does not hold the search's app, when nothing is sent
 * and `withheld` says why. `options.enabled` keeps its meaning on top of that.
 */
export function useSearchQuery<K extends SearchQueryName>(
  what: K,
  params: { q: string } & Record<string, unknown>,
  options: QueryOptions = {},
): SearchQueryResult<QueryMap[K]> {
  const tooLong = searchTooLong(params.q);
  const viewer = useViewer();
  const app = SEARCH_APP[what];
  const mayAsk = holds(viewer.grants, app);
  const result = useQuery(what, params, {
    ...options,
    enabled: (options.enabled ?? true) && tooLong === null && mayAsk,
  });
  const withheld = !viewer.loading && !mayAsk ? needsWords("Search", [app]) : null;
  return { ...result, tooLong, withheld };
}
