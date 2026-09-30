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
 *
 * AND PER READER, for `recents.ts`'s other reason: one browser is often
 * several people, and one list per browser drew the last person's kept titles
 * in the next person's rail. Each principal the tab is read by
 * (`lib/reader.ts`) keeps their own, under a key of their own; a tab that has
 * not learned its reader draws none and keeps none.
 *
 * …AND GONE WITH ITS READER, for `recents.ts`'s third reason, although a star
 * is a decision: a key of one's own keeps a list out of the next person's
 * rail and not out of their browser, where every title in it is one read
 * away. So a sign-out deletes every list and a sign-in every list but the
 * signing-in person's own ([forgetOtherReaders], called by `lib/session.ts`).
 */

import { useCallback, useSyncExternalStore } from "react";
import { currentReader, forgetReadersUnder, onReader } from "./reader.ts";

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

/** Where every reader's stars are kept: [starsKey]'s prefix. */
const STARS_PREFIX = "crewlet_starred/";

/** The storage key a reader's stars are kept under. */
export function starsKey(reader: string): string {
  return `${STARS_PREFIX}${reader}`;
}

/**
 * Delete every reader's stars but `keep`'s — everybody's, where `keep` is
 * null. See the module doc for why a sign-out and a sign-in do.
 */
export function forgetOtherReaders(keep: string | null): void {
  forgetReadersUnder(STARS_PREFIX, keep === null ? null : starsKey(keep));
  cache = null;
  for (const fn of listeners) fn();
}

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

/**
 * WHAT THIS TAB IS PAINTING, and nothing else.
 *
 * It exists for one reason: `useSyncExternalStore` requires a snapshot that is
 * REFERENTIALLY STABLE between renders, and a fresh parse of localStorage
 * returns a new array every time, which makes React loop. It is a render
 * snapshot, so it may be stale — another tab's writes do not reach it until a
 * `storage` event or a reload.
 *
 * SO IT IS NEVER THE BASE OF A WRITE. That is `stored()` below, and the
 * separation is the whole of what stops one tab destroying another's list:
 * `write` replaces the WHOLE key, so a mutation built on a snapshot taken
 * before the other tab wrote hands back a list missing everything it did.
 * Reproduced over one origin: tab A stars two pages, tab B stars a third, and
 * the stored list is tab B's one star — with `toggleStar` returning
 * `"starred"`, so the reader is told it worked. This list refuses at the cap
 * on the stated grounds that silently evicting a decision is the one
 * behaviour a bookmark list may not have, and this path silently evicted up
 * to forty-nine of them.
 */
let cache: { reader: string; rows: Star[] } | null = null;
const listeners = new Set<() => void>();

/** No reader, no list: the same empty array every time, for a stable snapshot. */
const NOBODY: Star[] = [];

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

/** What is STORED for this reader, parsed fresh. The base of every write. */
function stored(reader: string): Star[] {
  try {
    const raw = localStorage.getItem(starsKey(reader));
    const parsed: unknown = raw ? JSON.parse(raw) : [];
    return Array.isArray(parsed) ? parsed.filter(valid).slice(0, MaxStars) : [];
  } catch {
    // A private window, blocked site data, or a value somebody's other tab
    // wrote in a shape this build does not have.
    return [];
  }
}

/** The snapshot this tab renders: its reader's stars. See [cache]. */
function read(): Star[] {
  const reader = currentReader();
  if (reader === null) return NOBODY;
  if (cache?.reader === reader) return cache.rows;
  cache = { reader, rows: stored(reader) };
  return cache.rows;
}

function write(reader: string, next: Star[]): void {
  cache = { reader, rows: next };
  try {
    localStorage.setItem(starsKey(reader), JSON.stringify(next));
  } catch {
    /* the list works for this tab's lifetime, which is when it is being used */
  }
  for (const fn of listeners) fn();
}

/** The key a path is stored under. */
function keyOf(path: string[]): string {
  return path.join("/");
}

/**
 * Whether this path is among these stars.
 *
 * THE PREDICATE TAKES THE LIST because the only component that asks already
 * holds one: `StarPage` subscribes with [useStarred] so the button fills the
 * moment the star is kept, and a predicate reading storage underneath it
 * would answer from the same rows without subscribing to them. It had its own
 * `stars.some(s => s.path.join("/") === key)` for exactly that reason — one
 * rule written twice, and the copies agree only until somebody decides that a
 * path compare should be case-insensitive.
 */
export function starredIn(stars: readonly Star[], path: string[]): boolean {
  const key = keyOf(path);
  return stars.some((s) => keyOf(s.path) === key);
}

/** Whether this path is starred by this tab's reader, read straight from storage. */
export function isStarred(path: string[]): boolean {
  const reader = currentReader();
  return reader !== null && starredIn(stored(reader), path);
}

/**
 * Star something, or unstar it if it already is.
 *
 * ONE GESTURE, because the control is one button: a reader clicking a filled
 * star means "not this one", and a separate `unstar` would be a second way to
 * say it that the button could get wrong.
 *
 * Returns what happened, so a caller can say so — including the REFUSAL at the
 * cap, which is not "it worked" and must not render as a filled star, and
 * `nobody` for a tab that has not learned who reads it: there is no list to
 * keep a star in.
 */
export function toggleStar(entry: Omit<Star, "at">): StarOutcome {
  if (entry.path.length === 0) return "full";
  const reader = currentReader();
  if (reader === null) return "nobody";
  const key = keyOf(entry.path);
  // STORED, NOT THE RENDER SNAPSHOT — see [cache]. A write replaces the whole
  // key, so building it on what this tab last painted hands back a list
  // missing everything another tab has done since.
  const held = stored(reader);
  const without = held.filter((s) => keyOf(s.path) !== key);
  if (without.length !== held.length) {
    write(reader, without);
    return "unstarred";
  }
  // AT THE CAP IT REFUSES rather than dropping the oldest. Every entry here
  // is one somebody chose, and silently evicting a decision to make room for
  // another is the one behaviour a bookmark list may not have.
  if (held.length >= MaxStars) return "full";
  // NEWEST LAST, unlike recents. A star's order is the order they were kept
  // in, which stays put — a list that reshuffled on every visit would be a
  // recents list wearing a star.
  write(reader, [...held, { ...entry, at: Date.now() }]);
  return "starred";
}

/** What a toggle did. */
export type StarOutcome = "starred" | "unstarred" | "full" | "nobody";

/**
 * Subscribe, and follow ANOTHER TAB while anybody is.
 *
 * `storage` fires in every OTHER document of the origin, so this is what
 * lets a second tab's star reach this one's rail rather than waiting for a
 * reload. Installed on the first subscriber and removed with the last: the
 * event only matters to a surface that is drawing the list, and every WRITE
 * reads storage for itself (see [cache]), so correctness does not depend on
 * having heard it.
 *
 * `e.key === null` is a `localStorage.clear()`, which names no key and
 * invalidates everything.
 */
function subscribe(fn: () => void): () => void {
  if (listeners.size === 0) window.addEventListener("storage", follow);
  listeners.add(fn);
  // AND THE READER: a tab learning who reads it draws that person's stars.
  const unread = onReader(fn);
  return () => {
    unread();
    listeners.delete(fn);
    if (listeners.size === 0) window.removeEventListener("storage", follow);
  };
}

function follow(e: StorageEvent): void {
  const reader = currentReader();
  if (e.key !== null && (reader === null || e.key !== starsKey(reader))) return;
  cache = null;
  for (const fn of listeners) fn();
}

/** The reader's stars, in the order they were kept. */
export function useStarred(): Star[] {
  return useSyncExternalStore(subscribe, read, () => []);
}

/** `toggleStar`, as a stable callback. */
export function useToggleStar(): (entry: Omit<Star, "at">) => StarOutcome {
  return useCallback(toggleStar, []);
}

/**
 * Test seam: drop the in-process cache so a fresh read hits storage.
 *
 * IT DOES NOT CLEAR STORAGE, which is the whole point of having it: a test
 * that seeds a reader's `starsKey` with what a previous build wrote calls this to
 * make the module read that, and a reset that also emptied the key would
 * delete the fixture it was called to load. A clean slate is
 * `localStorage.clear()` beside it — one line, in the caller's own
 * `beforeEach`, where the rest of that caller's storage is cleared too.
 *
 * There used to be a `forgetStars()` here for the clean-slate half, exported
 * from the product module and called by nothing but tests — both of which
 * already cleared storage themselves. Nothing in the product ever wanted it:
 * the palette can clear RECENTS because recents accumulate by accident, and a
 * star is a decision, so what drops one is the reader unstarring it — or the
 * reader leaving the browser, which drops them all ([forgetOtherReaders]).
 */
export function resetForTest(): void {
  cache = null;
}
