/**
 * One clock for the whole application.
 *
 * The dashboard this replaces baked every relative time at render: "in 4h" on
 * Schedules, "12s" on a Fleet lease, "waiting 4m" on the board, and every
 * timestamp in the feed were frozen until some unrelated push happened to
 * re-render them. Three row types even carried a `data-ts` attribute as if a
 * ticker were planned; none existed.
 *
 * One ticker, one instant, shared: every relative time on screen advances
 * together and they never disagree with each other. It ticks once a second and
 * only while the tab is visible — a background tab has nobody reading it, and
 * a per-second re-render there is pure battery.
 */

import { useSyncExternalStore } from "react";
import { browserDay } from "./format.ts";

/** How often the shared instant moves, and so how old it may ever be. */
const TICK_MS = 1000;

let now = Date.now();
const listeners = new Set<() => void>();
let timer: ReturnType<typeof setInterval> | 0 = 0;

function tick(): void {
  now = Date.now();
  for (const fn of listeners) fn();
}

/**
 * The shared instant, as a snapshot: the ticker's while it runs, and read
 * afresh while it does not once it is a tick out.
 *
 * THE TICKER RUNS ONLY WHILE SOMETHING IS SUBSCRIBED, so once the last
 * subscriber goes `now` stops moving — and before the first ever subscribes it
 * is the instant this module loaded. The first screen to read the clock after
 * that rendered against that old instant until the first tick a second later:
 * relative times a sign-in's length out, and a screen keyed on its window
 * asked the engine for the window that ended when the tab opened, then again
 * for the real one.
 *
 * Refreshed on the READ rather than when the ticker starts, because a
 * component subscribes after its first render and its effects run with what
 * that render read. And only once it is a whole tick out — in either
 * direction, since a wall clock can be set back — so that the several reads
 * of one render all get one instant, as `useSyncExternalStore` requires of a
 * snapshot, and the clock is never older than a running ticker would leave it.
 */
function snapshot(): number {
  if (!timer && Math.abs(Date.now() - now) >= TICK_MS) now = Date.now();
  return now;
}

function start(): void {
  if (timer) return;
  // NOTHING IS RE-READ HERE: [snapshot] already refreshed an instant left
  // stale while the clock was stopped, on the first render's own read, which
  // is the read a screen's effects and first query act on. Re-reading here as
  // well would move the instant a second time, by less than a tick, and
  // render every second-reader once more for nothing.
  timer = setInterval(() => {
    if (document.visibilityState === "visible") tick();
  }, TICK_MS);
  // A tab coming back from the background is exactly when the clock is most
  // wrong, so re-read it immediately rather than waiting out the interval.
  document.addEventListener("visibilitychange", onVisible);
}

function onVisible(): void {
  if (document.visibilityState === "visible") tick();
}

function stop(): void {
  if (!timer) return;
  clearInterval(timer);
  timer = 0;
  document.removeEventListener("visibilitychange", onVisible);
}

function subscribe(fn: () => void): () => void {
  listeners.add(fn);
  start();
  return () => {
    listeners.delete(fn);
    if (!listeners.size) stop();
  };
}

/**
 * The shared instant, in epoch ms, re-rendering the caller once a second.
 *
 * For a value that has to move with the clock as a whole — a strip of cells
 * that rolls, a header counting down. ONE INSTANT, read by every subscriber in
 * the same tick, so a component reading it deeper agrees with one reading it
 * higher; what drifts is a component reading `Date.now()` for itself, which is
 * how "3m ago" ends up next to "2m ago" for the same row.
 *
 * NOT FOR A ROW. Whatever calls this renders once a second, and a list or grid
 * that took it and handed it to its rows rendered every row once a second with
 * it — see [useClockReading], which is what a cell showing a time reads instead.
 */
export function useNow(): number {
  return useSyncExternalStore(subscribe, snapshot, snapshot);
}

/**
 * A reading of the shared instant — words, a day, a year, the column in
 * progress — re-rendering the caller only when the READING changes.
 *
 * FOR THE ONE ELEMENT THAT SHOWS A TIME, so the clock reaches it rather than
 * everything above it. A relative time handed down as `now` made a grid's
 * column definitions a function of the clock: every tick rebuilt them, and
 * every row rendered again — a hundred rows a second on the audit — to change
 * the handful of cells that read "12s ago". Read here, a tick re-reads
 * `read(now)` for each subscriber and React re-renders only those whose
 * reading moved, so "3h ago" renders once an hour and a due date's year once
 * a year.
 *
 * A PRIMITIVE, because the reading is compared by value (`Object.is`) to
 * decide whether anything moved: an object built per tick would be a new
 * reading every second, which is the defect this exists to end.
 *
 * STILL ONE CLOCK: every subscriber reads the same module instant in the same
 * tick, so two cells never disagree about how long ago one moment was.
 */
export function useClockReading<T extends string | number | boolean>(read: (now: number) => T): T {
  return useSyncExternalStore(
    subscribe,
    () => read(snapshot()),
    () => read(snapshot()),
  );
}

/**
 * Today, as the browser's calendar names it (`2026-06-15`), re-rendering the
 * caller when the day turns rather than when a second passes.
 *
 * For the screens whose only reading of the clock is WHICH DAY it is — a
 * calendar's today cell, a timeline's today line, the month a calendar opens
 * on. The work screen took the second for those and re-rendered every shape
 * it draws, the grid's rows and the timeline's layout included, once a second
 * to arrive at the same day. The BROWSER's day, because those cells are built
 * in it — see `browserDay`.
 */
export function useToday(): string {
  return useClockReading<string>(today);
}

function today(at: number): string {
  return browserDay(new Date(at));
}
