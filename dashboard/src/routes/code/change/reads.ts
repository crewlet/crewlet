/**
 * The parts of one change the page reads, each its own `code_change`
 * question, at the tab's floor on the code log.
 *
 * A PART IS READ A PAGE AT A TIME, its page the engine's largest (`code.
 * ReadQuery.Limit`: two hundred comments, reviews or findings): a change's
 * conversation past that is rare, and where a page carries a cursor the tab
 * that draws it SAYS that more exist rather than drawing a page as the whole.
 * A revision's FILES and a file's PATCH are not rare past one page — a large
 * change lists hundreds of files, and a generated file's patch runs to many
 * pages — so the Files tab follows both past their first page as the reader
 * asks (`Files.tsx`, through [changePartParams], so a later page asks its
 * first page's question).
 * The one set read to its end is the reader's own standing findings, which a
 * review DECIDES (`useStandingFindings`).
 *
 * # A part is read again when the detail says it moved, never on a clock
 *
 * The page's detail read (`ChangePage`) is the one read on a clock
 * ({@link CHANGE_POLL_MS}), and it carries what every part is a function of:
 * the change's `version`, which every record that changes its row counts — a
 * pushed revision, a retarget, its life — and its conversation's, which every
 * record on its conversation counts (`code.ConversationRow.Version`), and
 * beside them its newest revisions, each with the facts its home recorded. A
 * part is asked again, quietly, when what it reads moves, and not otherwise.
 * The `files` and `diff` parts above all: each is a git read on the
 * repository's home git node, holding a share of its read lane that pushes and
 * clones need too, and its answer is a function of the revision's head and the
 * base its facts record ({@link revisionInputs}) — facts its home records
 * again in a record of its own (`code/revise.go`: a retarget's new facts of
 * the same revision replace the row, after the record that moved the change's
 * version) — so it is asked again when either moves. Polled,
 * every open Files tab redid that diff each minute for an answer that could not
 * change. A revision whose base its home has not recorded yet is refused
 * `unavailable` with the engine's wait, which `useRest` honours, so a part that
 * waits on the home's facts is read again without a clock of its own.
 */

import { useCallback, useEffect, useRef, useState } from "react";
import { codeRest, useCodeQuery } from "~/lib/useCodeQuery.ts";
import {
  isAbort,
  QueryError,
  type CodeChangeDetail,
  type CodeFindingRow,
  type CodeRevisionRow,
  type LogRefusal,
  type QueryRefusal,
} from "~/protocol/index.ts";

/**
 * How often an open change's detail is read again: a MINUTE, the workspace's
 * cadence on the code log, which has no push to the dashboard — a seat's
 * review reaches the open page by this read or by the reader's own press,
 * which asks again at once. Every part follows the detail (the file's doc).
 */
export const CHANGE_POLL_MS = 60_000;
/** The most rows a part's page reads: the engine's own bound for the conversation's parts. */
export const PART_PAGE = 200;

/** A part of a change, as the page asks for it. */
export type ChangePartName =
  "reviewers" | "comments" | "reviews" | "findings" | "standing" | "files" | "diff";

/** What a part's question is asked with: the change, the part, its page's bound where it lists, and `extra`. */
export function changePartParams(
  change: Pick<CodeChangeDetail, "repo" | "number">,
  part: ChangePartName,
  extra: Readonly<Record<string, string | number | undefined>> = {},
) {
  const listed = part !== "files" && part !== "diff";
  return {
    name: change.repo,
    change: change.number,
    part,
    ...(listed ? { limit: PART_PAGE } : {}),
    ...extra,
  };
}

/**
 * What a part's answer is a function of, as the detail read says it, where its
 * reader names nothing finer: the change's version for `files` and `diff`,
 * which no conversation record moves, and the change's and its conversation's
 * versions for every other part.
 */
export function partMovedBy(change: CodeChangeDetail, part: ChangePartName): string {
  if (part === "files" || part === "diff") return String(change.version);
  return `${change.version}/${change.conversation?.version ?? 0}`;
}

/**
 * What revision `n`'s files are read from, as the detail's revisions say it:
 * its head and the base its facts record — `pending` before its home has
 * recorded them, "" for a revision older than the detail's page, which the
 * change's version alone then answers for.
 */
export function revisionInputs(revisions: readonly CodeRevisionRow[], n: number): string {
  const r = revisions.find((one) => one.n === n);
  if (!r) return "";
  return `${r.head}:${r.facts ? (r.facts.base_oid ?? "unrelated") : "pending"}`;
}

/**
 * Ask a read again, quietly, when what it is read from — `moved` — changes,
 * and not on its first render, which asked it already. `again` is the read's
 * own way to ask: a part's `refetch`, or a paged read's `refresh`, which asks
 * every page the reader holds again, since a diff whose inputs moved is a
 * different diff on every page.
 */
export function useAskAgainWhenMoved(moved: string, enabled: boolean, again: () => void) {
  const askedAt = useRef(moved);
  useEffect(() => {
    if (askedAt.current === moved) return;
    askedAt.current = moved;
    if (enabled) again();
  }, [moved, enabled, again]);
}

/**
 * One part of a change, as its tab reads it: asked again when what it reads
 * moves — `moved`, where the tab names it finer than {@link partMovedBy}.
 */
export function useChangePart(
  change: CodeChangeDetail,
  part: ChangePartName,
  extra: Readonly<Record<string, string | number | undefined>> = {},
  enabled = true,
  moved: string = partMovedBy(change, part),
) {
  const read = useCodeQuery("code_change", changePartParams(change, part, extra), { enabled });
  useAskAgainWhenMoved(moved, enabled, read.refetch);
  return read;
}

/**
 * The reader's standing findings as the review dialog holds them: every one,
 * once the read has reached its last page (`findings`), or why it has not —
 * still reading (`findings` null, no `error`), or refused.
 */
export interface StandingRead {
  findings: CodeFindingRow[] | null;
  error: string | null;
  refusal: QueryRefusal | LogRefusal | null;
  detail?: string;
  /** Read the set again from its first page, after a refusal. */
  retry: () => void;
}

/**
 * The reader's own STANDING findings on the change — open, or answered by an
 * author — which a review of theirs may carry (`code.Review.Carries`): the
 * findings a review of a later revision supersedes by leaving them out. READ
 * WHOLE, the engine's `standing` part paged to its last cursor, because a set
 * the dialog does not hold is a set the press would supersede unseen: the
 * findings part pages the change's whole history oldest first, so a
 * reviewer's NEWEST standing findings were past its first page. The engine
 * keeps at most 1 024 of them a reviewer (`code.MaxStandingFindings`), six
 * pages of {@link PART_PAGE}. None for a reader bound to no seat, who raised
 * none.
 */
export function useStandingFindings(change: CodeChangeDetail, handle: string): StandingRead {
  const [attempt, setAttempt] = useState(0);
  const [read, setRead] = useState<Omit<StandingRead, "retry">>({
    findings: null,
    error: null,
    refusal: null,
  });
  const { repo, number } = change;
  useEffect(() => {
    if (handle === "") {
      setRead({ findings: [], error: null, refusal: null });
      return;
    }
    setRead({ findings: null, error: null, refusal: null });
    const abort = new AbortController();
    void (async () => {
      const findings: CodeFindingRow[] = [];
      let cursor: string | undefined;
      try {
        for (;;) {
          const page = await codeRest.query(
            "code_change",
            {
              name: repo,
              change: number,
              part: "standing",
              limit: PART_PAGE,
              ...(cursor ? { cursor } : {}),
            },
            abort.signal,
          );
          findings.push(...(page.findings ?? []));
          if (!page.next) break;
          // A CURSOR THAT DOES NOT MOVE is no engine's of this build: said,
          // rather than asked again for ever.
          if (page.next === cursor) throw new QueryError("query_failed");
          cursor = page.next;
        }
        setRead({ findings, error: null, refusal: null });
      } catch (err) {
        if (abort.signal.aborted || isAbort(err)) return;
        const refused = err instanceof QueryError ? err : new QueryError("query_failed");
        setRead({
          findings: null,
          error: refused.message,
          refusal: refused.refusal,
          ...(refused.detail ? { detail: refused.detail } : {}),
        });
      }
    })();
    return () => abort.abort();
  }, [repo, number, handle, attempt]);
  const retry = useCallback(() => setAttempt((n) => n + 1), []);
  return { ...read, retry };
}

/** Whether one position on the code log is before another: by generation, then sequence. */
export function positionBefore(
  a: { generation: number; seq: number },
  b: { generation: number; seq: number },
): boolean {
  return a.generation === b.generation ? a.seq < b.seq : a.generation < b.generation;
}
