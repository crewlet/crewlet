/**
 * Spend › Budgets: every scope's day, week and month — what it has spent, the
 * ceiling it is held to, and whether the gate is refusing it — with each
 * ceiling editable in place by a reader holding `config:write`.
 *
 * # Its own screen, because it answers a different question from spend
 *
 * Spend is "what has this cost". A budget is "what will be REFUSED", and the
 * two are read at different moments by different people: a founder reads the
 * first at the end of a month and the second the instant a seat stops
 * working.
 *
 * # The company first, then its seats
 *
 * Everything a seat spends is the company's spend too, so a company ceiling
 * binds every seat at once and is the one read first: its three windows are
 * tiles, each with the date it resets on. The seats follow as one table with
 * a column per window, because a column is how a reader finds the one seat
 * near its ceiling among seven.
 *
 * # The state is the engine's
 *
 * Each bar is handed the window's `state` (`ok`, `near`, `refusing`) and draws
 * that: the engine calls a window near at `near_fraction` of its ceiling and
 * refusing once a charge does not fit, and there is no threshold here to
 * disagree with it (`docs/reference/dashboard-design.md`, "A budget bar is
 * coloured by the engine").
 *
 * # Raising a ceiling, and why there is no reset
 *
 * A window's `used` IS what it spent — there is nothing to reset. Its room
 * comes back when it turns over on the company clock, or now by raising the
 * ceiling (`components/budgetWrite.tsx`). The company's is a change to its
 * settings, checked against the whole company first and stored as a revision;
 * a seat's is its org chart runtime half, written as the seat's content and
 * arbitrated by the chart. Either is applied by every node in its own time,
 * and the figure says "applying" until this one has. The editor is drawn for
 * every reader and disabled with the reason for one without `config:write`.
 *
 * # The counter is the FLEET's, and unreadable is not zero
 *
 * `durable: false` means the coordination store could not be READ, which is
 * not the same as the counter being zero. Rendering it as zero would tell a
 * person their company has spent nothing when what happened is that nobody
 * could ask.
 */

import { useMemo } from "react";
import { Callout, Card, EmptyValue, Meter, Skeleton } from "@crewlethq/ui";
import { BuildingComplexGlyph, DatabaseGlyph, UsersGlyph } from "@crewlethq/icons/glyphs";
import { DataGrid, type GridColumn } from "~/app/frame/DataGrid.tsx";
import { ClockText, SeatLabel } from "~/app/frame/cells.tsx";
import { peekHref, rowPeekHandler, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { CeilingEditor, resetsWords } from "~/components/budgetWrite.tsx";
import { QueryState } from "~/components/common.tsx";
import { BUDGET_WINDOWS } from "~/contract/config.ts";
import { PERIOD_ADJECTIVE, windowOf } from "~/lib/budget.ts";
import type { CeilingScope, Period } from "~/lib/ceilings.ts";
import { companyDateLabel, dateFormatter, fmtCount, fmtExact, relTime } from "~/lib/format.ts";
import { dayLabelIn } from "~/lib/range.ts";
import { seatAddress, useSeatBadgeOf } from "~/lib/seats.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { useConfigWriteAccess } from "~/lib/useWriteAccess.ts";
import type { BudgetWindow, BudgetsAnswer } from "~/protocol/types.ts";

/** How often the counters are re-read: the gate's own report cadence is 15s,
 *  and a table read the instant a seat stops is worth one fresh answer a
 *  half-minute, not a poll that outruns what the fleet reports. */
const BUDGETS_POLL_MS = 30_000;

/** The column heading each window reads under. */
const HEADING: Readonly<Record<Period, string>> = {
  day: "Today",
  week: "This week",
  month: "This month",
};

const COMPANY: CeilingScope = { kind: "company" };

/** One seat's row of the budgets answer. */
type BudgetSeat = BudgetsAnswer["seats"][number];

/** A window's state in words, for the reader who is told the bar rather than shown it. */
function stateWords(w: BudgetWindow): string {
  return w.state === "refusing"
    ? "refusing charges"
    : w.state === "near"
      ? "nearly spent"
      : "within the ceiling";
}

/** The label a window's span carries: `Sep 29`, `W40`, `September`. */
function windowName(w: BudgetWindow, zone: string): string {
  if (w.period === "day") return companyDateLabel(w.window);
  if (w.period === "week") return `Week ${w.window.slice(-2).replace(/^0/, "")}`;
  const starts = Date.parse(w.starts_at);
  return Number.isFinite(starts)
    ? dateFormatter(undefined, { month: "long", timeZone: zone || "UTC" }).format(starts)
    : w.window;
}

/**
 * "Refusing charges since 12m ago" — the gate's own stamp, where it has one.
 *
 * THE CLOCK IS READ HERE, in the one line that shows it (`ClockText`), never
 * handed down from the screen: a screen holding the second rendered every
 * tile and every row once a second to move these few words.
 */
function RefusedLine({ w }: { w: BudgetWindow }) {
  if (w.state !== "refusing") return null;
  const at = w.refused_at;
  return (
    <span className="budget-refused t-caption">
      {at ? (
        <>
          Refusing charges since <ClockText read={(now) => relTime(at, now)} />
        </>
      ) : (
        "No further charge fits"
      )}
    </span>
  );
}

/**
 * One of the company's three windows: what it spent, the bar against its
 * ceiling, the ceiling (editable), and when it turns over.
 */
function CompanyWindow({
  w,
  period,
  zone,
  onApplied,
}: {
  w: BudgetWindow | undefined;
  period: Period;
  zone: string;
  onApplied: () => void;
}) {
  const capped = w?.limit !== undefined;
  return (
    <section className="budget-tile" aria-label={`The company ${HEADING[period].toLowerCase()}`}>
      <div className="budget-tile-head">
        <span className="budget-tile-title">{HEADING[period]}</span>
        {w && <span className="t-caption muted">{windowName(w, zone)}</span>}
      </div>
      {w ? (
        <>
          <div className="budget-tile-figure">
            <span className="budget-figure t-num">{fmtCount(w.used)}</span>
            <span className="t-caption muted">tokens</span>
          </div>
          {capped ? (
            <Meter
              value={w.used}
              max={w.limit!}
              state={w.state}
              hideLabel
              label={`The company's ${PERIOD_ADJECTIVE[period]} token budget`}
              valueText={`${fmtExact(w.used)} of ${fmtExact(w.limit)} tokens, ${stateWords(w)}`}
            />
          ) : (
            // NO BAR WITHOUT A CEILING: a bar needs something to be a
            // fraction of, and an empty track reads as "nothing spent".
            <div className="budget-track-none" aria-hidden="true" />
          )}
          <div className="budget-tile-foot">
            <CeilingEditor
              scope={COMPANY}
              period={period}
              window={w}
              zone={zone}
              onApplied={onApplied}
            />
            <span className="t-caption muted">{resetsWords(w, zone)}</span>
          </div>
          <RefusedLine w={w} />
        </>
      ) : (
        <EmptyValue label="Not stated" />
      )}
    </section>
  );
}

/** One seat's window in its column: spent, the ceiling beside it, the bar under. */
function SeatWindow({
  seat,
  name,
  w,
  period,
  zone,
  onApplied,
}: {
  seat: string;
  name: string;
  w: BudgetWindow | undefined;
  period: Period;
  zone: string;
  onApplied: () => void;
}) {
  if (!w) return <EmptyValue label="Not stated" />;
  const scope: CeilingScope = { kind: "seat", handle: seat, name };
  return (
    <div className="budget-cell">
      <div className="budget-cell-line">
        <span className="t-num budget-cell-used" title={`${fmtExact(w.used)} tokens`}>
          {fmtCount(w.used)}
        </span>
        <CeilingEditor scope={scope} period={period} window={w} zone={zone} onApplied={onApplied} />
      </div>
      {w.limit !== undefined && (
        <Meter
          size="compact"
          value={w.used}
          max={w.limit}
          state={w.state}
          hideLabel
          label={`${name}'s ${PERIOD_ADJECTIVE[period]} token budget`}
          valueText={`${fmtExact(w.used)} of ${fmtExact(w.limit)} tokens, ${stateWords(w)}`}
        />
      )}
      <RefusedLine w={w} />
    </div>
  );
}

/** When each window turns over, once for every seat: they share the company clock. */
function resetLine(answer: BudgetsAnswer): string {
  const zone = answer.timezone;
  const parts = BUDGET_WINDOWS.flatMap(({ period }) => {
    const w = windowOf(answer.org.windows, period);
    if (!w) return [];
    const at = Date.parse(w.resets_at);
    if (!Number.isFinite(at)) return [];
    return period === "day"
      ? ["today at midnight"]
      : [`the ${period} on ${companyDateLabel(dayLabelIn(at, zone))}`];
  });
  return parts.length ? `Resets ${parts.join(", ")}` : "";
}

export function Budgets() {
  const seatBadge = useSeatBadgeOf();
  const budgets = useQuery("budgets", undefined, { pollMs: BUDGETS_POLL_MS });
  const access = useConfigWriteAccess();
  const { open: openPeek } = usePeekControls();
  const answer = budgets.data;
  const zone = answer?.timezone ?? "";
  const refetch = budgets.refetch;

  // SORTED THE WAY THE TABLE OPENS — the grid applies `defaultSort` to whatever
  // it is handed, so seats published in the answer's own order would have `]`
  // walking a sequence nobody was shown.
  const seats = useMemo(
    () =>
      (answer?.seats ?? [])
        .slice()
        .sort(
          (a, b) =>
            (windowOf(b.windows, "month")?.used ?? 0) - (windowOf(a.windows, "month")?.used ?? 0),
        ),
    [answer],
  );
  // EACH SEAT BY ITS ADDRESS (`seatAddress`): its handle, or for a row the
  // chart no longer holds its agent id — never its name, which opened
  // whichever seat of that name came first.
  usePeekNeighbours(
    useMemo(() => seats.map((s) => ({ kind: "seat" as const, id: seatAddress(s) })), [seats]),
  );

  const nearPct = answer ? Math.round(answer.near_fraction * 100) : 90;

  // THE COLUMNS HOLD STILL until what they read moves — the chart's names, the
  // company clock, the re-read a save asks for: every row is memoised on this
  // list, and one built inline drew every seat on every render.
  const columns = useMemo<GridColumn<BudgetSeat>[]>(() => {
    const seatWindow = (s: BudgetSeat, period: Period) => (
      <SeatWindow
        seat={s.handle}
        name={seatBadge(s.handle || s.role).name}
        w={windowOf(s.windows, period)}
        period={period}
        zone={zone}
        onApplied={refetch}
      />
    );
    return [
      {
        key: "seat",
        header: "Seat",
        sortValue: (s) => s.role,
        cell: (s) => <SeatLabel {...seatBadge(s.handle || s.role)} />,
      },
      // ONE COLUMN PER WINDOW, spelled out: each heading is a word
      // the phone's card layout reads beside its value.
      {
        key: "day",
        header: "Today",
        sortValue: (s) => windowOf(s.windows, "day")?.used ?? 0,
        cell: (s) => seatWindow(s, "day"),
      },
      {
        key: "week",
        header: "This week",
        sortValue: (s) => windowOf(s.windows, "week")?.used ?? 0,
        cell: (s) => seatWindow(s, "week"),
      },
      {
        key: "month",
        header: "This month",
        sortValue: (s) => windowOf(s.windows, "month")?.used ?? 0,
        cell: (s) => seatWindow(s, "month"),
      },
    ];
  }, [seatBadge, zone, refetch]);

  return (
    <>
      <PageNote>
        What each window may spend on the company clock{zone ? ` (${zone})` : ""}, and what it has.
        The engine calls a window near at {nearPct}% of its ceiling and refuses a charge that does
        not fit — a refused seat stops until the window resets on its own, or until its ceiling is
        raised.
      </PageNote>

      {/* THE REASON, ONCE, where the pencils are all disabled for the same
          one: every control still says it on its own, but a reader scanning
          a table of dimmed pencils should not have to hover one to learn
          why. Not while the viewer is still being asked. */}
      {!access.can && access.block !== "loading" && (
        <p className="budget-readonly t-caption">{access.reason}</p>
      )}

      {budgets.loading && !answer && (
        <Skeleton variant="text" rows={4} label="Loading the budget counters" />
      )}
      {!answer && (
        <QueryState
          error={budgets.error}
          refusal={budgets.refusal}
          detail={budgets.detail ?? undefined}
          loading={budgets.loading}
        />
      )}

      {answer && answer.durable === false && (
        <Callout variant="neutral" icon={<DatabaseGlyph size="md" />}>
          The durable counter could not be READ — which is not the same as it being zero. It lives
          in the fleet's coordination store; this node could not reach it.
        </Callout>
      )}

      {answer && answer.durable && (
        <div className="budgets">
          <Card padding="none">
            <Card.Header
              icon={<BuildingComplexGlyph size="sm" />}
              subtitle="Every seat's spend counts here too, so these bind every seat at once"
            >
              <Card.Title>The company</Card.Title>
            </Card.Header>
            <div className="budget-tiles">
              {BUDGET_WINDOWS.map(({ period }) => (
                <CompanyWindow
                  key={period}
                  period={period}
                  w={windowOf(answer.org.windows, period)}
                  zone={zone}
                  onApplied={refetch}
                />
              ))}
            </div>
          </Card>

          <Card padding="none">
            <Card.Header icon={<UsersGlyph size="sm" />} subtitle={resetLine(answer)}>
              <Card.Title>Agent seats</Card.Title>
            </Card.Header>
            <DataGrid
              rows={seats}
              rowKey={(s) => s.agent_id || s.role}
              defaultSort="-month"
              // THE SEAT BESIDE ITS BUDGET: the rail answers "what is it, what
              // was it doing" without losing the row that raised it.
              rowHref={(s) => peekHref({ kind: "seat", id: seatAddress(s) })}
              onRowActivate={(s, e) => {
                const go = () => openPeek({ kind: "seat", id: seatAddress(s) });
                if (!("button" in e)) {
                  go();
                  return;
                }
                rowPeekHandler(go)?.(e);
              }}
              empty={{ title: "No agent seats to count" }}
              columns={columns}
            />
          </Card>
        </div>
      )}
    </>
  );
}
