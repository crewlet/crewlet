/**
 * Two texts compared line by line: the one line diff every diff and preview of
 * a text the dashboard draws is compared by.
 *
 * # The shortest edit
 *
 * Which lines changed is the fewest lines removed and added that turn the older
 * text into the newer: Myers' algorithm ("An O(ND) Difference Algorithm and Its
 * Variations", 1986), the one git's own diff runs. The answer is the design
 * system's shape — runs of changed lines with the unchanged lines kept either
 * side, every line numbered in both texts (`DiffSection` of `DiffLine`s) — so a
 * host draws it with `components/DiffView.tsx`, or cuts a preview of it with the
 * package's `firstChangedLines`, as it is. A run's removed lines come before its
 * added ones, the order a reader meets a line edited in.
 *
 * NOT THE DESIGN SYSTEM'S `diffLines`. That one fills the whole table of the two
 * texts' longest common subsequence, quadratic in memory on every input, and so
 * gives up at 2,000 lines a side — and gives up by answering every line of the
 * older text removed and every line of the newer added: a page edited on one
 * line drawn as a save that rewrote all of it (crewlet/crewlet#404). Nothing
 * here answers a comparison it did not make.
 *
 * # What a comparison costs follows what changed
 *
 * - THE SHARED START AND END are walked once, line against line, and take no
 *   further part: a long text edited in one place costs a walk of its lines and
 *   a comparison of the place, and of its unchanged lines only those kept as
 *   context around a change are built into the answer.
 * - LINES ARE NUMBERS. Each distinct line of what is left is interned once, so
 *   the algorithm compares integers, never strings.
 * - LINEAR SPACE. The middle snake (§4b of the paper) is where a shortest edit
 *   crosses the middle of its distance; the comparison cuts there and recurses
 *   on the two halves, holding two frontiers the length of the texts whatever
 *   their distance. The form that kept every round's frontier to walk back
 *   from the end held about 128 MB for two files 4,000 edits apart
 *   (crewlet/crewlet#392).
 *
 * # Bounded, and says so
 *
 * Past [MAX_COMPARE_LINES] lines a side in what changed, or more than
 * [MAX_EDITS] lines removed and added, two texts are NOT compared, and the
 * answer says why ([Uncompared]), in the words a host prints where the diff
 * would have been ([notComparedReason]). A floor on the distance read off how
 * often each line occurs ([distanceFloor]) answers a pair that cannot be within
 * the edit bound before a round of the algorithm runs.
 *
 * PURE, and in `lib/` for the reason the rest of this directory gives: what a
 * comparison says is held by its tests rather than by a screen.
 */

import type { DiffLine, DiffSection } from "@crewlethq/ui";

/**
 * The most lines a comparison reads on a side of what changed: the stretch
 * between the start and the end the two texts share.
 *
 * WITH [MAX_EDITS] IT BOUNDS THE WORK, which grows with the lines read times
 * the edits found, all of it on the thread that draws. A pair at both bounds —
 * 20,000 lines a side, every tenth one edited, so 4,000 lines removed and
 * added — took 95 ms (the median of five runs, the worst 121 ms) in Node 24 on
 * two efficiency cores of a Core Ultra 7 265U: about the 100 ms in which an
 * answer to a click still reads as immediate. One line edited in the same text
 * took 2.6 ms. So a comparison is made in the render that draws it rather than
 * handed to a Worker, which would make every diff a round trip and a loading
 * state to spare the rare one that costs more than a frame. And a text this
 * long in what changed is past any page of ordinary lines the engine keeps: a
 * page body is at most 512 KiB, so twenty thousand of its lines average 26
 * bytes.
 */
export const MAX_COMPARE_LINES = 20_000;

/**
 * The most lines removed and added a comparison finds before it calls two texts
 * too different to compare.
 *
 * THE OTHER HALF OF THE WORK'S BOUND (see [MAX_COMPARE_LINES]), and the point
 * where a diff stops being read as a change: four thousand marked lines is a
 * rewrite of what they stand in, whose two versions are the better read. A pair
 * past it is answered sooner than one at it, on the same machine: two
 * 20,000-line texts that share no line in 11.5 ms, read off how often each
 * line occurs ([distanceFloor]), and 20,000 lines of code against the same
 * lines shuffled, which only the rounds can tell from a close edit, in 50 ms.
 */
export const MAX_EDITS = 4_000;

/**
 * Unchanged lines kept either side of a change unless the host asks for others:
 * three, git's own default (`diff -U3`), enough to find the edit by what stands
 * round it.
 */
export const CONTEXT_LINES = 3;

/** Why two texts were not compared line by line. */
export type Uncompared =
  /** What changed spans more lines a side than a comparison reads: `lines` is the longer side's. */
  | { kind: "too-large"; lines: number }
  /** More than [MAX_EDITS] lines were removed and added between them. */
  | { kind: "too-different" };

/**
 * Two texts compared line by line — the runs that changed, each with its
 * context, in order, and none when the texts are the same lines — or why they
 * were not.
 */
export type LineComparison = { kind: "compared"; hunks: DiffSection[] } | Uncompared;

/**
 * Splits a text into lines: every line ending ends one, and a final newline
 * ends the last line rather than starting another.
 */
export function linesOf(text: string): string[] {
  if (text === "") return [];
  const out = text.split(/\r\n|\n|\r/);
  if (out[out.length - 1] === "") out.pop();
  return out;
}

/**
 * Compares `before` (the older text) with `after` line by line, keeping
 * `context` unchanged lines either side of each change.
 */
export function compareLines(
  before: string,
  after: string,
  context: number = CONTEXT_LINES,
): LineComparison {
  const a = linesOf(before);
  const b = linesOf(after);
  const shortest = Math.min(a.length, b.length);
  let start = 0;
  while (start < shortest && a[start] === b[start]) start++;
  let end = 0;
  while (end < shortest - start && a[a.length - 1 - end] === b[b.length - 1 - end]) end++;
  const n = a.length - start - end;
  const m = b.length - start - end;
  const longest = Math.max(n, m);
  if (longest > MAX_COMPARE_LINES) return { kind: "too-large", lines: longest };
  const middle = shortestEdit(a, b, start, end);
  if (middle === null) return { kind: "too-different" };
  const runs = new Runs();
  runs.same(start);
  runs.append(middle);
  runs.same(end);
  return { kind: "compared", hunks: sectionsOf(runs.all, a, b, context) };
}

/**
 * Why two texts were not compared, as the clause a host's sentence ends with:
 * "… not compared line by line: <this>."
 */
export function notComparedReason(why: Uncompared): string {
  return why.kind === "too-large"
    ? `what changed spans ${why.lines.toLocaleString()} lines, more than the ${MAX_COMPARE_LINES.toLocaleString()} a comparison reads`
    : `more than ${MAX_EDITS.toLocaleString()} lines were removed or added`;
}

/**
 * A floor under the number of lines removed and added that turns `a` into `b`.
 * An alignment keeps a line only where both texts hold it, so never more often
 * than the one that holds it fewer times: the lines the two have in common,
 * counted with their repeats, are at least as many as any alignment keeps, and
 * every other line is removed or added whatever the alignment.
 */
export function distanceFloor(a: ArrayLike<number>, b: ArrayLike<number>): number {
  const held = new Map<number, number>();
  for (let i = 0; i < a.length; i++) held.set(a[i]!, (held.get(a[i]!) ?? 0) + 1);
  let common = 0;
  for (let j = 0; j < b.length; j++) {
    const left = held.get(b[j]!) ?? 0;
    if (left === 0) continue;
    held.set(b[j]!, left - 1);
    common++;
  }
  return a.length + b.length - 2 * common;
}

/** One run of a comparison: lines both texts hold, or a change of lines removed and added. */
type Run = { kind: "same"; count: number } | { kind: "change"; removed: number; added: number };

/**
 * A comparison's runs, written front to back: a run of a kind follows one of
 * the other kind, so two changes with nothing kept between them are ONE change,
 * whichever order the algorithm found their lines in.
 */
class Runs {
  readonly all: Run[] = [];

  same(count: number): void {
    if (count === 0) return;
    const last = this.all[this.all.length - 1];
    if (last?.kind === "same") last.count += count;
    else this.all.push({ kind: "same", count });
  }

  change(removed: number, added: number): void {
    if (removed + added === 0) return;
    const last = this.all[this.all.length - 1];
    if (last?.kind === "change") {
      last.removed += removed;
      last.added += added;
    } else {
      this.all.push({ kind: "change", removed, added });
    }
  }

  append(other: Runs): void {
    for (const run of other.all) {
      if (run.kind === "same") this.same(run.count);
      else this.change(run.removed, run.added);
    }
  }
}

/**
 * The shortest edit turning `a` into `b` between their shared start and end,
 * or null when it removes and adds more than [MAX_EDITS] lines.
 */
function shortestEdit(a: string[], b: string[], start: number, end: number): Runs | null {
  const n = a.length - start - end;
  const m = b.length - start - end;
  const runs = new Runs();
  if (n === 0 || m === 0) {
    // ONE SIDE IS EMPTY: what is left of the other was all removed or all added.
    if (n + m > MAX_EDITS) return null;
    runs.change(n, m);
    return runs;
  }
  const ids = new Map<string, number>();
  const intern = (line: string) => {
    let id = ids.get(line);
    if (id === undefined) {
      id = ids.size;
      ids.set(line, id);
    }
    return id;
  };
  const x = new Int32Array(n);
  const y = new Int32Array(m);
  for (let i = 0; i < n; i++) x[i] = intern(a[start + i]!);
  for (let j = 0; j < m; j++) y[j] = intern(b[start + j]!);
  // A PAIR THAT CANNOT BE WITHIN THE BOUND is answered here, in one pass over
  // its lines, rather than after every round the bound allows.
  if (distanceFloor(x, y) > MAX_EDITS) return null;
  return edit(x, y, 0, n, 0, m, runs, MAX_EDITS) ? runs : null;
}

/** Where a shortest edit is cut in two, in the texts' own indexes. */
interface Cut {
  x: number;
  y: number;
}

/**
 * Writes the shortest edit turning `a[x0, x1)` into `b[y0, y1)` onto `runs`,
 * or writes nothing and answers false when it removes and adds more than
 * `budget` lines.
 *
 * The budget binds only the call that compares the whole: the two halves a cut
 * leaves are each a part of an edit already within it.
 */
function edit(
  a: Int32Array,
  b: Int32Array,
  x0: number,
  x1: number,
  y0: number,
  y1: number,
  runs: Runs,
  budget: number,
): boolean {
  let start = 0;
  while (x0 + start < x1 && y0 + start < y1 && a[x0 + start] === b[y0 + start]) start++;
  let end = 0;
  while (x1 - end > x0 + start && y1 - end > y0 + start && a[x1 - 1 - end] === b[y1 - 1 - end]) {
    end++;
  }
  const fromX = x0 + start;
  const toX = x1 - end;
  const fromY = y0 + start;
  const toY = y1 - end;
  const n = toX - fromX;
  const m = toY - fromY;
  const cut =
    n === 0 || m === 0
      ? n + m > budget
        ? null
        : "nothing in common"
      : middleSnake(a, b, fromX, toX, fromY, toY, budget);
  if (cut === null) return false;
  runs.same(start);
  if (cut === "nothing in common") {
    runs.change(n, m);
  } else {
    edit(a, b, fromX, cut.x, fromY, cut.y, runs, Infinity);
    edit(a, b, cut.x, toX, cut.y, toY, runs, Infinity);
  }
  runs.same(end);
  return true;
}

/**
 * Where a shortest edit turning `a[x0, x1)` into `b[y0, y1)` crosses the middle
 * of its distance: a point on it, found by running the furthest-reaching paths
 * from both corners at once until they overlap (Myers 1986, §4b). Both ranges
 * hold a line, and they share neither their first line nor their last.
 *
 * "nothing in common" when the two share no line, so every line of each is
 * removed or added; null when the edit removes and adds more than `budget`.
 *
 * WHY THE ROUNDS END WHERE THEY DO: a shortest edit of D lines is found in
 * round ⌈D/2⌉, on the forward pass when D is odd and the reverse pass when it
 * is even (the paper's Lemma 3), so a budget of B is decided by round ⌈B/2⌉,
 * and a pair that has shared nothing by round ⌈(n + m)/2⌉ shares no line.
 */
function middleSnake(
  a: Int32Array,
  b: Int32Array,
  x0: number,
  x1: number,
  y0: number,
  y1: number,
  budget: number,
): Cut | "nothing in common" | null {
  const n = x1 - x0;
  const m = y1 - y0;
  const most = Math.ceil((n + m) / 2);
  const rounds = Math.min(most, Math.ceil(budget / 2) + 1);
  // EACH FRONTIER holds, for every diagonal k = x − y it has reached, how far
  // along it its paths have come: the forward one from the start, the reverse
  // one from the end (in lines from the end). -1 is a diagonal not yet reached.
  const offset = rounds;
  const size = 2 * rounds + 2;
  const forward = new Int32Array(size).fill(-1);
  const reverse = new Int32Array(size).fill(-1);
  forward[offset + 1] = 0;
  reverse[offset + 1] = 0;
  const delta = n - m;
  // THE PASS THAT MEETS THE OTHER: an odd delta means an odd distance, which
  // the forward pass completes.
  const odd = (delta & 1) !== 0;
  // Diagonals that ran off the grid on either side are not walked again.
  let forwardFrom = 0;
  let forwardTo = 0;
  let reverseFrom = 0;
  let reverseTo = 0;
  const within = (distance: number, cut: Cut) => (distance > budget ? null : cut);
  for (let d = 0; d < rounds; d++) {
    for (let k = -d + forwardFrom; k <= d - forwardTo; k += 2) {
      const at = offset + k;
      let x =
        k === -d || (k !== d && forward[at - 1]! < forward[at + 1]!)
          ? forward[at + 1]!
          : forward[at - 1]! + 1;
      let y = x - k;
      while (x < n && y < m && a[x0 + x] === b[y0 + y]) {
        x++;
        y++;
      }
      forward[at] = x;
      if (x > n) forwardTo += 2;
      else if (y > m) forwardFrom += 2;
      else if (odd) {
        const mirror = offset + delta - k;
        if (mirror >= 0 && mirror < size && reverse[mirror] !== -1 && x >= n - reverse[mirror]!) {
          return within(2 * d - 1, { x: x0 + x, y: y0 + y });
        }
      }
    }
    for (let k = -d + reverseFrom; k <= d - reverseTo; k += 2) {
      const at = offset + k;
      let x =
        k === -d || (k !== d && reverse[at - 1]! < reverse[at + 1]!)
          ? reverse[at + 1]!
          : reverse[at - 1]! + 1;
      let y = x - k;
      while (x < n && y < m && a[x1 - 1 - x] === b[y1 - 1 - y]) {
        x++;
        y++;
      }
      reverse[at] = x;
      if (x > n) reverseTo += 2;
      else if (y > m) reverseFrom += 2;
      else if (!odd) {
        const mirror = offset + delta - k;
        if (mirror >= 0 && mirror < size && forward[mirror] !== -1) {
          const fx = forward[mirror]!;
          if (fx >= n - x) return within(2 * d, { x: x0 + fx, y: y0 + fx - (mirror - offset) });
        }
      }
    }
  }
  return rounds === most && n + m <= budget ? "nothing in common" : null;
}

/**
 * A comparison's runs as the sections a diff draws: each change with up to
 * `context` unchanged lines either side, two changes whose context meets drawn
 * as one section, and every section opening on how many unchanged lines were
 * left out before it — exactly what the design system's `collapseDiff` makes
 * of the same lines, built without the unchanged lines it leaves out, which in
 * a long text edited in one place are nearly all of it.
 */
function sectionsOf(
  runs: readonly Run[],
  a: string[],
  b: string[],
  context: number,
): DiffSection[] {
  const out: DiffSection[] = [];
  let x = 0;
  let y = 0;
  let open: DiffLine[] | null = null;
  let skipped = 0;
  const section = (): DiffLine[] => {
    if (open) return open;
    const lines: DiffLine[] = [];
    out.push({ lines, skipped });
    skipped = 0;
    open = lines;
    return lines;
  };
  const keep = (count: number) => {
    const lines = section();
    for (let i = 0; i < count; i++, x++, y++) {
      lines.push({ kind: "same", text: a[x]!, before: x + 1, after: y + 1 });
    }
  };
  runs.forEach((run, r) => {
    if (run.kind === "change") {
      const lines = section();
      for (let i = 0; i < run.removed; i++, x++) {
        lines.push({ kind: "remove", text: a[x]!, before: x + 1, after: null });
      }
      for (let i = 0; i < run.added; i++, y++) {
        lines.push({ kind: "add", text: b[y]!, before: null, after: y + 1 });
      }
      return;
    }
    // A RUN OF UNCHANGED LINES keeps the context after the change before it
    // and the context before the change after it; whatever lies between is
    // left out, and closes the section it would have continued. The runs
    // alternate, so a change stands either side of this run but at the ends.
    const after = r > 0 ? Math.min(context, run.count) : 0;
    const before = r < runs.length - 1 ? Math.min(context, run.count - after) : 0;
    if (after > 0) keep(after);
    const gap = run.count - after - before;
    if (gap > 0) {
      x += gap;
      y += gap;
      skipped += gap;
      open = null;
    }
    if (before > 0) keep(before);
  });
  return out;
}
