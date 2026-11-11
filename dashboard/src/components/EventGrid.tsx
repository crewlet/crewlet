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
 */

import { useMemo, type ReactNode } from "react";
import { EmptyValue, TableTextCell, type DataGridBand } from "@crewlethq/ui";
import { TriangleAlertGlyph } from "@crewlethq/icons/glyphs";
import { DataGrid } from "~/app/frame/DataGrid.tsx";
import { href } from "~/app/router.tsx";
import { fmtDate, fmtDateTime, fmtTime, humanize, relTime } from "~/lib/format.ts";
import { useNow } from "~/lib/clock.ts";
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

export function EventGrid({
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
  const now = useNow();
  const bands = useMemo(() => eventDayBands(events), [events]);
  return (
    <DataGrid<FeedRow>
      name={name}
      aria-label={label}
      flush
      bands={bands}
      rowKey={(e) => e.id}
      serverSorted
      rowHref={(e) => href(["live", "events", e.id])}
      isFailed={(e) => Boolean(e.failed)}
      phoneRows="compact"
      empty={empty}
      footer={footer}
      columns={[
        {
          key: "time",
          header: "Time",
          align: "right",
          shrink: true,
          // A WALL CLOCK: the day is the band's, the full instant the title's.
          title: (e) => `${fmtDateTime(e.timestamp)} · ${relTime(e.timestamp, now)}`,
          cell: (e) => (
            <time className="t-num" dateTime={e.timestamp}>
              {fmtTime(e.timestamp)}
            </time>
          ),
        },
        {
          key: "actor",
          header: "Actor",
          shrink: true,
          drop: 3,
          cell: (e) => <TableTextCell>{eventActor(e)}</TableTextCell>,
        },
        {
          key: "what",
          header: "What happened",
          // THE POINT OF THE LOG, so every fact gives way before it does.
          floor: "12rem",
          overflow: "truncate",
          phoneLead: true,
          title: (e) => eventWhat(e),
          cell: (e) => (
            <span className="row gap-1">
              {e.failed && (
                <TriangleAlertGlyph
                  size="xs"
                  aria-label="failed"
                  style={{ color: "var(--color-feedback-danger-ink)", flex: "none" }}
                />
              )}
              <span className="truncate">{eventWhat(e)}</span>
              {/* AN ANSWER A DELEGATE GAVE says so, with its rule (ADR-0125). */}
              {delegatedRuleOf(e) !== null && <DelegatedMark rule={delegatedRuleOf(e)!} />}
            </span>
          ),
        },
        {
          key: "category",
          header: "Category",
          shrink: true,
          drop: 2,
          cell: (e) => <TableTextCell>{humanize(e.category) || "system"}</TableTextCell>,
        },
        {
          key: "source",
          header: "Source",
          shrink: true,
          drop: 1,
          phoneOmit: true,
          cell: (e) => {
            const source = eventSource(e);
            return source ? (
              <TableTextCell>{source}</TableTextCell>
            ) : (
              <EmptyValue label="Published by its actor" />
            );
          },
        },
      ]}
    />
  );
}
