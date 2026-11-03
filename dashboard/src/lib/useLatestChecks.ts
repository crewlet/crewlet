/**
 * What each check of a change's head, or of a commit for one branch, STANDS
 * AT: its latest attempt — `code_checks` asked with `latest`, every page of it.
 *
 * # Why the engine's latest, and why every page
 *
 * A meter of a queue's candidate and an Inbox card's checks say what each
 * required check stands at. Rebuilt here from the attempts' history, one page
 * of it was all a screen saw: a head's history passes a page at ordinary
 * scale (a checks file declares up to 64 checks, each rerun up to ten times),
 * so the checks named after the page's end went missing and a check rerun
 * across it was drawn at its superseded attempt — "failed" after its rerun
 * passed. The engine answers the latest attempt of each check alone, one row
 * a check, and up to 64 a page — every check one checks file declares — so
 * this is one read; and where a commit holds more (a branch whose checks file
 * changed between dispatches), `next` is followed to its end, since a meter
 * of some of the checks says something false about the rest.
 *
 * # Nothing drawn from part of an answer
 *
 * `rows` is undefined until every page of the question has answered, and a
 * poll's re-read keeps the last whole answer on screen until the new one is
 * whole: a meter is never drawn from a first page alone, and never blinks out
 * while the rest is read.
 */

import { useEffect, useMemo, useState } from "react";
import type { QueryErrorCode } from "~/contract/errors.ts";
import { codeRest, useCodeQuery, type CodeParams } from "./useCodeQuery.ts";
import type { CodeCheckRow } from "~/protocol/index.ts";

/** What [useLatestChecks] answers: every row, once whole, and the first page's refusal. */
export interface LatestChecks {
  rows: readonly CodeCheckRow[] | undefined;
  error: QueryErrorCode | null;
}

/**
 * Each check's latest attempt that `params` names — a change's (`repo`,
 * `change`) or a commit's for a branch (`repo`, `oid`, `target`) — every page
 * of it, re-read every `pollMs`.
 */
export function useLatestChecks(params: CodeParams, pollMs?: number): LatestChecks {
  // THE QUESTION IS ITS SERIALISATION: the parameters are a fresh literal on
  // every render, as `useCodeQuery`'s are.
  const key = JSON.stringify(params);
  const question = useMemo<CodeParams>(
    () => ({ ...(JSON.parse(key) as CodeParams), latest: true }),
    [key],
  );
  const first = useCodeQuery("code_checks", question, { pollMs });
  const [held, setHeld] = useState<{ key: string; rows: readonly CodeCheckRow[] } | null>(null);

  // THE REST OF THE PAGES, read once the first names a `next`, and held —
  // beside the first page's rows — only once the last has answered.
  useEffect(() => {
    const answer = first.data;
    if (!answer) return;
    if (!answer.next) {
      setHeld({ key, rows: answer.checks ?? [] });
      return;
    }
    const abort = new AbortController();
    void (async () => {
      const rows = [...(answer.checks ?? [])];
      let cursor = answer.next ?? "";
      try {
        while (cursor) {
          const page = await codeRest.query("code_checks", { ...question, cursor }, abort.signal);
          rows.push(...(page.checks ?? []));
          cursor = page.next ?? "";
        }
      } catch {
        // A LATER PAGE REFUSED leaves the last whole answer standing, as a
        // failed poll does: the next poll asks again.
        return;
      }
      if (!abort.signal.aborted) setHeld({ key, rows });
    })();
    return () => abort.abort();
  }, [first.data, key, question]);

  return { rows: held?.key === key ? held.rows : undefined, error: first.error };
}
