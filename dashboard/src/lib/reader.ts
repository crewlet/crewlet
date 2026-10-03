/**
 * Who this TAB is being read by: the principal it last knew it was signed in
 * as.
 *
 * # Why a tab needs to know
 *
 * Everything a tab holds in memory and in its own storage was read by, or
 * kept for, one person: the store's company state, every cached answer, the
 * organization builder's kept draft, the recents and stars the rail draws. A
 * sign-out hands the tab on by reloading it with its storage emptied
 * (`lib/session.ts`), but a session also ends WITHOUT one — an idle deadline,
 * a revocation, a sign-out in another tab — and the tab is then routed to the
 * sign-in screen with all of that still in it. Whoever signs in next was
 * handed the last person's company, and their unsaved draft as their own. So
 * every sign-in asks whether it is the SAME person this tab was read by, and
 * that question needs an answer that outlives the session.
 *
 * # What it is, and where it is kept
 *
 * The principal's ID — a person's, or the one a Tier A token acts under —
 * which is what every sign-in answers and `GET /auth/session` says. Never a
 * login: a login is a name somebody renames, and the next holder of a freed
 * one is somebody else.
 *
 * In the tab's `sessionStorage`, because it describes THIS TAB — two tabs of
 * one browser may each have been read by a different person before the cookie
 * they share moved — and because it must survive the reload a hand-over is.
 * It goes with everything else the tab holds when a sign-out empties that
 * storage.
 *
 * # Two writers, and they differ on purpose
 *
 * A SIGN-IN RECORDS ([noteReader]): it is the one moment this tab learns, from
 * the engine's own answer, who it is about to be read by — and it asks
 * [currentReader] first, to decide whether the tab changes hands. The FRAME
 * ADOPTS ([adoptReader]) the session it finds on a tab nobody has recorded yet
 * — one opened with a session already in the browser — and never overwrites
 * one: a cookie that moved under an open tab (somebody signing in as somebody
 * else in another tab) does not make this tab's memory theirs, and the next
 * sign-in here is decided against who actually read it.
 */

import { useSyncExternalStore } from "react";
import { STORAGE_KEYS } from "./storage.ts";

const KEY = STORAGE_KEYS.reader;

const listeners = new Set<() => void>();

/**
 * The principal this tab is read by, or null when it has not learned one.
 *
 * READ FROM STORAGE EVERY TIME rather than cached: it is one small value, a
 * string is a stable snapshot by value, and a cache is a second idea of who
 * the tab is — one a `sessionStorage.clear()` would not reach.
 */
export function currentReader(): string | null {
  try {
    return sessionStorage.getItem(KEY) || null;
  } catch {
    // A browser that refuses storage keeps nothing — and has nothing kept in
    // it for the next person either.
    return null;
  }
}

function write(person: string): void {
  try {
    sessionStorage.setItem(KEY, person);
  } catch {
    // Refused: the tab cannot remember who read it, so the next sign-in has
    // nothing to compare — and nothing this tab kept in storage survives for
    // anybody to be handed.
  }
  for (const fn of listeners) fn();
}

/** Record who a sign-in answered for. See the module doc. */
export function noteReader(person: string): void {
  if (person === "" || currentReader() === person) return;
  write(person);
}

/** Record the session the frame found, on a tab nobody has recorded yet. */
export function adoptReader(person: string): void {
  if (person === "" || currentReader() !== null) return;
  write(person);
}

/**
 * Delete every per-reader key under `prefix` in the browser's `localStorage`
 * but `keep` — every one of them, where `keep` is null.
 *
 * FOR A LIST KEPT PER READER (`lib/recents.ts`, `lib/starred.ts`), which is
 * kept per browser and so outlives the tab: its key names whose it is, and
 * this is how the browser comes to hold nobody's but the person signed in.
 * The keys are gathered before any is removed, because removing one moves
 * every index after it. A storage that refuses is left alone — it could keep
 * nothing to leave behind.
 */
export function forgetReadersUnder(prefix: string, keep: string | null): void {
  try {
    const doomed: string[] = [];
    for (let i = 0; i < localStorage.length; i++) {
      const key = localStorage.key(i);
      if (key !== null && key.startsWith(prefix) && key !== keep) doomed.push(key);
    }
    for (const key of doomed) localStorage.removeItem(key);
  } catch {
    // Blocked site data or a private window: nothing was written to delete.
  }
}

/** Subscribe to a change of reader. Returns the unsubscribe. */
export function onReader(fn: () => void): () => void {
  listeners.add(fn);
  return () => {
    listeners.delete(fn);
  };
}

/** The tab's reader, re-rendering when it is learned. */
export function useReader(): string | null {
  return useSyncExternalStore(onReader, currentReader, () => null);
}
