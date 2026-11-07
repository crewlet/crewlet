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

/** How often the clock ticks, and so how stale a read of it may be. */
const TICK_MS = 1000;

let now = Date.now();
/** Whether this synchronous pass has re-read a stopped clock already. */
let decided = false;
const listeners = new Set<() => void>();
let timer: ReturnType<typeof setInterval> | 0 = 0;

function tick(): void {
  now = Date.now();
  for (const fn of listeners) fn();
}

function start(): void {
  if (timer) return;
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
 * Use it wherever a relative time is displayed, and pass the value down rather
 * than calling it again deeper: two components reading their own clock is how
 * "3m ago" ends up next to "2m ago" for the same row.
 */
export function useNow(): number {
  return useSyncExternalStore(subscribe, read, read);
}

/**
 * What a component shows of the shared instant, re-rendering it only when that
 * changes: today's date for a day's rule, "5m ago" for a thread's last reply.
 *
 * A list whose every row read [useNow] re-rendered every row every second for
 * a label that moves once a minute, and a transcript that did so redrew under
 * the reader's hands while they typed. `select` must return a value that
 * compares equal while it means the same (a string, a number).
 */
export function useClock<T extends string | number | boolean>(select: (now: number) => T): T {
  return useSyncExternalStore(
    subscribe,
    () => select(read()),
    () => select(read()),
  );
}

/** The local midnight an instant falls in: [useClock]'s select for a screen that reads only the day. */
export function startOfDay(now: number): number {
  return new Date(now).setHours(0, 0, 0, 0);
}

/**
 * The shared instant, as a render reads it.
 *
 * A STOPPED CLOCK IS NOT A CLOCK. The instant is refreshed only by the ticker,
 * and the ticker runs only while something is subscribed — so the first
 * component to mount after every subscriber had gone rendered with the
 * instant of the LAST tick, however long ago that was: minutes after a reader
 * left every screen with a relative time, or, in a test that sets the system
 * time, the moment the module was imported. Spend's "last 30 days" was cut at
 * that stale instant, and on a day whose company date differed from the
 * import's it opened the custom window a day short.
 *
 * So a read while stopped re-reads the wall clock — but only when it has
 * moved by a tick or more (either way: a clock set back is as stale as one
 * left behind), which leaves it no staler than a running clock ever is.
 *
 * AND ONLY ONCE PER SYNCHRONOUS PASS. React reads a snapshot twice in a row
 * and warns where the two differ, and two reads either side of a tick did:
 * the first a millisecond short of it, the second past it. So the first read
 * of a pass decides, and a microtask lets the next pass decide again.
 */
function read(): number {
  if (!timer && !decided) {
    decided = true;
    queueMicrotask(() => {
      decided = false;
    });
    const t = Date.now();
    if (Math.abs(t - now) >= TICK_MS) now = t;
  }
  return now;
}
