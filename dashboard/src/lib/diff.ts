/**
 * What changed between two versions of a document, by line.
 *
 * A page keeps every revision and the screen could show you any ONE of them.
 * "What did this save change" — the question somebody opens a history for —
 * was answerable only by opening two versions and reading both.
 *
 * OVER LINES, by the classic longest-common-subsequence table. The shape is
 * the one a prose document wants: a paragraph somebody rewrote is a line
 * replaced, not thirty character edits scattered through it. Word-level
 * refinement inside a changed line is deliberately absent — it makes a small
 * edit prettier and a large one unreadable, and these documents are rewritten
 * in paragraphs.
 *
 * THE TABLE RATHER THAN MYERS, and [MaxDiffLines] is what pays for that
 * choice: Myers is O(ND) and would be cheap on exactly the input a page
 * history has, where the table is quadratic on every input alike. What the
 * table buys is thirteen lines anybody can check by reading, against a page of
 * index arithmetic nobody re-derives; what it costs is a cap low enough that
 * the quadratic term stays small, and a whole-document replacement past it.
 *
 * PURE, and in `lib/` for the reason the rest of this directory gives: a
 * derivation that can only be exercised by rendering a component is one nobody
 * re-measures.
 */

/** One line of the result. `same` lines carry both numbers. */
export interface DiffLine {
  kind: "same" | "add" | "remove";
  text: string;
  /** 1-based line number in the OLD document, or null on an addition. */
  before: number | null;
  /** 1-based line number in the NEW document, or null on a removal. */
  after: number | null;
}

/** What a diff amounts to, for a one-line summary above it. */
export interface DiffStat {
  added: number;
  removed: number;
  /** True when the two documents are identical, which is a real outcome. */
  identical: boolean;
}

/**
 * The size past which the table is not worth building.
 *
 * SIZED FROM THE TABLE'S MEMORY, because that is the quantity that binds and
 * it is paid on EVERY input: [common] allocates `(N+1)` rows of `(M+1)`
 * `Uint32` entries and fills every cell with no early exit, so two revisions
 * differing in one line — the commonest thing a page history holds — cost
 * exactly as much as two documents sharing nothing. There is no cheap common
 * case to lean on.
 *
 * At two thousand lines that table is 4·(N+1)² ≈ 16 MB and about 60 ms, which
 * a tab holds without noticing. Measured on the same machine, the ten thousand
 * this used to be is 382 MB and 1.7 s — on the main thread, inside the
 * `useMemo` the history pane calls this from, for two revisions one line
 * apart. That is the frozen tab the cap exists to prevent, let through by a
 * number picked against an algorithm this file does not contain.
 *
 * Two thousand lines is still far past any page a person writes, and well
 * inside `pages.MaxBody` (512 KiB), so nothing legitimate is newly pushed onto
 * the whole-document-replacement path. A page longer than that gets that path
 * — which is honest, and the alternative is not a better diff but an
 * unresponsive browser. Raising it means replacing the table with Myers or the
 * Hirschberg refinement first, not raising it.
 */
export const MaxDiffLines = 2_000;

/**
 * Split a document into the lines a diff compares.
 *
 * A trailing newline is not a line. Every file that ends properly has one, so
 * counting it would report an added last line on every document that gained
 * one and on no document that gained a sentence.
 */
export function lines(text: string): string[] {
  const normalised = (text ?? "").replace(/\r\n?/g, "\n");
  if (normalised === "") return [];
  return normalised.replace(/\n$/, "").split("\n");
}

/**
 * The line diff of two documents.
 *
 * Returns every line of the result in order — context included — because a
 * page diff is read as a document rather than as a patch: a reader wants to
 * see the paragraph a sentence was added to.
 */
export function diffLines(before: string, after: string): DiffLine[] {
  const a = lines(before);
  const b = lines(after);
  if (a.length > MaxDiffLines || b.length > MaxDiffLines) {
    // TOO BIG TO COMPARE HONESTLY. Reported as a whole-document replacement,
    // which is true — and truthful in the direction that matters, since the
    // alternative is a tab that stops responding.
    return [
      ...a.map((text, i) => ({ kind: "remove" as const, text, before: i + 1, after: null })),
      ...b.map((text, i) => ({ kind: "add" as const, text, before: null, after: i + 1 })),
    ];
  }
  return walk(a, b, common(a, b));
}

/** Count the additions and removals, and say when there are none. */
export function diffStat(diff: DiffLine[]): DiffStat {
  let added = 0;
  let removed = 0;
  for (const line of diff) {
    if (line.kind === "add") added++;
    if (line.kind === "remove") removed++;
  }
  // IDENTICAL IS ITS OWN ANSWER, not "0 added, 0 removed". A save that
  // changed only a page's title or its labels leaves the body untouched, and
  // a diff pane showing the whole document with no marks reads as one that
  // failed to load.
  return { added, removed, identical: added === 0 && removed === 0 };
}

/**
 * The length of the longest common subsequence, as the classic table.
 *
 * QUADRATIC IN BOTH TIME AND MEMORY, unconditionally — there is no early exit
 * and no shortcut for two documents that are nearly the same. The cap above is
 * what bounds it, and is set from this function's allocation rather than from
 * any notion of how much the two documents have in common; see [MaxDiffLines].
 *
 * The table rather than Hirschberg's linear-space refinement: at the cap the
 * 16 MB is a size a browser holds without noticing, and the refinement costs a
 * page of code nobody can check by reading. That trade is a function of the
 * cap — move one and the other has to move with it.
 */
function common(a: string[], b: string[]): Uint32Array[] {
  const table: Uint32Array[] = [];
  for (let i = 0; i <= a.length; i++) table.push(new Uint32Array(b.length + 1));
  for (let i = a.length - 1; i >= 0; i--) {
    const row = table[i] as Uint32Array;
    const next = table[i + 1] as Uint32Array;
    for (let j = b.length - 1; j >= 0; j--) {
      row[j] =
        a[i] === b[j]
          ? (next[j + 1] as number) + 1
          : Math.max(next[j] as number, row[j + 1] as number);
    }
  }
  return table;
}

/**
 * Walk the table into a result.
 *
 * A REMOVAL BEFORE AN ADDITION at every equal branch, so a replaced line reads
 * as the old one struck out above the new one. The other order puts the new
 * text first and reads as an insertion followed by an unrelated deletion.
 */
function walk(a: string[], b: string[], table: Uint32Array[]): DiffLine[] {
  const out: DiffLine[] = [];
  let i = 0;
  let j = 0;
  while (i < a.length && j < b.length) {
    if (a[i] === b[j]) {
      out.push({ kind: "same", text: a[i] as string, before: i + 1, after: j + 1 });
      i++;
      j++;
      continue;
    }
    const down = (table[i + 1] as Uint32Array)[j] as number;
    const right = (table[i] as Uint32Array)[j + 1] as number;
    if (down >= right) {
      out.push({ kind: "remove", text: a[i] as string, before: i + 1, after: null });
      i++;
    } else {
      out.push({ kind: "add", text: b[j] as string, before: null, after: j + 1 });
      j++;
    }
  }
  while (i < a.length) {
    out.push({ kind: "remove", text: a[i] as string, before: i + 1, after: null });
    i++;
  }
  while (j < b.length) {
    out.push({ kind: "add", text: b[j] as string, before: null, after: j + 1 });
    j++;
  }
  return out;
}

/**
 * Drop the runs of unchanged lines a reader does not need, keeping `context`
 * either side of every change.
 *
 * A HOLE IS REPORTED, never silently closed: the gap carries how many lines it
 * stands for, so a reader can tell a document with two edits at its ends from
 * one that was rewritten in the middle. Returned as a list of sections rather
 * than as a marker line, so the renderer decides what a gap looks like.
 */
export interface DiffSection {
  /** The lines of this section, in order. */
  lines: DiffLine[];
  /** How many unchanged lines were dropped BEFORE this section. */
  skipped: number;
}

export function collapse(diff: DiffLine[], context = 3): DiffSection[] {
  const keep = new Array<boolean>(diff.length).fill(false);
  diff.forEach((line, i) => {
    if (line.kind === "same") return;
    for (let k = Math.max(0, i - context); k <= Math.min(diff.length - 1, i + context); k++) {
      keep[k] = true;
    }
  });

  const out: DiffSection[] = [];
  let skipped = 0;
  let current: DiffLine[] | null = null;
  diff.forEach((line, i) => {
    if (keep[i]) {
      if (!current) {
        current = [];
        out.push({ lines: current, skipped });
        skipped = 0;
      }
      current.push(line);
      return;
    }
    skipped++;
    current = null;
  });
  return out;
}
