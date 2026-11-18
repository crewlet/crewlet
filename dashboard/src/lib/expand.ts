/** A patch's hidden unchanged lines put back in place: the lines a reader revealed in each gap,
 *  read from the newer blob and numbered on both sides. */
import type { DiffHunk, DiffLine } from "~/components/DiffView.tsx";
import { HUNK_HEADER } from "~/lib/patch.ts";

/** Lines revealed per gap: above hunk i from its bottom, after the last (the hunks' count) from its top. */
export type Revealed = ReadonlyMap<number, number>;

/** Lines one Expand press reveals in a gap larger than it. */
export const EXPAND_STEP = 20;

/** Where a hunk starts and ends on each side, read off its header as `parsePatch` numbers its lines. */
interface Span {
  oldStart: number;
  newStart: number;
  oldEnd: number;
  newEnd: number;
}

function spanOf(hunk: DiffHunk): Span | null {
  const m = hunk.header ? HUNK_HEADER.exec(hunk.header) : null;
  if (!m) return null;
  const oldLines = m[2] === undefined ? 1 : Number(m[2]);
  const newLines = m[4] === undefined ? 1 : Number(m[4]);
  // AN EMPTY SIDE names the line before it, as `parsePatch` reads it.
  const oldStart = oldLines === 0 ? Number(m[1]) + 1 : Number(m[1]);
  const newStart = newLines === 0 ? Number(m[3]) + 1 : Number(m[3]);
  return { oldStart, newStart, oldEnd: oldStart + oldLines, newEnd: newStart + newLines };
}

/** What each gap still hides above each hunk, and after the last where the newer blob's length is known. */
export function gapsOf(
  hunks: readonly DiffHunk[],
  newerLines: number | null,
): { above: number[]; trailing: number | null } {
  const last = hunks.length > 0 ? spanOf(hunks[hunks.length - 1]!) : null;
  return {
    above: hunks.map((h) => h.skipped),
    trailing: last && newerLines !== null ? Math.max(0, newerLines - last.newEnd + 1) : null,
  };
}

/** The same lines of both sides, `newer` numbering, `shift` the newer number less the older. */
function sameLines(
  from: number,
  count: number,
  shift: number,
  newer: (n: number) => string | undefined,
): DiffLine[] {
  const out: DiffLine[] = [];
  for (let n = from; n < from + count; n++) {
    out.push({ kind: "same", text: newer(n) ?? "", before: n - shift, after: n });
  }
  return out;
}

/** A hunk's header redrawn for the lines it now holds. Git's section words name the hunk's own
 *  start, so lines revealed above it drop them. */
function headerFor(lines: readonly DiffLine[], was: string | undefined, grown: boolean): string {
  const olds = lines.filter((l) => l.before !== null);
  const news = lines.filter((l) => l.after !== null);
  const section = was && !grown ? was.replace(/^@@[^@]*@@/, "") : "";
  const side = (sign: string, all: DiffLine[], key: "before" | "after") =>
    `${sign}${all.length > 0 ? all[0]![key] : 0},${all.length}`;
  return `@@ ${side("-", olds, "before")} ${side("+", news, "after")} @@${section}`;
}

/** The hunks with the revealed lines of `newer` (1-based lines of the newer blob, `newerLines` long
 *  where known) in place; a gap revealed whole joins its hunk to the one above, and `gaps[k]` is
 *  the original gap above output hunk k. */
export function expandHunks(
  hunks: readonly DiffHunk[],
  revealed: Revealed,
  newer: (n: number) => string | undefined,
  newerLines: number | null = null,
): { hunks: DiffHunk[]; gaps: number[] } {
  if (revealed.size === 0) return { hunks: [...hunks], gaps: hunks.map((_, i) => i) };
  const out: DiffHunk[] = [];
  const gaps: number[] = [];
  const touched = new Set<number>();
  const grown = new Set<number>();
  hunks.forEach((hunk, i) => {
    const span = spanOf(hunk);
    const r = span ? Math.min(revealed.get(i) ?? 0, hunk.skipped) : 0;
    if (r === 0 || !span) {
      out.push(hunk);
      gaps.push(i);
      return;
    }
    const shift = span.newStart - span.oldStart;
    const lines = [...sameLines(span.newStart - r, r, shift, newer), ...hunk.lines];
    const left = hunk.skipped - r;
    if (left === 0 && out.length > 0) {
      const above = out[out.length - 1]!;
      out[out.length - 1] = { ...above, lines: [...above.lines, ...lines] };
      touched.add(out.length - 1);
      return;
    }
    out.push({ ...hunk, lines, skipped: left });
    touched.add(out.length - 1);
    grown.add(out.length - 1);
    gaps.push(i);
  });
  const last = hunks.length > 0 ? spanOf(hunks[hunks.length - 1]!) : null;
  // NEVER PAST THE BLOB'S END, once its length is known.
  const below = Math.min(
    revealed.get(hunks.length) ?? 0,
    last && newerLines !== null ? Math.max(0, newerLines - last.newEnd + 1) : Infinity,
  );
  if (below > 0 && last && out.length > 0) {
    const tail = out[out.length - 1]!;
    const shift = last.newEnd - last.oldEnd;
    out[out.length - 1] = {
      ...tail,
      lines: [...tail.lines, ...sameLines(last.newEnd, below, shift, newer)],
    };
    touched.add(out.length - 1);
  }
  for (const k of touched)
    out[k] = { ...out[k]!, header: headerFor(out[k]!.lines, out[k]!.header, grown.has(k)) };
  return { hunks: out, gaps };
}
