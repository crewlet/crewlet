/**
 * What this reader keeps a shortcut to.
 *
 * THE COMPANION TO `lib/recents.ts` AND ITS OPPOSITE: recents are what you
 * happened to open, stars are what you decided to keep. The first decays and
 * the second does not, which is the whole distinction — a rail built only from
 * recents loses the board you check every morning the moment you spend a day
 * somewhere else.
 *
 * PER BROWSER, in localStorage, deliberately not on the engine. A tracker PIN
 * is a different object and lives in `work_person`: it is a fact about the
 * company's work that a seat's tools can read, and it is what an assistant
 * sets when you ask it to. A star is a fact about one person at one desk —
 * this dashboard's own bookmark, over any kind of object, including the ones
 * the tracker has no pin for (a node, a schedule, a config revision). Two
 * mechanisms because there are two facts; folding them would put a browser's
 * bookmark in the company's audit trail.
 *
 * Every access is wrapped, for `recents.ts`'s reason: a private window,
 * blocked site data or a quota refusal makes it throw, and a rail that crashed
 * on a storage setting would be worse than one with no stars.
 */

import { useCallback, useSyncExternalStore } from "react";

/** One thing the reader kept. */
export interface Star {
  /** The route's own path segments — the identity, and what a click restores. */
  path: string[];
  /** What to call it. The label the screen itself showed. */
  label: string;
  /** Which workspace it belongs to, for the icon beside it. */
  workspace: string;
  /** When it was starred, epoch ms. */
  at: number;
}

const KEY = "crewlet_starred";

/**
 * How many are kept.
 *
 * Fifty: a star is a decision rather than a side effect, so the cap is not
 * about keeping the list scannable — the rail groups and scrolls. It is a
 * bound on the STORED VALUE, because localStorage is a shared quota and an
 * unbounded list written by a long-lived tab is how a dashboard breaks
 * somebody's unrelated site data. Reaching it is a refusal that SAYS SO
 * rather than a silent drop of the oldest, since every entry here is one
 * somebody chose.
 */
export const MaxStars = 50;

let cache: Star[] | null = null;
const listeners = new Set<() => void>();

function valid(row: unknown): row is Star {
  if (typeof row !== "object" || row === null) return false;
  const r = row as Partial<Star>;
  return (
    Array.isArray(r.path) &&
    r.path.length > 0 &&
    r.path.every((p) => typeof p === "string") &&
    typeof r.label === "string" &&
    typeof r.at === "number"
  );
}

function read(): Star[] {
  if (cache) return cache;
  try {
    const raw = localStorage.getItem(KEY);
    const parsed: unknown = raw ? JSON.parse(raw) : [];
    cache = Array.isArray(parsed) ? parsed.filter(valid).slice(0, MaxStars) : [];
  } catch {
    cache = [];
  }
  return cache;
}

function write(next: Star[]): void {
  cache = next;
  try {
    localStorage.setItem(KEY, JSON.stringify(next));
  } catch {
    /* the list works for this tab's lifetime, which is when it is being used */
  }
  for (const fn of listeners) fn();
}

/** The key a path is stored under. */
function keyOf(path: string[]): string {
  return path.join("/");
}

/** Whether this path is starred. */
export function isStarred(path: string[]): boolean {
  const key = keyOf(path);
  return read().some((s) => keyOf(s.path) === key);
}

/**
 * Star something, or unstar it if it already is.
 *
 * ONE GESTURE, because the control is one button: a reader clicking a filled
 * star means "not this one", and a separate `unstar` would be a second way to
 * say it that the button could get wrong.
 *
 * Returns what happened, so a caller can say so — including the REFUSAL at the
 * cap, which is not "it worked" and must not render as a filled star.
 */
export function toggleStar(entry: Omit<Star, "at">): "starred" | "unstarred" | "full" {
  if (entry.path.length === 0) return "full";
  const key = keyOf(entry.path);
  const held = read();
  const without = held.filter((s) => keyOf(s.path) !== key);
  if (without.length !== held.length) {
    write(without);
    return "unstarred";
  }
  // AT THE CAP IT REFUSES rather than dropping the oldest. Every entry here
  // is one somebody chose, and silently evicting a decision to make room for
  // another is the one behaviour a bookmark list may not have.
  if (held.length >= MaxStars) return "full";
  // NEWEST LAST, unlike recents. A star's order is the order they were kept
  // in, which stays put — a list that reshuffled on every visit would be a
  // recents list wearing a star.
  write([...held, { ...entry, at: Date.now() }]);
  return "starred";
}

/** Drop everything. */
export function forgetStars(): void {
  write([]);
}

function subscribe(fn: () => void): () => void {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

/** The reader's stars, in the order they were kept. */
export function useStarred(): Star[] {
  return useSyncExternalStore(subscribe, read, () => []);
}

/** `toggleStar`, as a stable callback. */
export function useToggleStar(): (entry: Omit<Star, "at">) => "starred" | "unstarred" | "full" {
  return useCallback(toggleStar, []);
}

/** Test seam: drop the in-process cache so a fresh read hits storage. */
export function resetForTest(): void {
  cache = null;
}
