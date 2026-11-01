/**
 * A screen's `window=` as the kit's `TimeRangePicker` takes it.
 *
 * Every screen with a range had its own control once: Spend's picker in whole
 * days, the live strip's `STRIP_MINUTES` constant, and the event log's
 * implicit "whatever happens to be loaded". So three screens showing the same
 * hour disagreed about how long an hour was. The control is the kit's now, and
 * it never interprets a window: it hands back the option chosen or the two
 * strings typed. What a window MEANS on this engine (a company day cut at
 * midnight on the company's clock, an instant read on the reader's chosen
 * zone) is this module's, so it is written once for every screen rather than
 * once per caller.
 *
 * THE OFFERED SET IS THE SCREEN'S, not the whole vocabulary, and it is the
 * SAME declaration the screen's `useTimeRange` reads, carried on the range
 * itself: two lists would be two answers to "which windows does this screen
 * have". The options are drawn in the VOCABULARY'S own order, so two screens
 * offering overlapping sets still read left to right the same way.
 */

import { TIME_RANGE_CUSTOM, type TimeRangePickerProps } from "@crewlethq/ui";
import {
  RANGES,
  RANGE_LABEL,
  TODAY,
  companyDays,
  dayStartIn,
  daysInterval,
  daysLabel,
  isRange,
  todayWindow,
  windowLabel,
  type Range,
  type TimeRange,
} from "./range.ts";
import { fromWall, toWall, tsKey } from "./format.ts";

/** What a segment of a screen's range control names: a duration, or today. */
export type RangeChoice = Range | typeof TODAY;

/**
 * The kit picker's props for a screen's range, with `label` naming the group.
 *
 * `now` is when Today is pressed, read at the press; a suite passes a clock.
 */
export function rangePickerProps(
  range: TimeRange,
  label = "Time range",
  now: () => number = Date.now,
): TimeRangePickerProps<RangeChoice> {
  const { window, offer, set } = range;
  const options: TimeRangePickerProps<RangeChoice>["options"][number][] = [];
  // TODAY LEADS, because it is the shortest window a screen offering it has
  // and the vocabulary's order is shortest first.
  if (offer.today) {
    options.push({ value: TODAY, label: "Today", title: "Since midnight on the company's clock" });
  }
  for (const r of RANGES) {
    if (offer.ranges.includes(r)) options.push({ value: r, label: r, title: RANGE_LABEL[r] });
  }
  const value = isRange(window) ? window : window.today ? TODAY : TIME_RANGE_CUSTOM;
  return {
    options,
    value,
    label,
    onChange: (next) => set(next === TODAY ? todayWindow(now(), offer.zone) : next),
    custom: offer.custom ? customWindow(range) : undefined,
  };
}

/** The Custom segment: prefilled with the window on screen, read on the right clock. */
function customWindow({ window, offer, since, until, set }: TimeRange) {
  const from = tsKey(since);
  const to = tsKey(until);
  const zone = offer.zone;
  const named = !isRange(window) && !window.today;
  if (offer.customDays) {
    // A SCREEN WHOSE WINDOWS ARE WHOLE COMPANY DAYS: two dates on the
    // company's clock, both inclusive. The last day is the one the window's
    // final instant falls on, since the end is exclusive.
    const days = companyDays({ from, to }, zone);
    return {
      mode: "days" as const,
      // THE CHOSEN INTERVAL IS THE TOOLTIP, not the label: two dates do not
      // fit in a segment, and a segment whose width moved with its value
      // would reflow the toolbar beside it every time.
      title: named ? daysLabel(companyDays(window, zone)) : "Pick two dates",
      start: days.since,
      end: days.until,
      helper: `Company days, in ${zone || "UTC"}.`,
      validate: (start: string, end: string) =>
        dayStartIn(start, zone) === null
          ? "The first day is not a date."
          : dayStartIn(end, zone) === null
            ? "The last day is not a date."
            : undefined,
      onApply: (start: string, end: string) => {
        const picked = daysInterval(start, end, zone);
        if (picked) set(picked);
      },
    };
  }
  // TWO INSTANTS, typed as wall clock in the READER'S chosen zone, which is
  // what [fromWall] reads: a `datetime-local` is always the browser's, and
  // this dashboard renders in whichever zone the reader picked.
  return {
    mode: "instants" as const,
    title: named ? windowLabel(window) : "Name two instants of your own",
    start: toWall(from),
    end: toWall(to),
    helper: "In your time zone.",
    validate: (start: string, end: string) => {
      const at = fromWall(start);
      const till = fromWall(end);
      if (at === null) return "The start is not a date and time.";
      if (till === null) return "The end is not a date and time.";
      return till <= at ? "The end must be after the start." : undefined;
    },
    onApply: (start: string, end: string) => {
      const at = fromWall(start);
      const till = fromWall(end);
      if (at !== null && till !== null && till > at) set({ from: at, to: till });
    },
  };
}
