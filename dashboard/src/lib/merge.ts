/**
 * Bringing somebody else's save into an edit that started before it: a
 * three-way merge by line.
 *
 * A page save states the version it edited, and the engine refuses one that
 * raced a newer save (`stale_version`) — so the refusal is what keeps a
 * paragraph from being silently overwritten. The editor can only honour that
 * if moving an edit onto the newer version CARRIES the newer version's
 * change: a draft built on revision 2, re-based onto revision 3 with its
 * text untouched, saves revision 2's text plus the person's edit, and the
 * engine — told the edit is against 3 — accepts it and deletes whatever 3
 * added. That is the exact loss the refusal exists to prevent, laundered
 * through the browser.
 *
 * THE CLASSIC DIFF3 SHAPE. Both edits are expressed as hunks over the
 * common ancestor (the version the edit started from); a hunk only one side
 * made is applied, a hunk both sides made identically is applied once, and
 * hunks that overlap and differ are a CONFLICT the person resolves. Nothing is
 * guessed: the browser never decides whose sentence wins.
 *
 * TOUCHING IS NOT OVERLAPPING — two hunks that replace adjacent ranges of
 * the ancestor merge cleanly — EXCEPT for an insertion at the boundary of
 * the other side's change, whose order relative to that change nothing
 * decides. Treated as a conflict, the person places it; merged, the browser
 * would pick an order and present it as what somebody wrote.
 *
 * Over the diff in `diff.ts`, so its cap applies: a document past
 * `MaxDiffLines` diffs as a whole-document replacement, and two of those
 * conflict as a whole — which is honest about a comparison the browser
 * declined to make.
 *
 * PURE, and in `lib/` for the reason the rest of this directory gives.
 */

import { diffLines, lines } from "./diff.ts";

/** One side's change to the ancestor: replace `[start, end)` with `lines`. */
interface Hunk {
  start: number;
  end: number;
  lines: string[];
  side: "mine" | "theirs";
}

/** A passage of the merged document. */
export type MergeChunk =
  | { kind: "clean"; lines: string[] }
  | {
      kind: "conflict";
      /** The ancestor's lines both sides changed. */
      base: string[];
      /** What this edit has there. */
      mine: string[];
      /** What the newer save has there. */
      theirs: string[];
    };

/** The merge of two edits of one ancestor. */
export interface Merge {
  chunks: MergeChunk[];
  /** How many chunks are conflicts; zero means `text(merge)` is the answer. */
  conflicts: number;
  /** Whether the result should end with a newline — the draft's own choice. */
  trailingNewline: boolean;
}

/** How a person resolved one conflict. */
export type Resolution = "mine" | "theirs" | "both";

/** The hunks one side made, in ancestor order. */
function hunks(base: string, edited: string, side: Hunk["side"]): Hunk[] {
  const out: Hunk[] = [];
  let open: Hunk | null = null;
  let at = 0; // lines of the ancestor consumed so far
  for (const line of diffLines(base, edited)) {
    if (line.kind === "same") {
      open = null;
      at++;
      continue;
    }
    if (!open) {
      open = { start: at, end: at, lines: [], side };
      out.push(open);
    }
    if (line.kind === "remove") {
      open.end++;
      at++;
    } else {
      open.lines.push(line.text);
    }
  }
  return out;
}

/** Whether two hunks from different sides cannot both be applied as they are. */
function clash(a: Hunk, b: Hunk): boolean {
  if (a.start < b.end && b.start < a.end) return true; // overlapping ranges
  // AN INSERTION AT THE OTHER'S BOUNDARY: which comes first is nobody's
  // decision on record. Two replacements that merely touch are independent.
  const aInsert = a.start === a.end;
  const bInsert = b.start === b.end;
  if (aInsert && (a.start === b.start || a.start === b.end)) return true;
  if (bInsert && (b.start === a.start || b.start === a.end)) return true;
  return false;
}

/** Whether two lists of lines are the same text. */
function same(a: string[], b: string[]): boolean {
  return a.length === b.length && a.every((line, i) => line === b[i]);
}

/**
 * One side's text over the ancestor range `[start, end)`, given the hunks it
 * made inside it: the ancestor's lines, with each hunk's replacement.
 */
function over(ancestor: string[], start: number, end: number, made: Hunk[]): string[] {
  const out: string[] = [];
  let at = start;
  for (const h of made) {
    out.push(...ancestor.slice(at, h.start), ...h.lines);
    at = h.end;
  }
  out.push(...ancestor.slice(at, end));
  return out;
}

/**
 * Merge `mine` and `theirs`, two edits of `base`.
 *
 * The result is a sequence of clean passages and conflicts, in document order.
 */
export function merge3(base: string, mine: string, theirs: string): Merge {
  const ancestor = lines(base);
  const all = [...hunks(base, mine, "mine"), ...hunks(base, theirs, "theirs")].sort(
    (a, b) => a.start - b.start || a.end - b.end,
  );

  // GROUP what must be decided together: a hunk joins the current group when
  // it clashes with any hunk of the OTHER side already in it. Hunks from one
  // side never clash with each other — one diff does not overlap itself.
  const groups: Hunk[][] = [];
  for (const h of all) {
    const last = groups[groups.length - 1];
    if (last && last.some((g) => g.side !== h.side && clash(g, h))) last.push(h);
    else groups.push([h]);
  }

  const chunks: MergeChunk[] = [];
  const clean = (text: string[]) => {
    if (text.length === 0) return;
    const prev = chunks[chunks.length - 1];
    if (prev?.kind === "clean") prev.lines.push(...text);
    else chunks.push({ kind: "clean", lines: [...text] });
  };

  let at = 0;
  let conflicts = 0;
  for (const group of groups) {
    const start = Math.min(...group.map((h) => h.start));
    const end = Math.max(...group.map((h) => h.end));
    clean(ancestor.slice(at, start));
    const ours = group.filter((h) => h.side === "mine");
    const their = group.filter((h) => h.side === "theirs");
    const mineText = over(ancestor, start, end, ours);
    const theirText = over(ancestor, start, end, their);
    if (ours.length === 0) clean(theirText);
    else if (their.length === 0) clean(mineText);
    else if (same(mineText, theirText))
      clean(mineText); // both made the same change
    else {
      conflicts++;
      chunks.push({
        kind: "conflict",
        base: ancestor.slice(start, end),
        mine: mineText,
        theirs: theirText,
      });
    }
    at = end;
  }
  clean(ancestor.slice(at));

  const ends = (s: string) => /\n$/.test(s.replace(/\r\n?/g, "\n"));
  return { chunks, conflicts, trailingNewline: ends(mine) || (mine === "" && ends(theirs)) };
}

/**
 * The merged document, with each conflict resolved as `choices` says.
 *
 * A conflict with no choice is refused rather than defaulted: which side wins
 * is the person's answer, and a default would be the browser's.
 */
export function mergedText(merge: Merge, choices: Resolution[] = []): string {
  const out: string[] = [];
  let n = 0;
  for (const chunk of merge.chunks) {
    if (chunk.kind === "clean") {
      out.push(...chunk.lines);
      continue;
    }
    const choice = choices[n++];
    if (choice === "mine") out.push(...chunk.mine);
    else if (choice === "theirs") out.push(...chunk.theirs);
    else if (choice === "both") out.push(...chunk.mine, ...chunk.theirs);
    else throw new Error(`conflict ${n} of ${merge.conflicts} has no resolution`);
  }
  if (out.length === 0) return "";
  return out.join("\n") + (merge.trailingNewline ? "\n" : "");
}
