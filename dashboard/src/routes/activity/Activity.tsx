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
 */

import { Fragment, useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useParam } from "~/app/router.tsx";
import { EventRow, QueryState } from "~/components/common.tsx";
import { Button, Card, FilterChip, Input, Skeleton, Tag } from "@crewlethq/ui";
import { CloseGlyph, SearchGlyph, TimelineGlyph } from "@crewlethq/icons/glyphs";
import { useClient, useEngineHealth, useEvents } from "~/lib/store-hooks.ts";
import { eventHistoryLabel, fmtDate, newestFirst, plural, tsKey } from "~/lib/format.ts";
import type { FeedRow } from "~/protocol/index.ts";
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
import type { Offer, Range } from "~/lib/range.ts";
import { TimeRangePicker } from "~/ui/TimeRange.tsx";
import { Histogram } from "~/ui/Histogram.tsx";
import { FacetRail } from "~/ui/FacetRail.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";

/**
 * The categories the engine assigns, as a CLOSED set.
 *
 * Mirrors `events.CategoryNames()`, which returns EIGHT. This list held ten:
 * `communication` and `knowledge` are categories no event is registered
 * under, so two of the chips could never match a row and the reader was
 * invited to filter a log down to nothing and conclude the engine was quiet.
 * A chip for a category with nothing in it is still useful — it says the
 * category exists and is quiet — but only where the category exists.
 */
const CATEGORIES = [
  "a2a",
  "decision",
  "learning",
  "lifecycle",
  "notification",
  "system",
  "task",
  "webhook",
] as const;

/**
 * One page of the log, the first and every older one alike.
 *
 * THE ENGINE'S OWN DEFAULT (`queries.DefaultEventPage`), which it sizes to one
 * screen of this feed: larger spends a round trip on rows nobody scrolls to,
 * and smaller makes the first scroll a second query.
 */
const PAGE = 100;

/**
 * The window that reaches the whole log: `store.EventHistory`, the floor every
 * read of the event log stops at, so no row a read can return is older.
 *
 * WHAT A LINK MEANING "THE REST OF THESE ROWS" OPENS ON. The log's own
 * fallback is a day, and a list elsewhere that is cut to its newest rows has
 * no time bound — so a link that lets the fallback stand lands on a day that
 * holds none of the rows it promises whenever they are older than that.
 */
export const WHOLE_LOG: Range = "30d";

/**
 * WHICH WINDOWS THE LOG HAS.
 *
 * The whole vocabulary bar the two shortest, and a custom interval: an event
 * log is asked "what just happened" and "what happened last Tuesday" in the
 * same breath, and both are questions about the store rather than about what
 * this tab is holding. `1h` is the short end because that is where the minute
 * bucket stops being drawable — sixty bars — and the long end is [WHOLE_LOG]:
 * a `90d` offer would be a window two thirds of which can never have rows in
 * it. The engine reports its actual floor (`event_history_seconds`) and the
 * footer says what it reported; this offer is a fixed vocabulary of windows
 * rather than a claim about retention.
 */
const LOG_OFFER: Offer = {
  ranges: ["1h", "6h", "1d", "7d", WHOLE_LOG],
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
  const { socket } = useClient();
  const liveEvents = useEvents();
  const { data: engine } = useEngineHealth();
  const now = useNow();
  const [category, setCategory] = useParam("category", "");
  const [actor, setActor] = useParam("actor", "");
  // WHICH SURFACE PUBLISHED IT, compared for equality on the server like the
  // actor. An integration's own page links here with it, so "older deliveries
  // on Slack" is a page of Slack's deliveries rather than every webhook the
  // company has received, with Slack's somewhere among them.
  const [source, setSource] = useParam("source", "");
  // ONE TRACE, compared for equality on the server — the trace screen links
  // here with it. The search box cannot stand in for it: it matches a row's
  // summary, type and source, and a trace id is none of those.
  const [trace, setTrace] = useParam("trace", "");
  const [q, setQ] = useParam("q", "");
  const [onlyFailed, setOnlyFailed] = useParam("failed", "");
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
  //
  // AND A PAGE STILL IN FLIGHT FOR THE OLD QUERY LANDS NOWHERE. It answers
  // after this reset, so appending it put the old query's rows back and its
  // cursor in place of the new one's — `walk` names the query a fetch was
  // asked under, and [loadOlder] drops an answer for one that has ended.
  const walk = useRef(0);
  useEffect(() => {
    walk.current += 1;
    setOlder([]);
    setCursor(null);
    setExhausted(false);
    setPaging(false);
    setPageError(null);
    setFetched(false);
  }, [category, actor, source, trace, windowKey]);

  const rows = useMemo(() => {
    const seen = new Set<string>();
    const all = [...liveEvents, ...older].filter((e) => {
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
        .filter((e) => !source || e.source === source)
        .filter((e) => !trace || e.trace_id === trace)
        .filter((e) => !onlyFailed || e.failed)
        .filter(
          (e) =>
            !needle ||
            (e.summary ?? "").toLowerCase().includes(needle) ||
            (e.type ?? "").toLowerCase().includes(needle) ||
            (e.source ?? "").toLowerCase().includes(needle),
        )
        .sort(newestFirst)
    );
  }, [liveEvents, older, category, actor, source, trace, q, onlyFailed, since, until]);

  // THE AXIS IS THE ENGINE'S. This tab holds at most the last 400 events and
  // the store's window it never holds, so a histogram folded here would be
  // right for one window and absent for every other — and it is counted
  // through the same predicate the listing filters with, so a bar can never
  // claim rows the list below it would not show.
  //
  // The SERVER-SIDE filters only. `q` and `failed` are applied in the browser
  // to whatever arrived, so an axis carrying them would be counting a set the
  // engine was never asked about.
  const series = useQuery("event_series", {
    since: axis.since,
    until: axis.until,
    bucket,
    ...(category ? { category } : {}),
    ...(actor ? { actor } : {}),
    ...(source ? { source } : {}),
    ...(trace ? { trace_id: trace } : {}),
  });

  const loadOlder = useCallback(async () => {
    const asked = walk.current;
    setPaging(true);
    setPageError(null);
    try {
      // The cursor names BOTH halves. The engine reads `before_time` and
      // `before_id`; a client sending one bare `before` gets every page
      // rejected with `query_failed`.
      const params: Record<string, unknown> = { limit: PAGE, since, until };
      if (category) params.category = category;
      if (actor) params.actor = actor;
      if (source) params.source = source;
      if (trace) params.trace_id = trace;
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
      if (asked !== walk.current) return;
      setOlder((prev) => [...prev, ...(page.events ?? [])]);
      setCursor(page.next ?? null);
      setExhausted(page.exhausted || !page.next);
    } catch (err) {
      if (asked !== walk.current) return;
      setPageError(err instanceof Error ? err.message : "query_failed");
    }
    setPaging(false);
  }, [socket, cursor, rows, category, actor, source, trace, since, until]);

  // THE FIRST PAGE OF THE WINDOW, once per window. `loadOlder` is a
  // dependency and changes with every render that changes `rows`, so the
  // `fetched` flag is what makes this once rather than a loop — a plain
  // dependency on the callback would refetch on its own answer.
  useEffect(() => {
    if (fetched) return;
    setFetched(true);
    void loadOlder();
  }, [fetched, loadOlder]);

  const filtered = !!(category || actor || source || trace || q || onlyFailed);

  return (
    <>
      <PageActions>
        <Tag appearance="outline">{windowLabel(range.window)}</Tag>
        <TimeRangePicker range={range} ariaLabel="Window" />
        {filtered ? (
          <Button
            leadingIcon={<CloseGlyph size="xs" />}
            size="small"
            variant="secondary"
            onClick={() => {
              setCategory("");
              setActor("");
              setSource("");
              setTrace("");
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
        last 400 in memory; older rows are fetched.{" "}
        {eventHistoryLabel(engine?.event_history_seconds)}.
      </PageNote>

      <Card>
        <Card.Header
          icon={<TimelineGlyph size="sm" />}
          subtitle={
            series.data
              ? `${plural(series.data.total, "event")} over ${spanWords(series.data.since, series.data.until)}`
              : undefined
          }
          actions={
            series.data ? (
              <span className="t-caption">
                one bar per {series.data.bucket} — click one to narrow the window
              </span>
            ) : undefined
          }
        >
          <Card.Title>When</Card.Title>
        </Card.Header>
        <QueryState
          error={series.error}
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
        <Input
          type="search"
          value={source}
          onChange={(e) => setSource(e.target.value)}
          aria-label="Filter by source"
          placeholder="Source"
          leading={<SearchGlyph size="sm" />}
          width="sm"
        />
        <FilterChip pressed={!!onlyFailed} onPressedChange={(on) => setOnlyFailed(on ? "1" : "")}>
          Failures only
        </FilterChip>
        {/* THE TRACE, on screen while it narrows the log: a filter the reader
            cannot see is a filter they cannot lift. No box sets it — a trace
            id is something a link carries, not something anybody types. */}
        {trace && (
          <FilterChip pressed onPressedChange={(on) => !on && setTrace("")}>
            Trace <span className="mono">{trace}</span>
          </FilterChip>
        )}
        <span className="spacer" />
      </div>

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
                hint: filtered
                  ? "Older rows may still match — load more history below."
                  : "The log fills as the engine works. A company with no integrations and no schedules has nothing to react to.",
              }}
            />
          )
        )}
        <footer className="panel-foot">
          {pageError ? (
            <QueryState error={pageError} loading={false} />
          ) : exhausted ? (
            <span>That is the beginning of the retained history.</span>
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
