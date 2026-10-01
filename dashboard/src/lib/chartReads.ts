/**
 * Reading the org chart from a screen that is not the builder: one seat, one
 * unit, or the whole chart, with its RUNTIME half where the reader may have it.
 *
 * WHY THIS IS A REST READ. The chart is its own log (`/chart`), and the parts
 * of it these screens need are the ones the anonymous org projection leaves
 * out on purpose: a seat's model chain, token budget, contact identities and
 * tool credentials, a unit's knowledge space and the credentials its members
 * inherit. No socket question answers them, so they are read the way the
 * builder reads them, over the dashboard's one REST transport.
 *
 * FIVE OUTCOMES, NOT A NULLABLE VALUE, for the reason `lib/seats.ts` gives for
 * the seat page: "this object is not in the chart", "you may not read it",
 * "the engine could not answer", "the read has not come back" and "here it
 * is" are five different facts, and a screen that folded them together told
 * a reader who lacked a grant that the company had nothing to show.
 *
 * A RUNTIME HALF WITHHELD IS AN ANSWER, not a refusal. The chart serves a
 * reader who may read the chart but not the company's configuration the rows
 * WITHOUT that half, and says so (`runtime: false`), so a caller tells "this
 * seat has no model chain" from "you were not shown it" by the answer's own
 * flag rather than by an empty field.
 *
 * RE-READ ON EVERY ORG PUSH. A chart write that lands is followed by an org
 * projection the engine pushes, so the push is the signal that what was read
 * may have moved; `refresh` is that push's identity. A refusal REPLACES what
 * was read rather than sitting beside it, because a guarded answer a reader
 * has since lost the right to must not stay on screen.
 *
 * AND ASKED AGAIN ON ITS OWN, because it is the shared REST read
 * (`./restRead.ts`) rather than a loader of its own. It was a fifth
 * hand-written loader beside the four that hook replaced, with the defect they
 * had: a read nobody answered — thirty seconds on a slow engine, one request
 * dropped — or a `503` the engine wrote was `failed`, and nothing asked again
 * until the next org push, which with the socket up and the chart quiet never
 * came. The seat's settings panel said "This panel fills in when it does"
 * over a read nothing was going to repeat. Now a read nobody answered backs
 * off on its own, a `503` is asked when the engine says, the socket coming
 * back asks at once, and `failed` carries WHICH failure it was, in the terms
 * `QueryState` draws, so a screen can say whether it asks again.
 */

import { useEffect, useRef } from "react";
import { useRestRead } from "./restRead.ts";
import { rest, RestError, type QueryValue, type RestFailure } from "~/protocol/index.ts";

/** What a guarded chart read answered, or why it did not. */
export type ChartReading<T> =
  | { readonly state: "read"; readonly value: T }
  /** The chart holds no such object: the ENGINE's 404, never a gateway's. */
  | { readonly state: "absent" }
  /**
   * Refused on authority. `grants` are the ones the engine named, any ONE of
   * which would admit; `reason` the deciding rule's own, "" for a 401, where
   * nothing the engine accepted was presented.
   */
  | { readonly state: "refused"; readonly grants: readonly string[]; readonly reason: string }
  /**
   * The engine could not answer, or nothing came back from it: a node
   * catching up, a fault, a request past its deadline, a gateway in its place.
   * `failure` is which, as the pair `QueryState` draws — and so whether the
   * read is asking again on its own.
   */
  | { readonly state: "failed"; readonly failure: RestFailure }
  /** Nothing has been asked, or nothing has come back. Never a claim. */
  | { readonly state: "unread" };

const UNREAD: ChartReading<never> = { state: "unread" };

/** What one chart GET answered: the object, or the engine's word that there is none. */
type Answer<T> = { readonly found: true; readonly value: T } | { readonly found: false };

/**
 * One GET against the chart, re-read whenever `refresh` changes identity.
 * `path` null asks nothing (a parameter not known yet) and answers `unread`.
 */
export function useChartRead<T>(
  path: string | null,
  query: Record<string, QueryValue> | undefined,
  refresh: unknown,
): ChartReading<T> {
  const queryKey = query ? JSON.stringify(query) : "";
  const read = useRestRead<Answer<T>>(
    // THE QUESTION IS THE PATH AND ITS QUERY, so an answer about one object
    // is never shown under another's: a screen that moves from one seat to
    // the next reads `unread` until the new seat's answer arrives, rather than
    // the previous seat's settings under the new seat's name.
    `${path ?? ""}?${queryKey}`,
    async (signal) => {
      try {
        const answer = await rest.request("GET", path ?? "", {
          ...(queryKey ? { query: JSON.parse(queryKey) as Record<string, QueryValue> } : {}),
          signal,
        });
        return { found: true, value: answer.body as T };
      } catch (err) {
        // NO SUCH OBJECT IS AN ANSWER, and it is the ENGINE's to give: a
        // gateway's 404 says nothing about the chart, and drawn as `absent` it
        // told a reader the company has no such seat.
        if (err instanceof RestError && err.status === 404 && !err.unanswered) {
          return { found: false };
        }
        throw err;
      }
    },
    { enabled: path !== null },
  );

  // RE-READ ON EVERY ORG PUSH, quietly, keeping what is drawn until the answer
  // lands. A change of identity is the signal, never the first one: the read
  // already asks when it mounts.
  const { refetch } = read;
  const seen = useRef(refresh);
  useEffect(() => {
    if (Object.is(seen.current, refresh)) return;
    seen.current = refresh;
    refetch();
  }, [refresh, refetch]);

  if (read.failure) return failureOf(read.failure, read.error);
  if (read.data === null) return UNREAD;
  return read.data.found ? { state: "read", value: read.data.value } : { state: "absent" };
}

/**
 * A failed read as the outcome it is. ANY failure replaces what an earlier
 * read showed, a refusal above all: a guarded answer the reader has since lost
 * the right to must not stay on screen, and an answer from before a failed
 * re-read is one the push that asked for it says has moved.
 */
function failureOf(failure: RestFailure, err: unknown): ChartReading<never> {
  if (failure.error === "unauthorized" && err instanceof RestError) {
    const reason = typeof err.body.reason === "string" ? err.body.reason : "";
    return { state: "refused", grants: err.grants, reason: err.status === 401 ? "" : reason };
  }
  return { state: "failed", failure };
}

/** The path of one seat's chart read. */
export function chartSeatPath(handle: string): string {
  return `/chart/seats/${encodeURIComponent(handle)}`;
}

/** The path of one unit's chart read. */
export function chartUnitPath(key: string): string {
  return `/chart/units/${encodeURIComponent(key)}`;
}

/** The query that asks for the runtime half where the reader may have it. */
export const WITH_RUNTIME: Record<string, QueryValue> = { runtime: "true" };
