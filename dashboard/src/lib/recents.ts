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
 * # A SLOT IS STABLE, AND THAT IS THE ORDER THIS LIST KEEPS
 *
 * The stored order is ARRIVAL order — a place the reader has not been enters
 * at the top, and going back to one already here moves nothing. It used to be
 * visit order, every visit re-inserting at index 0, and that is a defect the
 * moment the list is DRAWN rather than searched: pressing the third row of
 * the workspace sidebar's Recent section sent it to the first and slid the two
 * above it down, so the list rearranged itself under the pointer at the
 * instant it was hit. `.side-row` eases its colours and nothing else, and the
 * React key is the path, so the browser MOVES the existing node — an
 * untweened jump rather than anything a transition could soften.
 *
 * No launcher does this. VS Code's Recent list, JetBrains' Recent Files and a
 * browser's own history sidebar all re-sort when the surface is opened, never
 * while it is being used, because a rail is navigated by POSITION and a rail
 * that re-sorts on use destroys the only thing that makes it faster than
 * searching.
 *
 * # …AND THE PALETTE STILL GETS VISIT ORDER
 *
 * Which is not a contradiction, it is the other surface. The palette is
 * opened fresh, ranks what it offers, and closes — so it HAS a re-sort
 * boundary the rail does not, and "where I just was" is exactly what a reader
 * opening an empty one wants first. [useRecentsByVisit] is that order,
 * derived at read time from the `at` this module has stored all along and
 * until now never read.
 *
 * The cap evicts by `at` for the same reason: the entry to lose is the one
 * nobody has opened in longest, never whichever happens to sit at the bottom
 * of a list that no longer moves.
 *
 * AND THAT EVICTION CAN TAKE A ROW OUT OF THE MIDDLE, which is the one thing
 * this arrangement gives up: the old rule always dropped the bottom row. It
 * only ever fires when a place the reader has not been arrives, which already
 * moves every row down one, so there is no gesture under which the rail moves
 * and the reader was not asking for it — and dropping the bottom row instead
 * is exactly what would lose the board somebody opens every morning.
 */

import { useMemo, useSyncExternalStore } from "react";

/** One place the reader was. */
export interface Recent {
  /** The route's own path segments — the identity, and what a click restores. */
  path: string[];
  /** What to call it. The label the screen itself showed, never re-derived. */
  label: string;
  /**
   * Which workspace it belongs to.
   *
   * PART OF THE IDENTITY, not a decoration: the rail draws one workspace's
   * rows and the cap counts them per workspace, so a row with none is a row
   * nothing can draw and nothing can evict. `valid` refuses one, and the
   * Shell does not record a route no workspace owns.
   */
  workspace: string;
  /** When it was last opened, epoch ms. */
  at: number;
}

const KEY = "crewlet_recents";

/**
 * How many are kept, PER WORKSPACE.
 *
 * Eight: enough that a morning's work is in the list and few enough that the
 * list is scanned rather than searched — which is the whole difference between
 * recents and a history.
 *
 * AND THAT IS A CLAIM ABOUT THE DRAWN LIST, which is why the bucket is the
 * workspace. It was a single global bound, written when the command palette
 * was the only reader and the list it offered was the whole of it. The
 * workspace sidebar's Recent section came later and draws one workspace's
 * share — `sections` are appended to whichever tree is shown — so across a
 * rail of eight workspaces the reader saw one or two rows where the number
 * says eight, and a morning spent in Work could push every Activity row out
 * of a rail that had no Work rows in it either.
 */
export const MaxRecents = 8;

/**
 * At most [MaxRecents] per workspace, oldest visit first to go.
 *
 * ARRIVAL ORDER IS PRESERVED — this drops rows, it never reorders them — and
 * the bucket is what the rail filters on, so the number the cap counts and
 * the number a reader sees are the same number.
 */
function capped(rows: readonly Recent[]): Recent[] {
  const over = new Map<string, Recent[]>();
  for (const row of rows) {
    const bucket = over.get(row.workspace);
    if (bucket) bucket.push(row);
    else over.set(row.workspace, [row]);
  }
  const drop = new Set<Recent>();
  for (const bucket of over.values()) {
    if (bucket.length <= MaxRecents) continue;
    // The oldest VISIT leaves, not the last row in the bucket — see
    // [remember]. Sorted on a copy: `bucket` is this pass's own array, but
    // its entries are the caller's and their order is the answer.
    const byVisit = [...bucket].sort((a, b) => a.at - b.at);
    for (const row of byVisit.slice(0, bucket.length - MaxRecents)) drop.add(row);
  }
  return drop.size === 0 ? [...rows] : rows.filter((row) => !drop.has(row));
}

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
    return Array.isArray(parsed) ? capped(parsed.filter(valid)) : [];
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
    // A ROW WITH NO WORKSPACE CAN BE DRAWN BY NOTHING — see [Recent.workspace]
    // — so it is a shape this build cannot use rather than one it renders
    // into a rail that has no section for it.
    typeof r.workspace === "string" &&
    r.workspace !== "" &&
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
 * KEYED ON THE PATH, so a place is here once however often it is opened.
 *
 * `named` IS WHETHER A SCREEN SUPPLIED THE LABEL, and it is required rather
 * than optional because its zero value is the bug. A label the screen has not
 * resolved yet is the route's own segment — a uuid for a turn — and every
 * screen publishes its name a render AFTER the route, so the first write of
 * every navigation carries the identifier and the second carries the name.
 * That is fine on a first visit and wrong on a REVISIT: the row the reader
 * pressed already had its name, and the click replaced it with a hex string
 * until the query came back. Which is the flash they see, on the row they
 * aimed at, caused by the act of aiming at it.
 *
 * So a name is never replaced by an identifier: an unnamed write to a path
 * this list already holds keeps the stored label and moves only `at`. A path
 * it does not hold is stored either way, because an object NOTHING ever names
 * — a turn with no plan summary — has its id and nothing else, and a rail that
 * dropped it would lose a place the reader was.
 *
 * A REVISIT KEEPS ITS SLOT: only `at` moves, and nothing the reader is
 * looking at does. See the module doc for why a drawn list may not re-sort
 * under the pointer that is using it.
 *
 * A PLACE THE READER HAS NOT BEEN ENTERS AT THE TOP, and at the cap the entry
 * with the OLDEST VISIT leaves — not the last one in the list, which under
 * arrival order is simply the one that has been here longest. That is what
 * keeps the board somebody opens every morning alive although it never moves.
 * The cap is per WORKSPACE, so a morning in Work cannot empty the Activity
 * rail; see [MaxRecents].
 */
export function remember(entry: Omit<Recent, "at">, named: boolean): void {
  if (entry.path.length === 0 || entry.workspace === "") return;
  const key = entry.path.join("/");
  const at = Date.now();
  // STORED, NOT THE RENDER SNAPSHOT — see [cache]. A write replaces the whole
  // key, so building it on what this tab last painted hands back a list
  // missing everything another tab has done since.
  const held = stored();
  const found = held.findIndex((r) => r.path.join("/") === key);
  if (found >= 0) {
    const was = held[found]!;
    const next = held.slice();
    next[found] = { ...entry, label: named ? entry.label : was.label, at };
    write(next);
    return;
  }
  write(capped([{ ...entry, at }, ...held]));
}

/** Drop everything. For the palette's own "clear" command. */
export function forgetAll(): void {
  write([]);
}

/**
 * Subscribe, and follow ANOTHER TAB while anybody is.
 *
 * `storage` fires in every OTHER document of the origin, so this is what
 * lets a second tab's recent reach this one's rail rather than waiting for a
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

/**
 * The reader's recents, in ARRIVAL order — newest arrival first, and a
 * revisit moving nothing. This is the order a drawn list takes.
 */
export function useRecents(): Recent[] {
  return useSyncExternalStore(subscribe, read, () => []);
}

/**
 * The same places, most recently VISITED first.
 *
 * For a surface that is opened fresh each time and therefore has a re-sort
 * boundary a rail does not — the command palette, which ranks an empty query
 * on "where you just were". Sorted on read rather than stored that way,
 * because storing it is what made the rail jump; see the module doc.
 *
 * A COPY, never `held.sort(...)`: the argument is the live snapshot every
 * other reader shares, and sorting in place would reorder the rail from
 * inside the palette.
 */
export function useRecentsByVisit(): Recent[] {
  const held = useRecents();
  return useMemo(() => [...held].sort((a, b) => b.at - a.at), [held]);
}

/** Test seam: drop the in-process cache so a fresh read hits storage. */
export function resetForTest(): void {
  cache = null;
}
