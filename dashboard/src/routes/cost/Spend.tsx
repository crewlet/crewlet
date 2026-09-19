/**
 * Spend and budgets.
 *
 * Two different facts share this screen and the previous one let them blur:
 *
 *  - the **spend rollup** is a WINDOW (24 hours by default, up to 30 days) over
 *    what the company's model calls consumed;
 *  - a **meter** is PROCESS-LIFETIME — it resets when the engine restarts.
 *
 * They are never comparable, and every number here says which it is.
 *
 * # The unit is TOKENS, and there is no money on this screen
 *
 * The engine records a price where one is reported — a subscription coding CLI
 * quotes `total_cost_usd` and nothing else does — and that figure is still on
 * the wire and still in the store. It is simply not RENDERED: a currency shown
 * for the small minority of calls that quote one, beside a token count covering
 * all of them, reads as the company's spend and is a fraction of it. Tokens are
 * the one unit every call here is measured in, so tokens are what this says.
 *
 * Nothing about that is irreversible: the field is untouched, so putting a
 * price back is a rendering change rather than a migration.
 */

import { useMemo } from "react";
import { useParam } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import {
  BarList,
  Card,
  dataColor,
  EmptyValue,
  Legend,
  Meter,
  StackedBar,
  StatCard,
  StatGroup,
  Tag,
} from "@crewlethq/ui";
import {
  ArrowForwardGlyph,
  LayersGlyph,
  MemoryGlyph,
  RefreshGlyph,
  ScheduleGlyph,
  TargetGlyph,
  TimelineGlyph,
  GroupGlyph,
} from "@crewlethq/icons/glyphs";
// OURS, AND DELIBERATELY. `SegmentedControl` welds keyboard ACTIVATION to its
// `semantics` — `radio` commits the option the arrows land on — and both
// strips here drive a `useParam` that re-runs `token_series`. Arrowing across
// the six split dimensions under that control is six queries nobody asked for
// and six history entries to press Back through. See the report.
import { Segmented } from "~/ui/primitives.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { DateCell, KeyCell, NumberCell, TextCell, TokenCell } from "~/app/frame/cells.tsx";
import { peekHref, peekRow, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import type { AgentSpendRow, TurnSpendRow } from "~/protocol/types.ts";
// OURS, AND THERE IS NO PEER FOR EITHER. `Charts` exports a line `TimeSeries`
// and a `StackedBar`, and this axis is neither: it is a column per bucket,
// STACKED into bands, with the previous period drawn behind it as a ghost.
// `phaseColor` has no peer either — uilet publishes `--color-phase-*` but no
// function that picks one. See the report.
import { StackedTimeSeries, phaseColor } from "~/ui/charts.tsx";
import { bandsOf, columnsOf, ghostHeights, unbandedTokens } from "~/lib/spend.ts";
import { useTimeRange, windowLabel } from "~/lib/range.ts";
import type { Offer, TimeRange } from "~/lib/range.ts";
import { TimeRangePicker } from "~/ui/TimeRange.tsx";
import { useOrgBudget, useTokens } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { fmtCount, fmtDate, fmtDateTime, fmtExact, fmtPct, relTime, tsKey } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";

/**
 * Which dimension the time axis is split on.
 *
 * The engine's own closed set minus nothing: a value it does not know is
 * refused naming what it accepts, so this list and `tokens.Groups` have to
 * agree — and they are checked against each other by the engine's own gate
 * over this file.
 */
const GROUPS = [
  { value: "phase", label: "Phase" },
  { value: "model", label: "Model" },
  { value: "seat", label: "Seat" },
  { value: "unit", label: "Unit" },
  { value: "worker", label: "Worker" },
  { value: "turn", label: "Turn" },
] as const;

/**
 * WHICH WINDOWS THIS SCREEN HAS.
 *
 * Days and up. A quarter of hourly bars is 2,160 columns and a spend chart has
 * nothing useful to say about fifteen minutes — one model call lands in one
 * bucket and the rest of the axis is empty. The live strip offers the short
 * end of the same vocabulary, so `1d` means the same thing on both.
 *
 * Declared ONCE and read by both the hook and the control, so the set the URL
 * is checked against cannot differ from the set a reader can see.
 */
const SPEND_OFFER: Offer = {
  ranges: ["1d", "7d", "30d", "90d"],
  custom: true,
  fallback: "1d",
  // THE BUCKETS `token_series` ACCEPTS, which is two: a minute bucket over a
  // week is ten thousand points nobody can read, and the engine refuses a
  // third value rather than guessing which branch its switch should end on.
  buckets: ["hour", "day"],
};

/**
 * The spend, over time, split into bands.
 *
 * THE ONE THING THE BREAKDOWN CANNOT SAY. Every row of `tokens` is a sum over
 * the whole window, so a runaway loop, a spike and a quiet weekend all look
 * like the same number. The bucketing is the engine's — the browser holds at
 * most the live window's records, so an axis folded here would be right for a
 * day and absent for every other range.
 */
function SpendOverTime({ range }: { range: TimeRange }) {
  const [group, setGroup] = useParam("group", "phase", "section");
  const [compare, setCompare] = useParam("compare", "", "section");
  const { bucket, since, until } = range;

  const params = { group, bucket, since, until };
  const series = useQuery("token_series", params);
  // The previous period is a SECOND query with the same shape, shifted by the
  // engine: subtracting here would be a local-time subtraction, and across a
  // DST boundary the two windows would be different lengths — a change the
  // chart would report and nobody made.
  const prior = useQuery(
    "token_series",
    { ...params, previous: true },
    { enabled: compare === "previous" },
  );

  const data = series.data;
  const bands = useMemo(() => bandsOf(data, group), [data, group]);
  const columns = useMemo(() => columnsOf(data, bands), [data, bands]);
  const ghost = useMemo(() => ghostHeights(prior.data), [prior.data]);
  const unbanded = unbandedTokens(data);

  return (
    <Card>
      <Card.Header
        icon={<TimelineGlyph size="sm" />}
        subtitle={bucket === "day" ? "one column per day" : "one column per hour"}
      >
        <Card.Title>Over time</Card.Title>
      </Card.Header>
      <QueryState
        error={series.error}
        loading={series.loading}
        empty={
          data && data.totals.calls === 0
            ? {
                title: "No model calls in this window",
                // THE SECOND SENTENCE, which is the whole reason `QueryState`
                // exists: this empty list and a node whose store could not be
                // read are the same headline and completely different
                // problems. This was the last caller in the tree with only a
                // headline, and the prop is required now.
                hint: "Nothing ran, or nothing ran that reports tokens. Widen the window, or check that the seats you expect are running.",
              }
            : undefined
        }
      >
        {data && (
          <div className="col gap-3">
            {/* THE CONTROLS ARE IN THE BODY, not the panel header. Six
                dimensions and a comparison toggle do not fit beside a title
                at a laptop width — the header truncates rather than wraps,
                so the title read "Over ti…" and the last dimension was
                clipped off the right edge. */}
            <div className="row wrap gap-2">
              <Segmented
                ariaLabel="Split by"
                value={group}
                onChange={setGroup}
                options={GROUPS.map((g) => ({ value: g.value, label: g.label }))}
              />
              <span className="spacer" />
              <Segmented
                ariaLabel="Compare"
                value={compare}
                onChange={setCompare}
                options={[
                  { value: "", label: "This period" },
                  { value: "previous", label: "vs previous" },
                ]}
              />
            </div>
            <StackedTimeSeries
              buckets={columns}
              bands={bands}
              ghost={compare === "previous" ? ghost : undefined}
              format={(n) => `${fmtCount(n)} tokens`}
              label={(at) => (bucket === "day" ? fmtDate(at) : fmtDateTime(at))}
            />
            <div className="row wrap gap-2">
              <span className="t-caption">{fmtDateTime(data.since)}</span>
              <span className="spacer" />
              <span className="t-caption">{fmtDateTime(data.until)}</span>
            </div>
            <Legend
              items={bands.map((b) => ({ id: b.key || "other", label: b.label, color: b.color }))}
            />
            {unbanded > 0 && (
              // THE GAP IS SAID OUT LOUD. Grouping by worker leaves out every
              // phase that is not a worker's, and a chart whose bands sum to
              // less than the window's total otherwise reads as spend that
              // went missing.
              <p className="t-caption">
                {fmtCount(unbanded)} tokens in this window fall under no {group} and are not in the
                bands above — the window's own total is {fmtExact(data.totals.total_tokens)}.
              </p>
            )}
            {data.totals.priced_calls > 0 && (
              // WHAT THE CALLS COST IS NOT SHOWN, deliberately — see the note
              // at the head of this file. What survives is the fact the price
              // was standing in for: how much of the window ANY figure here
              // can account for, since only a subscription coding CLI reports
              // per-call detail and the rest quote none.
              <p className="t-caption">
                {fmtExact(data.totals.priced_calls)} of {fmtExact(data.totals.calls)} calls came
                back with their own accounting — only a subscription coding CLI reports it, so the
                rest are counted by tokens alone.
              </p>
            )}
          </div>
        )}
      </QueryState>
    </Card>
  );
}

export function Spend() {
  const pushed = useTokens();
  const { open: openPeek } = usePeekControls();
  const orgBudget = useOrgBudget();
  const now = useNow();
  const range = useTimeRange(now, SPEND_OFFER);

  // The pushed rollup covers the live window. Any other window is a query,
  // and while it loads the pushed one stays on screen rather than blanking.
  //
  // THE SAME TWO INSTANTS THE CHART BELOW ASKS FOR. The breakdown used to take
  // a day count, which can only name a window ending now — so a reader who
  // scrubbed to a week in March got a chart over March with figures above it
  // from this afternoon, two facts on one screen that cannot be compared.
  const live = range.window === "1d";
  const asked = useQuery(
    "tokens",
    { since: range.since, until: range.until, recent_turns: 100 },
    { enabled: !live },
  );
  // AND A FAILED READ IS NOT A WINDOW'S ANSWER. The fallback above covers the
  // moment BEFORE the first answer arrives; past a refusal there is no answer
  // coming, and the live rollup left standing under the chosen window's badge
  // is the March-chart-with-this-afternoon's-figures this query was built to
  // stop — reintroduced on the one path nobody looks at. A 90-day scan is
  // exactly what times out, and `internal/api/queries` returns the store's own
  // error unchanged. Nothing is invented in its place: every consumer below
  // already draws an em dash or its own empty state for an absent rollup, and
  // the refusal above the tiles says which of the two this is.
  //
  // The last GOOD answer still wins where there is one — it is an answer for
  // THIS window, and a reconnect that failed is no reason to throw it away.
  const tokens = live ? pushed : (asked.data ?? (asked.error ? null : pushed));

  // SORTED THE WAY EACH TABLE OPENS, which is what makes the stepper below
  // honest: the grid applies its own `defaultSort` to whatever it is handed, so
  // rows published in the answer's order would have `]` walking a sequence the
  // table never showed. Sorted here to the same key, the grid's sort is
  // idempotent over them and the two agree — until the reader sorts a column,
  // which is grid state and leaves the stepper on the opening order rather than
  // on a guess about it.
  const seats = useMemo(
    () => (tokens?.by_agent ?? []).slice().sort((a, b) => b.total_tokens - a.total_tokens),
    [tokens],
  );
  const turns = useMemo(
    () => (tokens?.by_turn ?? []).slice().sort((a, b) => tsKey(b.started_at) - tsKey(a.started_at)),
    [tokens],
  );
  // WHAT `[` AND `]` WALK — BOTH tables, in the order this screen draws them.
  //
  // There is ONE publisher per screen and the last caller owns the stepper, so
  // two `usePeekNeighbours` calls here would not give each grid a stepper of
  // its own: they would race, and whichever list re-rendered last would
  // silently take the other's. Concatenated, stepping past the last seat
  // reaches the first turn — a walk down the screen, which is the one order
  // both grids can agree on and the order the reader actually sees.
  usePeekNeighbours(
    useMemo(
      () => [
        ...seats.map((a) => ({ kind: "seat" as const, id: a.handle || a.role })),
        ...turns.map((t) => ({ kind: "turn" as const, id: t.turn_id })),
      ],
      [seats, turns],
    ),
  );

  const phase = useMemo(
    () =>
      (tokens?.by_phase ?? [])
        .map((p) => ({
          id: p.phase,
          label: p.phase,
          value: p.total_tokens,
          display: fmtCount(p.total_tokens),
          color: phaseColor(p.phase),
          sub: `${p.calls.toLocaleString()} calls · ${fmtCount(Math.round(p.total_tokens / Math.max(1, p.calls)))} per call`,
        }))
        .sort((a, b) => b.value - a.value),
    [tokens],
  );

  const models = useMemo(
    () =>
      (tokens?.by_model ?? [])
        .slice()
        .sort((a, b) => b.total_tokens - a.total_tokens)
        .map((m, i) => ({
          id: m.model,
          label: m.model,
          value: m.total_tokens,
          display: fmtCount(m.total_tokens),
          // uilet publishes the data ramp, so `vizColor` goes with it.
          color: dataColor(i),
          sub: `${m.calls.toLocaleString()} calls`,
        })),
    [tokens],
  );

  const phaseKeys = useMemo(
    () => [...new Set((tokens?.by_phase ?? []).map((p) => p.phase))],
    [tokens],
  );

  const org = orgBudget?.org;

  return (
    <>
      <PageActions>
        <Tag appearance="outline">{windowLabel(range.window)}</Tag>
        <TimeRangePicker range={range} ariaLabel="Window" />
      </PageActions>
      <PageNote>
        How many tokens the company&rsquo;s model calls have spent, and how much headroom the budget
        gate has left. Every figure here is in tokens.
      </PageNote>

      {/* THE REFUSAL, ABOVE THE FIGURES IT IS ABOUT. `SpendOverTime` routes its
          own series error, and the breakdown's had no reader at all — so a
          window that could not be read drew a chart's error banner over stat
          tiles, bar lists and two tables that were all still rendering, with
          nothing saying where their numbers came from. */}
      {!live && asked.error && <QueryState error={asked.error} loading={false} />}

      {/* The flush Panel is gone: StatGroup draws that surface itself. */}
      <StatGroup columns={4}>
        {/* A COIN LABELS MONEY, and this counts tokens — the only glyph on
            the screen that claimed a unit the figure beside it is not in. */}
        <StatCard
          icon={<LayersGlyph size="xs" />}
          label="Tokens"
          value={
            tokens ? fmtCount(tokens.totals.total_tokens) : <EmptyValue label="Nothing recorded" />
          }
          sub={tokens ? `${fmtExact(tokens.totals.total_tokens)} exactly` : "nothing recorded"}
        />
        <StatCard
          icon={<MemoryGlyph size="xs" />}
          label="Model calls"
          value={tokens ? fmtCount(tokens.totals.calls) : <EmptyValue label="Nothing recorded" />}
          sub={
            tokens && tokens.totals.calls
              ? `${fmtCount(Math.round(tokens.totals.total_tokens / tokens.totals.calls))} tokens per call`
              : ""
          }
        />
        <StatCard
          icon={<ArrowForwardGlyph size="xs" />}
          label="Input / output"
          value={
            tokens ? (
              `${fmtCount(tokens.totals.input_tokens)} / ${fmtCount(tokens.totals.output_tokens)}`
            ) : (
              <EmptyValue label="Not counted yet" />
            )
          }
          sub="input includes any cached prefix, as the provider reports it"
        />
        <StatCard
          icon={<ScheduleGlyph size="xs" />}
          label="Counted through"
          value={
            tokens?.aggregated_through ? (
              relTime(tokens.aggregated_through, now)
            ) : (
              <EmptyValue label="Nothing has been counted yet" />
            )
          }
          sub={
            tokens?.aggregated_through
              ? fmtDateTime(tokens.aggregated_through)
              : "no high-water mark yet"
          }
        />
      </StatGroup>

      <SpendOverTime range={range} />

      {org && org.max > 0 && (
        <Card>
          <Card.Header
            icon={<TargetGlyph size="sm" />}
            subtitle="process-lifetime — not the window above"
            actions={org.used >= org.max ? <Tag variant="danger">spent</Tag> : undefined}
          >
            <Card.Title>Company budget meter</Card.Title>
          </Card.Header>
          {/* THEIR `label` IS THE ACCESSIBLE NAME, tied to the bar, so the
              separate `ariaLabel` ours needed is gone — and with it the reason
              the visible legend could not be the name. `valueText` carries the
              true figures past the clamp, which is what ours put in
              `aria-valuetext`. `fullMeans="spent"` is gone because that is the
              only reading theirs has; see the report for what that costs the
              screens measuring progress. */}
          <Meter
            value={org.used}
            max={org.max}
            label="Company token budget"
            valueText={`${fmtExact(org.used)} of ${fmtExact(org.max)} tokens`}
            hint={`${fmtPct(org.used, org.max, 1)} used · ${fmtExact(org.used)} / ${fmtExact(org.max)}`}
            tone={org.used >= org.max ? "danger" : undefined}
          />
          {org.used >= org.max && (
            // AT THE CAP IS WHAT THE SHARED COUNTER CAN SAY. It is
            // sufficient and not necessary — a refused charge increments
            // nothing, so a company that is refusing can sit just short of
            // its cap — which is why the attention queue also warns at 90%.
            <p className="t-caption" style={{ marginTop: "var(--space-2)" }}>
              The meter is at its cap, so no further charge can be accepted and turns are being
              declined at the gate.
            </p>
          )}
        </Card>
      )}

      <div className="grid grid-auto-lg">
        <Card>
          <Card.Header icon={<LayersGlyph size="sm" />} subtitle="where the tokens actually go">
            <Card.Title>By phase</Card.Title>
          </Card.Header>
          <div className="col gap-3">
            <BarList data={phase} emptyLabel="No model calls in this window." />
            {phase.length > 0 && (
              <Legend items={phase.map((p) => ({ id: p.id, label: p.label, color: p.color }))} />
            )}
          </div>
        </Card>
        <Card>
          <Card.Header
            icon={<MemoryGlyph size="sm" />}
            subtitle="from each completion's own reported model"
          >
            <Card.Title>By model</Card.Title>
          </Card.Header>
          <div className="col gap-3">
            <BarList data={models} limit={8} emptyLabel="No model calls in this window." />
            <p className="t-caption">
              Built from what each completion reported, never from a provider's configured name — a
              fallback chain serves several models under one key.
            </p>
          </div>
        </Card>
      </div>

      {(tokens?.by_worker ?? []).length > 0 && (
        <Card>
          <Card.Header icon={<RefreshGlyph size="sm" />} subtitle="spend outside any seat's turn">
            <Card.Title>Background workers</Card.Title>
          </Card.Header>
          <BarList
            data={(tokens?.by_worker ?? []).map((w, i) => ({
              id: w.worker,
              label: w.worker,
              value: w.total_tokens,
              display: fmtCount(w.total_tokens),
              color: dataColor(i),
              sub: `${w.calls} calls`,
            }))}
          />
        </Card>
      )}

      <Card padding="none">
        <Card.Header icon={<GroupGlyph size="sm" />} count={seats.length}>
          <Card.Title>By seat</Card.Title>
        </Card.Header>
        <DataGrid
          rows={seats}
          rowKey={(a) => a.agent_id || a.role}
          defaultSort="-total"
          // THE SEAT BESIDE THE TABLE, not instead of it. A plain click used to
          // navigate to the seat's own cost tab, which threw away the ranking
          // the reader was in the middle of reading — and that tab is a place
          // the rail's `Open ↗` cannot name, so the row's link and the way out
          // of the panel it opens would have pointed at two different screens.
          // Both are built from one reference now, and ⌘-click still goes to
          // the seat's page.
          rowHref={(a) => peekHref({ kind: "seat", id: a.handle || a.role })}
          onRowActivate={peekRow<AgentSpendRow>((a) =>
            openPeek({ kind: "seat", id: a.handle || a.role }),
          )}
          empty={{ title: "No seat has spent tokens in this window" }}
          columns={[
            {
              key: "seat",
              header: "Seat",
              sortValue: (a) => a.role,
              // NOT `SeatCell`, and not the seat chip this column used to draw:
              // both are anchors, and this row is one now whose target is that
              // very seat — a second link over the name would swallow the plain
              // click the peek opens on and send the reader to the page the
              // rail was built to save them from.
              cell: (a) => <TextCell icon="memory">{a.role}</TextCell>,
            },
            {
              key: "total",
              header: "Tokens",
              align: "right",
              sortValue: (a) => a.total_tokens,
              // THE CELL, so a token count is spelled here the way it is spelled
              // everywhere else in the product. `fmtCount`'s threshold is 10,000
              // — a four-digit count stays exact and only what nobody reads digit
              // by digit is abbreviated — and the window's exact total is in the
              // stat row at the top of this screen.
              cell: (a) => <TokenCell value={a.total_tokens} />,
            },
            {
              key: "share",
              header: "Share",
              width: "180px",
              // NOT `MeterCell`: this is a BREAKDOWN across every phase, not one
              // fraction of one whole, and a single bar cannot say which phase
              // the tokens went to.
              cell: (a) => (
                <StackedBar
                  segments={phaseKeys.map((p) => ({
                    id: p,
                    label: p,
                    value: a.by_phase?.[p]?.total_tokens ?? 0,
                    color: phaseColor(p),
                  }))}
                />
              ),
            },
            {
              key: "calls",
              header: "Calls",
              align: "right",
              sortValue: (a) => a.calls,
              cell: (a) => <NumberCell value={a.calls} />,
            },
            {
              key: "avg",
              header: "Per call",
              align: "right",
              sortValue: (a) => (a.calls ? a.total_tokens / a.calls : 0),
              // A DASH THAT SAYS WHICH ABSENCE THIS IS. A seat with no calls has
              // no average rather than an average of nothing, and the cell's own
              // "nothing recorded" would be the wrong sentence for a row whose
              // tokens are right beside it.
              cell: (a) =>
                a.calls > 0 ? (
                  <TokenCell value={Math.round(a.total_tokens / a.calls)} />
                ) : (
                  <EmptyValue label="No calls to average over" />
                ),
            },
          ]}
        />
        {phaseKeys.length > 0 && (
          <Card.Footer variant="meta">
            <Legend items={phaseKeys.map((p) => ({ id: p, label: p, color: phaseColor(p) }))} />
          </Card.Footer>
        )}
      </Card>

      <Card padding="none">
        <Card.Header icon={<LayersGlyph size="sm" />} count={turns.length}>
          <Card.Title>Recent turns</Card.Title>
        </Card.Header>
        <DataGrid
          name="turns"
          rows={turns}
          rowKey={(t) => t.turn_id}
          defaultSort="-started"
          // THE TURN BESIDE THE SPEND, which is the whole reason a reader scans
          // this table: the expensive row is found by comparing it against the
          // ones above and below it, and navigating away to read one turn loses
          // the comparison that made it interesting.
          rowHref={(t) => peekHref({ kind: "turn", id: t.turn_id })}
          onRowActivate={peekRow<TurnSpendRow>((t) => openPeek({ kind: "turn", id: t.turn_id }))}
          empty={{ title: "No turns in this window" }}
          columns={[
            {
              key: "started",
              header: "Started",
              shrink: true,
              sortValue: (t) => tsKey(t.started_at),
              // RELATIVE IN THE CELL, absolute in its title — the same trade the
              // turn log makes for the same column. A spend table is scanned for
              // what ran recently, and a wall-clock stamp is what somebody wants
              // only once they have found the row.
              cell: (t) => <DateCell at={t.started_at} now={now} />,
            },
            {
              key: "seat",
              header: "Seat",
              sortValue: (t) => t.role,
              // NOT `SeatCell` or the chip this drew: both are anchors and every
              // row here is one. The seat is a link again in the turn's own peek.
              cell: (t) => <TextCell icon="memory">{t.role}</TextCell>,
            },
            {
              key: "total",
              header: "Tokens",
              align: "right",
              sortValue: (t) => t.total_tokens,
              cell: (t) => <TokenCell value={t.total_tokens} />,
            },
            {
              key: "calls",
              header: "Calls",
              align: "right",
              sortValue: (t) => t.calls,
              // A BARE `{t.calls}` RENDERED AN ABSENT COUNT AS NOTHING AT ALL —
              // an empty cell reads as a column that does not apply to this row,
              // where the cell says "nothing recorded" and still spells a real
              // zero as `0`.
              cell: (t) => <NumberCell value={t.calls} />,
            },
            {
              key: "turn",
              header: "Turn",
              shrink: true,
              // UNLINKED: the row is already a link to this turn, and `KeyCell`
              // is the one spelling of an identifier every grid here uses.
              cell: (t) => <KeyCell value={t.turn_id.slice(0, 8)} />,
            },
          ]}
        />
      </Card>
    </>
  );
}
