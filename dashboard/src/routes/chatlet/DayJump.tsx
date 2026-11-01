/**
 * Moving through a room by date. Every day's pill is a Jump to menu: the most
 * recent messages (Today, when the newest is today's), a week back and a
 * month back where the room's history reaches that far, the very beginning,
 * or a date picked on a calendar. A jump to a date lands on the first message
 * from that day on, with its day's rule and pill at the top of the view.
 *
 * A JUMP READS WHAT IT NEEDS FIRST. A room is held from its newest page back,
 * so a jump past what is held pages back until it gets there
 * (`Transcript.tsx`'s jumps); the menu only says where to.
 */

import { useState } from "react";

import {
  CalendarGrid,
  ChatDayPill,
  IconButton,
  Menu,
  Modal,
  compareDays,
  shiftMonth,
  type CalendarDate,
  type MenuEntry,
} from "@crewlethq/ui";
import { CalendarGlyph, ChevronLeftGlyph, ChevronRightGlyph } from "@crewlethq/icons/glyphs";
import { civilAt, civilKey } from "~/lib/format.ts";
import { dateTimeFormat, type DateShape } from "~/lib/formatters.ts";

import { daysBefore, daysBetween } from "./days.ts";

/** How far back "Last week" and "Last month" go, in days. */
export const WEEK_DAYS = 7;
export const MONTH_DAYS = 30;

/** Where a room can be moved to. */
export interface Jumps {
  /** The newest messages, at the foot of the list. */
  latest: () => void;
  /** The top of the room: its intro and its first message. */
  beginning: () => void;
  /** The first message from the civil day `key` on, or the newest when none is that recent. */
  toDay: (key: string) => void;
}

/** A civil `YYYY-MM-DD` key as the calendar's date, and back. */
function asDate(key: string): CalendarDate {
  const at = civilAt(key) ?? 0;
  const d = new Date(at);
  return { year: d.getUTCFullYear(), month: d.getUTCMonth(), day: d.getUTCDate() };
}

function asKey(date: CalendarDate): string {
  return civilKey(Date.UTC(date.year, date.month, date.day));
}

/** A day's pill as a Jump to menu. */
export function DayJump({
  label,
  day,
  today,
  newest,
  first,
  jumps,
}: {
  label: string;
  /** This pill's own day, which the calendar opens on. */
  day: string;
  today: string;
  /** The newest message's day, or "" in an empty room. */
  newest: string;
  /** The day the room's history starts on: the room's own creation. */
  first: string;
  jumps: Jumps;
}) {
  const [picking, setPicking] = useState(false);
  // A WEEK OR A MONTH BACK IS OFFERED ONLY WHERE THE ROOM GOES BACK THAT FAR:
  // a jump that could only land on the room's first message is The very
  // beginning, which is already there.
  const reaches = (days: number) => first !== "" && daysBetween(first, today) > days;
  const items: MenuEntry[] = [
    newest === today
      ? { key: "today", label: "Today", onSelect: () => jumps.toDay(today) }
      : { key: "latest", label: "Most recent", onSelect: jumps.latest },
    ...(reaches(WEEK_DAYS)
      ? [
          {
            key: "week",
            label: "Last week",
            onSelect: () => jumps.toDay(daysBefore(today, WEEK_DAYS)),
          },
        ]
      : []),
    ...(reaches(MONTH_DAYS)
      ? [
          {
            key: "month",
            label: "Last month",
            onSelect: () => jumps.toDay(daysBefore(today, MONTH_DAYS)),
          },
        ]
      : []),
    { key: "beginning", label: "The very beginning", onSelect: jumps.beginning },
    { kind: "separator", key: "s" },
    { key: "date", label: "Jump to a specific date", onSelect: () => setPicking(true) },
  ];
  return (
    <>
      <Menu
        label={`${label}. Jump to a date`}
        align="start"
        header={<span className="chat-jump-head">Jump to…</span>}
        renderTrigger={(props) => <ChatDayPill {...props} label={label} />}
        items={items}
      />
      {picking && (
        <JumpToDate
          day={day}
          today={today}
          onClose={() => setPicking(false)}
          onPick={(key) => {
            setPicking(false);
            jumps.toDay(key);
          }}
        />
      )}
    </>
  );
}

const MONTH_TITLE: DateShape = Object.freeze({ month: "long", year: "numeric" });

/** A calendar for one day: picking it jumps there. Days after today are not offered. */
function JumpToDate({
  day,
  today,
  onClose,
  onPick,
}: {
  day: string;
  today: string;
  onClose: () => void;
  onPick: (key: string) => void;
}) {
  const start = asDate(day);
  const latest = asDate(today);
  const [focused, setFocused] = useState<CalendarDate>(start);
  const [view, setView] = useState({ year: start.year, month: start.month });
  const atLatest = view.year === latest.year && view.month === latest.month;
  const turn = (months: number) => {
    const next = shiftMonth({ ...view, day: 1 }, months);
    setView({ year: next.year, month: next.month });
  };
  return (
    <Modal
      open
      onClose={onClose}
      size="sm"
      icon={<CalendarGlyph size="md" />}
      title="Jump to a specific date"
      subtitle="Go to the first message from that day on."
      // THE CALENDAR TAKES FOCUS ON ITS CHOSEN DAY, so the arrows move through
      // days at once, as the grid's own keys are built for.
      initialFocus={() =>
        document.querySelector<HTMLElement>('.chat-jump-date [role="grid"] [tabindex="0"]')
      }
    >
      <div className="chat-jump-date">
        <div className="chat-jump-date-head">
          <IconButton
            label="Previous month"
            size="sm"
            variant="ghost"
            icon={<ChevronLeftGlyph size="sm" />}
            onClick={() => turn(-1)}
          />
          <span className="chat-jump-date-month" aria-live="polite">
            {dateTimeFormat("en-US", "UTC", MONTH_TITLE).format(
              new Date(Date.UTC(view.year, view.month, 1)),
            )}
          </span>
          <IconButton
            label="Next month"
            size="sm"
            variant="ghost"
            icon={<ChevronRightGlyph size="sm" />}
            onClick={() => turn(1)}
            disabledReason={atLatest ? "There are no messages after this month yet." : undefined}
          />
        </div>
        <CalendarGrid
          label="Choose a date"
          year={view.year}
          month={view.month}
          focused={focused}
          onFocusedChange={setFocused}
          onMonthChange={(year, month) => setView({ year, month })}
          selected={[start]}
          isDisabled={(date) => compareDays(date, latest) > 0}
          onSelect={(date) => onPick(asKey(date))}
        />
      </div>
    </Modal>
  );
}
