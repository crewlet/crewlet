/**
 * The one clock, and what it costs to read it.
 *
 * The clock ticks once a second, and whatever subscribes to the SECOND renders
 * once a second. A screen that took it to draw a relative time handed it down
 * to every row it drew, so the audit rendered its hundred rows every second
 * and the work screen its whole grid and board. These cases hold the two
 * shapes a reader of the clock can take: a component that wants the second
 * gets it, and one that wants a READING — words, a day, a year — renders when
 * the reading moves and at no other tick.
 *
 * And the shared instant is never older than a tick, including the first time
 * a screen reads it after nothing has. The ticker runs only while something is
 * subscribed; before the first subscriber and after the last one goes, the
 * instant used to sit where it was — the moment the module loaded, or the last
 * tick before everything unmounted — and the next screen rendered against it
 * until its first tick: relative times out by however long the tab had shown
 * nothing that read the clock, and a list keyed on its window asking the
 * engine twice, once for a stale window and once for the real one.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { act } from "~/test/inCase.ts";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { useClockReading, useNow, useToday } from "./clock.ts";
import { browserDay } from "./format.ts";

const T0 = Date.parse("2031-04-16T12:00:00Z");

beforeEach(() => {
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "Date"] });
  vi.setSystemTime(T0);
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

/** Moves the clock one tick at a time, as an open tab sees it. */
function tick(times = 1): void {
  for (let i = 0; i < times; i++) {
    act(() => {
      vi.advanceTimersByTime(1_000);
    });
  }
}

// A READING RENDERS WHEN IT MOVES. Ten seconds of ticks under a reading that
// changes every ten seconds is ONE render, not ten — which is the whole of
// why a date cell holding "3h ago" costs nothing for an hour.
test("a reading renders its reader only when the reading changes", () => {
  let renders = 0;
  function Reader() {
    renders += 1;
    const bucket = useClockReading((now) => Math.floor(now / 10_000));
    return <span>{bucket}</span>;
  }
  render(<Reader />);
  const mounted = renders;

  tick(9);
  expect(renders).toBe(mounted);

  tick(1);
  expect(renders).toBe(mounted + 1);
});

// AND THE SECOND IS STILL THERE FOR WHAT IS ABOUT THE SECOND — the contrast
// that makes the case above mean something: the same ten ticks render a
// subscriber to `useNow` ten times.
test("a subscriber to the second renders every second", () => {
  let renders = 0;
  function Reader() {
    renders += 1;
    return <span>{useNow()}</span>;
  }
  render(<Reader />);
  const mounted = renders;
  tick(10);
  expect(renders).toBe(mounted + 10);
});

// TODAY IS A DAY. A calendar's today cell and a timeline's today line read
// only which day it is, and the hook that hands it over renders its reader
// when midnight passes rather than on each of the 86,400 seconds before it.
test("today renders its reader when the day turns, and not before", () => {
  let renders = 0;
  let seen = "";
  function Reader() {
    renders += 1;
    seen = useToday();
    return <span>{seen}</span>;
  }
  // A MINUTE BEFORE THE BROWSER'S MIDNIGHT, so the next minute crosses it.
  const day = new Date(T0);
  const midnight = new Date(day.getFullYear(), day.getMonth(), day.getDate() + 1).getTime();
  vi.setSystemTime(midnight - 60_000);
  render(<Reader />);
  const mounted = renders;
  const before = seen;

  tick(59);
  expect(renders).toBe(mounted);
  expect(seen).toBe(before);

  tick(1);
  expect(renders).toBe(mounted + 1);
  expect(seen).toBe(browserDay(new Date(midnight)));
});

/** What a screen rendering the clock draws: the instant, and every read of one render. */
function Instant() {
  const first = useNow();
  const second = useNow();
  return (
    <span data-testid="now">
      {new Date(first).toISOString()} {first === second ? "one instant" : "two instants"}
    </span>
  );
}

const shown = () => screen.getByTestId("now").textContent;

// THE FIRST READ AFTER NOTHING HAS BEEN READING THE CLOCK IS THE TIME NOW, on
// the first render and not a tick later: a screen's effects run with what its
// first render read, and a query asked from one is asked for that.
test("the first read after nothing has been reading the clock is the time now", () => {
  render(<Instant />);
  expect(shown()).toBe(`${new Date(T0).toISOString()} one instant`);
  cleanup();

  // An hour on a screen that reads no clock — a sign-in form, say.
  const later = T0 + 3_600_000;
  vi.setSystemTime(later);
  render(<Instant />);
  expect(shown()).toBe(`${new Date(later).toISOString()} one instant`);
});

// THE SAME FOR A READING, which is what a date cell holds: an hour-old instant
// read by `useClockReading` drew "1h ago" where the cell meant "just now".
test("the first reading after nothing has been reading the clock is of the time now", () => {
  function Hour() {
    return <span data-testid="hour">{useClockReading((now) => Math.floor(now / 3_600_000))}</span>;
  }
  const first = render(<Hour />);
  tick(1);
  first.unmount();

  const later = T0 + 3_600_000;
  vi.setSystemTime(later);
  render(<Hour />);
  expect(Number(screen.getByTestId("hour").textContent)).toBe(Math.floor(later / 3_600_000));
});

test("while the ticker runs, the instant is the ticker's and moves once a tick", () => {
  render(<Instant />);
  expect(shown()).toBe(`${new Date(T0).toISOString()} one instant`);
  // Within a tick, nothing moves: every reader on the page agrees.
  act(() => vi.advanceTimersByTime(400));
  expect(shown()).toBe(`${new Date(T0).toISOString()} one instant`);
  act(() => vi.advanceTimersByTime(600));
  expect(shown()).toBe(`${new Date(T0 + 1_000).toISOString()} one instant`);
});

test("a wall clock set back by more than a tick is read afresh too", () => {
  render(<Instant />);
  cleanup();
  const earlier = T0 - 3 * 3_600_000;
  vi.setSystemTime(earlier);
  render(<Instant />);
  expect(shown()).toBe(`${new Date(earlier).toISOString()} one instant`);
});
