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
 * package's tokens and its pure line diff (`diffLines`, `collapseDiff`), and
 * none of the three is imported anywhere (`styles/diffs.test.ts`).
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
 * each removed line beside the added line it pairs with, and it is drawn only
 * where the diff is at least the phone breakpoint wide (640px, the kit's
 * `breakpoint.phone`): two columns of code under that are a word or two each.
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
 */

import {
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
import { VisuallyHidden, collapseDiff, cx, diffLines } from "@crewlethq/ui";
import { PHONE_BREAKPOINT } from "~/app/layout.ts";
import { plural } from "~/lib/format.ts";
import { scrollerOf } from "~/lib/scroller.ts";
import { useNarrowerThan } from "~/lib/useNarrowerThan.ts";
import { changedRuns, type Run } from "~/lib/wordDiff.ts";
// THE DIFF'S OWN SHEET: every screen that draws a diff is a lazy chunk, so the
// rules arrive with the first of them rather than in the first paint.
import "~/styles/diff.css";

/** What happened to one line. */
export type DiffLineKind = "same" | "add" | "remove";

/**
 * One line of a diff: the design system's `DiffLine`, so its line diff's
 * output is this shape as it is, with the `path` a setting is located by.
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
 * design system's `DiffSection`, which `collapseDiff` returns.
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
      /** Unchanged lines kept either side of a change: `collapseDiff`'s own three when unset. */
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
};

/**
 * How many rows a diff draws before it is windowed.
 *
 * A row is about 21px (13px mono on the body's 1.45 leading, and a quarter
 * step of padding above and below), so a 900px view shows some 43 rows and
 * the window — the view and one view either side of it — about 130: two
 * blocks, three at most. A thousand rows is some seven windows' worth, and
 * under it mounting all of them is a few thousand elements, which a tab lays
 * out in a frame or two; the cost of windowing them instead is find-in-page
 * and a screen reader's reading of the whole diff, which only a diff past it
 * is worth paying. Above it the mount grows with the diff and the window does
 * not.
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

/** One side of a split row: a line and its underline. */
interface Side {
  line: DiffLine;
  run: Run | null;
}

/** One row of the diff, as it is drawn. */
type Row =
  | { kind: "skip"; count: number }
  | { kind: "line"; line: DiffLine; run: Run | null }
  | { kind: "pair"; left: Side | null; right: Side | null };

/**
 * The underline of every line of one CHANGE — a run of lines none of which is
 * unchanged — by its index in the run.
 *
 * Which removed line an added one replaced: in a text, the kth removed with
 * the kth added, the pairing a reader makes down the two columns of a split
 * diff; in a keyed diff, the removed and the added line of ONE path, since a
 * change can hold one setting removed and another one edited, and pairing
 * those by position would underline one setting against another.
 */
function changeRuns(change: readonly DiffLine[], keyed: boolean): (Run | null)[] {
  const runs: (Run | null)[] = change.map(() => null);
  const pair = (r: number, a: number) => {
    const found = changedRuns(change[r]!.text, change[a]!.text);
    runs[r] = found.before;
    runs[a] = found.after;
  };
  const removed = change.flatMap((line, i) => (line.kind === "remove" ? [i] : []));
  const added = change.flatMap((line, i) => (line.kind === "add" ? [i] : []));
  if (keyed) {
    for (const r of removed) {
      const a = added.find((i) => change[i]!.path === change[r]!.path);
      if (a !== undefined) pair(r, a);
    }
  } else {
    for (let k = 0; k < Math.min(removed.length, added.length); k++) pair(removed[k]!, added[k]!);
  }
  return runs;
}

/** The rows a diff draws, in the layout it is drawn in. */
function rowsOf(hunks: readonly DiffHunk[], split: boolean, keyed: boolean): Row[] {
  const rows: Row[] = [];
  for (const hunk of hunks) {
    if (hunk.skipped > 0) rows.push({ kind: "skip", count: hunk.skipped });
    const lines = hunk.lines;
    for (let i = 0; i < lines.length;) {
      const line = lines[i]!;
      if (line.kind === "same") {
        rows.push(
          split
            ? { kind: "pair", left: { line, run: null }, right: { line, run: null } }
            : { kind: "line", line, run: null },
        );
        i++;
        continue;
      }
      let end = i;
      while (end < lines.length && lines[end]!.kind !== "same") end++;
      const change = lines.slice(i, end);
      const runs = changeRuns(change, keyed);
      if (split) {
        const side = (k: number | undefined): Side | null =>
          k === undefined ? null : { line: change[k]!, run: runs[k] ?? null };
        const removed = change.flatMap((l, k) => (l.kind === "remove" ? [k] : []));
        const added = change.flatMap((l, k) => (l.kind === "add" ? [k] : []));
        for (let k = 0; k < Math.max(removed.length, added.length); k++) {
          rows.push({ kind: "pair", left: side(removed[k]), right: side(added[k]) });
        }
      } else {
        change.forEach((l, k) => rows.push({ kind: "line", line: l, run: runs[k] ?? null }));
      }
      i = end;
    }
  }
  return rows;
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
    <div className="diff-view__line" data-kind={line.kind} aria-hidden={hidden || undefined}>
      {tracks.numbered && side !== "right" && <LineNumber n={line.before} />}
      {tracks.numbered && side !== "left" && <LineNumber n={line.after} />}
      <Mark kind={line.kind} />
      {tracks.keyed && <span className="diff-view__path">{line.path ?? ""}</span>}
      <Text text={line.text} run={run} />
    </div>
  );
}

function RowView({ row, tracks }: { row: Row; tracks: Tracks }) {
  switch (row.kind) {
    case "skip":
      return <div className="diff-view__skip">{plural(row.count, "unchanged line")}</div>;
    case "line":
      return <Line line={row.line} run={row.run} tracks={tracks} />;
    case "pair": {
      const same = row.left?.line.kind === "same";
      return (
        <div className="diff-view__pair">
          {/* A SIDE THE CHANGE DID NOT REACH is an empty cell on the diff's
              own ground: a removal has no newer line beside it. */}
          {row.left ? (
            <Line line={row.left.line} run={row.left.run} tracks={tracks} side="left" />
          ) : (
            <div aria-hidden="true" />
          )}
          {row.right ? (
            // AN UNCHANGED LINE IS READ ONCE: its newer copy is the same words.
            <Line
              line={row.right.line}
              run={row.right.run}
              tracks={tracks}
              side="right"
              hidden={same}
            />
          ) : (
            <div aria-hidden="true" />
          )}
        </div>
      );
    }
  }
}

export function DiffView(props: DiffViewProps) {
  const { layout = "unified", against, empty, "aria-label": label } = props;
  const { hunks: given, before, after, context } = props;
  const hunks = useMemo<readonly DiffHunk[]>(
    () => given ?? collapseDiff(diffLines(before ?? "", after ?? ""), context),
    [given, before, after, context],
  );
  const shape = useMemo(() => shapeOf(hunks), [hunks]);
  const changed = shape.added > 0 || shape.removed > 0;
  const root = useRef<HTMLDivElement>(null);
  const body = useRef<HTMLDivElement>(null);
  // MEASURED ONCE THERE IS A DIFF TO MEASURE: an identical pair draws no root,
  // and a width read before the root exists would stay zero for good.
  const narrow = useNarrowerThan(root, PHONE_BREAKPOINT, changed);
  const split = layout === "split" && !shape.keyed && !narrow;
  const rows = useMemo(() => rowsOf(hunks, split, shape.keyed), [hunks, split, shape.keyed]);
  const blocks = useMemo(() => blocksOf(rows), [rows]);
  const shown = useWindow(body, blocks, rows.length > WINDOW_FROM_ROWS);

  if (!changed) {
    return <>{empty ?? <p className="diff-view__empty">Nothing changed.</p>}</>;
  }
  const tracks: Tracks = { numbered: shape.numbered, keyed: shape.keyed };
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
                <RowView key={i} row={row} tracks={tracks} />
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
