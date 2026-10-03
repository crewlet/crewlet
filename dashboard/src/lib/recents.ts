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
 *
 * # ONE READER, SO ONE ORDER: MOST RECENTLY VISITED FIRST
 *
 * The palette is the only surface that draws this list, and it is opened
 * fresh, ranks what it offers and closes — so it has a re-sort boundary, and
 * "where I just was" is exactly what a reader opening an empty one wants
 * first. The list is STORED in that order: a visit moves its place to the top.
 *
 * It was not always. While the workspace sidebar drew a Recent section, a
 * visit that moved its row slid the list under the pointer that had just
 * pressed it, so the list was stored in arrival order, the palette re-sorted
 * a copy, and the cap was counted per workspace because the rail drew one
 * workspace's share. The sidebar keeps no recents now — a place kept on
 * purpose is a star (`lib/starred.ts`), and a sidebar that also carried what
 * was merely opened is the tree this one replaced — so all three of those
 * arrangements lost their reader, and each would have been a rule defending
 * a surface that no longer exists.
 */

import { useSyncExternalStore } from "react";
import { STORAGE_KEYS } from "~/lib/storage.ts";
import { resolves } from "~/app/routes.ts";

/** One place the reader was. */
export interface Recent {
  /** The route's own path segments — the identity, and what a click restores. */
  path: string[];
  /** What to call it. The label the screen itself showed, never re-derived. */
  label: string;
  /**
   * Which workspace it belongs to — the palette's hint beside the label, so
   * two places with one name ("Runbook" in two spaces) are told apart. The
   * Shell does not record a route no workspace owns, and `valid` refuses a
   * row without one.
   */
  workspace: string;
}

const KEY = STORAGE_KEYS.recents;

/**
 * How many are kept.
 *
 * Eight: enough that a morning's work is in the list and few enough that an
 * empty palette is scanned rather than searched — which is the whole
 * difference between recents and a history. ONE BOUND OVER THE WHOLE LIST,
 * because the palette draws the whole list: counted per workspace, as it was
 * while a sidebar drew one workspace's share, it let an empty palette open on
 * up to eight rows for each of nine workspaces.
 */
export const MaxRecents = 8;

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
 * separation is what stops one tab destroying another's list: `write`
 * replaces the WHOLE key, so a mutation built on a snapshot taken before the
 * other tab wrote hands back a list missing everything it did. Reproduced
 * over one origin: with a second tab open, three places visited in the first
 * were gone at the second tab's next click. `lib/starred.ts` had the same
 * shape and it was worse there, because a star is a decision somebody made.
 */
let cache: Recent[] | null = null;
const listeners = new Set<() => void>();

/** What is STORED, parsed fresh. The base of every write. */
function stored(): Recent[] {
  try {
    const raw = localStorage.getItem(KEY);
    const parsed: unknown = raw ? JSON.parse(raw) : [];
    return Array.isArray(parsed) ? parsed.filter(valid).slice(0, MaxRecents) : [];
  } catch {
    // A private window, blocked site data, or a value somebody's other tab
    // wrote in a shape this build does not have. None of them is a reason
    // for a palette not to open.
    return [];
  }
}

/** The snapshot this tab renders. See [cache]. */
function read(): Recent[] {
  if (cache) return cache;
  cache = stored();
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
    // A ROW WITH NO WORKSPACE is a shape an older build wrote, and one the
    // palette has no hint for — see [Recent.workspace].
    typeof r.workspace === "string" &&
    r.workspace !== "" &&
    // A PATH THE ROUTE TABLE NO LONGER HAS IS DROPPED ON READ. Storage
    // outlives every build: a row kept before a route moved would be drawn
    // as a row that leads to Not Found, forever, with nothing to say why.
    resolves(r.path)
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
 * KEYED ON THE PATH, so a place is here once however often it is opened, and
 * a visit moves it to the top. At the cap the bottom row leaves, which in
 * visit order is the place nobody has opened in longest.
 *
 * `named` IS WHETHER A SCREEN SUPPLIED THE LABEL, and it is required rather
 * than optional because its zero value is the bug. A label the screen has not
 * resolved yet is the route's own segment — a uuid for a turn — and every
 * screen publishes its name a render AFTER the route, so the first write of
 * every navigation carries the identifier and the second carries the name.
 * That is fine on a first visit and wrong on a REVISIT: the place already had
 * its name, and the visit replaced it with a hex string until the query came
 * back. So a name is never replaced by an identifier: an unnamed write to a
 * path this list already holds keeps the stored label. A path it does not
 * hold is stored either way, because an object NOTHING ever names — a turn
 * with no plan summary — has its id and nothing else, and dropping it would
 * lose a place the reader was.
 */
export function remember(entry: Recent, named: boolean): void {
  if (entry.path.length === 0 || entry.workspace === "") return;
  const key = entry.path.join("/");
  // STORED, NOT THE RENDER SNAPSHOT — see [cache]. A write replaces the whole
  // key, so building it on what this tab last painted hands back a list
  // missing everything another tab has done since.
  const held = stored();
  const was = held.find((r) => r.path.join("/") === key);
  const label = was && !named ? was.label : entry.label;
  const rest = held.filter((r) => r !== was);
  write([{ path: entry.path, label, workspace: entry.workspace }, ...rest].slice(0, MaxRecents));
}

/** Drop everything. For the palette's own "clear" command. */
export function forgetAll(): void {
  write([]);
}

/**
 * Subscribe, and follow ANOTHER TAB while anybody is.
 *
 * `storage` fires in every OTHER document of the origin, so this is what
 * lets a second tab's recent reach this one's palette rather than waiting for a
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
  return () => {
    listeners.delete(fn);
    if (listeners.size === 0) window.removeEventListener("storage", follow);
  };
}

function follow(e: StorageEvent): void {
  if (e.key !== null && e.key !== KEY) return;
  cache = null;
  for (const fn of listeners) fn();
}

/** The reader's recents, most recently visited first. */
export function useRecents(): Recent[] {
  return useSyncExternalStore(subscribe, read, () => []);
}

/** Test seam: drop the in-process cache so a fresh read hits storage. */
export function resetForTest(): void {
  cache = null;
}
