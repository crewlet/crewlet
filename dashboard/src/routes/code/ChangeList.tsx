/**
 * The changes and reviews a screen OUTSIDE the Code workspace lists: a task's
 * changes, a seat's own and the reviews it gave, the reviews waiting on a
 * person's day — each one listing the engine answers for a filter the screen
 * fixes, drawn as Code › Changes draws its rows.
 *
 * ONE GRID FOR EVERY LISTING OF CHANGES. Code › Changes' own grid
 * (`ChangesGrid`), never a second rendering of a change: a change read on a
 * seat's page and on the Changes listing says the same reference, readiness,
 * owner and blockers, and opens the same peek.
 *
 * THE FILTER IS THE SCREEN'S, AND FIXED. The screen that mounts a listing
 * decides what it IS — this work item's changes, this owner's — so there is no
 * bar to narrow it; its rows page with Load more as the Changes listing's do,
 * its first page held still from the press (`lib/usePaged.ts`), and its Show
 * all is that listing, narrowed the same way, where the reader can widen it.
 */

import type { ReactNode } from "react";
import {
  EmptyValue,
  Skeleton,
  TableDateCell,
  TableKeyCell,
  TableNumberCell,
  TableTextCell,
  Tag,
} from "@crewlethq/ui";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { href } from "~/app/router.tsx";
import { PageFailed, QueryState } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import { changePath, parseChangeRef, verdictWord } from "~/lib/code.ts";
import { fmtCount, plural } from "~/lib/format.ts";
import { codeRest, useCodeQuery, type CodeParams } from "~/lib/useCodeQuery.ts";
import { usePageHold, usePaged } from "~/lib/usePaged.ts";
import { useServicesAsk } from "~/lib/services.ts";
import type {
  CodeChangeSummary,
  CodeChangesAnswer,
  CodeReviewRow,
  CodeReviewsAnswer,
} from "~/protocol/index.ts";
import { ChangesGrid } from "./Changes.tsx";
import { changesHref } from "./paths.ts";

/**
 * How often a listing's first page is read again while it is open and
 * nothing past it is loaded: a MINUTE, the cadence every reader of the code
 * log keeps (Code › Changes, Repositories, Pushes). The code log has no push
 * to the dashboard, so another seat's review reaches the screen by this read
 * or not at all.
 */
export const CODE_LIST_POLL_MS = 60_000;

const changesOf = (a: CodeChangesAnswer) => a.changes ?? [];
const changeIdOf = (c: CodeChangeSummary) => c.id;
const reviewsOf = (a: CodeReviewsAnswer) => a.reviews ?? [];
const reviewIdOf = (r: CodeReviewRow) => r.id;
const nextOf = (a: { next?: string }) => a.next ?? "";

/** What an empty listing says it found none of, and where to look next. */
export interface ListEmpty {
  title: string;
  hint: string;
}

/**
 * The changes one `code_changes` question lists — the parameters are the
 * screen's, fixed — paged, in the engine's order, each row opening its change
 * in the peek.
 */
export function ChangeList({
  params,
  empty,
  enabled = true,
  label,
}: {
  params: CodeParams;
  empty: ListEmpty;
  enabled?: boolean;
  /** A line above the grid saying what it lists, where the screen has none. */
  label?: ReactNode;
}) {
  // ASKED ONCE THE COMPANY IS KNOWN TO HOST CODE (`useServicesAsk`): a
  // screen drawn on a cold load before the org projection lands asks nothing
  // the engine may refuse as a question it does not answer.
  const ask = useServicesAsk();
  const hold = usePageHold<CodeChangesAnswer>(params);
  const first = useCodeQuery("code_changes", params, {
    enabled: enabled && ask.code,
    pollMs: CODE_LIST_POLL_MS,
    hold: hold.holding,
  });
  const paged = usePaged(
    hold,
    first,
    (cursor) => codeRest.query("code_changes", { ...params, cursor }),
    changesOf,
    changeIdOf,
    nextOf,
  );
  const rows = paged.items;
  return (
    <div className="col gap-2">
      {label}
      {paged.loading && rows.length === 0 && (
        <Skeleton variant="text" rows={4} label="Loading changes" />
      )}
      <QueryState
        error={paged.error}
        refusal={paged.refusal}
        detail={first.detail}
        loading={paged.loading}
        empty={rows.length > 0 || paged.more ? undefined : empty}
      >
        <ChangesGrid
          rows={rows}
          onLoadMore={paged.more ? paged.older : undefined}
          loadedNote={loadedNote(paged.more, paged.held, rows.length, "change")}
        />
      </QueryState>
      {paged.pageError && <PageFailed error={paged.pageError} />}
    </div>
  );
}

/**
 * The reviews one reviewer gave (`code_reviews?reviewer=`), newest first
 * across the repositories the reader may read, each naming the change it
 * judged and opening that change's page.
 */
export function ReviewList({ reviewer, empty }: { reviewer: string; empty: ListEmpty }) {
  const params = { reviewer };
  const ask = useServicesAsk();
  const hold = usePageHold<CodeReviewsAnswer>(params);
  const first = useCodeQuery("code_reviews", params, {
    enabled: reviewer !== "" && ask.code,
    pollMs: CODE_LIST_POLL_MS,
    hold: hold.holding,
  });
  const paged = usePaged(
    hold,
    first,
    (cursor) => codeRest.query("code_reviews", { ...params, cursor }),
    reviewsOf,
    reviewIdOf,
    nextOf,
  );
  const rows = paged.items;
  return (
    <div className="col gap-2">
      {paged.loading && rows.length === 0 && (
        <Skeleton variant="text" rows={4} label="Loading reviews" />
      )}
      <QueryState
        error={paged.error}
        refusal={paged.refusal}
        detail={first.detail}
        loading={paged.loading}
        empty={rows.length > 0 || paged.more ? undefined : empty}
      >
        <ReviewsGrid
          rows={rows}
          onLoadMore={paged.more ? paged.older : undefined}
          loadedNote={loadedNote(paged.more, paged.held, rows.length, "review")}
        />
      </QueryState>
      {paged.pageError && <PageFailed error={paged.pageError} />}
    </div>
  );
}

function loadedNote(more: boolean, held: boolean, n: number, what: string): string | undefined {
  if (!more && !held) return undefined;
  return `${plural(n, what)} loaded${held ? ", paused while more are loaded" : ""}`;
}

/**
 * The address of the change a review judged. A `code_reviews` listing names
 * the change on every row it serves (`code.ReviewRow.Change`); a row whose
 * reference this build cannot read opens the Changes listing rather than
 * nothing, so every row of the grid goes somewhere.
 */
function reviewedHref(r: CodeReviewRow): string {
  const ref = r.change ? parseChangeRef(r.change.ref) : null;
  return ref ? href(changePath(ref.repo, ref.number)) : changesHref();
}

/**
 * The reviews a reviewer gave, a row each: the change it judged, the verdict,
 * the revision, the findings it raised, when. COLOUR IS STATE, and a verdict
 * is a judgement rather than a state of anything, so every verdict is drawn
 * alike, in words.
 */
function ReviewsGrid({
  rows,
  onLoadMore,
  loadedNote,
}: {
  rows: CodeReviewRow[];
  onLoadMore: (() => void) | undefined;
  loadedNote: string | undefined;
}) {
  const now = useNow();
  return (
    <DataGrid<CodeReviewRow>
      rows={rows}
      rowKey={(r) => r.id}
      serverSorted
      rowHref={reviewedHref}
      onLoadMore={onLoadMore}
      loadedNote={loadedNote}
      aria-label="Reviews"
      columns={[
        {
          key: "change",
          header: "Change",
          shrink: true,
          phoneLead: true,
          cell: (r) =>
            r.change ? <TableKeyCell value={r.change.ref} /> : <EmptyValue label="Not named" />,
        },
        {
          key: "title",
          header: "Title",
          floor: "12rem",
          cell: (r) => <TableTextCell>{r.change?.title ?? ""}</TableTextCell>,
        },
        {
          key: "verdict",
          header: "Verdict",
          shrink: true,
          cell: (r) => <Tag appearance="outline">{verdictWord(r.verdict)}</Tag>,
        },
        {
          key: "revision",
          header: "Revision",
          shrink: true,
          align: "right",
          drop: 3,
          cell: (r) => <TableNumberCell value={r.revision} format={fmtCount} />,
        },
        {
          key: "findings",
          header: "Findings",
          shrink: true,
          align: "right",
          drop: 2,
          cell: (r) => <TableNumberCell value={r.findings} format={fmtCount} />,
        },
        {
          key: "at",
          header: "Given",
          shrink: true,
          drop: 1,
          cell: (r) => <TableDateCell value={r.at} now={now} />,
        },
      ]}
    />
  );
}
