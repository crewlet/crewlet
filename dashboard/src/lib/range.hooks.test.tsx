/**
 * What a screen holding a window pays the clock.
 *
 * A window's edges are a function of WHEN they are read. Read at render off
 * the one-second clock, they were a new pair every second: the audit asked
 * the tracker for its feed once a second where its poll said once a minute,
 * and every chart screen drew itself once a second to arrive at edges that
 * move once an hour. These cases hold the two hooks to what they now cost: a
 * list's window renders nothing on a tick, and a chart's renders when its
 * column rolls.
 */

import { act, cleanup, render } from "@testing-library/react";
import { afterEach, beforeEach, expect, test, vi } from "vitest";

import { Router } from "~/app/router.tsx";
import { useTimeRange, useWindow, type Offer, type TimeRange } from "./range.ts";

/** A day of hours, which is the commonest chart this product draws. */
const DAY: Offer = { ranges: ["1d", "7d"], custom: true, fallback: "1d", buckets: ["hour", "day"] };

/** Half a second past 11:59, so no tick lands exactly on the hour's edge. */
const BEFORE_NOON = Date.parse("2031-04-16T11:59:00.500Z");
const HOUR = 3_600_000;

beforeEach(() => {
  location.hash = "#/";
  vi.useFakeTimers({ toFake: ["setInterval", "clearInterval", "Date"] });
  vi.setSystemTime(BEFORE_NOON);
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  location.hash = "";
});

function tick(times: number): void {
  for (let i = 0; i < times; i++) {
    act(() => {
      vi.advanceTimersByTime(1_000);
    });
  }
}

// A CHART'S EDGES ARE ITS COLUMN'S. Fifty-nine ticks inside the hour are no
// render at all, and the tick that crosses into the next hour is one, with
// both edges an hour on — which is the only moment the chart's question is a
// new one.
test("a chart's range renders when its column rolls, not when a second passes", () => {
  let renders = 0;
  let seen: TimeRange | null = null;
  function Chart() {
    renders += 1;
    seen = useTimeRange(DAY);
    return null;
  }
  render(
    <Router>
      <Chart />
    </Router>,
  );
  const mounted = renders;
  const first = seen as TimeRange | null;
  expect(first?.until).toBe("2031-04-16T12:00:00.000Z");

  tick(59);
  expect(renders).toBe(mounted);

  tick(1);
  expect(renders).toBe(mounted + 1);
  const next = seen as TimeRange | null;
  expect(Date.parse(next?.until ?? "") - Date.parse(first?.until ?? "")).toBe(HOUR);
  expect(Date.parse(next?.since ?? "") - Date.parse(first?.since ?? "")).toBe(HOUR);
});

// AND A LIST'S WINDOW HAS NO EDGES TO MOVE. It is the choice — `7d`, or two
// instants a reader named — and a list computes its edges when it asks, so a
// minute of ticks renders nothing that holds it.
test("a list's window renders nothing while the clock ticks", () => {
  let renders = 0;
  function List() {
    renders += 1;
    useWindow(DAY);
    return null;
  }
  render(
    <Router>
      <List />
    </Router>,
  );
  const mounted = renders;
  tick(120);
  expect(renders).toBe(mounted);
});
