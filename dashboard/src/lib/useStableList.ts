/**
 * A derived list that keeps its rows when nothing in them changed.
 *
 * A screen that derives its rows from live state derives NEW rows on every
 * push, the same in every field, and a row component compared by identity
 * draws each one again: a list fed thirteen seat pushes a second redraws
 * thirteen times a second for nothing a person can see. This hands back the
 * row held for a key while it says the same thing (`sameValue`), and the list
 * held while every row is held in the same order, so `memo` and the effects
 * keyed on the list see a change only where there is one.
 *
 * A ROW'S KEY IS ITS IDENTITY: a key that moves is a row that moved, and one
 * that is gone is not held, so the cache is the list last drawn and nothing
 * older.
 */

import { useRef } from "react";
import { sameValue } from "~/protocol/equal.ts";

export function useStableList<T>(list: readonly T[], keyOf: (row: T) => string): readonly T[] {
  const held = useRef<{ list: readonly T[]; byKey: Map<string, T> }>({
    list: [],
    byKey: new Map(),
  });
  const was = held.current;
  if (list === was.list) return was.list;
  const byKey = new Map<string, T>();
  let same = list.length === was.list.length;
  const next = list.map((row, i) => {
    const key = keyOf(row);
    const prior = was.byKey.get(key);
    const kept = prior !== undefined && sameValue(prior, row) ? prior : row;
    byKey.set(key, kept);
    if (kept !== was.list[i]) same = false;
    return kept;
  });
  // WRITTEN DURING RENDER, as a cache and nothing else: what it holds is a
  // function of the lists drawn, so a render React discards and runs again
  // computes the same answer from it.
  held.current = same ? { list: was.list, byKey: was.byKey } : { list: next, byKey };
  return held.current.list;
}
