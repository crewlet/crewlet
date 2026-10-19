/**
 * Two versions of a deck, paired slide by slide: what the History tab's By
 * slide lens draws.
 *
 * PAIRED BY ID, the slide's own name in both versions, so an edited slide is
 * one row with two drawings rather than a removal and an addition. The rows
 * follow the newer version's order, a slide only the older one held placed
 * after the slide it followed there.
 *
 * A MOVE IS WHAT THE ORDER CHANGED, not every slide whose number shifted: the
 * slides both versions hold are aligned by their longest common order, and only
 * a slide outside it moved. Swapping two slides moves one; deleting the second
 * slide moves none.
 *
 * PURE: decks in, rows out.
 */

import type { ArtifactDeck, DeckSlide } from "~/protocol/index.ts";
import { slideSource } from "../slides.tsx";

/** What happened to one slide between two versions. */
export type SlideChange = "unchanged" | "changed" | "added" | "removed";

/** One slide of two versions. */
export interface DeckCompareRow {
  id: string;
  title: string;
  change: SlideChange;
  fromPosition?: number;
  toPosition?: number;
  moved: boolean;
  from?: DeckSlide;
  to?: DeckSlide;
}

/** The ids of a and b's longest common subsequence. */
function commonOrder(a: readonly string[], b: readonly string[]): Set<string> {
  const n = a.length;
  const m = b.length;
  const table: number[][] = Array.from({ length: n + 1 }, () => new Array<number>(m + 1).fill(0));
  for (let i = n - 1; i >= 0; i--) {
    for (let j = m - 1; j >= 0; j--) {
      table[i]![j] =
        a[i] === b[j] ? table[i + 1]![j + 1]! + 1 : Math.max(table[i + 1]![j]!, table[i]![j + 1]!);
    }
  }
  const out = new Set<string>();
  let i = 0;
  let j = 0;
  while (i < n && j < m) {
    if (a[i] === b[j]) {
      out.add(a[i]!);
      i++;
      j++;
    } else if (table[i + 1]![j]! >= table[i]![j + 1]!) i++;
    else j++;
  }
  return out;
}

/** older and newer, paired slide by slide, in the newer version's order. */
export function compareDecks(older: ArtifactDeck, newer: ArtifactDeck): DeckCompareRow[] {
  const from = older.slides ?? [];
  const to = newer.slides ?? [];
  const fromAt = new Map(from.map((s, i) => [s.id, i]));
  const toAt = new Map(to.map((s, i) => [s.id, i]));
  const both = (ids: DeckSlide[], other: Map<string, number>) =>
    ids.filter((s) => other.has(s.id)).map((s) => s.id);
  const kept = commonOrder(both(from, toAt), both(to, fromAt));

  const rows: DeckCompareRow[] = to.map((s, i) => {
    const j = fromAt.get(s.id);
    const was = j === undefined ? undefined : from[j];
    return {
      id: s.id,
      title: s.title || was?.title || s.id,
      change: !was ? "added" : slideSource(was) === slideSource(s) ? "unchanged" : "changed",
      ...(j === undefined ? {} : { fromPosition: j + 1 }),
      toPosition: i + 1,
      moved: was !== undefined && !kept.has(s.id),
      ...(was ? { from: was } : {}),
      to: s,
    };
  });
  // A SLIDE ONLY THE OLDER VERSION HELD goes after the slide it followed there
  // that the newer version still holds, or first.
  from.forEach((s, j) => {
    if (toAt.has(s.id)) return;
    const before = from
      .slice(0, j)
      .reverse()
      .find((p) => rows.some((r) => r.id === p.id));
    const at = before ? rows.findIndex((r) => r.id === before.id) + 1 : 0;
    rows.splice(at, 0, {
      id: s.id,
      title: s.title || s.id,
      change: "removed",
      fromPosition: j + 1,
      moved: false,
      from: s,
    });
  });
  return rows;
}
