/**
 * A configuration comparison (`config_diff`, `configapi.Change`) as the one
 * diff the dashboard draws: Settings › Configuration's Diff lens, and what a
 * revert would apply, read before it is confirmed.
 */

import type { DiffHunk } from "~/components/DiffView.tsx";
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
 * A comparison as the one diff the dashboard draws (`components/DiffView.tsx`),
 * KEYED by path: an added setting is its new value added, a removed one its
 * old value removed, and a CHANGED one is both, its old value removed and its
 * new value added, so the diff underlines what moved inside the value rather
 * than printing the two halves either side of an arrow.
 *
 * ONE HUNK A CHANGE, each skipping nothing: the changes are the whole answer,
 * not runs cut out of a longer text, and a hunk is what keeps one setting's
 * two lines together however the next change reads.
 */
export function configHunks(changes: readonly ConfigChange[]): DiffHunk[] {
  return changes.map(({ path, kind, from, to }) => ({
    skipped: 0,
    lines: [
      ...(kind === "added"
        ? []
        : [{ kind: "remove" as const, text: shownValue(from), before: null, after: null, path }]),
      ...(kind === "removed"
        ? []
        : [{ kind: "add" as const, text: shownValue(to), before: null, after: null, path }]),
    ],
  }));
}
