/**
 * What this reader opened last, so the palette has something to offer before
 * they type.
 *
 * An empty palette is a blank box asking a question the reader already
 * answered by opening it: they want to go somewhere, and the most likely
 * somewhere is one of the last few places they were. Every product with a
 * launcher does this, and the reason is not habit — it is that "back to the
 * item I was reading" has no other gesture in a hash-routed application whose
 * back button is the browser's.
 *
 * PER BROWSER, in localStorage, and deliberately not on the engine: it is a
 * fact about one person at one desk, it is worth nothing to anybody else, and
 * a company's shared store is not the place for a reading habit. Every access
 * is wrapped, because a private window, blocked site data or a quota refusal
 * makes it throw — and a palette that crashed on a storage setting would be
 * worse than one with no recents.
 */

import { useCallback, useSyncExternalStore } from "react";

/** One place the reader was. */
export interface Recent {
  /** The route's own path segments — the identity, and what a click restores. */
  path: string[];
  /** What to call it. The label the screen itself showed, never re-derived. */
  label: string;
  /** Which workspace it belongs to, for the icon beside it. */
  workspace: string;
  /** When it was last opened, epoch ms. */
  at: number;
}

const KEY = "crewlet_recents";

/**
 * How many are kept.
 *
 * Eight: enough that a morning's work is in the list and few enough that the
 * list is scanned rather than searched — which is the whole difference between
 * recents and a history.
 */
export const MaxRecents = 8;

let cache: Recent[] | null = null;
const listeners = new Set<() => void>();

function read(): Recent[] {
  if (cache) return cache;
  try {
    const raw = localStorage.getItem(KEY);
    const parsed: unknown = raw ? JSON.parse(raw) : [];
    cache = Array.isArray(parsed) ? parsed.filter(valid).slice(0, MaxRecents) : [];
  } catch {
    // A private window, blocked site data, or a value somebody's other tab
    // wrote in a shape this build does not have. None of them is a reason
    // for a palette not to open.
    cache = [];
  }
  return cache;
}

/**
 * Whether a stored entry is one this build can use.
 *
 * VALIDATED ON READ rather than trusted, because the value survives upgrades:
 * a row written by a build whose shape has since changed is an ordinary thing
 * to find here, and the honest response is to drop that row rather than to
 * render a recent with no path.
 */
function valid(row: unknown): row is Recent {
  if (typeof row !== "object" || row === null) return false;
  const r = row as Partial<Recent>;
  return (
    Array.isArray(r.path) &&
    r.path.length > 0 &&
    r.path.every((p) => typeof p === "string") &&
    typeof r.label === "string" &&
    typeof r.at === "number"
  );
}

function write(next: Recent[]): void {
  cache = next;
  try {
    localStorage.setItem(KEY, JSON.stringify(next));
  } catch {
    // Out of quota, or storage refused. The list still works for this tab's
    // lifetime, which is the case that matters while somebody is working.
  }
  for (const fn of listeners) fn();
}

/**
 * Record that the reader opened something.
 *
 * KEYED ON THE PATH, so revisiting a page moves it to the top rather than
 * filling the list with one object. A label that arrives later — a page whose
 * title loads after its route — overwrites the one stored, because the last
 * label a screen showed is the one the reader recognises.
 */
export function remember(entry: Omit<Recent, "at">): void {
  if (entry.path.length === 0) return;
  const key = entry.path.join("/");
  const now = Date.now();
  const rest = read().filter((r) => r.path.join("/") !== key);
  write([{ ...entry, at: now }, ...rest].slice(0, MaxRecents));
}

/** Drop everything. For the palette's own "clear" command. */
export function forgetAll(): void {
  write([]);
}

function subscribe(fn: () => void): () => void {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

/** The reader's recents, newest first. */
export function useRecents(): Recent[] {
  return useSyncExternalStore(subscribe, read, () => []);
}

/** `remember`, as a stable callback for an effect's dependency list. */
export function useRemember(): (entry: Omit<Recent, "at">) => void {
  return useCallback(remember, []);
}

/** Test seam: drop the in-process cache so a fresh read hits storage. */
export function resetForTest(): void {
  cache = null;
}
