/**
 * A configuration comparison (`config_diff`, `configapi.Change`) as the one
 * diff the dashboard draws: Settings › Configuration's Diff lens, and what a
 * revert would apply, read before it is confirmed.
 */

import type { DiffHunk, DiffLine } from "~/components/DiffView.tsx";
import type { ConfigChange } from "~/protocol/index.ts";

/**
 * A value as a diff draws it: JSON, so `"true"` and `true` stay two different
 * settings (the harness's `renderValue` rule), and an object or a list in the
 * document's own indented shape rather than as one run-on line.
 *
 * ABSENT IS `null`. `configapi.Change` omits an empty side, and Go's
 * `omitempty` on an `any` drops a JSON null with it, so a setting changed
 * from null arrives with no `from` at all, and `JSON.stringify` of that is
 * `undefined`, which the line would print as the word.
 */
function shownValue(value: unknown): string {
  if (value !== null && typeof value === "object") return JSON.stringify(value, null, 2);
  return JSON.stringify(value) ?? "null";
}

/**
 * One side of a setting as the diff's lines: ONE DIFF LINE PER LINE OF THE
 * VALUE, so every line of it carries its glyph and its surface step.
 *
 * Handed to the diff as one line, a value of twelve lines (a seat,
 * `roles[1]`) was marked on its first row alone: the glyph sat beside the
 * path and the rest of the object hung in the value track unmarked, so a
 * removed seat read as one removed line over eleven of nothing. Every line
 * names its setting's path, which is what pairs it with the other side's
 * lines of that setting; the diff writes the path once, on the first line of
 * the run (`components/DiffView.tsx`).
 */
function valueLines(kind: "add" | "remove", value: unknown, path: string): DiffLine[] {
  return shownValue(value)
    .split("\n")
    .map((text) => ({ kind, text, before: null, after: null, path }));
}

/**
 * A comparison as the one diff the dashboard draws (`components/DiffView.tsx`),
 * KEYED by path: an added setting is its new value added, a removed one its
 * old value removed, and a CHANGED one is both, its old value removed and its
 * new value added, so the diff underlines what moved inside the value rather
 * than printing the two halves either side of an arrow.
 *
 * ONE HUNK A CHANGE, each skipping nothing: the changes are the whole answer,
 * not runs cut out of a longer text, and a hunk is what keeps one setting's
 * lines together however the next change reads.
 */
export function configHunks(changes: readonly ConfigChange[]): DiffHunk[] {
  return changes.map(({ path, kind, from, to }) => ({
    skipped: 0,
    lines: [
      ...(kind === "added" ? [] : valueLines("remove", from, path)),
      ...(kind === "removed" ? [] : valueLines("add", to, path)),
    ],
  }));
}
