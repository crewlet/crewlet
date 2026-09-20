/**
 * Every unit of work the company has done, one row each.
 *
 * # There was no list of turns anywhere
 *
 * A turn is what this engine DOES — a wake, a decision, some tool rounds, a
 * reply — and every other surface is a projection of one: the spend rollup
 * groups them, the seat page shows one seat's, an item's history links to the
 * ones that touched it. None of them is a list of them.
 *
 * This screen used to be the PHASE monitor, which is a level below: sixty rows
 * for one turn on a busy seat, so "what has the company been doing" was
 * answered by scrolling past the rounds of whatever ran last. The phase view
 * is still here, as the other lens, because "which model call is hung right
 * now" is a real question and a turn row cannot answer it.
 *
 * # The fold is the engine's, not the browser's
 *
 * The list was previously assembled client-side by paging the raw event feed
 * sixty-one times and grouping in JavaScript. That is slow, capped at whatever
 * the caller gave up on, and WRONG at the page boundary: a turn whose events
 * straddled two pages appeared twice. The engine folds it now, over promoted
 * columns rather than payloads, with a cursor on the turn's own start.
 *
 * # A turn that has not finished is not a turn that finished instantly
 *
 * `complete` says whether a completion record exists, and the duration is the
 * turn's OWN measurement rather than the span of its events — the span covers
 * the reflection pass that publishes afterwards. Rendering a running turn's
 * zero as a duration would make the busiest turns look like the cheapest.
 *
 * # A log needs a time axis, and this one had none
 *
 * A list of turns is log-shaped: a hundred rows in a column say what happened
 * and have no dimension for WHEN, so a burst at four in the morning and a
 * steady trickle across a week read identically. The axis is the shape every
 * log tool has for exactly that reason — and it is a control, so the spike it
 * draws is the window the next click narrows to.
 */

import { useMemo, useState } from "react";
import { useParam } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { Button, Card, Skeleton, Tag } from "@crewlethq/ui";
import { GroupGlyph, TimelineGlyph } from "@crewlethq/icons/glyphs";
// OURS, AND DELIBERATELY. `SegmentedControl` welds keyboard ACTIVATION to its
// `semantics`: `radio` selects as the arrows move, `tabs` is manual but
// demands a `panelId` naming a TabPanel neither of these rows controls. Both
// rows here drive a `useParam` that re-runs this screen's query, which is the
// exact case our own `activate="manual"` exists for — arrowing across three
// options would ask the engine three times. See the report.
import { Segmented } from "~/ui/primitives.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { peekHref, rowPeekHandler, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { plural, tsKey } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { BUCKET_MS, RANGE_MS, spanOf, spanWords, useTimeRange, windowLabel } from "~/lib/range.ts";
import type { Bucket, Offer, Window } from "~/lib/range.ts";
import { TimeRangePicker } from "~/ui/TimeRange.tsx";
import { Histogram, type Bar } from "~/ui/Histogram.tsx";
import { ModelActivity } from "../company/Model.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { DateCell, DurationCell, NumberCell, TextCell, TokenCell } from "~/app/frame/cells.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import type { TurnRow } from "~/protocol/index.ts";

/**
 * WHICH WINDOWS A TURNS LIST HAS.
 *
 * Seven days is the fallback because it is the window this screen already had
 * without saying so — `store.DefaultTurnDays` is what the read takes from a
 * caller that names none — and thirty is the widest offer because that is both
 * `store.MaxTurnDays` and the event store's own retention, so a `90d` here
 * would offer a window two thirds of which can never hold a row. An hour is
 * the short end, where a minute bucket stops being drawable at sixty bars, and
 * "what has been running this hour" is asked of a turns list exactly as it is
 * asked of the event log.
 */
const TURN_OFFER: Offer = {
  ranges: ["1h", "6h", "1d", "7d", "30d"],
  custom: true,
  fallback: "7d",
  buckets: ["minute", "hour", "day"],
};

/** `store.MaxTurnDays` — the read refuses to look further back than this. */
const MAX_DAYS = 30;

/**
 * How many rows one answer carries.
 *
 * THE ENGINE'S OWN CEILING (`store.MaxTurnPage`) rather than the read's default
 * of fifty, and the axis is why: the bars below are folded from these rows, so
 * a page sized to "what a person scans" would draw a seven-day axis out of the
 * newest fifty turns and report a busy company as quiet before lunch on its
 * first day. A turn row carries no payload and no prompts — it is a group-by
 * over promoted columns — so two hundred of them is a narrow answer.
 */
const PAGE = 200;

/**
 * How many seat chips the filter row opens with.
 *
 * EIGHT, which is one line of them above the chart on an ordinary window, and
 * the row is expandable rather than fixed: a picker that showed eight of
 * twenty and said nothing made the other twelve reachable only by typing
 * `?role=` into the address bar.
 */
const SEAT_CHIPS = 8;

export function Turns() {
  // TWO LENSES ON ONE PATH, which is what a `view=` is for: turns are what
  // the company did, and phases are the model calls inside them. They are
  // not two destinations — a reader switching between them is asking the
  // same question at two magnifications.
  const [view, setView] = useParam("view", "turns", "filter");
  if (view === "phases") {
    return (
      <>
        <TurnLens view={view} onChange={setView} />
        <ModelActivity />
      </>
    );
  }
  return <TurnList view={view} onChange={setView} />;
}

function TurnLens({
  view,
  onChange,
  children,
}: {
  view: string;
  onChange: (v: string) => void;
  /** The controls only one of the two lenses has — the turn list's window. */
  children?: React.ReactNode;
}) {
  return (
    <PageActions>
      {
        <>
          <Segmented
            ariaLabel="Turns or the phases inside them"
            value={view}
            onChange={onChange}
            options={[
              { value: "turns", label: "Turns" },
              { value: "phases", label: "Phases" },
            ]}
          />
          {children}
        </>
      }
    </PageActions>
  );
}

/**
 * How many whole days of turns the engine has to be asked for.
 *
 * THE READ TAKES DAYS, not two instants: `store.TurnQuery` has `SinceDays` and
 * a cursor on the turn's start, so an hour and a day are the same question at
 * the wire and the window itself is applied here, over the rows that came
 * back. Rounded UP and floored at one, because a six-hour window asking for
 * zero days would fall through to the read's own default of seven and quietly
 * fetch a week to draw six hours of it.
 */
function windowDays(window: Window): number {
  return Math.min(MAX_DAYS, Math.max(1, Math.ceil(spanOf(window) / RANGE_MS["1d"])));
}

/**
 * The axis, folded from the rows this screen loaded.
 *
 * THE ONE PLACE A CLIENT-SIDE FOLD IS THE HONEST ANSWER, and `ui/Histogram`
 * states why it is normally not: bars folded in the browser are right for
 * whatever the tab happens to hold and absent for every other window. There is
 * no turn series at the engine to ask instead. `event_series` counts EVENTS
 * and a turn is dozens of them, so an axis drawn from it would put a bar of
 * three hundred over a list of five; and the turns read has no `since`/`until`
 * at all. So the fold is over the page of rows, the panel says so in those
 * words, and the total under the axis is the count of the rows above it —
 * never a claim about the store.
 *
 * EVERY BUCKET IS A BAR, including the empty ones, for `Histogram`'s own
 * reason: a quiet stretch is a fact about the company and a missing column is
 * a gap in the chart, and a reader who has not counted the columns cannot tell
 * those apart.
 */
function foldBars(rows: TurnRow[], since: string, until: string, bucket: Bucket): Bar[] {
  const step = BUCKET_MS[bucket];
  // FLOORED TO THE BUCKET, so the first column covers a whole one. The rows
  // are already filtered to the window, so a bar can never claim a turn the
  // list below it would not show.
  const from = Math.floor(tsKey(since) / step) * step;
  const to = tsKey(until);
  const counts = new Map<number, number>();
  for (const row of rows) {
    const at = tsKey(row.started_at);
    if (at <= 0) continue;
    const cell = Math.floor(at / step) * step;
    counts.set(cell, (counts.get(cell) ?? 0) + 1);
  }
  const bars: Bar[] = [];
  for (let at = from; at < to; at += step) {
    bars.push({ at: new Date(at).toISOString(), count: counts.get(at) ?? 0 });
  }
  return bars;
}

function TurnList({ view, onChange }: { view: string; onChange: (v: string) => void }) {
  const now = useNow();
  const org = useOrg();
  const { open: openPeek } = usePeekControls();
  // EVERY FILTER IS A FILTER, so it replaces the history entry: a reader
  // narrowing to one seat and then to the failures has walked one screen,
  // not three.
  const [role, setRole] = useParam("role", "", "filter");
  const [failed, setFailed] = useParam("failed", "", "filter");
  // ALIGNED, because this window drives a chart as well as a list: the top
  // edge is the END of the bucket in progress, so the current column is drawn
  // while it is still being spent and the window's identity — and therefore
  // the query — changes once per column rather than once per second.
  const range = useTimeRange(now, TURN_OFFER);
  const { since, until, bucket } = range;
  const list = useQuery(
    "turns",
    {
      days: windowDays(range.window),
      limit: PAGE,
      ...(role ? { role } : {}),
      ...(failed ? { failed } : {}),
    },
    { pollMs: 20_000 },
  );
  const turns = useMemo(() => list.data?.turns ?? [], [list.data]);
  // THE WINDOW, half-open, applied to what came back. The engine was asked in
  // whole days, so a six-hour window arrives holding a day of turns — and a
  // list that showed them under a heading saying six hours would disagree
  // with its own axis on every row.
  const rows = useMemo(() => {
    const from = tsKey(since);
    const to = tsKey(until);
    return turns.filter((t) => {
      const at = tsKey(t.started_at);
      return at >= from && at < to;
    });
  }, [turns, since, until]);
  const bars = useMemo(() => foldBars(rows, since, until, bucket), [rows, since, until, bucket]);
  // HOW MANY RUNS EACH TRIGGER GOT, over the rows this page holds. A turn id
  // names one run (see `adr/0017`), so a redelivered trigger is several rows
  // and nothing else on the screen says they are the same work.
  const reruns = useMemo(() => {
    const counts = new Map<string, number>();
    for (const t of rows) {
      // An empty work key is the ABSENCE of an identity — a trigger with
      // nothing to collapse on — so counting them together would report
      // every such turn as a re-run of every other.
      if (t.work_key) counts.set(t.work_key, (counts.get(t.work_key) ?? 0) + 1);
    }
    return counts;
  }, [rows]);
  // WHAT `[` AND `]` WALK: the rows this list actually loaded, windowed and
  // sorted as the reader left them. Published rather than handed to the rail,
  // because only the list knows that order — see `PeekHost`.
  usePeekNeighbours(
    useMemo(() => rows.map((t) => ({ kind: "turn" as const, id: t.turn_id })), [rows]),
  );
  const seats = (org?.roles ?? []).filter((r) => r.kind !== "human");
  // THE CHIP ROW IS A PICKER, AND A PICKER THAT HIDES ITS OPTIONS IS A LIE.
  // Eight chips with nothing after them read as the whole roster, so on a
  // company with more seats than that the rest were unreachable from this
  // screen — reachable only by typing `?role=` into the address bar, which is
  // not a filter anybody finds. The row still opens at eight, because a wall
  // of chips above a chart is its own kind of unusable, and says how many it
  // is holding back.
  const [allSeats, setAllSeats] = useState(false);
  const shown = allSeats ? seats : seats.slice(0, SEAT_CHIPS);

  return (
    <>
      <TurnLens view={view} onChange={onChange}>
        <Tag appearance="outline">{windowLabel(range.window)}</Tag>
        <TimeRangePicker range={range} ariaLabel="Window" />
      </TurnLens>
      <PageNote>
        One row per unit of work — a wake, a decision, its rounds and its reply. The phase lens is
        the same window one level down, where a single turn can be sixty rows.
      </PageNote>

      <Card>
        <Card.Header
          icon={<TimelineGlyph size="sm" />}
          subtitle={`${plural(rows.length, "turn")} on this page, over ${spanWords(since, until)}`}
          actions={
            <span className="t-caption">one bar per {bucket} — click one to narrow the window</span>
          }
        >
          <Card.Title>When</Card.Title>
        </Card.Header>
        {rows.length > 0 ? (
          <Histogram
            bars={bars}
            bucket={bucket}
            total={rows.length}
            onPick={range.set}
            label="Turns over the window"
          />
        ) : (
          // NOT AN EMPTY CHART. Bars of zero across a window say "the company
          // was quiet"; what is true here is that this page holds no turn in
          // the window, which a widened range or a cleared filter may change.
          <span className="t-caption">No turn on this page started in this window.</span>
        )}
      </Card>

      <div className="row gap-2 wrap">
        <Button
          size="small"
          variant={role ? "primary" : "secondary"}
          onClick={() => setRole("")}
          leadingIcon={<GroupGlyph size="xs" />}
        >
          {role || "every seat"}
        </Button>
        {shown.map((seat) => (
          <Button
            key={seat.name}
            size="small"
            variant={role === seat.name ? "primary" : "secondary"}
            onClick={() => setRole(role === seat.name ? "" : seat.name)}
          >
            {seat.name}
          </Button>
        ))}
        {seats.length > shown.length && (
          <Button size="small" variant="secondary" onClick={() => setAllSeats(true)}>
            {plural(seats.length - shown.length, "more seat")}
          </Button>
        )}
        <span className="spacer" />
        <Segmented
          ariaLabel="Which turns"
          value={failed || "all"}
          onChange={(v) => setFailed(v === "all" ? "" : v)}
          options={[
            { value: "all", label: "All" },
            { value: "true", label: "Carried a failure" },
            { value: "false", label: "Clean" },
          ]}
        />
      </div>

      {list.loading && rows.length === 0 && (
        <Skeleton variant="text" rows={6} label="Loading turns" />
      )}
      <QueryState
        error={list.error}
        loading={list.loading}
        empty={
          rows.length
            ? undefined
            : {
                title: "No turns in this window",
                hint: "A turn is recorded when a seat is woken and does something. Widen the range, or clear the seat and failure filters — a company whose seats have not been triggered has none.",
              }
        }
      >
        <DataGrid<TurnRow>
          rows={rows}
          rowKey={(t) => t.turn_id}
          // THE ROW IS A REAL LINK to the turn's page, so ⌘-click, the middle
          // button and the status bar all behave — and a plain click opens the
          // turn beside the list instead, because this screen is scanned and
          // sending a reader away to read one summary is the navigation every
          // log learned not to make.
          // THE FRAME'S OWN ANSWER to where a turn lives, rather than a
          // second copy of the route: the rail's `Open ↗` is built from the
          // same reference, so the link a row carries and the way out of the
          // panel it opens can never name different pages.
          rowHref={(t) => peekHref({ kind: "turn", id: t.turn_id })}
          onRowActivate={(t, e) => {
            const go = () => openPeek({ kind: "turn", id: t.turn_id });
            // THE GRID HANDS THIS BOTH EVENTS. `rowPeekHandler` is the frame's
            // one copy of "which clicks mean elsewhere" and reads a mouse
            // event; the `enter` chord carries no button at all and is never
            // "open elsewhere".
            if (!("button" in e)) {
              go();
              return;
            }
            rowPeekHandler(go)?.(e);
          }}
          defaultSort="-started"
          columns={[
            {
              key: "started",
              header: "Started",
              shrink: true,
              sortValue: (t) => tsKey(t.started_at),
              cell: (t) => <DateCell at={t.started_at} now={now} />,
            },
            {
              key: "seat",
              header: "Seat",
              shrink: true,
              sortValue: (t) => t.role ?? "",
              // NOT `SeatCell`, and not the seat chip this column used to
              // draw: both are links, and the row around them is one now — an
              // anchor inside an anchor is markup no browser agrees about.
              // The seat's own page is one click away from the turn.
              cell: (t) =>
                t.role ? (
                  <TextCell icon="memory">{t.role}</TextCell>
                ) : (
                  // NOT a dash: a turn with no seat is not a turn whose seat
                  // went unrecorded, it is the engine's own work.
                  <span className="muted">the engine</span>
                ),
            },
            {
              key: "summary",
              header: "What it did",
              sortValue: (t) => t.summary ?? "",
              cell: (t) => (
                <span className="row gap-1">
                  <span className="truncate">
                    {t.summary || <span className="muted">no summary recorded</span>}
                  </span>
                  {t.task_id && (
                    // THE KEY, not a link to it — see the row's own comment.
                    // It still says which item this turn was about, and the
                    // turn's page links to it from inside.
                    <span className="mono t-caption" title="the work item this turn was about">
                      {t.task_id}
                    </span>
                  )}
                </span>
              ),
            },
            {
              key: "state",
              header: "",
              label: "State",
              shrink: true,
              cell: (t) => (
                <span className="row gap-1">
                  {/* A RE-RUN SAYS SO. A turn id names one run, so a trigger
                      that failed without reaching outside the engine and was
                      redelivered is several rows here — and two rows for one
                      message read as the company having done the work twice.
                      Counted over the rows this page holds, which is what the
                      tooltip says. */}
                  {t.work_key && reruns.get(t.work_key)! > 1 && (
                    <Tag
                      appearance="outline"
                      title={
                        `one of ${reruns.get(t.work_key)} runs of the same trigger on this ` +
                        `page — a turn that fails without acting is redelivered and runs again`
                      }
                    >
                      re-run
                    </Tag>
                  )}
                  {!t.complete && (
                    <Tag
                      variant="info"
                      title="no completion record — running, or it died mid-flight"
                    >
                      running
                    </Tag>
                  )}
                  {t.failed && (
                    <Tag variant="warning" title="at least one event of this turn was a failure">
                      failure
                    </Tag>
                  )}
                </span>
              ),
            },
            {
              key: "iterations",
              // SELF-ITERATE ROUNDS, and the word says so. Headed "Rounds" this
              // column sat directly above phase rows printing TOOL rounds under
              // the same word — "Rounds 1" over a 3r execute and a 1r review.
              header: (
                <span title="self-iterate rounds — the tool rounds each phase used are on the phase row">
                  Iterations
                </span>
              ),
              label: "Iterations",
              shrink: true,
              align: "right",
              sortValue: (t) => t.iterations,
              cell: (t) => <NumberCell value={t.iterations} />,
            },
            {
              key: "phases",
              header: "Phases",
              shrink: true,
              align: "right",
              sortValue: (t) => t.phases,
              cell: (t) => <NumberCell value={t.phases} />,
            },
            {
              key: "tokens",
              header: "Tokens",
              shrink: true,
              align: "right",
              sortValue: (t) => t.total_tokens,
              cell: (t) => <TokenCell value={t.total_tokens} />,
            },
            {
              key: "took",
              header: "Took",
              shrink: true,
              align: "right",
              sortValue: (t) => t.duration_ms,
              // A RUNNING TURN HAS NO DURATION, and rendering its zero would
              // make the busiest turns look like the cheapest — so the cell is
              // handed null rather than the zero, and draws the dash that says
              // nothing was measured. `DurationCell` is also the right
              // spelling for a FINISHED span: `fmtElapsed`, which this column
              // used, is the live-stopwatch format, and a turn with a
              // completion record is not a stopwatch.
              cell: (t) => (
                <DurationCell ms={t.complete && t.duration_ms > 0 ? t.duration_ms : null} />
              ),
            },
          ]}
        />
      </QueryState>
    </>
  );
}
