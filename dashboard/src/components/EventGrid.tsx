/**
 * The event log as a table: when, who, what happened, its category and where
 * it came from, grouped under one band per day.
 *
 * It was a hand-built ledger (`.feed-row`, four fixed tracks and no head row),
 * so its columns had no names, nothing gave way at a narrow width and a cut
 * sentence was read by its title alone. On the kit's grid the head names every
 * column, the facts give way in their `drop` order before the sentence does,
 * and the foot says which went.
 *
 * NO HEAD SORTS. The rows are the engine's walk of the log, newest first (or a
 * channel's, oldest first), paged by a cursor: a head that re-sorted the pages
 * loaded would answer a different question under the same arrow.
 *
 * A card's one-line row is `EventRow compact`, beside the other cards on Live
 * and Home.
 *
 * # A row is drawn when IT changes, never when the log does
 *
 * The log is fed by the socket, one event at a time, and read beside a clock
 * that ticks every second. The grid read that clock itself, for the relative
 * time in each row's hover title, so every row the log held was drawn again
 * once a second; and every push, a new list with one new row, drew every row
 * again too. Five hundred rows of ten thousand elements, redrawn on every
 * tick and every push, held the browser's main thread for seconds at a time.
 *
 * So the grid holds no clock and is `memo`, its columns are one constant set,
 * and every cell is a `memo` component of its row alone: a push draws the new
 * row's cells and nothing else, and a tick draws only the relative times whose
 * words changed (`useClock`). How many rows a screen hands it is the screen's
 * to bound (the event log draws a page at a time, `useGridPage`).
 */

import { memo, useMemo, type ReactNode } from "react";
import { EmptyValue, TableTextCell, type DataGridBand, type DataGridColumn } from "@crewlethq/ui";
import { TriangleAlertGlyph } from "@crewlethq/icons/glyphs";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { href } from "~/app/router.tsx";
import { fmtDate, fmtDateTime, fmtTime, humanize, relTime } from "~/lib/format.ts";
import { useClock } from "~/lib/clock.ts";
import type { FeedRow } from "~/protocol/index.ts";
import { withoutActor } from "~/lib/words.ts";
import { delegatedRuleOf } from "~/lib/delegation.ts";
import { DelegatedMark } from "./Delegation.tsx";

/**
 * Which day a row belongs to, AS ITS BAND DRAWS IT: the key is the label, in
 * the zone the reader chose (`lib/prefs.ts`), so two rows share a band exactly
 * when they would draw the same heading.
 */
export function eventDay(ts: string): string {
  return fmtDate(ts);
}

/** The rows in their given order, cut into one band per day. */
export function eventDayBands(rows: readonly FeedRow[]): DataGridBand<FeedRow>[] {
  const bands: DataGridBand<FeedRow>[] = [];
  for (const row of rows) {
    const day = eventDay(row.timestamp);
    const last = bands[bands.length - 1];
    if (last && last.key === day) last.rows.push(row);
    else bands.push({ key: day, label: day, rows: [row] });
  }
  return bands;
}

/** Who the row is about: the engine where the event names nobody. */
export function eventActor(event: FeedRow): string {
  return event.actor || "engine";
}

/** Where the event came from, where that is not its actor (said once, not twice). */
export function eventSource(event: FeedRow): string {
  return event.source && event.source !== eventActor(event) ? event.source : "";
}

/**
 * What happened, said beside the Actor column that names who did it: the
 * engine's sentence without the actor it opens with.
 */
export function eventWhat(event: FeedRow): string {
  return withoutActor(event.summary || "", eventActor(event)) || event.type;
}

/** A row's cell, drawn again only when its row is a different row. */
type CellOf = (props: { event: FeedRow }) => ReactNode;

/**
 * The time: a wall clock, the day being the band's. Its hover title carries
 * the full instant and how long ago it was, read off the shared clock HERE,
 * so a tick draws the rows whose "5m ago" moved rather than the whole grid.
 */
const TimeCell: CellOf = memo(function TimeCell({ event }) {
  const ago = useClock((now) => relTime(event.timestamp, now));
  return (
    <time
      className="t-num"
      dateTime={event.timestamp}
      title={`${fmtDateTime(event.timestamp)} · ${ago}`}
    >
      {fmtTime(event.timestamp)}
    </time>
  );
});

const ActorCell: CellOf = memo(function ActorCell({ event }) {
  return <TableTextCell>{eventActor(event)}</TableTextCell>;
});

const WhatCell: CellOf = memo(function WhatCell({ event }) {
  const rule = delegatedRuleOf(event);
  return (
    <span className="row gap-1">
      {event.failed && (
        <TriangleAlertGlyph
          size="xs"
          aria-label="failed"
          style={{ color: "var(--color-feedback-danger-ink)", flex: "none" }}
        />
      )}
      <span className="truncate">{eventWhat(event)}</span>
      {/* AN ANSWER A DELEGATE GAVE says so, with its rule (ADR-0125). */}
      {rule !== null && <DelegatedMark rule={rule} />}
    </span>
  );
});

const CategoryCell: CellOf = memo(function CategoryCell({ event }) {
  return <TableTextCell>{humanize(event.category) || "system"}</TableTextCell>;
});

const SourceCell: CellOf = memo(function SourceCell({ event }) {
  const source = eventSource(event);
  return source ? (
    <TableTextCell>{source}</TableTextCell>
  ) : (
    <EmptyValue label="Published by its actor" />
  );
});

/**
 * ONE SET, for every render and every grid: the kit keys its column fit on
 * this array, and a fresh one each render re-derived it for every push.
 */
const COLUMNS: DataGridColumn<FeedRow>[] = [
  {
    key: "time",
    header: "Time",
    align: "right",
    shrink: true,
    cell: (e) => <TimeCell event={e} />,
  },
  {
    key: "actor",
    header: "Actor",
    shrink: true,
    drop: 3,
    cell: (e) => <ActorCell event={e} />,
  },
  {
    key: "what",
    header: "What happened",
    // THE POINT OF THE LOG, so every fact gives way before it does.
    floor: "12rem",
    overflow: "truncate",
    phoneLead: true,
    title: (e) => eventWhat(e),
    cell: (e) => <WhatCell event={e} />,
  },
  {
    key: "category",
    header: "Category",
    shrink: true,
    drop: 2,
    cell: (e) => <CategoryCell event={e} />,
  },
  {
    key: "source",
    header: "Source",
    shrink: true,
    drop: 1,
    phoneOmit: true,
    cell: (e) => <SourceCell event={e} />,
  },
];

const eventKey = (e: FeedRow) => e.id;
const eventHref = (e: FeedRow) => href(["live", "events", e.id]);
const eventFailed = (e: FeedRow) => Boolean(e.failed);

export const EventGrid = memo(function EventGrid({
  events,
  name,
  empty,
  footer,
  label,
}: {
  events: readonly FeedRow[];
  /** Which grid on the screen, for its column set in the address. */
  name?: string;
  empty?: { title: ReactNode; hint?: ReactNode };
  footer?: ReactNode;
  label: string;
}) {
  const bands = useMemo(() => eventDayBands(events), [events]);
  return (
    <DataGrid<FeedRow>
      name={name}
      aria-label={label}
      flush
      bands={bands}
      rowKey={eventKey}
      serverSorted
      rowHref={eventHref}
      isFailed={eventFailed}
      phoneRows="compact"
      empty={empty}
      footer={footer}
      columns={COLUMNS}
    />
  );
});
