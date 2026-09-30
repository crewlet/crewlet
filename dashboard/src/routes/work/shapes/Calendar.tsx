/**
 * The calendar shape: a month, and what is due on each of its days.
 *
 * ITS AXIS IS THE `due` KEY, which the query grammar has exactly one of — so
 * the grid's own window spends it, the Overdue chip is not offered on this
 * shape, and the toolbar's count says what it counted. `lib/work.ts`'s
 * [buildItemsParams] carries the whole of that reasoning.
 *
 * A CELL IS A DAY AND A DAY IS BOUNDED. Three chips and then a count, because
 * a cell that grew with its day would make one busy Tuesday as tall as the
 * week beside it; the count is a link into the list, where the day is the
 * whole screen.
 */

import { useMemo } from "react";
import { Button, Card, IconButton } from "@crewlethq/ui";
import { ChevronLeftGlyph, ChevronRightGlyph } from "@crewlethq/icons/glyphs";
import { rowPeekHandler } from "~/app/frame/DetailRail.tsx";
import { TypeIcon, type RowChrome } from "~/components/work.tsx";
import {
  bucketByDay,
  CALENDAR_CELL_CHIPS,
  dayLabel,
  monthLabel,
  shiftMonth,
  WEEKDAYS,
  type CalendarCell,
} from "~/lib/work.ts";
import type { WorkSummary } from "~/protocol/index.ts";

export function CalendarView({
  weeks,
  rows,
  month,
  chrome,
  hrefOf,
  onOpen,
  onMonth,
  onToday,
}: {
  weeks: CalendarCell[][];
  rows: WorkSummary[];
  month: string;
  chrome: RowChrome;
  hrefOf: (row: WorkSummary) => string;
  onOpen: (row: WorkSummary) => void;
  onMonth: (month: string) => void;
  onToday: () => void;
}) {
  const buckets = useMemo(() => bucketByDay(rows), [rows]);
  return (
    <Card padding="none">
      <div className="work-cal">
        <div className="work-cal-nav">
          {/* AN ICON WITH NO LABEL IS AN `IconButton` OVER THERE, and the name
              is a required prop rather than a `title` a caller remembers. */}
          <IconButton
            size="sm"
            variant="ghost"
            icon={<ChevronLeftGlyph size="sm" />}
            label="The month before"
            title="The month before"
            onClick={() => onMonth(shiftMonth(month, -1))}
          />
          <span className="work-cal-month">{monthLabel(month)}</span>
          <IconButton
            size="sm"
            variant="ghost"
            icon={<ChevronRightGlyph size="sm" />}
            label="The month after"
            title="The month after"
            onClick={() => onMonth(shiftMonth(month, 1))}
          />
          <Button size="small" variant="tertiary" onClick={onToday}>
            Today
          </Button>
        </div>
        <div className="work-cal-grid">
          {WEEKDAYS.map((day) => (
            <div className="work-cal-dow" key={day}>
              {day}
            </div>
          ))}
          {weeks.flat().map((cell) => {
            const due = buckets.get(cell.key) ?? [];
            const shownChips = due.slice(0, CALENDAR_CELL_CHIPS);
            return (
              <div
                className={`work-cal-cell${cell.inMonth ? "" : " out"}${cell.today ? " today" : ""}`}
                key={cell.key}
              >
                {/* THE SAME FACT THE TINT CARRIES, IN WORDS. Which month a
                    cell belongs to is a ground and an ink, and two cells of one
                    grid can hold the same numeral — April 2031 runs 31 March to
                    4 May, so `1` and `4` each appear twice — so the date is
                    spoken in full and the numeral is left to the eye. `sr-only`
                    is out of flow, so it adds no gap to the column. */}
                <span className="sr-only">{dayLabel(cell.key)}</span>
                <span className="work-cal-day" aria-hidden="true">
                  {cell.day}
                </span>
                {shownChips.map((row) => (
                  <a
                    key={row.id}
                    className="work-cal-chip"
                    data-overdue={row.overdue ? "true" : undefined}
                    data-done={
                      row.status_group === "done" || row.status_group === "closed"
                        ? "true"
                        : undefined
                    }
                    href={hrefOf(row)}
                    title={`${row.key} · ${row.title}`}
                    // THE FRAME'S ONE COPY of which clicks mean "open
                    // elsewhere". This chip carried its own — four modifiers
                    // and no `button` test — which is exactly the drift
                    // `rowPeekHandler` was written to end: the second spelling
                    // is the one that forgets the middle button.
                    onClick={rowPeekHandler(() => onOpen(row))}
                  >
                    <TypeIcon type={row.type} types={chrome.types} />
                    <span className="work-cal-key">{row.key}</span>
                    <span className="truncate">{row.title}</span>
                  </a>
                ))}
                {due.length > shownChips.length && (
                  <span className="work-cal-more">+{due.length - shownChips.length} more</span>
                )}
              </div>
            );
          })}
        </div>
        {/* THERE IS NO `due=null` IN THE GRAMMAR, so the count of undated work
            is what this answer happens to carry rather than the company's. It
            is said all the same: a reader who cannot see the omission reads
            the month as the whole backlog.

            WHAT THIS GRID IS NOT SHOWING IS TWO SETS rather than one:
            [buildItemsParams] bounds the fetch to these days, so work due
            OUTSIDE them is missing as well as work with no due date at all.
            Stated unconditionally, because neither count is ours to give —
            `due=range:` compiles to `due_at IS NOT NULL AND due_at >= ? AND
            due_at < ?`, so neither set is in this answer and the grammar has no
            `due=null` to ask for one. This counted the undated rows ON THE PAGE
            and printed the number when it was positive: it never was, and never
            could be, so the sentence a reader needed was the one that never
            rendered. The toolbar's count says the same thing from the other end
            — see [countedLabel]. */}
        <div className="work-cal-note">
          Only work due in this window appears here. Work with no due date, and work due outside
          these days, is on the list.
        </div>
      </div>
    </Card>
  );
}
