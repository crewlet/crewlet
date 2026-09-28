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
 * answered by scrolling past the rounds of whatever ran last. The phases are
 * Live › Now running's now, as its settled half, beside the running turns a
 * phase is a leg of; a lens here was a second copy of that list.
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
 * `complete` says whether the turn ended, and the duration is the turn's OWN
 * measurement rather than the span of its events — the span covers the
 * reflection pass that publishes afterwards. Rendering a running turn's zero
 * as a duration would make the busiest turns look like the cheapest. A turn
 * waiting on a coding run it launched is `parked`, which is neither: it has
 * completed a segment and will complete again, so it is marked as such rather
 * than as running or as finished.
 *
 * # A log needs a time axis, and the ENGINE counts it
 *
 * A list of turns is log-shaped: a hundred rows in a column say what happened
 * and have no dimension for WHEN, so a burst at four in the morning and a
 * steady trickle across a week read identically. The axis is the shape every
 * log tool has for exactly that reason — and it is a control, so the spike it
 * draws is the window the next click narrows to.
 *
 * It was folded in the browser from the page of rows this screen held, which
 * drew a seven-day axis out of the newest two hundred turns and called a busy
 * company quiet before lunch on its first day. It is `event_series` now, over
 * the turns that ENDED — `agent_turn_completed` with `suspended=false`, one
 * record per turn, since a turn that parked on a coding run writes a
 * completion for the segment that parked it — counted by the engine over the
 * whole window on every node, with the failed share of each bar drawn at its
 * foot. The list is paged; the axis is not.
 */

import { useCallback, useMemo, useState } from "react";
import { useParam } from "~/app/router.tsx";
import { QueryState } from "~/components/common.tsx";
import { CoverageNote } from "~/components/CoverageNote.tsx";
import { Button, Card, Select, Skeleton, Tag } from "@crewlethq/ui";
import { ChartNoAxesGanttGlyph } from "@crewlethq/icons/glyphs";
// OURS, AND DELIBERATELY. `SegmentedControl` welds keyboard ACTIVATION to its
// `semantics`: `radio` selects as the arrows move, `tabs` is manual but
// demands a `panelId` naming a TabPanel neither of these rows controls. Both
// rows here drive a `useParam` that re-runs this screen's query, which is the
// exact case our own `activate="manual"` exists for — arrowing across three
// options would ask the engine three times.
import { Segmented } from "~/ui/primitives.tsx";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { peekHref, rowPeekHandler, usePeekControls } from "~/app/frame/DetailRail.tsx";
import { usePeekNeighbours } from "~/app/frame/PeekHost.tsx";
import { useClient, useAgents, useOrg } from "~/lib/store-hooks.ts";
import { useQuery } from "~/lib/useQuery.ts";
import { indexOrg, useSeatBadgeOf } from "~/lib/seats.ts";
import { plural } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
import { spanWords, useTimeRange } from "~/lib/range.ts";
import type { Offer } from "~/lib/range.ts";
import { TimeRangePicker } from "~/ui/TimeRange.tsx";
import { Histogram, type Bar } from "~/ui/Histogram.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import {
  DateCell,
  DurationCell,
  NumberCell,
  SeatLabel,
  TokenCell,
  TurnWhatCell,
  UnsettledCell,
} from "~/app/frame/cells.tsx";
import { runningNow } from "~/lib/turns.ts";
import type { EventSeries, TurnRow, TurnsAnswer } from "~/protocol/index.ts";

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

/**
 * How many rows one page carries.
 *
 * ONE HUNDRED: the axis above is the engine's count over the whole window, so
 * the page no longer has to hold the window to draw it — it was the engine's
 * ceiling of two hundred for exactly that reason. A hundred is two screens of
 * rows a person scans before they narrow by seat or failure, and "Load older"
 * is one press past it.
 */
export const TURN_PAGE = 100;

/** The two orders the engine's turn list answers in (`store.TurnQuery.Sort`). */
export const TURN_SORTS = ["-started", "-tokens"] as const;

/**
 * What the axis says while the list is narrowed by failure.
 *
 * THE AXIS IS NOT NARROWED BY IT, and says so. "Carried a failure" is a fact
 * about a whole turn — any of its records failed — and the axis counts
 * completion records, one per turn, each with only its OWN failed flag: an
 * `event_series` narrowed that way would count a different set of turns from
 * the list under it, under the same heading. So the axis stays every turn,
 * with the failed share already drawn at each bar's foot.
 */
export const AXIS_UNFILTERED = " · every turn, not only the filtered ones";

/** The turns that ENDED, one record each — what the axis counts. */
export const ENDED_TURNS = { type: "agent_turn_completed", suspended: "false" } as const;

export function Turns() {
  const seatBadge = useSeatBadgeOf();
  const now = useNow();
  const org = useOrg();
  const { socket } = useClient();
  // THE PUSH, for what a turn still running is doing (`runningNow`).
  const agents = useAgents();
  const { open: openPeek } = usePeekControls();
  // EVERY FILTER IS A FILTER, so it replaces the history entry: a reader
  // narrowing to one seat and then to the failures has walked one screen,
  // not three.
  // A SEAT BY ITS HANDLE, which the engine resolves to the seat's own id —
  // never a role name, which a rename changes and two unit seats share.
  const [seat, setSeat] = useParam("seat", "", "filter");
  const [failed, setFailed] = useParam("failed", "", "filter");
  // THE ENGINE'S ORDER, and the one key the grid below would read for its
  // own: the list is sorted where the whole set is, so a column head that
  // sorted the loaded page would be a different question.
  const [sortRaw, setSort] = useParam("sort", "-started", "filter");
  const sort = (TURN_SORTS as readonly string[]).includes(sortRaw) ? sortRaw : "-started";
  // ALIGNED, because this window drives a chart as well as a list: the top
  // edge is the END of the bucket in progress, so the current column is drawn
  // while it is still being spent and the window's identity — and therefore
  // the query — changes once per column rather than once per second.
  const range = useTimeRange(now, TURN_OFFER);
  const { since, until, bucket } = range;
  // THE WINDOW ITSELF, as its two instants on the turn's start. The read took
  // whole days back from NOW, so a bar picked three days ago was asked as "the
  // last day", every row that came back was newer than the bar, and the list
  // under an axis counting a dozen turns said there were none.
  const params = useMemo(
    () => ({
      since,
      until,
      limit: TURN_PAGE,
      sort,
      ...(seat ? { seat } : {}),
      ...(failed ? { failed } : {}),
    }),
    [since, until, sort, seat, failed],
  );
  const list = useQuery("turns", params, { pollMs: 20_000 });
  const series = useQuery(
    "event_series",
    { ...ENDED_TURNS, since, until, bucket, ...(seat ? { seat } : {}) },
    { pollMs: 20_000 },
  );

  // THE OLDER PAGES, keyed on the question they continue — a new filter or
  // window is a new list, and its first page must not be followed by the
  // previous list's second.
  const question = JSON.stringify(params);
  const [older, setOlder] = useState<{ for: string; pages: TurnsAnswer[] }>({
    for: "",
    pages: [],
  });
  const [paging, setPaging] = useState(false);
  const [pageError, setPageError] = useState<string | null>(null);
  const pages = older.for === question ? older.pages : [];
  const last = pages.at(-1) ?? list.data;

  const turns = useMemo(() => {
    // ONE ROW PER TURN across pages: a turn still running when the first page
    // was answered can be listed again by a later one.
    const seen = new Set<string>();
    const out: TurnRow[] = [];
    for (const t of [...(list.data?.turns ?? []), ...pages.flatMap((p) => p.turns)]) {
      if (seen.has(t.turn_id)) continue;
      seen.add(t.turn_id);
      out.push(t);
    }
    return out;
  }, [list.data, pages]);
  // THE ENGINE HELD EVERY PAGE TO THE WINDOW, so what came back is the list:
  // a second filter here was how an answer for the wrong window became an
  // empty screen rather than a wrong one.
  const rows = turns;
  // THE CURSOR ALONE SAYS WHETHER THERE IS MORE — and it can be present on a
  // page with no row, when a node stopped before any turn above it could be
  // shown, so "Load older" is offered on an empty page too.
  const more = !!last?.next;

  const loadOlder = useCallback(async () => {
    if (!last?.next) return;
    setPaging(true);
    setPageError(null);
    try {
      const page = await socket.query("turns", { ...params, before: last.next });
      setOlder((prev) => ({
        for: question,
        pages: [...(prev.for === question ? prev.pages : []), page],
      }));
    } catch (err) {
      setPageError(err instanceof Error ? err.message : "query_failed");
    } finally {
      setPaging(false);
    }
  }, [socket, params, question, last]);

  const bars = useMemo(() => barsOf(series.data), [series.data]);
  // HOW MANY RUNS EACH TRIGGER GOT, over the rows this list holds. A turn id
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
  // WHAT `[` AND `]` WALK: the rows this list actually loaded, in the order
  // the engine answered them. Published rather than handed to the rail,
  // because only the list knows that order — see `PeekHost`.
  usePeekNeighbours(
    useMemo(() => rows.map((t) => ({ kind: "turn" as const, id: t.turn_id })), [rows]),
  );
  // EVERY AGENT SEAT THE ORG HOLDS, from the one index every screen walks it
  // with. `org.roles` is only the seats ABOVE every unit — on a real company
  // that is the founders, humans with no turns — so a menu built from it
  // offered "Every seat" and nothing else, and `?seat=` from a link drew the
  // bare handle because no option matched it.
  const index = useMemo(() => indexOrg(org), [org]);
  const seats = useMemo(
    () => index.seats.filter((s) => s.kind === "agent" && !!s.handle),
    [index.seats],
  );

  const total = series.data?.total ?? 0;
  const failedTotal = series.data?.failed ?? 0;

  return (
    <>
      <PageActions>
        <TimeRangePicker range={range} ariaLabel="Window" />
      </PageActions>

      <Card>
        <Card.Header
          icon={<ChartNoAxesGanttGlyph size="sm" />}
          // UNDER THE TITLE, the chart key's place on every chart card (the
          // seat's own turns chart included): beside the title a phone left the
          // subtitle about thirty characters and cut it at the failed count —
          // the one number the red stack encodes. The count comes first and
          // the explanatory tail last, so what gives way is the hint.
          className="card-head-stacked"
          subtitle={`${
            series.data
              ? `${plural(total, "turn")} ended over ${spanWords(since, until)}${failedTotal > 0 ? `, ${failedTotal.toLocaleString()} failed` : ""}`
              : `over ${spanWords(since, until)}`
          }${failed ? AXIS_UNFILTERED : ""} · one bar per ${bucket}, click one to narrow the window`}
        >
          <Card.Title>Turns that ended</Card.Title>
        </Card.Header>
        {series.error && !series.data ? (
          <QueryState error={series.error} loading={false} />
        ) : series.data ? (
          <Histogram
            bars={bars}
            bucket={bucket}
            total={total}
            noun="turn"
            // THE ENGINE'S COUNT, over the whole window and every node that
            // answered — never the page this screen happens to hold.
            over="window"
            axis
            now={now}
            onPick={range.set}
            label="Turns that ended over the window"
          />
        ) : (
          <Skeleton variant="box" height={96} label="Counting turns" />
        )}
      </Card>

      <CoverageNote
        coverage={[series.data?.coverage, list.data?.coverage, ...pages.map((p) => p.coverage)]}
        what="these turns"
      />

      <div className="toolbar">
        <Select
          width="auto"
          value={seat}
          onChange={(value) => setSeat(String(value))}
          options={[
            { value: "", label: "Every seat" },
            ...seats.map((s) => ({ value: s.handle, label: s.name })),
          ]}
          ariaLabel="Seat"
          menuClassName="seat-filter-menu"
          placeholder="Every seat"
          active={seat !== ""}
        />
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
        <span className="spacer" />
        <Segmented
          ariaLabel="Order"
          value={sort}
          onChange={setSort}
          options={[
            { value: "-started", label: "Newest" },
            { value: "-tokens", label: "Most tokens" },
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
          // THE ENGINE ORDERED THE WHOLE SET; a head that re-sorted the pages
          // loaded would answer a different question under the same arrow.
          serverSorted
          // THE ROW IS A REAL LINK to the turn's page, so ⌘-click, the middle
          // button and the status bar all behave — and a plain click opens the
          // turn beside the list instead, because this screen is scanned.
          rowHref={(t) => peekHref({ kind: "turn", id: t.turn_id })}
          onRowActivate={(t, e) => {
            const go = () => openPeek({ kind: "turn", id: t.turn_id });
            // THE GRID HANDS THIS BOTH EVENTS. `rowPeekHandler` is the frame's
            // one copy of "which clicks mean elsewhere" and reads a mouse
            // event; the `enter` chord carries no button at all.
            if (!("button" in e)) {
              go();
              return;
            }
            rowPeekHandler(go)?.(e);
          }}
          columns={[
            {
              key: "started",
              header: "Started",
              shrink: true,
              cell: (t) => <DateCell at={t.started_at} now={now} />,
            },
            {
              key: "seat",
              header: "Seat",
              shrink: true,
              // NOT `SeatCell`: it is a link, and the row around it is one.
              cell: (t) =>
                t.role ? (
                  <SeatLabel {...seatBadge(t.role)} />
                ) : (
                  // NOT a dash: a turn with no seat is the engine's own work.
                  <span className="muted">the engine</span>
                ),
            },
            {
              key: "summary",
              header: "What it did",
              // A TURN STILL RUNNING says what it is doing and what it is on,
              // from the push (`runningNow`): the store's row has neither yet.
              cell: (t) => {
                const live = runningNow(t, agents);
                return (
                  <TurnWhatCell
                    summary={t.summary}
                    item={t.work_item ?? live?.item}
                    doing={live?.words}
                  />
                );
              },
            },
            {
              key: "state",
              // HEADED, like every other column: an unlabelled slot between
              // What it did and Iterations read as a gap, and a badge under no
              // heading has to be decoded from its colour.
              header: "State",
              shrink: true,
              // NOTHING AT ALL for a turn with no state to show, so a phone's
              // card drops the line rather than printing a bare label.
              cell: (t) => <TurnState turn={t} reruns={reruns} />,
            },
            {
              key: "iterations",
              // SELF-ITERATE ROUNDS, and the word says so — a phase's TOOL
              // rounds are on its own record, under another word.
              header: (
                <span title="self-iterate rounds — the tool rounds each phase used are on the turn's own page">
                  Iterations
                </span>
              ),
              label: "Iterations",
              shrink: true,
              align: "right",
              cell: (t) => (t.complete ? <NumberCell value={t.iterations} /> : <UnsettledCell />),
            },
            {
              key: "tokens",
              header: "Tokens",
              shrink: true,
              align: "right",
              cell: (t) => (t.complete ? <TokenCell value={t.total_tokens} /> : <UnsettledCell />),
            },
            {
              key: "took",
              header: "Took",
              shrink: true,
              align: "right",
              // A RUNNING TURN HAS NO DURATION, and its zero would make the
              // busiest turns look like the cheapest — so the cell is handed
              // null and draws the dash that says nothing was measured.
              cell: (t) => (
                <DurationCell ms={t.complete && t.duration_ms > 0 ? t.duration_ms : null} />
              ),
            },
          ]}
        />
      </QueryState>

      {(rows.length > 0 || more || pageError) && (
        <div className="row gap-2">
          {pageError ? (
            <QueryState error={pageError} loading={false} />
          ) : more ? (
            <Button
              size="small"
              variant="secondary"
              onClick={() => void loadOlder()}
              disabled={paging}
              loading={paging}
            >
              {sort === "-started" ? "Load older" : "Load more"}
            </Button>
          ) : (
            <span className="t-caption">
              {sort === "-started"
                ? "That is every turn that started in this window."
                : "That is every turn in this window."}
            </span>
          )}
          {rows.length > 0 && (
            <span className="t-caption">{plural(rows.length, "turn")} listed</span>
          )}
        </div>
      )}
    </>
  );
}

/** The engine's bars, with their failed share. */
export function barsOf(series: EventSeries | null | undefined): Bar[] {
  return (series?.bars ?? []).map((b) => ({ at: b.at, count: b.count, failed: b.failed }));
}

/** What a turn IS beyond finished — re-run, parked, running, failed — or nothing. */
function TurnState({ turn: t, reruns }: { turn: TurnRow; reruns: Map<string, number> }) {
  const rerun = !!t.work_key && (reruns.get(t.work_key) ?? 0) > 1;
  if (!rerun && !t.parked && t.complete && !t.failed) return null;
  return (
    <span className="row gap-1">
      {/* A RE-RUN SAYS SO. A turn id names one run, so a trigger that failed
          without reaching outside the engine and was redelivered is several
          rows here — and two rows for one message read as the company having
          done the work twice. Counted over the rows this list holds. */}
      {rerun && (
        <Tag
          appearance="outline"
          title={
            `one of ${reruns.get(t.work_key!)} runs of the same trigger listed here — ` +
            `a turn that fails without acting is redelivered and runs again`
          }
        >
          re-run
        </Tag>
      )}
      {t.parked && (
        <Tag
          appearance="outline"
          title="waiting on a coding run it launched — it completes again when the run is collected"
        >
          parked
        </Tag>
      )}
      {!t.complete && !t.parked && (
        <Tag variant="info" title="no completion record — running, or it died mid-flight">
          running
        </Tag>
      )}
      {/* A FAILURE IS THE DANGER TONE, the one every failure mark wears. */}
      {t.failed && (
        <Tag variant="danger" title="at least one event of this turn was a failure">
          failure
        </Tag>
      )}
    </span>
  );
}
