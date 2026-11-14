/**
 * The range a reader last chose on a screen, kept so the screen opens on it.
 *
 * # Why the address alone was not enough
 *
 * A screen's range lives in its address (`range=` on Home, `window=` wherever
 * `lib/range.ts` is read), which is right: a link carries it, Back walks it,
 * and a reload of that address keeps it. But the address is ONE HISTORY
 * ENTRY'S. Every other way to the screen (the sidebar, the palette, a new
 * tab, the page a sign-in lands on) arrives with no parameter, and a screen
 * that then read a constant showed seven days to the reader who had asked for
 * thirty a minute before. The choice was lost on every arrival that was not a
 * literal reload.
 *
 * # Two homes, one rule
 *
 * THE ADDRESS WINS, WHEN IT NAMES A RANGE: a link somebody shared opens the
 * range they were looking at, whatever its reader last chose. WHERE IT NAMES
 * NONE, the reader's own last choice for that screen is the range, and the
 * screen's fallback only where they have never made one.
 *
 * A CHOICE IS WRITTEN TO BOTH, and to the address in full, the fallback
 * included: an address with no range now means "the reader's own", so the one
 * way to say "seven days" to whoever opens the link is to say it.
 *
 * AND A KEPT CHOICE IS NOTED INTO THE ADDRESS on an arrival that named none,
 * without a history entry (`Navigator.note`), so the address a reader copies
 * says the range they are looking at. Left bare where the kept choice is the
 * screen's fallback or nothing is kept, so a reader who never touched the
 * control keeps a clean address.
 *
 * OPENING A LINK IS NOT A CHOICE. Only the control writes the store, so a
 * colleague's link to a quarter does not make every later visit a quarter.
 *
 * # What is kept
 *
 * ONE RECORD PER READER (`lib/readerStore.ts`), a word per screen. Only a
 * NAMED window is kept (`7d`, `today`): two instants a reader typed are a
 * question about one stretch of time, and a screen that reopened on last
 * Tuesday afternoon every morning would be answering a question nobody asked
 * again. STORAGE IS UNTRUSTED INPUT, so every word is checked against what the
 * screen offers at the moment it is read, and one the screen no longer has is
 * the fallback.
 */

import { useCallback, useEffect } from "react";
import { useNavigator, useRoute } from "~/app/router.tsx";
import { readerStore } from "./readerStore.ts";
import { STORAGE_KEYS } from "./storage.ts";

type Kept = Readonly<Record<string, string>>;

const NOTHING: Kept = {};

/** A bound on a stored word: every named window is a handful of characters. */
const MAX_WORD = 16;

/** Whatever parsed, as screen-to-word pairs; anything else is dropped. */
function keptOf(raw: unknown): Kept {
  if (typeof raw !== "object" || raw === null || Array.isArray(raw)) return NOTHING;
  const out: Record<string, string> = {};
  for (const [screen, word] of Object.entries(raw)) {
    if (typeof word === "string" && word !== "" && word.length <= MAX_WORD) out[screen] = word;
  }
  return out;
}

const store = readerStore<Kept>(STORAGE_KEYS.keptRanges, NOTHING, keptOf);

/** Keep `word` as this reader's range on `screen`. */
export function keepRange(screen: string, word: string): void {
  store.update((kept) => (kept[screen] === word ? kept : { ...kept, [screen]: word }));
}

/**
 * A screen's range parameter, kept across visits.
 *
 * `key` is the address parameter, `screen` names whose choice it is (screens
 * that share a name share a choice), `accepts` says which words the screen
 * keeps and reads back, and `kind` is the history rule a choice follows
 * (`useParam`'s). The setter's `keep` is false for a value that is shown but
 * not remembered: an interval a reader typed.
 */
export function useKeptRange(
  key: string,
  screen: string,
  fallback: string,
  accepts: (word: string) => boolean,
  kind: "section" | "filter",
): [string, (next: string, keep?: boolean) => void] {
  const route = useRoute();
  const nav = useNavigator();
  const kept = store.use()[screen];
  const own = kept !== undefined && accepts(kept) ? kept : null;
  const named = route.query.get(key);
  const value = named ?? own ?? fallback;

  // SAID IN THE ADDRESS, so a copied link opens what its sender saw.
  const unsaid = named === null && own !== null && own !== fallback ? own : null;
  // ON EVERY ARRIVAL, not once per value: the sidebar's link to the screen a
  // reader is already on moves the address back to the bare one.
  const arrival = route.hash;
  useEffect(() => {
    if (unsaid !== null) nav.note({ [key]: unsaid });
  }, [nav, key, unsaid, arrival]);

  const set = useCallback(
    (next: string, keep = true) => {
      if (keep) keepRange(screen, next);
      if (kind === "section") nav.section(key, next);
      else nav.filter({ [key]: next });
    },
    [nav, key, screen, kind],
  );
  return [value, set];
}
