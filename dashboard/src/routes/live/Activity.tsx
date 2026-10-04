/**
 * The event log.
 *
 * Two things this screen gets right that its predecessor did not:
 *
 *  1. **The filter vocabulary is FIXED.** Category chips came from the live
 *     400-event ring, so chips appeared and vanished as it evicted — including
 *     the one you were reaching for — and an actor who had gone quiet could not
 *     be filtered to AT ALL, because no chip existed for them. The categories
 *     are a closed set the engine defines; the actor filter is a text box over
 *     the roster.
 *  2. **A row says WHO.** The old feed rendered a nub, a colour dot, a summary
 *     and a relative time, and offered actor filter chips for a field it never
 *     displayed.
 *
 * History paging asks the engine for older rows past the live ring. The cursor
 * is `before_time`+`before_id`, which is what the server actually reads — the
 * previous client sent `before`, so `time.Parse("")` failed and EVERY cursored
 * page was rejected, then read the rejection as "that is the beginning of the
 * retained history".
 *
 * # Every filter but the text search is the ENGINE's
 *
 * The category, the actor, the seat, one trace (`trace=`), one agent-to-agent
 * channel (`channel=`) and "Failures only" (`failed=true`) are asked of the
 * engine, so the axis, every page it fetches and the live rows merged over
 * them are one set. "Failures only" used to be applied here to whatever rows
 * the tab held — so the axis counted the whole window while the list showed
 * the failures among the newest hundred, and every older page came back
 * unfiltered for the mark to hide. The search box is the one filter the
 * engine does not have, and the list says so where it matters: an empty
 * search result offers the older pages rather than claiming there is nothing.
 *
 * A trace and a channel arrive from a link — a trace's "In the log", a
 * channel's "Read this channel's events" — so each is a chip that says what
 * the log is narrowed to and takes itself off, like the seat.
 */

import { Fragment, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useSearchTarget } from "~/app/searchTarget.ts";
import { useParam } from "~/app/router.tsx";
import { EventRow, QueryState } from "~/components/common.tsx";
import { Button, Card, FilterChip, Input, Skeleton, Tag } from "@crewlethq/ui";
import { XGlyph, SearchGlyph, ChartNoAxesGanttGlyph } from "@crewlethq/icons/glyphs";
import { useAgents, useClient, useEngineHealth, useEvents, useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { eventHistoryLabel, fmtDate, newestFirst, plural, tsKey } from "~/lib/format.ts";
import type { FeedRow } from "~/protocol/index.ts";
import type { Coverage } from "~/contract/coverage.ts";
import { CoverageNote } from "~/components/CoverageNote.tsx";
import { useNow } from "~/lib/clock.ts";
import { useQuery } from "~/lib/useQuery.ts";
import {
  spanWords,
  stepOf,
  useTimeRange,
  windowEdges,
  windowLabel,
  windowParam,
} from "~/lib/range.ts";
import type { Offer } from "~/lib/range.ts";
import { TimeRangePicker } from "~/ui/TimeRange.tsx";
import { Histogram } from "~/ui/Histogram.tsx";
import { FacetRail } from "~/ui/FacetRail.tsx";
import { CATEGORIES } from "~/contract/categories.ts";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";

const PAGE = 100;

/**
 * WHICH WINDOWS THE LOG HAS.
 *
 * The whole vocabulary bar the two shortest, and a custom interval: an event
 * log is asked "what just happened" and "what happened last Tuesday" in the
 * same breath, and both are questions about the store rather than about what
 * this tab is holding. `1h` is the short end because that is where the minute
 * bucket stops being drawable — sixty bars — and the long end is `30d` because
 * that is `store.EventHistory`'s own default: a `90d` offer would be a window
 * two thirds of which can never have rows in it. The engine reports its actual
 * floor (`event_history_seconds`) and the footer says what it reported; this
 * offer is a fixed vocabulary of windows rather than a claim about retention.
 */
const LOG_OFFER: Offer = {
  ranges: ["1h", "6h", "1d", "7d", "30d"],
  custom: true,
  fallback: "1d",
  // ALL THREE, which is why `event_series` has a bucket the spend series does
  // not: "what just happened" is the commonest question asked of a log, and an
  // hour is the whole of that answer's window.
  buckets: ["minute", "hour", "day"],
};

/**
 * Which day a row belongs to, AS THE HEADING WOULD DRAW IT.
 *
 * The obvious version reads `getFullYear/getMonth/getDate` off a `Date`,
 * which is the BROWSER's local day — and `fmtDate` renders in the zone the
 * reader chose (`lib/prefs.ts`). A reader viewing a company in another zone
 * then got rows grouped on one day boundary under a heading naming another:
 * around midnight, the rows under "14 September" were the ones that fell on
 * the 14th *here*.
 *
 * So the key IS the label. Two rows are the same day when they draw the same
 * heading, which is true by construction rather than by two pieces of date
 * arithmetic agreeing — the same reason the recents cap counts the rows a rail
 * draws rather than a number beside them.
 */
export function dayKey(ts: string): string {
  return fmtDate(ts);
}

export function Activity() {
  // `/` FOCUSES THIS SCREEN'S SEARCH rather than opening the palette over it.
  const searchBox = useRef<HTMLInputElement>(null);
  useSearchTarget(searchBox);
  const { socket } = useClient();
  const liveEvents = useEvents();
  const engine = useEngineHealth();
  const now = useNow();
  const [category, setCategory] = useParam("category", "");
  const [actor, setActor] = useParam("actor", "");
  const [q, setQ] = useParam("q", "");
  // THE ENGINE'S FILTER, three-valued on the wire; the log offers the one
  // half a reader asks for, so the chip sets `true` or nothing.
  const [onlyFailed, setOnlyFailed] = useParam("failed", "");
  const failedOnly = onlyFailed === "true";
  // ONE TRACE AND ONE CHANNEL, by id, as the links that open the log name them.
  const [trace, setTrace] = useParam("trace", "");
  const [channel, setChannel] = useParam("channel", "");
  // ONE SEAT'S EVENTS, by its handle — what a profile's "Events" opens. The
  // engine resolves the handle to the id its events carry, so the pages it
  // answers are that seat's; the LIVE rows are narrowed here by the same id,
  // off the agents push, which is the only place this tab learns it.
  const [seat, setSeat] = useParam("seat", "");
  const agents = useAgents();
  const org = useOrg();
  const seatId = seat ? agents.find((a) => a.handle === seat)?.agent_id : undefined;
  const seatName = useMemo(
    () => (seat ? (indexOrg(org).byHandle.get(seat)?.name ?? seat) : ""),
    [org, seat],
  );
  // NOT ALIGNED to the bucket. A chart rounds its edges up so the column in
  // progress is drawn and the query changes once per column; a LIST's newest
  // row is the newest row, and rounding up would ask the store for rows that
  // do not exist yet.
  const range = useTimeRange(now, LOG_OFFER, false);
  const { since, until, bucket } = range;
  // WHICH WINDOW THIS IS, as an identity rather than as two instants.
  //
  // The price of the unaligned range above is that `since` and `until` ARE the
  // clock: a fresh pair of millisecond instants on every tick of `useNow`. They
  // are the right values to FILTER and to ASK with, and the wrong thing for
  // anything to be keyed on — keyed on them, the reset below ran once a second,
  // so every page a reader had loaded was thrown away and page one re-fetched,
  // for as long as the tab stayed open.
  const windowKey = windowParam(range.window);
  // THE AXIS'S OWN EDGES, snapped OUT to the bucket it draws in — the rounding
  // the comment above says a chart wants, and the reason `windowEdges` takes a
  // step at all. It buys two things: the query's identity stands still between
  // ticks (a query is keyed on its parameters, so instants carrying the
  // millisecond re-asked the engine for the same bars once a second, on every
  // open tab), and the bars cover exactly the window the badge names — whole
  // buckets ending at the end of the one in progress, rather than the extra
  // part-bucket the engine's own outward snap adds under a raw `now`.
  const axis = windowEdges(range.window, now, stepOf(bucket));

  const [older, setOlder] = useState<FeedRow[]>([]);
  // Whether this window's FIRST page has been asked for. A window is a query
  // now, so the screen asks it rather than waiting to be told to: without
  // this, a reader who scrubbed to a day the axis says holds nine thousand
  // events saw whatever the live socket had pushed since the tab opened —
  // one row on a freshly started engine — under a heading claiming the day.
  const [fetched, setFetched] = useState(false);
  const [cursor, setCursor] = useState<{ before_time: string; before_id: string } | null>(null);
  const [exhausted, setExhausted] = useState(false);
  const [paging, setPaging] = useState(false);
  const [pageError, setPageError] = useState<string | null>(null);
  // WHICH NODES EACH PAGE WAS MERGED FROM, so the note below names a node
  // that did not answer for any of them.
  const [pageCoverage, setPageCoverage] = useState<(Coverage | undefined)[]>([]);

  // A FILTER CHANGE IS A NEW QUERY, so the pages fetched under the old one go
  // with it. They used to survive, and it broke three ways at once: rows
  // fetched under the previous category stayed in the list and were filtered
  // client-side into a set that no longer matched what the server would have
  // answered; the cursor kept pointing into the old query's history, so "load
  // older" walked the wrong sequence; and `exhausted` stayed true, so a
  // narrower filter reported that there was no more history to fetch when its
  // own first page had never been asked for.
  //
  // The server-side keys only. `q` and `failed` are applied in the browser to
  // whatever arrived, so changing them cannot invalidate a page.
  //
  // THE WINDOW'S IDENTITY, not its two instants — see [windowKey]. A reader
  // choosing another range is a new query and its pages go; a second passing
  // is not.
  useEffect(() => {
    setOlder([]);
    setCursor(null);
    setExhausted(false);
    setPageError(null);
    setPageCoverage([]);
    setFetched(false);
  }, [category, actor, seat, trace, channel, failedOnly, windowKey]);

  const rows = useMemo(() => {
    const seen = new Set<string>();
    // A LIVE ROW IS ANY SEAT'S until the id says otherwise. The paged rows
    // are already the seat's — the engine filtered them — but the socket
    // pushes everything, and a seat this tab has no id for yet has no live
    // row that can be shown as its own: none is, rather than every one.
    const live = seat ? liveEvents.filter((e) => !!seatId && e.agent_id === seatId) : liveEvents;
    const all = [...live, ...older].filter((e) => {
      if (seen.has(e.id)) return false;
      seen.add(e.id);
      return true;
    });
    const needle = q.trim().toLowerCase();
    const from = tsKey(since);
    const to = tsKey(until);
    return (
      all
        // THE WINDOW, half-open, applied to the LIVE rows too. They arrive on
        // the socket regardless of what the reader is looking at, so without
        // this a reader scrubbed back to last Tuesday would watch this
        // afternoon's events appear at the top of it.
        .filter((e) => {
          const at = tsKey(e.timestamp);
          return at >= from && at < to;
        })
        .filter((e) => !category || e.category === category)
        // EQUALITY, because that is what the server does. `store.List` compares
        // the actor for equality, so a prefix typed here narrowed the loaded
        // rows by substring and then fetched older pages by exact match — two
        // different filters over one list, and the paged half came back empty
        // for every prefix. The search box is where substring lives.
        .filter((e) => !actor || (e.actor ?? "") === actor)
        // THE SAME THREE FILTERS THE ENGINE APPLIED TO THE PAGES, applied to
        // the live rows by the same values: each row carries its trace, its
        // channel (the store's own column, stamped on the live row too) and
        // its failure mark.
        .filter((e) => !trace || e.trace_id === trace)
        .filter((e) => !channel || e.channel_id === channel)
        .filter((e) => !failedOnly || e.failed)
        .filter(
          (e) =>
            !needle ||
            (e.summary ?? "").toLowerCase().includes(needle) ||
            (e.type ?? "").toLowerCase().includes(needle) ||
            (e.source ?? "").toLowerCase().includes(needle),
        )
        .sort(newestFirst)
    );
  }, [
    liveEvents,
    older,
    category,
    actor,
    seat,
    seatId,
    trace,
    channel,
    failedOnly,
    q,
    since,
    until,
  ]);

  // THE AXIS IS THE ENGINE'S. This tab holds at most the last 400 events and
  // the store's window it never holds, so a histogram folded here would be
  // right for one window and absent for every other — and it is counted
  // through the same predicate the listing filters with, so a bar can never
  // claim rows the list below it would not show.
  //
  // The SERVER-SIDE filters only — every one but `q`, which is applied in
  // the browser to whatever arrived, so an axis carrying it would be counting
  // a set the engine was never asked about.
  const filters = useMemo(
    () => ({
      ...(category ? { category } : {}),
      ...(actor ? { actor } : {}),
      ...(seat ? { seat } : {}),
      ...(trace ? { trace_id: trace } : {}),
      ...(channel ? { channel_id: channel } : {}),
      ...(failedOnly ? { failed: "true" } : {}),
    }),
    [category, actor, seat, trace, channel, failedOnly],
  );
  const series = useQuery("event_series", {
    since: axis.since,
    until: axis.until,
    bucket,
    ...filters,
  });

  const loadOlder = useCallback(async () => {
    setPaging(true);
    setPageError(null);
    try {
      // The cursor names BOTH halves. The engine reads `before_time` and
      // `before_id`; a client sending one bare `before` gets every page
      // rejected with `query_failed`.
      const params: Record<string, unknown> = { limit: PAGE, since, until, ...filters };
      if (cursor) {
        params.before_time = cursor.before_time;
        params.before_id = cursor.before_id;
      } else {
        const last = rows[rows.length - 1];
        if (last) {
          params.before_time = last.timestamp;
          params.before_id = last.id;
        }
      }
      // The answer is an OBJECT — `{events, next, exhausted}` — not a bare
      // array. Reading it as an array yielded [] every time, which the caller
      // then read as "the beginning of the retained history".
      const page = await socket.query("events", params);
      setOlder((prev) => [...prev, ...(page.events ?? [])]);
      setPageCoverage((prev) => [...prev, page.coverage]);
      setCursor(page.next ?? null);
      setExhausted(page.exhausted || !page.next);
    } catch (err) {
      setPageError(err instanceof Error ? err.message : "query_failed");
    } finally {
      setPaging(false);
    }
  }, [socket, cursor, rows, filters, since, until]);

  // THE FIRST PAGE OF THE WINDOW, once per window. `loadOlder` is a
  // dependency and changes with every render that changes `rows`, so the
  // `fetched` flag is what makes this once rather than a loop — a plain
  // dependency on the callback would refetch on its own answer.
  useEffect(() => {
    if (fetched) return;
    setFetched(true);
    void loadOlder();
  }, [fetched, loadOlder]);

  const filtered = !!(category || actor || seat || trace || channel || q || failedOnly);

  return (
    <>
      <PageActions>
        <Tag appearance="outline">{windowLabel(range.window)}</Tag>
        <TimeRangePicker range={range} ariaLabel="Window" />
        {filtered ? (
          <Button
            leadingIcon={<XGlyph size="xs" />}
            size="small"
            variant="secondary"
            onClick={() => {
              setCategory("");
              setActor("");
              setSeat("");
              setTrace("");
              setChannel("");
              setQ("");
              setOnlyFailed("");
            }}
          >
            Clear filters
          </Button>
        ) : undefined}
      </PageActions>
      <PageNote>
        Everything the engine published, live and then paged out of the store. This tab holds the
        last 400 in memory; older rows are fetched, and{" "}
        {eventHistoryLabel(engine?.event_history_seconds)}.
      </PageNote>

      <Card>
        <Card.Header
          icon={<ChartNoAxesGanttGlyph size="sm" />}
          // THE HINT RIDES THE SUBTITLE, which gives way before the title
          // does; as an action it never shrank, and cut the title on a phone.
          subtitle={
            series.data
              ? `${plural(series.data.total, "event")} over ${spanWords(series.data.since, series.data.until)} · one bar per ${series.data.bucket}, click one to narrow the window`
              : undefined
          }
        >
          <Card.Title>When</Card.Title>
        </Card.Header>
        <QueryState
          error={series.error}
          refusal={series.refusal}
          loading={series.loading}
          empty={
            series.data && series.data.total === 0
              ? {
                  title: "Nothing was published in this window",
                  hint: "Widen the range, or clear the filters above it.",
                }
              : undefined
          }
        >
          {series.data && series.data.total > 0 && (
            <Histogram
              bars={series.data.bars}
              bucket={series.data.bucket}
              total={series.data.total}
              noun="event"
              // THE ENGINE COUNTED THESE, over the whole window and through the
              // same predicate the listing filters with — which is the claim
              // the tracker's own log cannot make about its bars, and the
              // reason the scope is a prop rather than an assumption.
              over="window"
              // THE SAME FRAME AS THE TRACKER'S LOG, dates included: a reader
              // who has learnt one log in this product has learnt the other,
              // and an axis on one of them only is two frames again.
              axis
              now={now}
              onPick={range.set}
            />
          )}
        </QueryState>
      </Card>

      <div className="toolbar">
        {/* The wrappers these replace were a `style={{ maxWidth: 300 }}` and a
            `style={{ maxWidth: 180 }}` — two numbers nobody had reconciled.
            `width` is the same idea as a named step, so the search box and the
            filter box beside it are sized by what they hold. */}
        <Input
          type="search"
          value={q}
          onChange={(e) => setQ(e.target.value)}
          aria-label="Search events"
          ref={searchBox}
          placeholder="Search summary, type or source"
          leading={<SearchGlyph size="sm" />}
          width="md"
        />
        <Input
          type="search"
          value={actor}
          onChange={(e) => setActor(e.target.value)}
          aria-label="Filter by actor"
          placeholder="Actor"
          leading={<SearchGlyph size="sm" />}
          width="sm"
        />
        <FilterChip pressed={failedOnly} onPressedChange={(on) => setOnlyFailed(on ? "true" : "")}>
          Failures only
        </FilterChip>
        {/* THE SEAT, as a chip that says whose log this is and takes itself
            off — the filter arrives from a link, so it has no box to clear. */}
        {seat && (
          <FilterChip
            pressed
            onPressedChange={(on) => !on && setSeat("")}
            aria-label={`Only ${seatName}'s events — remove`}
          >
            {/* ONE ROW, the name and its ×: the chip's label is an inline
                box, and the glyph, a block, broke under the name inside a
                26px pill. */}
            <span className="chip-removable">
              {seatName}
              <XGlyph size="xs" aria-hidden="true" />
            </span>
          </FilterChip>
        )}
        {trace && (
          <FilterChip
            pressed
            onPressedChange={(on) => !on && setTrace("")}
            aria-label={`Only trace ${trace}'s events — remove`}
          >
            <span className="chip-removable">
              Trace <code className="inline">{trace.slice(0, 8)}</code>
              <XGlyph size="xs" aria-hidden="true" />
            </span>
          </FilterChip>
        )}
        {channel && (
          <FilterChip
            pressed
            onPressedChange={(on) => !on && setChannel("")}
            aria-label={`Only channel ${channel}'s events — remove`}
          >
            <span className="chip-removable">
              A2A channel <code className="inline">{channel.slice(0, 8)}</code>
              <XGlyph size="xs" aria-hidden="true" />
            </span>
          </FilterChip>
        )}
        <span className="spacer" />
      </div>

      {/* A NODE THAT DID NOT ANSWER is named where the rows are drawn: the
          log is every node's, read at query time (ADR-0021), and a short
          answer that did not say so would read exactly like a quiet company. */}
      <CoverageNote coverage={[series.data?.coverage, ...pageCoverage]} what="this log" />

      {/* THE CLOSED SET, so a category with nothing in it is still offered:
          that says the category exists and is quiet, which is an answer — and
          a key the engine omits is exactly that zero. */}
      <FacetRail
        name="Category"
        value={category}
        onChange={setCategory}
        over="window"
        facets={CATEGORIES.map((c) => ({
          value: c,
          label: c,
          // THE ENGINE'S, over the whole window, with the category filter
          // lifted — so a chip says how many rows choosing it would show.
          // Counted over the rows this tab happened to be holding, a log
          // reporting nine thousand events in its window offered a `system`
          // chip reading 0, which is a false statement about somebody's
          // company however carefully the caption hedged it. Null while the
          // axis is still loading, because "0" is a claim and "we do not
          // know yet" is the truth.
          count: series.data ? (series.data.by_category[c] ?? 0) : null,
          title: series.data?.by_category[c]
            ? undefined
            : "No events of this category are in this window — the category still exists.",
        }))}
      />

      <Card padding="none">
        {rows.length ? (
          <div className="list">
            {rows.map((ev, i) => (
              // THE DATE, once per day, AS A BAND. A list that pages back a
              // month rendered every row as a bare wall clock, so 09:14 on the
              // fourteenth and 09:14 three weeks earlier were the same string
              // in the same column — which is what `EventRow`'s `showDate`
              // was for, and it answered it in the wrong place. The prop put
              // a full `fmtDateTime` in the 62px track a wall clock is sized
              // for, so one row per day wrapped to three lines and the feed
              // read as a rendering fault rather than as a date marker. A
              // date is a property of the ROWS UNDER IT rather than of the
              // first of them, so it is a heading between days: the column
              // stays right for the ninety-nine per cent, and the one row
              // that has something extra to say says it at full width.
              <Fragment key={ev.id}>
                {(i === 0 || dayKey(rows[i - 1]!.timestamp) !== dayKey(ev.timestamp)) && (
                  <div className="list-day">{dayKey(ev.timestamp)}</div>
                )}
                <EventRow event={ev} />
              </Fragment>
            ))}
          </div>
        ) : (
          // AN EMPTY LIST IS A CLAIM, and only one of the three ways to hold no
          // rows supports it. A page still in flight has not been shown to hold
          // nothing — the mount asks for this window's own first page, so this
          // used to say "Nothing has been published yet" over every load, on a
          // company with thirty days of history. A page the engine REFUSED is
          // the footer's banner to report, and an empty state drawn over it
          // tells a reader their log is gone when nobody managed to read it.
          // Both are absences of an answer; only the third is an answer.
          !paging &&
          !pageError && (
            <QueryState
              error={null}
              loading={false}
              empty={{
                title: filtered
                  ? "Nothing matches these filters"
                  : "Nothing has been published yet",
                // THE SEARCH IS THE ONE FILTER OVER WHAT THIS TAB HOLDS, so it
                // is the one whose empty answer older pages can still change;
                // every other filter was the engine's, over the whole window.
                hint: q.trim()
                  ? "The search reads the rows loaded so far — older rows may still match, so load more history below."
                  : filtered
                    ? "Nothing in this window matches. Widen the range, or clear the filters."
                    : "The log fills as the engine works. A company with no integrations and no schedules has nothing to react to.",
              }}
            />
          )
        )}
        <footer className="panel-foot">
          {pageError ? (
            <QueryState error={pageError} loading={false} />
          ) : exhausted ? (
            // THE END OF WHAT WAS ASKED, NOT OF THE STORE. Pages are bounded
            // by the window and narrowed by the filters, so "exhausted" means
            // nothing older in THIS window matches — it said "the beginning of
            // the retained history" under two rows of a filtered 24-hour log
            // over a store that keeps thirty days.
            <span>
              {filtered
                ? "No older event in this window matches these filters."
                : "That is the oldest event in this window."}
            </span>
          ) : (
            <>
              <Button
                size="small"
                variant="secondary"
                onClick={() => void loadOlder()}
                disabled={paging}
              >
                {paging ? "Loading…" : `Load ${PAGE} older`}
              </Button>
              <span className="spacer" />
              <span>
                {older.length > 0 && `${older.length} older rows fetched · `}
                {eventHistoryLabel(engine?.event_history_seconds)}
              </span>
            </>
          )}
        </footer>
      </Card>
      {paging && <Skeleton variant="text" rows={3} label="Loading older events" />}
    </>
  );
}
