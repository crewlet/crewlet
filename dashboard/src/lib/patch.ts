/**
 * One file's patch, as git writes it, read into the hunks `DiffView` draws.
 *
 * The engine answers a change's diff a file at a time, as the text git's own
 * `diff` writes (`code.DiffAnswer.Patch`): header lines naming the paths, then
 * hunks, each opening `@@ -<old>,<n> +<new>,<m> @@` and holding its lines, a
 * space before an unchanged one, `+` before an added one and `-` before a
 * removed one. That text IS the diff — git chose which lines it pairs and
 * where each hunk starts — so it is read here as written, never compared
 * again in the browser: a second comparison could only disagree with the one
 * the reviewer's tools and the engine's findings were placed by.
 *
 * WHAT A HUNK SKIPPED is counted from its header, so a hunk says how many
 * unchanged lines lie between it and the last (`DiffView` draws the count): a
 * gap closed in silence makes a file edited at both ends look like one
 * rewritten in the middle. A page cut at a line boundary ends inside a hunk,
 * and the lines it holds are drawn as far as they go.
 */

import type { DiffHunk, DiffLine } from "~/components/DiffView.tsx";

/** A hunk's header: where it starts on each side, and how many lines it spans there. */
const HUNK_HEADER = /^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@/;

/** One file's patch, as hunks with their lines numbered on both sides. */
export function parsePatch(patch: string): DiffHunk[] {
  const hunks: { lines: DiffLine[]; skipped: number }[] = [];
  let current: { lines: DiffLine[]; skipped: number } | null = null;
  let before = 0;
  let after = 0;
  // WHERE THE LAST HUNK ENDED in the older text: the line after its last.
  let ended = 1;
  for (const raw of patch.split("\n")) {
    const header = HUNK_HEADER.exec(raw);
    if (header) {
      const oldStart = Number(header[1]);
      const oldLines = header[2] === undefined ? 1 : Number(header[2]);
      const newStart = Number(header[3]);
      // A HUNK THAT ADDS TO AN EMPTY SIDE starts at 0 there, git's way of
      // saying "before the first line".
      before = oldLines === 0 ? oldStart + 1 : oldStart;
      after = newStart === 0 ? 1 : newStart;
      current = { lines: [], skipped: Math.max(0, before - ended) };
      ended = before + oldLines;
      hunks.push(current);
      continue;
    }
    // EVERYTHING BEFORE THE FIRST HUNK IS ITS HEADER — the paths, the modes,
    // the blobs — and a line git marks as no line ("\ No newline at end of
    // file") is no line of either text.
    if (!current || raw.startsWith("\\")) continue;
    const mark = raw[0];
    const text = raw.slice(1);
    if (mark === "+") {
      current.lines.push({ kind: "add", text, before: null, after: after++ });
    } else if (mark === "-") {
      current.lines.push({ kind: "remove", text, before: before++, after: null });
    } else if (mark === " ") {
      current.lines.push({ kind: "same", text, before: before++, after: after++ });
    } else if (raw === "") {
      // THE TEXT'S OWN END: the newline that closes the last line splits into
      // one empty string after it, which is no line.
      continue;
    }
  }
  return hunks.filter((h) => h.lines.length > 0);
}

/** Where a line is on the side of a change's diff a finding names: its newer number, else its older. */
export function lineSide(line: DiffLine): { side: "new" | "old"; n: number } | null {
  if (line.after !== null) return { side: "new", n: line.after };
  if (line.before !== null) return { side: "old", n: line.before };
  return null;
}
