/**
 * Every diff the dashboard draws: one component, and no hue.
 *
 * # Why it is local
 *
 * The design system has a `DiffView`, a `DiffSummary` and a `DiffList`, and
 * all three paint an added line in the success state's tint and a removed one
 * in the danger state's. Here colour carries STATE ("The one rule" in
 * docs/reference/dashboard-design.md), and a diff is not one: a removed line is
 * not a failure and an added one is not done, and on a page that also draws a
 * failed check, a red removed line beside a red failed-check chip makes one red
 * mean two things. So every diff is drawn by this one component, from the
 * package's tokens, and none of the three is imported anywhere
 * (`styles/diffs.test.ts`).
 *
 * # Two texts are compared by the dashboard's one line diff
 *
 * Handed two texts rather than hunks, the diff compares them with
 * `lib/lineDiff.ts` — the shortest edit, at a cost that follows what changed —
 * never with the package's `diffLines`, which past 2,000 lines a side compares
 * nothing and answers every line of one text removed and every line of the
 * other added. Where the comparison is not made (past what it reads, or past
 * the lines it marks), a sentence says so and why, in place of every row: a
 * comparison not made is never drawn as though it had been.
 *
 * # What tells a line's kind, with no hue
 *
 * Three things, each enough alone:
 *
 * - THE GLYPH GUTTER: `+` for an added line and `−` for a removed one, with
 *   the word ("Added", "Removed") read in its place, because a minus sign is
 *   announced as anything from "minus" to nothing at all.
 * - TWO SURFACE STEPS of the package's ramp, taken from the diff's own ground,
 *   the card rung: an added line is RAISED one rung (`--color-surface-
 *   elevated`, which the palette holds three ΔE off the card) and a removed
 *   one is SUNK one rung (`--color-surface-background`, the sheet, held 1.5 ΔE
 *   off it). The diff paints that ground itself, because a rung is right only
 *   against the ground it was picked for: drawn on whatever the host stood it
 *   on, a raised line inside a raised panel would be the panel's own colour.
 * - THE WORD UNDERLINE: where a removed line and an added line are one line
 *   edited, the run of each that differs is underlined (`lib/wordDiff.ts`).
 *   Which two lines those are is read off the change's shape, a setting's
 *   path or a text's place ([pairsOf]), and where the shape cannot say, no
 *   line is underlined: an underline is a claim that two lines are one.
 *
 * # The shapes it takes
 *
 * A diff of a TEXT is numbered: each line carries its number in the older text
 * and in the newer, in Geist Mono at the document's 13px. A diff of SETTINGS
 * — a configuration's paths, the files of a release — is keyed instead: each
 * line names its `path`, and a setting that changed is its old value removed
 * and its new value added, so the underline finds what moved inside it. A
 * hunk opens with a row saying how many unchanged lines it skipped, because a
 * gap silently closed makes a document edited at both ends look like one
 * rewritten in the middle.
 *
 * UNIFIED OR SPLIT. Split draws the older text and the newer side by side,
 * each removed line of a change beside the added line in its place in the
 * change — a layout, which says nothing of whether the two are one line
 * edited: that is the underline's to say. It is drawn only where the diff is
 * at least the phone breakpoint wide (640px, the kit's `breakpoint.phone`):
 * two columns of code under that are a word or two each.
 * A keyed diff is always unified: a setting's two values are one path read
 * down, and there is no text whose lines a second column would line up.
 *
 * # Windowed past a thousand rows
 *
 * A diff of a regenerated file or a rewritten document runs to thousands of
 * rows, and every mounted row is elements the tab lays out and keeps. Past
 * [WINDOW_FROM_ROWS] rows the diff is cut into blocks of [WINDOW_BLOCK_ROWS],
 * and only the blocks within one view of what is on screen hold their rows;
 * the rest are empty boxes of the height they last measured (or an estimate
 * until they have been drawn once), so the scroll bar stays honest. Below it
 * every row is mounted, which keeps find-in-page and a screen reader's reading
 * of the whole diff.
 *
 * THE WINDOW BOUNDS THE READING TOO, not only the mounting: which line an
 * edited one is paired with is settled from the diff's shape alone, and the
 * words of a pair are read — both lines tokenized, for the underline — only
 * once one of its lines is drawn ([Pair]). Once its hunks exist, a diff of
 * any length costs, before its first paint, a walk over its lines and the
 * underlines of the rows it draws, never the tokens of every line it holds.
 *
 * # A host can write on it
 *
 * A change's review is read IN its diff: a finding or a thread is drawn under
 * the line it is on ([DiffViewProps.notes]), and a reviewer picks the lines a
 * new finding is about by pressing their numbers ([DiffViewProps.onPick]) —
 * the line's own number, on the side it is on (its newer one, else its
 * older), which is the number a finding names. What is drawn under a line and
 * what a pick does are the host's: the diff draws neither a finding nor a
 * composer of its own, and a diff given neither draws exactly what it drew
 * before. Under a split diff a line's notes are drawn under its row, across
 * both sides, since a note is about a line and not about a column.
 */

import {
  Fragment,
  useEffect,
  useLayoutEffect,
  useMemo,
  useReducer,
  useRef,
  useState,
  type CSSProperties,
  type ReactNode,
  type RefObject,
} from "react";
import { VisuallyHidden, cx } from "@crewlethq/ui";
import { PHONE_BREAKPOINT } from "~/app/layout.ts";
import { plural } from "~/lib/format.ts";
import { compareLines, notComparedReason } from "~/lib/lineDiff.ts";
import { scrollerOf } from "~/lib/scroller.ts";
import { useNarrowerThan } from "~/lib/useNarrowerThan.ts";
import { changedRuns, type ChangedRuns, type Run } from "~/lib/wordDiff.ts";
// THE DIFF'S OWN SHEET: every screen that draws a diff is a lazy chunk, so the
// rules arrive with the first of them rather than in the first paint.
import "~/styles/diff.css";

/** What happened to one line. */
export type DiffLineKind = "same" | "add" | "remove";

/**
 * One line of a diff: the design system's `DiffLine`, the shape the
 * dashboard's line diff answers in, with the `path` a setting is located by.
 */
export interface DiffLine {
  kind: DiffLineKind;
  text: string;
  /** 1-based line in the OLDER text, or null where the line is not in it. */
  before: number | null;
  /** 1-based line in the NEWER text, or null where the line is not in it. */
  after: number | null;
  /**
   * Where the line is when it is a setting rather than a line of a text: a
   * configuration's path, a file's name. A diff whose lines carry one is
   * KEYED: the path is drawn in place of the line numbers, which a setting
   * has none of.
   */
  path?: string | undefined;
}

/**
 * A run of lines, and how many unchanged lines were left out before it: the
 * design system's `DiffSection`, the shape `lib/lineDiff.ts` answers in.
 */
export interface DiffHunk {
  lines: readonly DiffLine[];
  skipped: number;
}

/** How the two sides are laid out. */
export type DiffLayout = "unified" | "split";

/**
 * The diff itself: hunks a host computed, or two texts compared here by line.
 *
 * HUNKS ARE BUILT ONCE PER ANSWER (a `useMemo` beside the read): a diff handed
 * new hunks starts its window and its measured heights over, so hunks rebuilt
 * on every render would remount a long diff's window each time its host drew.
 */
type DiffSource =
  | { hunks: readonly DiffHunk[]; before?: undefined; after?: undefined; context?: undefined }
  | {
      before: string;
      after: string;
      /** Unchanged lines kept either side of a change: the line diff's own three when unset. */
      context?: number | undefined;
      hunks?: undefined;
    };

export type DiffViewProps = DiffSource & {
  /** Unified by default; split only where there is room for two columns (see the package note). */
  layout?: DiffLayout | undefined;
  /**
   * What the diff is measured against, named the way the reader knows it
   * ("rev 12"). Given, a line above the diff counts what was added and
   * removed: `+3 −1 against rev 12`.
   */
  against?: string | undefined;
  /**
   * What is drawn when nothing changed, in place of the diff: two identical
   * documents are a fact rather than a gap, and the host knows what was
   * compared to say so.
   */
  empty?: ReactNode;
  /** The diff's accessible name. */
  "aria-label"?: string | undefined;
  /**
   * What is drawn under a line, or nothing — the findings and threads a host
   * anchors to it. Called as rows are drawn, so a windowed diff asks only for
   * the lines it holds.
   */
  notes?: ((line: DiffLine) => ReactNode) | undefined;
  /**
   * Pick a line: with it, each line's own number is a button, and pressing it
   * picks the line — with Shift held, `extend` is true, extending the host's
   * pick from where it started.
   */
  onPick?: ((line: DiffLine, extend: boolean) => void) | undefined;
  /** Whether the host has picked a line, which is drawn marked. */
  picked?: ((line: DiffLine) => boolean) | undefined;
};

/**
 * How many rows a diff draws before it is windowed. PROVISIONAL: the value
 * was set without its cost measured; the measurement that decides it, and who
 * decides it while nothing here can take it, are named below.
 *
 * A row is about 21px (13px mono on the body's 1.45 leading, and a quarter
 * step of padding above and below), so a 900px view shows some 43 rows and
 * the window — the view and one view either side of it — about 130: two
 * blocks, three at most. A thousand rows is some seven windows' worth.
 *
 * WHAT IT MOUNTS AT THE THRESHOLD, counted from what a row draws rather than
 * timed. A unified row is one line: the line, its two numbers (a keyed line's
 * path in their place), its mark and the glyph in it, the word read in the
 * glyph's place, its text, and an underline where it was edited — five to
 * eight elements, about 7. A split row is a PAIR of lines, since the
 * threshold counts rows as they are drawn, so a split diff holds up to twice
 * the lines under it: the pair, and five to seven elements on each side that
 * holds a line — up to fifteen, about 13. Every line and every pair is a grid
 * of its own. So a diff at the threshold mounts about 7,000 elements unified
 * and about 13,000 split, every one of them laid out before the first paint.
 *
 * WHAT IT WEIGHS: a mounted diff keeps find-in-page and a screen reader's
 * reading of the whole of it, which windowing takes away; windowing keeps the
 * mount and the layout to the rows near the view, whatever the diff's length.
 * WHAT DECIDES IT: the time from mounting a 1,000-row split diff to its first
 * paint, the worst the threshold allows, measured on CI's class of runner.
 * The value stands while that is within 100 ms, RAIL's budget for a response
 * the reader sees (a diff is drawn in answer to a click); past it, the
 * threshold comes down to the rows that fit in it.
 *
 * WHO DECIDES IT: nothing in this tree can time a first paint — jsdom lays
 * nothing out, and the suites run no browser — so the value is the
 * maintainers' to settle (crewlet/crewlet#422), from three answers: (a) keep
 * it, and add a browser to the suites that times the case above, a new dev
 * dependency (the recommendation, so the change page's split diffs are held
 * to it); (b) keep it as a decision, unmeasured; (c) 300 rows, the window's
 * own three blocks, so that no diff mounts more than a windowed one does — a
 * bound that needs no timing, and costs find-in-page and a screen reader every
 * diff past it.
 */
export const WINDOW_FROM_ROWS = 1000;

/**
 * Rows a window block holds: about two 900px views of them, so the window is
 * a handful of blocks and a scroll from one end to the other is a few dozen
 * mounts rather than one per row.
 */
export const WINDOW_BLOCK_ROWS = 100;

/**
 * The height a block is given per row, in px, while no drawn block of the
 * diff has been measured: one unwrapped row, 13px on the 1.45 leading
 * (18.85px) and a quarter step of padding above and below it.
 *
 * A LITERAL, NOT THE TOKENS IT IS WORKED FROM: the token module sits in the
 * first paint's chunk, and reading `font` from it here pulled the whole font
 * table into that chunk (0.8 KiB) for a figure that is replaced the moment the
 * first block is drawn and measured, before anything is painted ([useWindow]).
 * It only stands where nothing measurable is drawn, such as a diff inside a
 * hidden panel, and there it keeps the empty boxes the height of real rows
 * rather than none, which would bring every block within the window at once.
 */
const ROW_ESTIMATE_PX = 21;

/** One row of the diff, as it is drawn: what is underlined in it is its lines' [Pair]s'. */
type Row =
  | { kind: "skip"; count: number }
  | { kind: "line"; line: DiffLine }
  | { kind: "pair"; left: DiffLine | null; right: DiffLine | null };

/**
 * A removed line and the added line it is one line edited into: the two
 * halves of one underline.
 *
 * READ WHEN FIRST DRAWN, never when the diff is: tokenizing both lines is all
 * the underline costs, and it is a cost in proportion to the text — every
 * token an object, over a pattern that classifies each character — so it is
 * paid for the pairs the window holds rather than for every pair of a diff
 * that may run to thousands. Once paid it is kept, so the other half of the
 * pair, drawn beside it in a split diff or rows later in a unified one, and a
 * block the window lets go and takes in again, read nothing a second time.
 */
class Pair {
  private runs: ChangedRuns | undefined;

  constructor(
    private readonly removed: string,
    private readonly added: string,
  ) {}

  /** What the half of the pair a line of `kind` is underlines. */
  runOf(kind: "remove" | "add"): Run | null {
    this.runs ??= changedRuns(this.removed, this.added);
    return kind === "remove" ? this.runs.before : this.runs.after;
  }
}

/** Every line of a diff that is one line edited, to the [Pair] it is half of. */
type Pairs = ReadonlyMap<DiffLine, Pair>;

/**
 * Which removed line each added one replaced, in every CHANGE of a diff — a
 * run of lines none of which is unchanged — decided from the lines' kinds and
 * paths alone: not a word of them is read here ([Pair]).
 *
 * In a text, the kth removed with the kth added, the pairing a reader makes
 * down the two columns of a split diff, and only in a change whose shape lets
 * position still mean one line edited ([alignedByPosition]). In a keyed diff,
 * the removed and the added line of ONE path, since a change can hold one
 * setting removed and another one edited, and pairing those by position would
 * underline one setting against another; one to one and in order, so a path's
 * lines meet once each, and a line with no path names no setting and pairs
 * with none. A path is the setting's identity rather than a place, so it
 * pairs at any size.
 *
 * Linear in the diff's lines, keyed or not: a path finds its partner through
 * a map rather than a search of the change, which a change holding a release's
 * every file would make a walk over the files for each of them.
 */
function pairsOf(hunks: readonly DiffHunk[], keyed: boolean): Pairs {
  const pairs = new Map<DiffLine, Pair>();
  const join = (removed: DiffLine, added: DiffLine) => {
    const pair = new Pair(removed.text, added.text);
    pairs.set(removed, pair);
    pairs.set(added, pair);
  };
  for (const hunk of hunks) {
    const lines = hunk.lines;
    for (let i = 0; i < lines.length;) {
      if (lines[i]!.kind === "same") {
        i++;
        continue;
      }
      const removed: DiffLine[] = [];
      const added: DiffLine[] = [];
      for (; i < lines.length && lines[i]!.kind !== "same"; i++) {
        const line = lines[i]!;
        (line.kind === "remove" ? removed : added).push(line);
      }
      if (keyed) {
        pairByPath(removed, added, join);
        continue;
      }
      if (!alignedByPosition(removed.length, added.length)) continue;
      for (let k = 0; k < Math.min(removed.length, added.length); k++) {
        join(removed[k]!, added[k]!);
      }
    }
  }
  return pairs;
}

/**
 * Whether a change of a text, of `removed` lines and `added` lines, is one in
 * which the kth removed line and the kth added line can still be one line
 * edited — position being all a text's lines have to be paired by.
 *
 * - AS MANY ADDED AS REMOVED: each removed line has a line in its own place,
 *   the one shape git's own `contrib/diff-highlight` pairs ("It will only
 *   highlight hunks in which the number of removed and added lines is the
 *   same", its README). A line taken out at one end and another put in at
 *   the other shift every pair between them, but a diff that aligned the
 *   lines between draws those as two changes rather than one, unless every
 *   line between changed too: the case git's README calls pathological, and
 *   the one this shape still misreads.
 * - ONE LINE AGAINST SEVERAL: a line rewritten with lines written after it
 *   (a paragraph edited and one added under it) or deleted after it, the
 *   commonest uneven edit there is. It is one pair, the first line of each
 *   side, so what the edit added or removed misplaces that pair at most —
 *   lines written ABOVE the edited one do — and a misplaced pair is
 *   underlined only where its two lines share a word at an end
 *   (`lib/wordDiff.ts`).
 *
 * NOWHERE ELSE. With two lines or more on both sides and the counts unequal,
 * the lines one side has beyond the other sit at places position cannot see,
 * and every pair after the first of them is two neighbours, underlined as one
 * line edited wherever neighbours share a word at an end: a YAML key, a
 * table's cells, a `return`. A change's LENGTH decides nothing: a long change
 * of a real diff is as many lines replaced as any short one, and its lines are
 * paired, or not, by the same two shapes.
 */
function alignedByPosition(removed: number, added: number): boolean {
  return removed === added || Math.min(removed, added) === 1;
}

/** One change of a keyed diff, paired by path (see [pairsOf]). */
function pairByPath(
  removed: readonly DiffLine[],
  added: readonly DiffLine[],
  join: (removed: DiffLine, added: DiffLine) => void,
) {
  // Each path's added lines, the last first, so the first is the one popped.
  const waiting = new Map<string, DiffLine[]>();
  for (let k = added.length - 1; k >= 0; k--) {
    const line = added[k]!;
    if (line.path === undefined) continue;
    const queue = waiting.get(line.path);
    if (queue) queue.push(line);
    else waiting.set(line.path, [line]);
  }
  for (const line of removed) {
    const partner = line.path === undefined ? undefined : waiting.get(line.path)?.pop();
    if (partner) join(line, partner);
  }
}

/**
 * The rows a diff draws, in the layout it is drawn in. Split, a change's
 * removed lines run down the left and its added lines down the right, side
 * by side from its first line, which is where the layout pairs them; what is
 * underlined is the [Pair]s' business, and the same in either layout.
 */
function rowsOf(hunks: readonly DiffHunk[], split: boolean): Row[] {
  const rows: Row[] = [];
  for (const hunk of hunks) {
    if (hunk.skipped > 0) rows.push({ kind: "skip", count: hunk.skipped });
    const lines = hunk.lines;
    if (!split) {
      for (const line of lines) rows.push({ kind: "line", line });
      continue;
    }
    for (let i = 0; i < lines.length;) {
      const line = lines[i]!;
      if (line.kind === "same") {
        rows.push({ kind: "pair", left: line, right: line });
        i++;
        continue;
      }
      const removed: DiffLine[] = [];
      const added: DiffLine[] = [];
      for (; i < lines.length && lines[i]!.kind !== "same"; i++) {
        const changed = lines[i]!;
        (changed.kind === "remove" ? removed : added).push(changed);
      }
      for (let k = 0; k < Math.max(removed.length, added.length); k++) {
        rows.push({ kind: "pair", left: removed[k] ?? null, right: added[k] ?? null });
      }
    }
  }
  return rows;
}

/** What a line underlines as it is drawn: its half of its [Pair]'s run, or nothing. */
function underlineOf(pairs: Pairs, line: DiffLine): Run | null {
  return line.kind === "same" ? null : (pairs.get(line)?.runOf(line.kind) ?? null);
}

/** What a diff holds, which decides its tracks and its head. */
function shapeOf(hunks: readonly DiffHunk[]) {
  let keyed = false;
  let numbered = false;
  let widest = 1;
  let path = 0;
  let added = 0;
  let removed = 0;
  for (const hunk of hunks) {
    for (const line of hunk.lines) {
      if (line.path !== undefined) {
        keyed = true;
        path = Math.max(path, line.path.length);
      }
      for (const n of [line.before, line.after]) {
        if (n === null) continue;
        numbered = true;
        widest = Math.max(widest, n);
      }
      if (line.kind === "add") added++;
      if (line.kind === "remove") removed++;
    }
  }
  return {
    keyed,
    // A setting has no line number, so a keyed diff draws none (see [DiffLine]).
    numbered: numbered && !keyed,
    digits: String(widest).length,
    path,
    added,
    removed,
  };
}

/** The blocks a window mounts and unmounts, by their first row. */
function blocksOf(rows: readonly Row[]): Row[][] {
  const out: Row[][] = [];
  for (let at = 0; at < rows.length; at += WINDOW_BLOCK_ROWS) {
    out.push(rows.slice(at, at + WINDOW_BLOCK_ROWS));
  }
  return out;
}

/** The hunks of two texts that were not compared: none, and the sentence in their place. */
const NO_HUNKS: readonly DiffHunk[] = [];

/** The blocks a windowed diff holds before the observer has said where it is: the first. */
const FIRST: ReadonlySet<number> = new Set([0]);

/**
 * Which blocks of a windowed diff hold their rows, and the height each of the
 * others keeps while it holds none.
 *
 * THE OBSERVER'S ROOT IS THE DIFF'S OWN SCROLLER (`scrollerOf`), never the
 * viewport: a margin on the viewport does not reach past the scroller that
 * clips the diff, so a block just under the fold would only be mounted once
 * it was already on screen. The margin is one of that scroller's views above
 * and below.
 *
 * A DRAWN BLOCK IS MEASURED, twice. Once as it is drawn, before anything is
 * painted, so a block never drawn is estimated from the rows that were — the
 * average measured row times its rows — from the very first frame. And again
 * as it LEAVES, from the observer's own entry, which carries the box it had
 * while it still held its rows, so the box it then keeps is that height and
 * nothing above the reader's place moves when it empties. Only a block the
 * last COMMITTED render drew is measured: one the window took in and let go
 * before it was ever drawn is still its empty box, whose height is an
 * estimate rather than a measurement. A box with no height is no measurement
 * either: a diff in a hidden panel lays nothing out, and taking its zeros
 * would bring every block within the window at once when it is shown. With
 * nothing measured, a row is [ROW_ESTIMATE_PX].
 *
 * NO OBSERVER, NO WINDOW: a browser without one mounts every block, which is
 * slower and never wrong.
 */
function useWindow(
  body: RefObject<HTMLDivElement | null>,
  blocks: readonly Row[][],
  windowed: boolean,
) {
  const [near, setNear] = useState<ReadonlySet<number>>(FIRST);
  // A NEW DIFF'S BLOCKS ARE NOT THE OLD ONE'S, so their heights start over.
  const heights = useMemo(() => new Map<number, number>(), [blocks]);
  const [, measuredNew] = useReducer((n: number) => n + 1, 0);
  const observing = windowed && typeof IntersectionObserver !== "undefined";
  // The blocks on screen holding their rows, as of the last commit.
  const drawn = useRef<ReadonlySet<number>>(FIRST);
  useLayoutEffect(() => {
    drawn.current = near;
    const el = body.current;
    if (!observing || !el) return;
    let changed = false;
    for (const block of el.querySelectorAll<HTMLElement>("[data-block]")) {
      const at = Number(block.dataset.block);
      const height = block.getBoundingClientRect().height;
      if (!near.has(at) || height <= 0 || heights.get(at) === height) continue;
      heights.set(at, height);
      changed = true;
    }
    // A FIRST MEASUREMENT RESIZES THE EMPTY BOXES, before this frame paints.
    if (changed) measuredNew();
  }, [body, heights, near, observing]);

  useEffect(() => {
    const el = body.current;
    if (!observing || !el) return;
    const watch = new IntersectionObserver(
      (entries) => {
        const said = entries.map((entry) => ({
          at: Number((entry.target as HTMLElement).dataset.block),
          near: entry.isIntersecting,
          height: entry.boundingClientRect.height,
        }));
        for (const { at, near, height } of said) {
          if (!near && height > 0 && drawn.current.has(at)) heights.set(at, height);
        }
        // IN THE ORDER SAID: one batch can carry a block coming within a
        // view and leaving it again, and the later entry is where it is.
        setNear((was) => {
          let now: Set<number> | null = null;
          for (const { at, near } of said) {
            if (near === (now ?? was).has(at)) continue;
            now ??= new Set(was);
            if (near) now.add(at);
            else now.delete(at);
          }
          return now ?? was;
        });
      },
      { root: scrollerOf(el), rootMargin: "100% 0px" },
    );
    for (const block of el.querySelectorAll<HTMLElement>("[data-block]")) watch.observe(block);
    return () => watch.disconnect();
  }, [body, heights, observing]);

  let rows = 0;
  let height = 0;
  for (const [at, h] of heights) {
    rows += blocks[at]?.length ?? 0;
    height += h;
  }
  const perRow = rows > 0 ? height / rows : ROW_ESTIMATE_PX;
  return {
    holds: (at: number) => !observing || near.has(at),
    height: (at: number) => heights.get(at) ?? (blocks[at]?.length ?? 0) * perRow,
  };
}

/** A line's text, its changed run underlined. An empty line keeps its height. */
function Text({ text, run }: { text: string; run: Run | null }) {
  return (
    <span className="diff-view__text">
      {run ? (
        <>
          {text.slice(0, run.start)}
          <span className="diff-view__run">{text.slice(run.start, run.end)}</span>
          {text.slice(run.end)}
        </>
      ) : (
        text || " "
      )}
    </span>
  );
}

/** The gutter's glyph, and the word read in its place. */
function Mark({ kind }: { kind: DiffLineKind }) {
  return (
    <span className="diff-view__mark">
      <span aria-hidden="true">{kind === "add" ? "+" : kind === "remove" ? "−" : " "}</span>
      {kind === "add" && <VisuallyHidden>Added</VisuallyHidden>}
      {kind === "remove" && <VisuallyHidden>Removed</VisuallyHidden>}
    </span>
  );
}

/** A line number: the reader's way to the line in the version beside it, and not read aloud. */
function LineNumber({ n }: { n: number | null }) {
  return (
    <span className="diff-view__number" aria-hidden="true">
      {n ?? ""}
    </span>
  );
}

interface Tracks {
  numbered: boolean;
  keyed: boolean;
  notes?: ((line: DiffLine) => ReactNode) | undefined;
  onPick?: ((line: DiffLine, extend: boolean) => void) | undefined;
  picked?: ((line: DiffLine) => boolean) | undefined;
}

/**
 * A line's number in one of its two slots — as a button that picks the line,
 * where the host picks lines and this is the slot the line is picked by: its
 * own number's (its newer one, else its older — the number a finding names),
 * or, on the left of a split diff, the one number that side draws of an
 * unchanged line, whose newer copy on the right is not read twice.
 */
function SlotNumber({
  line,
  slot,
  side,
  tracks,
}: {
  line: DiffLine;
  slot: "before" | "after";
  side: "left" | "right" | undefined;
  tracks: Tracks;
}) {
  const n = slot === "after" ? line.after : line.before;
  const own =
    side === "left"
      ? true
      : side === "right"
        ? line.kind !== "same"
        : slot === (line.after !== null ? "after" : "before");
  const pick = tracks.onPick;
  if (!pick || n === null || !own) return <LineNumber n={n} />;
  const named = line.after ?? line.before;
  return (
    <button
      type="button"
      className="diff-view__number diff-view__pick"
      aria-label={`${line.after !== null ? "Line" : "Removed line"} ${named}: pick it`}
      onClick={(event) => pick(line, event.shiftKey)}
    >
      {n}
    </button>
  );
}

/** What the host draws under a row's lines, or nothing. */
function Notes({ lines, tracks }: { lines: readonly (DiffLine | null)[]; tracks: Tracks }) {
  if (!tracks.notes) return null;
  // AN UNCHANGED LINE IS BOTH SIDES OF ITS PAIR, and its notes are drawn once.
  const drawn = lines.flatMap((line, i) => {
    if (!line || (i > 0 && line === lines[0])) return [];
    const note = tracks.notes!(line);
    return note === null || note === undefined || note === false
      ? []
      : [<Fragment key={i}>{note}</Fragment>];
  });
  if (drawn.length === 0) return null;
  return <div className="diff-view__notes">{drawn}</div>;
}

/**
 * One line. In a unified diff it carries both numbers; on one side of a split
 * one, the older text's on the left and the newer's on the right.
 */
function Line({
  line,
  run,
  tracks,
  side,
  hidden = false,
}: {
  line: DiffLine;
  run: Run | null;
  tracks: Tracks;
  side?: "left" | "right" | undefined;
  hidden?: boolean | undefined;
}) {
  return (
    <div
      className="diff-view__line"
      data-kind={line.kind}
      data-picked={tracks.picked?.(line) || undefined}
      aria-hidden={hidden || undefined}
    >
      {tracks.numbered && side !== "right" && (
        <SlotNumber line={line} slot="before" side={side} tracks={tracks} />
      )}
      {tracks.numbered && side !== "left" && (
        <SlotNumber line={line} slot="after" side={side} tracks={tracks} />
      )}
      <Mark kind={line.kind} />
      {tracks.keyed && <span className="diff-view__path">{line.path ?? ""}</span>}
      <Text text={line.text} run={run} />
    </div>
  );
}

function RowView({ row, tracks, pairs }: { row: Row; tracks: Tracks; pairs: Pairs }) {
  switch (row.kind) {
    case "skip":
      return <div className="diff-view__skip">{plural(row.count, "unchanged line")}</div>;
    case "line":
      return (
        <>
          <Line line={row.line} run={underlineOf(pairs, row.line)} tracks={tracks} />
          <Notes lines={[row.line]} tracks={tracks} />
        </>
      );
    case "pair": {
      const same = row.left?.kind === "same";
      return (
        <>
          <div className="diff-view__pair">
            {/* A SIDE THE CHANGE DID NOT REACH is an empty cell on the diff's
              own ground: a removal has no newer line beside it. */}
            {row.left ? (
              <Line
                line={row.left}
                run={underlineOf(pairs, row.left)}
                tracks={tracks}
                side="left"
              />
            ) : (
              <div aria-hidden="true" />
            )}
            {row.right ? (
              // AN UNCHANGED LINE IS READ ONCE: its newer copy is the same words.
              <Line
                line={row.right}
                run={underlineOf(pairs, row.right)}
                tracks={tracks}
                side="right"
                hidden={same}
              />
            ) : (
              <div aria-hidden="true" />
            )}
          </div>
          <Notes lines={[row.left, row.right]} tracks={tracks} />
        </>
      );
    }
  }
}

export function DiffView(props: DiffViewProps) {
  const { layout = "unified", against, empty, "aria-label": label, notes, onPick, picked } = props;
  const { hunks: given, before, after, context } = props;
  // TWO TEXTS ARE COMPARED ONCE, for as long as they are the two texts.
  const compared = useMemo(
    () => (given ? null : compareLines(before ?? "", after ?? "", context)),
    [given, before, after, context],
  );
  const hunks = given ?? (compared?.kind === "compared" ? compared.hunks : NO_HUNKS);
  const shape = useMemo(() => shapeOf(hunks), [hunks]);
  const changed = shape.added > 0 || shape.removed > 0;
  const root = useRef<HTMLDivElement>(null);
  const body = useRef<HTMLDivElement>(null);
  // MEASURED ONCE THERE IS A DIFF TO MEASURE: an identical pair draws no root,
  // and a width read before the root exists would stay zero for good.
  const narrow = useNarrowerThan(root, PHONE_BREAKPOINT, changed);
  const split = layout === "split" && !shape.keyed && !narrow;
  // ONE DIFF, ONE SET OF PAIRS, whichever layout draws it: a diff that turns
  // unified as its column narrows keeps every underline it has already read.
  const pairs = useMemo(() => pairsOf(hunks, shape.keyed), [hunks, shape.keyed]);
  const rows = useMemo(() => rowsOf(hunks, split), [hunks, split]);
  const blocks = useMemo(() => blocksOf(rows), [rows]);
  const shown = useWindow(body, blocks, rows.length > WINDOW_FROM_ROWS);

  if (compared && compared.kind !== "compared") {
    return (
      <p className="diff-view__uncompared">
        The two versions were not compared line by line: {notComparedReason(compared)}.
      </p>
    );
  }
  if (!changed) {
    return <>{empty ?? <p className="diff-view__empty">Nothing changed.</p>}</>;
  }
  const tracks: Tracks = { numbered: shape.numbered, keyed: shape.keyed, notes, onPick, picked };
  return (
    <div
      ref={root}
      role="group"
      aria-label={label ?? "Line changes"}
      className={cx(
        "diff-view",
        shape.numbered && "diff-view--numbered",
        shape.keyed && "diff-view--keyed",
        split && "diff-view--split",
      )}
      style={
        {
          "--diff-digits": String(shape.digits),
          "--diff-path": String(shape.path),
        } as CSSProperties
      }
    >
      {against !== undefined && (
        <p className="diff-view__head">
          <span aria-hidden="true">
            <span className="diff-view__count">+{shape.added.toLocaleString()}</span>{" "}
            <span className="diff-view__count">−{shape.removed.toLocaleString()}</span>
          </span>
          <VisuallyHidden>
            {plural(shape.added, "line")} added, {shape.removed.toLocaleString()} removed
          </VisuallyHidden>{" "}
          against {against}
        </p>
      )}
      <div ref={body} className="diff-view__body">
        {blocks.map((block, at) =>
          shown.holds(at) ? (
            <div key={at} className="diff-view__block" data-block={at}>
              {block.map((row, i) => (
                <RowView key={i} row={row} tracks={tracks} pairs={pairs} />
              ))}
            </div>
          ) : (
            <div
              key={at}
              className="diff-view__block"
              data-block={at}
              aria-hidden="true"
              style={{ height: `${shown.height(at)}px` }}
            />
          ),
        )}
      </div>
    </div>
  );
}
