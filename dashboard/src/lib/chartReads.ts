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
 */

import { useEffect, useState } from "react";
import { isAbort, rest, RestError, type QueryValue } from "~/protocol/index.ts";

/** What a guarded chart read answered, or why it did not. */
export type ChartReading<T> =
  | { readonly state: "read"; readonly value: T }
  /** The chart holds no such object (404). */
  | { readonly state: "absent" }
  /**
   * Refused on authority. `grants` are the ones the engine named, any ONE of
   * which would admit; `reason` the deciding rule's own, "" for a 401, where
   * nothing the engine accepted was presented.
   */
  | { readonly state: "refused"; readonly grants: readonly string[]; readonly reason: string }
  /** The engine could not answer: unreachable, a node catching up, a fault. */
  | { readonly state: "failed" }
  /** Nothing has been asked, or nothing has come back. Never a claim. */
  | { readonly state: "unread" };

const UNREAD: ChartReading<never> = { state: "unread" };

/**
 * One GET against the chart, re-read whenever `refresh` changes identity.
 * `path` null asks nothing (a parameter not known yet) and answers `unread`.
 */
export function useChartRead<T>(
  path: string | null,
  query: Record<string, QueryValue> | undefined,
  refresh: unknown,
): ChartReading<T> {
  const [reading, setReading] = useState<{ path: string | null; answer: ChartReading<T> }>({
    path: null,
    answer: UNREAD,
  });
  const queryKey = query ? JSON.stringify(query) : "";
  useEffect(() => {
    if (path === null) return;
    const controller = new AbortController();
    rest
      .request("GET", path, {
        ...(queryKey ? { query: JSON.parse(queryKey) as Record<string, QueryValue> } : {}),
        signal: controller.signal,
      })
      .then(
        (answer) => setReading({ path, answer: { state: "read", value: answer.body as T } }),
        (err: unknown) => {
          if (isAbort(err)) return;
          setReading({ path, answer: failureOf(err) });
        },
      );
    return () => controller.abort();
  }, [path, queryKey, refresh]);
  // AN ANSWER ABOUT ANOTHER OBJECT IS NO ANSWER: a screen that moves from one
  // seat to the next reads `unread` until the new seat's answer arrives,
  // rather than the previous seat's settings under the new seat's name.
  return path !== null && reading.path === path ? reading.answer : UNREAD;
}

/** A failed read as the outcome it is. */
function failureOf(err: unknown): ChartReading<never> {
  if (err instanceof RestError) {
    if (err.status === 404) return { state: "absent" };
    if (err.unauthorized) {
      const reason = typeof err.body.reason === "string" ? err.body.reason : "";
      return { state: "refused", grants: err.grants, reason: err.status === 401 ? "" : reason };
    }
  }
  return { state: "failed" };
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
