/**
 * The most expensive tasks: on the overview, the top three; at
 * `#/spend/tasks`, the list — every task last changed inside the window,
 * most tokens first, each saying what drove it.
 *
 * A TASK'S TOKENS ARE ITS WHOLE LIFE'S. The tracker charges a turn to one work
 * item (ADR-0022) and keeps the running total on the task, not per day, so a
 * task that began last quarter and was reopened this week carries all of it.
 * The window chooses WHICH tasks are listed — the ones whose last change fell
 * inside it, bounded at BOTH edges (see [expensiveTasksParams]) — and the list
 * says so rather than presenting a lifetime as a window's.
 *
 * WHAT DROVE IT is the task's own spend facts (`fields=spend`: turns, workers,
 * send-backs, reopens), read in the same answer as the rows. No price, and no
 * verdict of this screen's about which task is a problem: the facts are the
 * engine's, and the reader judges.
 *
 * ABSENT ON A COMPANY WHOSE TRACKER IS NOT THE ENGINE'S: `work_items` is then
 * not served (`unknown_query`) and there is no task spend to rank.
 */

import { Card, Skeleton } from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { KeyCell, TokenCell } from "~/app/frame/cells.tsx";
import { peekHref, peekRow, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { QueryState } from "~/components/common.tsx";
import { useNow } from "~/lib/clock.ts";
import { fmtCount, fmtExact } from "~/lib/format.ts";
import { isRange, useTimeRange } from "~/lib/range.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { TimeRangePicker } from "~/ui/TimeRange.tsx";
import type { WorkSummary } from "~/protocol/types.ts";
import { useMemo } from "react";
import {
  expensiveTasksParams,
  spendOffer,
  spendWindowParams,
  taskFacts,
  windowWords,
} from "./model.ts";
import { itemAddress } from "~/lib/work.ts";

/** How many the overview's card lists — the design's three. */
export const TOP_TASKS = 3;

/** How many the list asks for: the tracker's page ceiling for one answer. */
export const TASK_ROWS = 50;

/** A task's peek, at its address ([itemAddress]). */
function itemRef(row: WorkSummary): { kind: "item"; id: string } {
  return { kind: "item", id: itemAddress(row) };
}

/**
 * The overview's card: the three tasks that spent the most among those last
 * changed inside the window, and the way to the whole list.
 */
export function ExpensiveTasksCard({
  since,
  until,
  window,
}: {
  /** The window's two instants, from the spend answer — `until` exclusive;
   *  absent until it arrives. */
  since?: string;
  until?: string;
  /** `window=` as the overview carries it, so "All" opens the same window. */
  window: string;
}) {
  const ready = Boolean(since && until);
  const answer = useQuery(
    "work_items",
    ready ? expensiveTasksParams(since!, until!, TOP_TASKS) : undefined,
    { enabled: ready },
  );
  if (answer.error === "unknown_query") return null;
  const rows = answer.data?.items ?? [];
  return (
    <Card className="spend-card" padding="none">
      <div className="spend-card-head">
        <h2 className="spend-title">Most expensive tasks</h2>
        <a className="t-link spend-caption" href={href(["spend", "tasks"], { window })}>
          All
        </a>
      </div>
      <QueryState
        error={answer.error}
        refusal={answer.refusal}
        detail={answer.detail}
        loading={false}
      >
        {!answer.data ? (
          <div className="spend-card-body">
            <Skeleton height="6rem" />
          </div>
        ) : rows.length === 0 ? (
          <p className="spend-card-body spend-muted">
            No task last changed in this window has spent tokens yet.
          </p>
        ) : (
          <ul className="spend-task-list">
            {rows.map((row) => (
              <li key={row.id}>
                <a className="spend-task" href={peekHref(itemRef(row))}>
                  <span className="spend-task-main">
                    <span className="spend-task-title">
                      {row.key && <span className="spend-task-key">{row.key}</span>}
                      <span className="truncate">{row.title}</span>
                    </span>
                    {row.spend && <span className="spend-caption">{taskFacts(row.spend)}</span>}
                  </span>
                  <span
                    className="spend-task-tokens"
                    title={`${fmtExact(row.spend?.tokens)} tokens`}
                  >
                    {fmtCount(row.spend?.tokens)}
                  </span>
                </a>
              </li>
            ))}
          </ul>
        )}
      </QueryState>
    </Card>
  );
}

/** `#/spend/tasks`: the list. */
export function ExpensiveTasks() {
  const zone = useOrg()?.timezone;
  const now = useNow();
  const range = useTimeRange(now, spendOffer(zone));
  // THE WINDOW'S TWO INSTANTS, from the same spend answer the overview reads
  // — the engine cuts the company days, and this list starts and ends where
  // they do.
  const spend = useQuery("tokens", spendWindowParams(range.window, zone));
  const since = spend.data?.since;
  const until = spend.data?.until;
  const ready = Boolean(since && until);
  const answer = useQuery(
    "work_items",
    ready ? expensiveTasksParams(since!, until!, TASK_ROWS) : undefined,
    { enabled: ready },
  );
  const { open } = usePeekControls();
  const rows = useMemo(() => answer.data?.items ?? [], [answer.data]);
  // THE BAR'S SCALE IS THE TOP TASK, which the engine's order puts first: a
  // task's tokens are its whole life's, so there is no window total for them
  // to be a share of, and a percentage would claim one.
  const top = rows.reduce((m, r) => Math.max(m, r.spend?.tokens ?? 0), 0);
  usePeekNeighbours(useMemo(() => rows.map(itemRef), [rows]));
  // THE ANSWER'S OWN DAYS, as every other card on Spend names its window —
  // never the control's, and never "since" a date with today as the tacit
  // other end, which was wrong for a window that ended in the past.
  const words = spend.data ? windowWords(spend.data, isRange(range.window)) : null;

  // A WINDOW THE ENGINE REFUSED HAS NO LIST, and says why. The list's
  // question starts where the window's answer does, so without that answer it
  // is never asked — and a grid handed no rows was a loading skeleton under
  // the refusal for as long as the page stayed open.
  const refused = spend.error ?? (answer.error === "unknown_query" ? answer.error : null);
  if (refused) {
    return (
      <>
        <PageActions>
          <TimeRangePicker range={range} ariaLabel="Window" />
        </PageActions>
        <QueryState
          error={refused}
          refusal={spend.error ? spend.refusal : answer.refusal}
          detail={spend.error ? spend.detail : undefined}
          loading={false}
        />
      </>
    );
  }
  return (
    <>
      <PageActions>
        <TimeRangePicker range={range} ariaLabel="Window" />
      </PageActions>
      <Card className="spend-card spend-tasks" padding="none">
        <div className="spend-card-head">
          <h2 className="spend-title">Most expensive tasks</h2>
          <span className="spend-caption">
            {words ? `${words} · ` : ""}tasks last changed in this window · each task&rsquo;s tokens
            over its whole life
          </span>
        </div>
        <QueryState
          error={answer.error}
          refusal={answer.refusal}
          detail={answer.detail}
          loading={false}
        >
          <DataGrid
            name="tasks"
            rows={answer.data ? rows : undefined}
            rowKey={(r) => r.id}
            // THE ENGINE ORDERED THE SET, by the column this list is about;
            // a head that re-sorted the fifty loaded rows would answer a
            // different question from the one asked.
            serverSorted
            // ON A PHONE, THE OVERVIEW CARD'S ROW: the key and the title with
            // what drove it under, the tokens at the end — no label repeated
            // over every task, which the labelled card stood four lines for.
            phoneRows="compact"
            flush
            rowHref={(r) => peekHref(itemRef(r))}
            onRowActivate={peekRow<WorkSummary>((r) => open(itemRef(r)))}
            empty={{
              title: "No task last changed in this window has spent tokens",
              hint: "A task appears here once an agent's turn is charged to it.",
            }}
            columns={[
              {
                key: "key",
                header: "Task",
                shrink: true,
                phoneLead: true,
                cell: (r) => <KeyCell value={r.key || r.id.slice(0, 8)} />,
              },
              {
                key: "title",
                header: "Title",
                floor: "12rem",
                phoneLead: true,
                cell: (r) => (
                  <span className="spend-task-main">
                    <span className="truncate">{r.title}</span>
                    {r.spend && <span className="spend-caption">{taskFacts(r.spend)}</span>}
                  </span>
                ),
              },
              {
                key: "tokens",
                header: "Tokens",
                align: "right",
                // A FIXED TRACK beside the figure, as By agent draws its
                // share: three columns stretched across a wide screen put the
                // figure seven hundred pixels from the task it belongs to,
                // with nothing between them to say how the rows compare.
                width: "14rem",
                phoneLead: true,
                cell: (r) => (
                  <span className="spend-task-spend">
                    <span className="spend-share-track" aria-hidden="true">
                      <span
                        className="spend-share-bar"
                        style={{
                          width: `${top > 0 ? ((r.spend?.tokens ?? 0) / top) * 100 : 0}%`,
                        }}
                      />
                    </span>
                    <TokenCell value={r.spend?.tokens} />
                  </span>
                ),
              },
            ]}
            footer={
              answer.data && answer.data.total_hint > rows.length ? (
                <span className="spend-caption">
                  The {rows.length} that spent the most, of {fmtExact(answer.data.total_hint)}
                  {answer.data.total_capped ? "+" : ""} last changed in this window
                </span>
              ) : undefined
            }
          />
        </QueryState>
      </Card>
    </>
  );
}
