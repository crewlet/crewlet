/**
 * The parts of one change the page reads, each its own `code_change`
 * question, at the tab's floor on the code log.
 *
 * A PART IS READ A PAGE AT A TIME, its page the engine's largest (`code.
 * ReadQuery.Limit`: two hundred comments, reviews or findings), and EVERY
 * PART A TAB DRAWS FOLLOWS ITS CURSOR AS THE READER ASKS — Load more — each
 * later page its first page's question asked after the cursor the page before
 * ended at ({@link usePagedChangePart}, through [changePartParams]). The
 * engine pages a conversation OLDEST FIRST, so what one page leaves out is
 * the newest: on a long-running change with several reviewers, the latest
 * review, the latest replies and the newest findings. A revision's FILES and a
 * file's PATCH page the same way (`Files.tsx`).
 * The one set read to its end is the reader's own standing findings, which a
 * review DECIDES (`useStandingFindings`).
 *
 * # A part is read again when the detail says it moved, never on a clock
 *
 * The page's detail read (`ChangePage`) is the one read on a clock
 * ({@link CHANGE_POLL_MS}), and it carries what every part is a function of:
 * the change's `version`, which every record that changes its row counts — a
 * pushed revision, a retarget, its life — and its conversation's, which every
 * record on its conversation counts (`code.ConversationRow.Version`), and when
 * somebody last wrote on it (`spoken_at`), which the reviews and comments are
 * keyed on ({@link partMovedBy}), and beside them its newest revisions, each
 * with the facts its home recorded. A part is asked again, quietly, when what
 * it reads moves, and not otherwise.
 * The `files` and `diff` parts above all: each is a git read on the
 * repository's home Gitlet node, holding a share of its read lane that pushes and
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

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { CHANGE_PART_PAGE } from "~/contract/code.ts";
import { codeRest, useCodeQuery } from "~/lib/useCodeQuery.ts";
import { usePaged, type Paged } from "~/lib/usePaged.ts";
import {
  isAbort,
  QueryError,
  type CodeChangeAnswer,
  type CodeChangeDetail,
  type CodeCommentRow,
  type CodeFindingRow,
  type CodeReviewRow,
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
    ...(listed ? { limit: CHANGE_PART_PAGE } : {}),
    ...extra,
  };
}

/**
 * What a part's answer is a function of, as the detail read says it, where its
 * reader names nothing finer: the change's version for `files` and `diff`,
 * which no conversation record moves; for `reviews` and `comments`, when
 * somebody last wrote on the change (`spoken_at`) beside how many rows the
 * part holds; and the change's and its conversation's versions for every other
 * part.
 *
 * A REVIEW OR A COMMENT ROW IS WRITTEN ONLY BY A RECORD SOMEBODY WROTE — a
 * review, a comment, a thread resolved — and every such record moves
 * `spoken_at` (`code.ConversationRow.SpokenAt`), where the conversation's
 * version moves with every record on its subject, the home's facts among them:
 * a revision's, and the mergeability it records again for every live change
 * into a target branch each time that branch moves. Keyed on the versions, a
 * reader holding k pages of reviews and comments asked all of them again on
 * every push to a busy target. The count beside the instant tells two such
 * records apart that the broker stamped in one microsecond (the resolution an
 * instant is kept at) where one of them added a row.
 */
export function partMovedBy(change: CodeChangeDetail, part: ChangePartName): string {
  if (part === "files" || part === "diff") return String(change.version);
  const conversation = change.conversation;
  if (part === "reviews") return `${conversation?.spoken_at ?? ""}/${conversation?.reviews ?? 0}`;
  if (part === "comments") return `${conversation?.spoken_at ?? ""}/${conversation?.comments ?? 0}`;
  return `${change.version}/${conversation?.version ?? 0}`;
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

/** The rows a page of a conversation's part carries, and each row's id. */
const reviewsOf = (a: CodeChangeAnswer) => a.reviews ?? [];
const commentsOf = (a: CodeChangeAnswer) => a.comments ?? [];
const findingsOf = (a: CodeChangeAnswer) => a.findings ?? [];
const rowId = (row: { id: string }) => row.id;
const partNext = (a: CodeChangeAnswer) => a.next ?? "";

/**
 * Every page of a conversation's part the reader has asked for, joined first
 * page first (`lib/usePaged.ts`): the first through the part's own question,
 * each later one that question after the cursor the page before ended at —
 * and every page held asked again when what the part reads moves (`moved`,
 * {@link partMovedBy} where the tab names nothing finer), since a page the
 * reader loaded is as stale as the first. `extra` narrows the part, as a
 * revision narrows the findings.
 */
function usePagedPart<T>(
  change: CodeChangeDetail,
  part: "reviews" | "comments" | "findings",
  pick: (a: CodeChangeAnswer) => readonly T[],
  idOf: (row: T) => string,
  extra: Readonly<Record<string, string | number | undefined>>,
  moved: string,
): Paged<T, CodeChangeAnswer> {
  const { repo, number } = change;
  const extraKey = JSON.stringify(extra);
  const params = useMemo(
    () => changePartParams({ repo, number }, part, JSON.parse(extraKey)),
    [repo, number, part, extraKey],
  );
  const first = useCodeQuery("code_change", params);
  const paged = usePaged(
    first,
    params,
    (cursor) => codeRest.query("code_change", { ...params, cursor }),
    pick,
    idOf,
    partNext,
  );
  useAskAgainWhenMoved(moved, true, paged.refresh);
  return paged;
}

/** A change's reviews, every page the reader asked for, oldest first. */
export function usePagedReviews(change: CodeChangeDetail): Paged<CodeReviewRow, CodeChangeAnswer> {
  return usePagedPart(change, "reviews", reviewsOf, rowId, {}, partMovedBy(change, "reviews"));
}

/** A change's comments, every page the reader asked for, in thread order. */
export function usePagedComments(
  change: CodeChangeDetail,
): Paged<CodeCommentRow, CodeChangeAnswer> {
  return usePagedPart(change, "comments", commentsOf, rowId, {}, partMovedBy(change, "comments"));
}

/**
 * A change's findings as revision `revision` reads them, every page the
 * reader asked for, asked again when `moved` does.
 */
export function usePagedFindings(
  change: CodeChangeDetail,
  revision: number,
  moved: string,
): Paged<CodeFindingRow, CodeChangeAnswer> {
  return usePagedPart(change, "findings", findingsOf, rowId, { revision }, moved);
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
 * pages of {@link CHANGE_PART_PAGE}. None for a reader bound to no seat, who
 * raised none.
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
              limit: CHANGE_PART_PAGE,
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
