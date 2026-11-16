/**
 * Two versions of a markdown document compared as a reader reads them: by
 * block, then by word inside a block that was edited.
 *
 * A line diff of the source shows a paragraph edited in three words as the
 * whole paragraph removed and added, in markdown with its link targets. Here
 * the blocks are compared first (a list item is a block of its own), a removed
 * block and an added one of the same shape and mostly the same words are one
 * block edited, and only its changed words are marked. A link, a code span,
 * an image and an address are compared whole, so a reader never sees half a
 * link target. Both comparisons are `lib/lineDiff.ts`'s, bounds included.
 *
 * PURE: what changed is held by `proseDiff.test.ts`; `components/ProseDiff.tsx`
 * draws it.
 */

import { markdownInline, parseMarkdown, type MarkdownBlock } from "@crewlethq/ui";
import { compareSequences, MAX_COMPARE_LINES, MAX_EDITS, type Uncompared } from "./lineDiff.ts";
import { inlineTokens } from "./inlineTokens.ts";
import { inlineRuns, inlineText } from "./markdownText.ts";
import { tokens } from "./wordDiff.ts";

/** One run of a block's inline markdown: kept (`=`), added (`+`) or removed (`-`). */
export interface WordPart {
  op: "=" | "+" | "-";
  /** The run's markdown source, whole inline tokens only, so it renders on its own. */
  text: string;
}

/** One line of a code block that changed. */
export interface CodeLine {
  op: "=" | "+" | "-";
  text: string;
}

/** One block of the comparison, in the newer document's reading order. */
export type ProseBlock =
  | { change: "same" | "added" | "removed"; block: MarkdownBlock; words: number }
  | {
      change: "changed";
      /** The newer block, or for a table the two merged row by row. */
      block: MarkdownBlock;
      /** One per inline run of `block`, in `inlineRuns` order. */
      runs: WordPart[][];
      /** A code block's lines, in place of `runs`. */
      code?: CodeLine[];
      added: number;
      removed: number;
    };

export type ProseComparison =
  | {
      kind: "compared";
      blocks: ProseBlock[];
      /** Words added and removed, whole blocks included. */
      added: number;
      removed: number;
    }
  | Uncompared;

/**
 * How much of two blocks' words must be shared for them to be one block
 * edited rather than one removed and another added: half, so a sentence with
 * a clause rewritten is still marked in place and a paragraph rewritten is not
 * a confetti of marks.
 */
export const SAME_BLOCK = 0.5;

/** How far ahead a removed block looks for the added block it became. */
const PAIR_WINDOW = 8;

/**
 * An unchanged stretch this short between two changes is folded into them:
 * one word, or only spaces and punctuation, marked on its own reads as noise.
 */
const SHORT_KEPT_WORDS = 1;

const WORD = /[\p{L}\p{N}]/u;

/** Compares two markdown documents by block, then by word. */
export function compareProse(before: string, after: string): ProseComparison {
  const a = blocksOf(before);
  const b = blocksOf(after);
  const compared = compareSequences(a.map(keyOf), b.map(keyOf));
  if (compared.kind !== "compared") return compared;
  const out: ProseBlock[] = [];
  let x = 0;
  let y = 0;
  for (const run of compared.runs) {
    if (run.kind === "same") {
      for (let i = 0; i < run.count; i++) {
        out.push({ change: "same", block: b[y + i]!, words: wordsOf(b[y + i]!) });
      }
      x += run.count;
      y += run.count;
      continue;
    }
    out.push(...paired(a.slice(x, x + run.removed), b.slice(y, y + run.added)));
    x += run.removed;
    y += run.added;
  }
  let added = 0;
  let removed = 0;
  for (const block of out) {
    if (block.change === "added") added += block.words;
    else if (block.change === "removed") removed += block.words;
    else if (block.change === "changed") {
      added += block.added;
      removed += block.removed;
    }
  }
  return { kind: "compared", blocks: out, added, removed };
}

/** A document's blocks, each list item a one-item list of its own. */
export function blocksOf(source: string): MarkdownBlock[] {
  return parseMarkdown(source ?? "").flatMap((block): MarkdownBlock[] =>
    block.kind === "list"
      ? block.items.map((item, i) => ({
          ...block,
          start: block.ordered ? block.start + i : block.start,
          items: [item],
        }))
      : [block],
  );
}

/** A block's identity for the comparison; an ordered item's number is not part of it. */
function keyOf(block: MarkdownBlock): string {
  return JSON.stringify(block.kind === "list" ? { ...block, start: 0 } : block);
}

/** How many words a block reads as. */
export function wordsOf(block: MarkdownBlock): number {
  const text = block.kind === "code" ? block.text : inlineRuns(block).map(inlineText).join(" ");
  return countWords(text);
}

function countWords(text: string): number {
  return tokens(text).filter((t) => t.word).length;
}

/**
 * The blocks one change removed and added, paired where a removed block and
 * an added one are one block edited, in the newer document's order: a removed
 * block stands where it stood, before the blocks added after it.
 */
function paired(removed: MarkdownBlock[], added: MarkdownBlock[]): ProseBlock[] {
  const out: ProseBlock[] = [];
  let j = 0;
  for (const old of removed) {
    let match = -1;
    let edited: ProseBlock | null = null;
    for (let k = j; k < Math.min(added.length, j + PAIR_WINDOW); k++) {
      edited = edit(old, added[k]!);
      if (edited) {
        match = k;
        break;
      }
    }
    if (!edited) {
      out.push({ change: "removed", block: old, words: wordsOf(old) });
      continue;
    }
    for (; j < match; j++)
      out.push({ change: "added", block: added[j]!, words: wordsOf(added[j]!) });
    out.push(edited);
    j = match + 1;
  }
  for (; j < added.length; j++)
    out.push({ change: "added", block: added[j]!, words: wordsOf(added[j]!) });
  return out;
}

/** `old` edited into `now`, or null when they are not one block: another shape, or too few shared words. */
function edit(old: MarkdownBlock, now: MarkdownBlock): ProseBlock | null {
  if (old.kind === "table" && now.kind === "table") return editTable(old, now);
  if (old.kind === "code" && now.kind === "code") return editCode(old, now);
  if (shapeOf(old) !== shapeOf(now)) return null;
  if (!sameBlock(wordList(old), wordList(now))) return null;
  const before = inlineRuns(old);
  const runs = inlineRuns(now).map((text, i) => compareWords(before[i] ?? "", text));
  return { change: "changed", block: now, runs, ...counted(runs) };
}

/** What must match for two blocks to be one edited: the kind, the nesting and the number of runs. */
function shapeOf(block: MarkdownBlock): string {
  switch (block.kind) {
    case "heading":
      return "heading";
    case "paragraph":
      return "paragraph";
    case "list":
      return `list:${block.ordered}:${block.items.map((i) => `${i.checked !== null}:${i.blocks.map(shapeOf).join(",")}`).join(";")}`;
    case "quote":
    case "callout":
      return `${block.kind}:${block.blocks.map(shapeOf).join(",")}`;
    default:
      return block.kind;
  }
}

/** A table edited: its rows aligned, and its cells compared by word where a row was edited. */
function editTable(
  old: Extract<MarkdownBlock, { kind: "table" }>,
  now: Extract<MarkdownBlock, { kind: "table" }>,
): ProseBlock | null {
  if (old.head.length !== now.head.length) return null;
  const rowKey = (row: string[]) => JSON.stringify(row);
  const compared = compareSequences(old.rows.map(rowKey), now.rows.map(rowKey));
  if (compared.kind !== "compared") return null;
  const head = now.head.map((cell, i) => compareWords(old.head[i] ?? "", cell));
  const rows: string[][] = [];
  const cells: WordPart[][] = [];
  const row = (texts: string[], parts: WordPart[][]) => {
    rows.push(texts);
    cells.push(...parts);
  };
  const whole = (texts: string[], op: WordPart["op"]) =>
    row(
      texts,
      texts.map((text) => (text ? [{ op, text }] : [])),
    );
  let x = 0;
  let y = 0;
  for (const run of compared.runs) {
    if (run.kind === "same") {
      for (let i = 0; i < run.count; i++) whole(now.rows[y + i]!, "=");
      x += run.count;
      y += run.count;
      continue;
    }
    // ROWS EDITED IN PLACE pair by position, the one shape that says they are one row.
    const pairs = Math.min(run.removed, run.added);
    for (let i = 0; i < pairs; i++) {
      const was = old.rows[x + i]!;
      const is = now.rows[y + i]!;
      row(
        is,
        is.map((cell, c) => compareWords(was[c] ?? "", cell)),
      );
    }
    for (let i = pairs; i < run.removed; i++) whole(old.rows[x + i]!, "-");
    for (let i = pairs; i < run.added; i++) whole(now.rows[y + i]!, "+");
    x += run.removed;
    y += run.added;
  }
  const runs = [...head, ...cells];
  return { change: "changed", block: { ...now, rows }, runs, ...counted(runs) };
}

/** A code block edited, line by line; another language is another block. */
function editCode(
  old: Extract<MarkdownBlock, { kind: "code" }>,
  now: Extract<MarkdownBlock, { kind: "code" }>,
): ProseBlock | null {
  if (old.lang !== now.lang) return null;
  const a = old.text.split("\n");
  const b = now.text.split("\n");
  const compared = compareSequences(a, b);
  if (compared.kind !== "compared") return null;
  const code: CodeLine[] = [];
  let x = 0;
  let y = 0;
  for (const run of compared.runs) {
    if (run.kind === "same") {
      for (let i = 0; i < run.count; i++) code.push({ op: "=", text: b[y + i]! });
      x += run.count;
      y += run.count;
      continue;
    }
    for (let i = 0; i < run.removed; i++) code.push({ op: "-", text: a[x + i]! });
    for (let i = 0; i < run.added; i++) code.push({ op: "+", text: b[y + i]! });
    x += run.removed;
    y += run.added;
  }
  const kept = code.filter((line) => line.op === "=").length;
  if (kept < SAME_BLOCK * Math.max(a.length, b.length)) return null;
  const sum = (op: CodeLine["op"]) =>
    code.filter((line) => line.op === op).reduce((n, line) => n + countWords(line.text), 0);
  return { change: "changed", block: now, runs: [], code, added: sum("+"), removed: sum("-") };
}

function counted(runs: WordPart[][]): { added: number; removed: number } {
  let added = 0;
  let removed = 0;
  for (const part of runs.flat()) {
    if (part.op === "+") added += countWords(inlineText(part.text));
    else if (part.op === "-") removed += countWords(inlineText(part.text));
  }
  return { added, removed };
}

function wordList(block: MarkdownBlock): string[] {
  return tokens(inlineRuns(block).map(inlineText).join(" "))
    .filter((t) => t.word)
    .map((t) => t.text.toLowerCase());
}

/**
 * Whether two blocks' words make them one block edited: half their words
 * shared, or a block of a few words or more kept nearly whole inside the
 * other, which is a sentence extended or cut.
 */
export function sameBlock(a: readonly string[], b: readonly string[]): boolean {
  if (overlap(a, b) >= SAME_BLOCK) return true;
  const shorter = Math.min(a.length, b.length);
  return shorter >= KEPT_WHOLE_WORDS && common(a, b) >= KEPT_WHOLE * shorter;
}

/** A block of at least this many words, this much of it kept, was extended or cut rather than replaced. */
const KEPT_WHOLE_WORDS = 3;
const KEPT_WHOLE = 0.8;

/** The share of two word lists they hold in common, counted with repeats: 2|A∩B| / (|A|+|B|). */
export function overlap(a: readonly string[], b: readonly string[]): number {
  if (a.length + b.length === 0) return 1;
  return (2 * common(a, b)) / (a.length + b.length);
}

/** How many words two lists share, counted with repeats. */
function common(a: readonly string[], b: readonly string[]): number {
  const held = new Map<string, number>();
  for (const w of a) held.set(w, (held.get(w) ?? 0) + 1);
  let shared = 0;
  for (const w of b) {
    const left = held.get(w) ?? 0;
    if (left === 0) continue;
    held.set(w, left - 1);
    shared++;
  }
  return shared;
}

/**
 * A run of inline markdown as the pieces a word comparison moves whole: a
 * link, a code span, an image, an emphasis or an escape is one piece; in the
 * plain words between them, an address, a mention or a key is one piece, and
 * the rest is words, spaces and punctuation one by one.
 */
export function atomsOf(text: string): string[] {
  const out: string[] = [];
  const plain = (from: number, to: number) => {
    const stretch = text.slice(from, to);
    let at = 0;
    for (const token of inlineTokens(stretch)) {
      for (const t of tokens(stretch.slice(at, token.at))) out.push(t.text);
      out.push(stretch.slice(token.at, token.end));
      at = token.end;
    }
    for (const t of tokens(stretch.slice(at))) out.push(t.text);
  };
  let at = 0;
  for (const { index, end } of markdownInline(text)) {
    if (index > at) plain(at, index);
    out.push(text.slice(index, end));
    at = end;
  }
  if (at < text.length) plain(at, text.length);
  return out;
}

/**
 * Two runs of inline markdown compared by word: what they share, and what one
 * removed and the other added, in reading order, removed before added.
 */
export function compareWords(before: string, after: string): WordPart[] {
  if (before === after) return after ? [{ op: "=", text: after }] : [];
  const a = atomsOf(before);
  const b = atomsOf(after);
  const compared = compareSequences(a, b);
  if (compared.kind !== "compared") return replaced(before, after);
  type Piece = { op: WordPart["op"]; atoms: string[] };
  let pieces: Piece[] = [];
  let x = 0;
  let y = 0;
  for (const run of compared.runs) {
    if (run.kind === "same") {
      pieces.push({ op: "=", atoms: b.slice(y, y + run.count) });
      x += run.count;
      y += run.count;
      continue;
    }
    if (run.removed > 0) pieces.push({ op: "-", atoms: a.slice(x, x + run.removed) });
    if (run.added > 0) pieces.push({ op: "+", atoms: b.slice(y, y + run.added) });
    x += run.removed;
    y += run.added;
  }
  pieces = foldShortKept(pieces);
  const kept = pieces
    .filter((p) => p.op === "=")
    .reduce((n, p) => n + p.atoms.filter((t) => WORD.test(t)).length, 0);
  // A RUN MOSTLY REWRITTEN reads better as the old words and then the new.
  // Measured on the shorter side, so a sentence only extended or cut is marked in place.
  const shorter = Math.min(countWords(inlineText(before)), countWords(inlineText(after)));
  if (kept < SAME_BLOCK * shorter || (shorter === 0 && kept === 0)) return replaced(before, after);
  return pieces.map((p) => ({ op: p.op, text: p.atoms.join("") }));
}

function replaced(before: string, after: string): WordPart[] {
  const out: WordPart[] = [];
  if (before) out.push({ op: "-", text: before });
  if (after) out.push({ op: "+", text: after });
  return out;
}

/**
 * Folds a short unchanged stretch between two changes into both, and joins
 * what that leaves side by side, so "on Monday after the review" against "on
 * Tuesday before the review" is one change, not two around a lone "after".
 */
function foldShortKept<P extends { op: WordPart["op"]; atoms: string[] }>(pieces: P[]): P[] {
  let out = pieces;
  for (let changed = true; changed;) {
    changed = false;
    const next: P[] = [];
    for (let i = 0; i < out.length; i++) {
      const piece = out[i]!;
      const between = i > 0 && i < out.length - 1 && piece.op === "=";
      const short =
        between &&
        piece.atoms.filter((t) => WORD.test(t)).length <= SHORT_KEPT_WORDS &&
        out[i - 1]!.op !== "=" &&
        out[i + 1]!.op !== "=";
      if (short) {
        next.push({ ...piece, op: "-" }, { ...piece, op: "+" });
        changed = true;
      } else {
        next.push(piece);
      }
    }
    out = joinChanges(next);
  }
  return out;
}

/** Each stretch between kept pieces as one removed piece then one added piece. */
function joinChanges<P extends { op: WordPart["op"]; atoms: string[] }>(pieces: P[]): P[] {
  const out: P[] = [];
  const open: { removed: P | null; added: P | null } = { removed: null, added: null };
  const flush = () => {
    if (open.removed) out.push(open.removed);
    if (open.added) out.push(open.added);
    open.removed = null;
    open.added = null;
  };
  for (const piece of pieces) {
    if (piece.op === "=") {
      flush();
      out.push(piece);
      continue;
    }
    const side = piece.op === "-" ? "removed" : "added";
    const held = open[side];
    open[side] = held ? { ...held, atoms: [...held.atoms, ...piece.atoms] } : piece;
  }
  flush();
  return out;
}

/** One entry of a change's preview: a block, or a run of blocks left out and of what kind. */
export type PreviewEntry =
  | { kind: "block"; block: ProseBlock }
  | { kind: "skipped"; count: number; of: "same" | "added" | "removed" };

/**
 * A change cut to a preview: at most `max` changed blocks. A run of more than
 * `run` blocks added or removed together keeps its headings, each with the
 * blocks under it counted, so a reader sees which sections went; a run with
 * no heading shows its first `run - 1` and counts the rest. Unchanged blocks
 * are counted where they stand, except the heading a change sits under, which
 * says where on the page it is. Nothing after the last change shown. `more`
 * is how many changed blocks the preview left out.
 */
export function previewOf(
  blocks: readonly ProseBlock[],
  max: number,
  run = 3,
): { entries: PreviewEntry[]; more: number } {
  const entries: PreviewEntry[] = [];
  let shown = 0;
  let more = 0;
  let pending: ProseBlock[] = [];
  const flushSame = () => {
    let heading = -1;
    for (let i = pending.length - 1; i >= 0; i--) {
      if (pending[i]!.block.kind === "heading") {
        heading = i;
        break;
      }
    }
    const skip = (count: number) => {
      if (count > 0) entries.push({ kind: "skipped", count, of: "same" });
    };
    if (heading < 0) skip(pending.length);
    else {
      skip(heading);
      entries.push({ kind: "block", block: pending[heading]! });
      skip(pending.length - heading - 1);
    }
    pending = [];
  };
  for (let i = 0; i < blocks.length; i++) {
    const block = blocks[i]!;
    if (block.change === "same") {
      pending.push(block);
      continue;
    }
    if (shown >= max) {
      more++;
      continue;
    }
    flushSame();
    if (block.change === "added" || block.change === "removed") {
      let end = i;
      while (end + 1 < blocks.length && blocks[end + 1]!.change === block.change) end++;
      const length = end - i + 1;
      const stretch = blocks.slice(i, end + 1);
      i = end;
      if (length > run && stretch.some((b) => b.block.kind === "heading")) {
        // EACH SECTION BY ITS HEADING, the blocks under it counted.
        let under = 0;
        const fold = () => {
          if (under > 0) entries.push({ kind: "skipped", count: under, of: block.change });
          under = 0;
        };
        for (const b of stretch) {
          if (b.block.kind !== "heading") under++;
          else if (shown < max) {
            fold();
            entries.push({ kind: "block", block: b });
            shown++;
          } else more++;
        }
        fold();
        continue;
      }
      const take = Math.min(length <= run ? length : run - 1, max - shown);
      for (let k = 0; k < take; k++) entries.push({ kind: "block", block: stretch[k]! });
      shown += take;
      const left = length - take;
      if (left > 0 && length > run) {
        entries.push({ kind: "skipped", count: left, of: block.change });
      } else {
        more += left;
      }
      continue;
    }
    entries.push({ kind: "block", block });
    shown++;
  }
  return { entries, more };
}

/** How many places a change touched: its runs of changed blocks with unchanged ones between. */
export function placesOf(blocks: readonly ProseBlock[]): number {
  let places = 0;
  let inside = false;
  for (const block of blocks) {
    if (block.change === "same") inside = false;
    else if (!inside) {
      places++;
      inside = true;
    }
  }
  return places;
}

/** How many words a document reads as. */
export function documentWords(source: string): number {
  return blocksOf(source).reduce((n, block) => n + wordsOf(block), 0);
}

/** Why two documents were not compared, as the clause a sentence ends with. */
export function proseUncomparedReason(why: Uncompared): string {
  return why.kind === "too-large"
    ? `what changed spans ${why.lines.toLocaleString()} blocks, more than the ${MAX_COMPARE_LINES.toLocaleString()} a comparison reads`
    : `more than ${MAX_EDITS.toLocaleString()} blocks were removed or added`;
}
