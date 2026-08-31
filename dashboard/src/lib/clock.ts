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

let now = Date.now();
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
  }, 1000);
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
  return useSyncExternalStore(
    subscribe,
    () => now,
    () => now,
  );
}

/** The current instant without subscribing — for event handlers and effects. */
export function currentNow(): number {
  return now;
}
