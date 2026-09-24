/**
 * Budgets: the caps, the fleet's own counter behind them, and what is being
 * refused right now.
 *
 * # Its own screen, because it answers a different question from spend
 *
 * Spend is "what has this cost". A budget is "what will be REFUSED", and the
 * two are read at different moments by different people: a founder reads the
 * first at the end of a month and the second the instant a seat stops
 * working. Folded into one screen, the budget table sat under four charts and
 * a turn list — which is where it was.
 *
 * # The counter is the FLEET's, not this process's meter
 *
 * `durable: false` means the coordination store could not be READ, which is
 * not the same as the counter being zero. Rendering it as zero would tell a
 * person their company has spent nothing when what happened is that nobody
 * could ask.
 */

import { useMemo } from "react";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { QueryState } from "~/components/common.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { MeterCell, TextCell, TokenCell } from "~/app/frame/cells.tsx";

import { stateTone, windowOf } from "~/lib/budget.ts";
import type { BudgetWindow } from "~/protocol/types.ts";
import { peekHref, rowPeekHandler, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { Callout, Card, EmptyValue, Skeleton } from "@crewlethq/ui";
import { DatabaseGlyph } from "@crewlethq/icons/glyphs";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtExact } from "~/lib/format.ts";

/**
 * One seat's spend in one calendar window, and its ceiling where it has one.
 *
 * THE BAR'S COLOUR IS THE ENGINE'S `state`, never a fraction of ours: this
 * table used to restate the 75% the old meter primitive derived, beside a 90%
 * in the attention queue, so a seat could be amber here and fine there. And a
 * refusing window outranks its ratio for the reason the engine's state does —
 * a refused charge increments nothing, so the counter stops short of its
 * ceiling by the size of the round that would not fit.
 */
function WindowCell({ role, w }: { role: string; w: BudgetWindow | undefined }) {
  if (!w) return <EmptyValue label="Not stated" />;
  const words =
    w.limit === undefined
      ? `${role}: ${fmtExact(w.used)} tokens spent in ${w.window}, no ceiling`
      : w.refused_at
        ? `${role}: refusing charges since ${w.refused_at}, ${fmtExact(w.used)} of ${fmtExact(w.limit)} tokens spent in ${w.window}`
        : `${role}: ${fmtExact(w.used)} of ${fmtExact(w.limit)} tokens spent in ${w.window}`;
  return (
    <span className="row" style={{ gap: 8, justifyContent: "flex-end" }} title={words}>
      <TokenCell value={w.used} />
      {w.limit === undefined ? (
        // NOT A DASH: no ceiling is a SETTING somebody chose, not a number
        // that went unrecorded, and the words are the only thing that say so.
        <span className="muted">no ceiling</span>
      ) : (
        <MeterCell used={w.used} max={w.limit} tone={stateTone(w.state)} label={words} />
      )}
    </span>
  );
}

/** A seat's spend this month, the widest window the counter keeps, which is
 *  what the table opens sorted on. */
function monthUsed(windows: readonly BudgetWindow[]): number {
  return windowOf(windows, "month")?.used ?? 0;
}

export function Budgets() {
  const budgets = useQuery("budgets", undefined, { pollMs: 30_000 });
  const { open: openPeek } = usePeekControls();
  // SORTED THE WAY THE TABLE OPENS — the grid applies `defaultSort` to whatever
  // it is handed, so seats published in the answer's own order would have `]`
  // walking a sequence nobody was shown. Sorted here to the same key, the
  // grid's sort is idempotent over them and the stepper matches the rows.
  const seats = useMemo(
    () =>
      (budgets.data?.seats ?? [])
        .slice()
        .sort((a, b) => monthUsed(b.windows) - monthUsed(a.windows)),
    [budgets.data],
  );
  // WHAT `[` AND `]` WALK: the seats this table is showing. A column sort after
  // that is grid state, and a stepper that cannot see it walks the opening
  // order rather than claiming to follow a sequence it does not have.
  usePeekNeighbours(
    useMemo(() => seats.map((s) => ({ kind: "seat" as const, id: s.handle || s.role })), [seats]),
  );

  return (
    <>
      <PageNote>
        What a seat is allowed to spend, and what the fleet's own durable counter says it has. A
        budget that is exhausted does not slow a seat down — it REFUSES the call, and the seat falls
        through to whatever its chain names next.
      </PageNote>

      <Card padding="none">
        <Card.Header
          icon={<DatabaseGlyph size="sm" />}
          subtitle={`the fleet's shared counters, cut on the company clock${budgets.data?.timezone ? ` (${budgets.data.timezone})` : ""}`}
        >
          <Card.Title>Durable budget counters</Card.Title>
        </Card.Header>
        {budgets.loading && !budgets.data && (
          <Skeleton variant="text" rows={3} label="Loading the budget counters" />
        )}
        {budgets.data && budgets.data.durable === false ? (
          <Callout
            variant="neutral"
            icon={<DatabaseGlyph size="md" />}
            style={{ margin: "var(--space-3)" }}
          >
            The durable counter could not be READ — which is not the same as it being zero. It lives
            in the fleet's coordination store; this node could not reach it.
          </Callout>
        ) : (
          <QueryState error={budgets.error} loading={budgets.loading}>
            <DataGrid
              rows={seats}
              rowKey={(s) => s.agent_id || s.role}
              defaultSort="-month"
              // THE SEAT BESIDE ITS BUDGET. This table is read the instant a
              // seat stops working, and the next question — what is it, what was
              // it doing — is one the rail answers without losing the row that
              // raised it. ⌘-click still opens the seat's own page, and the rail
              // is built from the same reference, so the two can never name
              // different screens.
              rowHref={(s) => peekHref({ kind: "seat", id: s.handle || s.role })}
              onRowActivate={(s, e) => {
                const go = () => openPeek({ kind: "seat", id: s.handle || s.role });
                // THE GRID HANDS THIS BOTH EVENTS. `rowPeekHandler` is the
                // frame's one copy of which clicks mean "open elsewhere" and it
                // reads a mouse event; the `enter` chord carries no button at
                // all and is never one of them.
                if (!("button" in e)) {
                  go();
                  return;
                }
                rowPeekHandler(go)?.(e);
              }}
              empty={{ title: "No agent seats to count" }}
              columns={[
                {
                  key: "seat",
                  header: "Seat",
                  sortValue: (s) => s.role,
                  // NOT `SeatCell`, and not the seat chip this column used to
                  // draw: both are anchors, and this row is one now whose target
                  // is that same seat — a second link over the name would take
                  // the plain click the peek opens on.
                  cell: (s) => <TextCell icon="memory">{s.role}</TextCell>,
                },
                {
                  key: "day",
                  header: "Today",
                  align: "right",
                  sortValue: (s) => windowOf(s.windows, "day")?.used ?? 0,
                  cell: (s) => <WindowCell role={s.role} w={windowOf(s.windows, "day")} />,
                },
                {
                  key: "week",
                  header: "This week",
                  align: "right",
                  sortValue: (s) => windowOf(s.windows, "week")?.used ?? 0,
                  cell: (s) => <WindowCell role={s.role} w={windowOf(s.windows, "week")} />,
                },
                {
                  key: "month",
                  header: "This month",
                  align: "right",
                  sortValue: (s) => windowOf(s.windows, "month")?.used ?? 0,
                  cell: (s) => <WindowCell role={s.role} w={windowOf(s.windows, "month")} />,
                },
              ]}
            />
          </QueryState>
        )}
      </Card>
    </>
  );
}
