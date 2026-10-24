/**
 * What the dashboard's histogram says that the kit's cannot know: the dates in
 * the reader's chosen zone, the caller's noun and scope, and the bucket a
 * click narrows the window to. The drawing (the fill, the floors, the failed
 * foot, which buckets are labelled and where a tick sits) is the kit's
 * `Histogram`, and its own suite holds it.
 *
 * AND THE SENTENCE NOBODY SEES IS STILL A CLAIM. The one statement this
 * component makes to a screen reader announced a page count as a window count
 * in the wrong noun: "3 events in this window" over a tracker chart whose
 * visible caption said three changes on one page. The noun and the scope are
 * the caller's, required, for the reason `FacetRail`'s `over` is.
 */

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, test, vi } from "vitest";

import { Histogram, type Bar } from "./Histogram.tsx";
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

const ticks = (root: HTMLElement) => [...root.querySelectorAll(".crewlet-histogram__tick")];
const slots = (root: HTMLElement) => [
  ...root.querySelectorAll<HTMLElement>(".crewlet-histogram__bar"),
];

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
  expect(ticks(container).length).toBeGreaterThan(0);
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
// screen reader can ask about one bucket, with the failed share where the
// engine split it and no mention of failures where it did not.
test("a bar names its own bucket in the caller's noun, and its failed share", () => {
  const bars = [{ ...days(2, [8])[0]!, failed: 2 }, { ...days(2, [0, 1])[1]! }];
  const { container } = render(
    <Histogram bars={bars} bucket="day" total={9} noun="turn" over="window" now={NOW} />,
  );
  const [split, plain] = slots(container);
  expect(split!.title).toMatch(/8 turns, 2 failed$/);
  expect(plain!.title).toMatch(/1 turn$/);
});

// A BAR NARROWS THE WINDOW TO THE BUCKET IT COVERS: from its start, one
// bucket's width long.
test("a click narrows the window to the bar's own bucket", () => {
  const onPick = vi.fn();
  const bars = days(3, [1, 4, 2]);
  render(
    <Histogram bars={bars} bucket="day" total={7} noun="turn" over="window" onPick={onPick} />,
  );
  fireEvent.click(screen.getAllByRole("button")[1]!);
  const from = Date.parse(bars[1]!.at);
  expect(onPick).toHaveBeenCalledWith({ from, to: from + DAY });
});
