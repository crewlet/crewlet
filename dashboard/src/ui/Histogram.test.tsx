/**
 * The two things a bar chart says that a bar cannot draw.
 *
 * A HISTOGRAM WITH NO DATES UNDER IT IS NOT AN AXIS. A seven-day window
 * holding one busy day rendered a single bar hard against the right edge of an
 * empty box with nothing beneath it, and the screenshot that caught it was
 * filed as a rendering failure rather than as a quiet company. The cases here
 * hold the labels to the buckets they name, because a tick that drifts off its
 * own bar is worse than none: it is a date a reader will act on.
 *
 * AND THE SENTENCE NOBODY SEES IS STILL A CLAIM. The one statement this
 * component makes to a screen reader announced a page count as a window count
 * in the wrong noun — "3 events in this window" over a tracker chart whose
 * visible caption said three changes on one page. The noun and the scope are
 * the caller's now, required, for the reason `FacetRail`'s `over` is.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";

import { Histogram, ticksFor, type Bar } from "./Histogram.tsx";
import { fmtDateCompact } from "~/lib/format.ts";

afterEach(cleanup);

const DAY = 24 * 60 * 60_000;
/** A fixed instant, so the compact date's own year rule is not the clock's. */
const NOW = Date.parse("2026-09-22T12:00:00Z");

/** `count` daily buckets ending on the day before [NOW]. */
function days(count: number, counts: number[] = []): Bar[] {
  const first = NOW - (count - 1) * DAY;
  return Array.from({ length: count }, (_, i) => ({
    at: new Date(first + i * DAY).toISOString(),
    count: counts[i] ?? 0,
  }));
}

const ticks = (root: HTMLElement) => [...root.querySelectorAll(".histogram-tick")];

// THE ENDS ARE THE TWO THE WINDOW'S OWN CONTROL ALREADY NAMES, so an axis whose
// first and last labels disagree with the picker above it is the one failure a
// reader cannot diagnose. Both are asserted against the bars they belong to
// rather than against a literal date, because the label is rendered in the
// reader's chosen zone and a literal would pin the test to the runner's.
test("the axis names its first and its last bucket", () => {
  const bars = days(14);
  const { container } = render(
    <Histogram bars={bars} bucket="day" total={0} noun="change" over="loaded" axis now={NOW} />,
  );
  const drawn = ticks(container);
  expect(drawn[0]?.textContent).toBe(fmtDateCompact(bars[0]!.at, NOW));
  expect(drawn[drawn.length - 1]?.textContent).toBe(fmtDateCompact(bars[13]!.at, NOW));
});

// AND THE ONES BETWEEN ARE EVENLY SPREAD OVER THE BARS, not over the calendar:
// a histogram's columns are whatever bucket the window chose, so a rule on
// weeks would put one tick on an axis of sixty minutes.
test("the ticks between the ends are an even spread over the buckets", () => {
  expect(ticksFor(14)).toEqual([0, 3, 7, 10, 13]);
  // THE WIDEST AXIS THIS PRODUCT DRAWS — `lib/range.ts`'s own `MAX_BARS` — still
  // carries five labels and no more.
  expect(ticksFor(90)).toEqual([0, 22, 45, 67, 89]);
  // FEWER BARS THAN LABELS IS NOT FEWER LABELS THAN BARS: every bucket gets its
  // own, and a single bucket gets exactly one rather than two of itself.
  expect(ticksFor(3)).toEqual([0, 1, 2]);
  expect(ticksFor(1)).toEqual([0]);
  expect(ticksFor(0)).toEqual([]);
});

// A TICK SITS OVER THE BUCKET IT NAMES. The labels are wider than the bars they
// point at, so they are positioned rather than laid out in flow — and the
// percentage is the bucket's own CENTRE, because a label at `i / n` names the
// boundary between two buckets rather than either of them. The two ends are
// pinned to the edges instead: centred, they would hang off the card.
test("an interior tick is centred on its own bucket and the ends are pinned", () => {
  const { container } = render(
    <Histogram bars={days(4)} bucket="day" total={0} noun="change" over="loaded" axis now={NOW} />,
  );
  const drawn = ticks(container) as HTMLElement[];
  expect(drawn).toHaveLength(4);
  expect(drawn[0]!.style.left).toBe("0px");
  expect(drawn[3]!.style.right).toBe("0px");
  // Bucket 1 of 4 centres at (1 + 0.5) / 4.
  expect(drawn[1]!.style.left).toBe("37.5%");
  expect(drawn[1]!.style.transform).toBe("translateX(-50%)");
});

// AN HOUR IS NOT A DATE. Five repetitions of today's date under a chart of
// minutes is the noise the compact form exists to remove, and the window is
// named by the range control directly above the chart.
test("a bucket shorter than a day is labelled with a clock", () => {
  const first = Date.parse("2026-09-22T00:00:00Z");
  const bars: Bar[] = Array.from({ length: 6 }, (_, i) => ({
    at: new Date(first + i * 60 * 60_000).toISOString(),
    count: 0,
  }));
  const { container } = render(
    <Histogram bars={bars} bucket="hour" total={0} noun="event" over="window" axis now={NOW} />,
  );
  for (const tick of ticks(container)) {
    expect(tick.textContent).toMatch(/^\d{2}:\d{2}$/);
  }
});

// THE AXIS IS OPT-IN, because a chart can be narrower than its own labels and a
// tick row that collides with itself is worse than none.
test("a chart that did not ask for an axis draws none", () => {
  const { container } = render(
    <Histogram bars={days(14)} bucket="day" total={0} noun="change" over="loaded" />,
  );
  expect(ticks(container)).toHaveLength(0);
});

// THE ONE SENTENCE A SIGHTED READER NEVER SEES IS THE ONE THAT WAS WRONG. It
// announced the caller's page count as a window count, in a noun the tracker
// does not use.
test("the announced total carries the caller's own noun and scope", () => {
  const { unmount } = render(
    <Histogram bars={days(2)} bucket="day" total={3} noun="change" over="loaded" />,
  );
  expect(screen.getByText("3 changes on the pages loaded")).toBeTruthy();
  unmount();
  render(<Histogram bars={days(2)} bucket="day" total={9412} noun="event" over="window" />);
  expect(screen.getByText("9,412 events in this window")).toBeTruthy();
});

// AND SO DOES EVERY BAR'S OWN TOOLTIP, which is the only thing a pointer or a
// screen reader can ask about one bucket.
test("a bar names its own bucket in the caller's noun", () => {
  const { container } = render(
    <Histogram bars={days(2, [0, 1])} bucket="day" total={1} noun="change" over="loaded" />,
  );
  const bars = [...container.querySelectorAll(".histogram-bar")] as HTMLElement[];
  expect(bars[0]!.title).toContain("0 changes");
  expect(bars[1]!.title).toContain("1 change");
});
