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
import { Dash, MeterCell, TextCell, TokenCell } from "~/app/frame/cells.tsx";
import { peekHref, rowPeekHandler, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { Callout, Card, Skeleton } from "@crewlethq/ui";
import { DatabaseGlyph } from "@crewlethq/icons/glyphs";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtExact } from "~/lib/format.ts";

/**
 * What a headroom bar's colour says.
 *
 * THE THRESHOLDS THE `Meter` PRIMITIVE DERIVES for a meter whose full end means
 * "spent", restated because a CELL takes its tone rather than deriving one. A
 * bar that stayed accent right up to the refusal would be colour saying nothing
 * at the only moment a budget is worth looking at — and colour is the one thing
 * here that carries STATE.
 *
 * A max of zero never reaches it: the cell refuses a max it cannot measure
 * against and draws its own dash before a tone is ever asked for.
 */
function headroomTone(used: number, max: number): "accent" | "caution" | "critical" {
  const pct = (used / max) * 100;
  return pct >= 100 ? "critical" : pct >= 75 ? "caution" : "accent";
}

export function Budgets() {
  const budgets = useQuery("budgets", undefined, { pollMs: 30_000 });
  const { open: openPeek } = usePeekControls();
  // SORTED THE WAY THE TABLE OPENS — the grid applies `defaultSort` to whatever
  // it is handed, so seats published in the answer's own order would have `]`
  // walking a sequence nobody was shown. Sorted here to the same key, the
  // grid's sort is idempotent over them and the stepper matches the rows.
  const seats = useMemo(
    () => (budgets.data?.seats ?? []).slice().sort((a, b) => b.durable_used - a.durable_used),
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
          subtitle="the fleet's shared ledger, not this process's meter"
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
              defaultSort="-used"
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
              empty={{ title: "No per-seat budgets are configured" }}
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
                  key: "used",
                  header: "Durable used",
                  align: "right",
                  sortValue: (s) => s.durable_used,
                  // THE CELL, so a token count is spelled here the way it is
                  // spelled on the spend table and everywhere else — the two are
                  // read one after the other and a figure that changed shape
                  // between them would read as a different quantity.
                  cell: (s) => <TokenCell value={s.durable_used} />,
                },
                {
                  key: "live",
                  header: "This process",
                  align: "right",
                  // NULL IS NOT ZERO, and the two are the whole point of
                  // this column: the answer sends null for a seat this
                  // node holds no live meter for — a seat it has never
                  // run — and `fmtExact` drew a confident 0 for it, one
                  // column away from a durable total that says otherwise.
                  // Sorted to -1 so the unmetered seats group together at
                  // one end rather than among the genuine zeroes.
                  sortValue: (s) => s.live_used ?? -1,
                  // THE DASH IS SPELT OUT rather than left to the cell's own: a
                  // token cell handed null says "nothing recorded", and what is
                  // true here is the narrower fact that this NODE holds no meter
                  // — the fleet's own total is one column to the left.
                  cell: (s) =>
                    s.live_used === null || s.live_used === undefined ? (
                      <Dash title="this node holds no live meter for this seat" />
                    ) : (
                      <TokenCell value={s.live_used} />
                    ),
                },
                {
                  key: "max",
                  header: "Budget",
                  align: "right",
                  sortValue: (s) => s.max_tokens,
                  // NOT A DASH, and therefore not the cell's absent branch: no
                  // cap is a SETTING somebody chose, not a number that went
                  // unrecorded, and the word is the only thing that says so.
                  cell: (s) =>
                    s.max_tokens > 0 ? (
                      <TokenCell value={s.max_tokens} />
                    ) : (
                      <span className="muted">unlimited</span>
                    ),
                },
                {
                  key: "headroom",
                  header: "Headroom",
                  // 120px rather than 160: the cell's bar is a fixed 64px, and
                  // the rest was a gap the eye had to cross to reach the seat's
                  // own row again.
                  width: "120px",
                  // `MeterCell` REFUSES A MAX OF ZERO itself, with the one dash
                  // that is right here — there is nothing to measure an
                  // unlimited seat against, which is a different absence from a
                  // budget nobody recorded.
                  cell: (s) => (
                    <MeterCell
                      used={s.durable_used}
                      max={s.max_tokens}
                      tone={headroomTone(s.durable_used, s.max_tokens)}
                      // The column heading names it for a sighted reader; a
                      // screen reader lands on the bar alone, so it carries the
                      // seat and the reading it is drawing.
                      label={`${s.role}: ${fmtExact(s.durable_used)} of ${fmtExact(s.max_tokens)} tokens spent`}
                    />
                  ),
                },
              ]}
            />
          </QueryState>
        )}
      </Card>
    </>
  );
}
