/**
 * Who the dashboard thinks you are.
 *
 * THE FRAME HAS NEVER HAD A VIEWER, and everything personal in it was fiction
 * without one. `routes/MyWork.tsx` picked the alphabetically first seat and
 * called the result "My work"; `work_views` was asked without a viewer, so no
 * pinned or personal view could arrive at all and a shared board was
 * pixel-identical to one somebody had pinned; and the five `preset=` values
 * the tracker's grammar resolves against the caller's own identity were parsed,
 * tested, documented and never sent.
 *
 * The answer is the PRINCIPAL the engine resolved for this browser — a person
 * signed in with a session, or a credential — and the seat the identity
 * directory binds them to. Nothing is derived here: the engine used to walk a
 * token id to a seat through a field on the seat's contact block, and that
 * binding is a directory row now (`crewlet iam bind`).
 *
 * THREE STATES, and a screen has to tell them apart:
 *
 *   - BOUND — a principal the directory binds to a seat. This person has an
 *     inbox, a queue, pins and favourites.
 *   - UNBOUND — a principal with no seat. An ORDINARY state: an operator who is
 *     not in the chart, a pipeline, an automation. A screen says what to bind
 *     rather than reporting a fault.
 *   - ANONYMOUS — nobody resolved at all. Every guarded surface is locked, and
 *     the lock says what it needs.
 *
 * And a FOURTH that is not one of them: NOBODY SAID YET. The three above are
 * answers; a query that has not come back is the absence of one, and it is
 * [ViewerState.loading] rather than a value. See [useViewer].
 */

import { useQuery } from "./useQuery.ts";
import type { Viewer } from "~/protocol/index.ts";

export interface ViewerState {
  /** The login the engine resolved this browser to, or "". */
  login: string;
  /** The capabilities this caller carries, as the engine reports them. */
  grants: string[];
  /**
   * Whether this caller holds `people:manage` — the one grant that reads and
   * changes ANYBODY's person record. A lead reads their reports' too, which
   * this screen cannot know; the engine decides, and a screen that asks on a
   * lead's behalf renders the refusal it gets.
   */
  managesPeople: boolean;
  /** The seat the directory binds this caller to, or "" — see the note above. */
  handle: string;
  /** That seat's display name, or "". */
  name: string;
  kind: "agent" | "human" | "";
  /** Somebody is resolved and the directory binds them to no seat. */
  unbound: boolean;
  /** Nobody is resolved at all. */
  anonymous: boolean;
  /** Nobody has said yet — no answer has arrived, or the last read failed. */
  loading: boolean;
}

/**
 * The viewer, polled rarely.
 *
 * A viewer changes on exactly two events — the reader signs in as somebody
 * else, or the directory binds them to a seat — so this is a slow poll rather
 * than a push, and a reconnect re-asks it.
 */
export function useViewer(): ViewerState {
  const { data, loading, error } = useQuery("viewer", undefined, { pollMs: 300_000 });
  const login = data?.login ?? "";
  const handle = data?.handle ?? "";
  const grants = data?.grants ?? [];

  // A FAILED READ IS NOT AN ANSWER, and reading it as one picks the worst of
  // the three. An error from this query means only that nothing came back:
  // `sendQuery` gives up after ten seconds, and the hub drops the OLDEST queued
  // envelope under per-client backpressure, so a busy company can evict this
  // very answer. Folded into ANONYMOUS it locked a signed-in person out of the
  // whole app rail, My work and the inbox — for the five minutes until the
  // next poll, with every other query on the page working.
  const unknown = loading || (error !== null && data === null);
  return {
    login,
    grants,
    managesPeople: grants.includes("people:manage"),
    handle,
    name: data?.name ?? "",
    kind: (data?.kind as ViewerState["kind"]) ?? "",
    unbound: login !== "" && handle === "",
    anonymous: !unknown && login === "",
    loading: unknown,
  };
}

/** The wire answer, re-exported so a screen types its own reads. */
export type { Viewer };
