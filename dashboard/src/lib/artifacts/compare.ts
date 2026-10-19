/**
 * Comparing two versions of an artifact: which files changed between them,
 * and, for a text file, which lines.
 *
 * PURE: the History panel's compare pane fetches the manifests and the texts and
 * hands them here, so what a comparison says is held by tests rather than by a
 * screen.
 *
 * FILES ARE COMPARED BY DIGEST. A version names every file by its SHA-256, and
 * a file the engine carried from one version to the next keeps its digest, so
 * two files with one digest are the same bytes whatever else changed.
 *
 * LINES ARE COMPARED BY THE SHORTEST EDIT SCRIPT (Myers' algorithm, the one
 * every line diff a reader has met uses), bounded: past [MAX_COMPARE_LINES] a
 * side, or [MAX_EDITS] edits, a file is reported too different to show line
 * by line rather than freezing the tab on a regenerated bundle.
 */

import type { ArtifactEntry, ArtifactManifest } from "~/protocol/index.ts";

/** What happened to one file between the two versions. */
export type FileChange =
  | { kind: "added"; path: string; to: ArtifactEntry }
  | { kind: "removed"; path: string; from: ArtifactEntry }
  | { kind: "changed"; path: string; from: ArtifactEntry; to: ArtifactEntry };

/** The files of two versions, compared. */
export interface FileComparison {
  /** Every file added, removed or changed, by path. */
  changes: FileChange[];
  /** How many files the two versions hold identically. */
  unchanged: number;
}

/** Compares the files of `from` (the older side) with those of `to`. */
export function compareFiles(from: ArtifactManifest, to: ArtifactManifest): FileComparison {
  const before = new Map(from.files.map((f) => [f.path, f]));
  const after = new Map(to.files.map((f) => [f.path, f]));
  const paths = [...new Set([...before.keys(), ...after.keys()])].sort();
  const changes: FileChange[] = [];
  let unchanged = 0;
  for (const path of paths) {
    const a = before.get(path);
    const b = after.get(path);
    if (a && b) {
      if (a.object.hash === b.object.hash) unchanged++;
      else changes.push({ kind: "changed", path, from: a, to: b });
    } else if (b) {
      changes.push({ kind: "added", path, to: b });
    } else if (a) {
      changes.push({ kind: "removed", path, from: a });
    }
  }
  return { changes, unchanged };
}

/**
 * Whether a file's served type is text a reader can compare line by line:
 * the text types and the structured text the web serves under application or
 * image types.
 */
export function isComparableText(contentType: string): boolean {
  const essence = (contentType.split(";")[0] ?? "").trim().toLowerCase();
  return (
    essence.startsWith("text/") ||
    [
      "application/json",
      "application/javascript",
      "application/xml",
      "application/manifest+json",
      "image/svg+xml",
    ].includes(essence)
  );
}

/** The most bytes a side of a line comparison is read for: a mebibyte. */
export const MAX_COMPARE_BYTES = 1 << 20;

/** The most lines a side of a line comparison holds. */
export const MAX_COMPARE_LINES = 20_000;

/** The most edits a line comparison computes before it calls two files too different. */
export const MAX_EDITS = 4_000;

/**
 * One run of changed lines: what the older version held at `fromStart` (one
 * based) and what the newer holds at `toStart`. An addition holds no `from`
 * lines and a removal no `to` lines.
 */
export interface Hunk {
  fromStart: number;
  from: string[];
  toStart: number;
  to: string[];
}

/** Two texts compared line by line, or the reason they were not. */
export type LineComparison =
  | { kind: "hunks"; hunks: Hunk[] }
  | { kind: "too-large"; lines: number }
  | { kind: "too-different" };

/** Splits a text into lines, a final newline ending the last line rather than starting another. */
export function lines(text: string): string[] {
  if (text === "") return [];
  const out = text.split(/\r\n|\n|\r/);
  if (out.length > 0 && out[out.length - 1] === "") out.pop();
  return out;
}

/** Compares two texts line by line. */
export function compareLines(fromText: string, toText: string): LineComparison {
  const a = lines(fromText);
  const b = lines(toText);
  const longest = Math.max(a.length, b.length);
  if (longest > MAX_COMPARE_LINES) return { kind: "too-large", lines: longest };
  const script = editScript(a, b);
  if (script === null) return { kind: "too-different" };
  return { kind: "hunks", hunks: hunksOf(script, a, b) };
}

/** One step of an edit script: a line kept, removed from `a` or added from `b`. */
type Step = "keep" | "remove" | "add";

/**
 * The shortest edit script turning `a` into `b`, or null past [MAX_EDITS]:
 * Myers' greedy algorithm over the edit graph, each round's furthest reaching
 * paths kept so the script is walked back from the end.
 */
function editScript(a: string[], b: string[]): Step[] | null {
  const n = a.length;
  const m = b.length;
  const max = Math.min(n + m, MAX_EDITS);
  const offset = max + 1;
  let v = new Int32Array(2 * max + 3);
  const trace: Int32Array[] = [];
  for (let d = 0; d <= max; d++) {
    trace.push(v.slice());
    const next = v.slice();
    for (let k = -d; k <= d; k += 2) {
      let x =
        k === -d || (k !== d && (v[offset + k - 1] ?? 0) < (v[offset + k + 1] ?? 0))
          ? (v[offset + k + 1] ?? 0)
          : (v[offset + k - 1] ?? 0) + 1;
      let y = x - k;
      while (x < n && y < m && a[x] === b[y]) {
        x++;
        y++;
      }
      next[offset + k] = x;
      if (x >= n && y >= m) {
        trace.push(next);
        return walkBack(trace, n, m, offset);
      }
    }
    v = next;
  }
  return null;
}

/** Walks the furthest reaching paths back from the end into a script, front first. */
function walkBack(trace: Int32Array[], n: number, m: number, offset: number): Step[] {
  const steps: Step[] = [];
  let x = n;
  let y = m;
  // trace[d] is the state before round d; the last entry is the round that
  // reached the end.
  for (let d = trace.length - 2; d >= 0; d--) {
    const v = trace[d] ?? new Int32Array(0);
    const k = x - y;
    const down = k === -d || (k !== d && (v[offset + k - 1] ?? 0) < (v[offset + k + 1] ?? 0));
    const prevK = down ? k + 1 : k - 1;
    const prevX = v[offset + prevK] ?? 0;
    const prevY = prevX - prevK;
    while (x > prevX && y > prevY) {
      steps.push("keep");
      x--;
      y--;
    }
    if (d > 0) {
      if (down) {
        steps.push("add");
        y--;
      } else {
        steps.push("remove");
        x--;
      }
    }
  }
  while (x > 0 && y > 0) {
    steps.push("keep");
    x--;
    y--;
  }
  return steps.reverse();
}

/** Groups an edit script's consecutive changes into hunks. */
function hunksOf(script: Step[], a: string[], b: string[]): Hunk[] {
  const out: Hunk[] = [];
  let x = 0;
  let y = 0;
  let open: Hunk | null = null;
  for (const step of script) {
    if (step === "keep") {
      if (open) out.push(open);
      open = null;
      x++;
      y++;
      continue;
    }
    open ??= { fromStart: x + 1, from: [], toStart: y + 1, to: [] };
    if (step === "remove") {
      open.from.push(a[x] ?? "");
      x++;
    } else {
      open.to.push(b[y] ?? "");
      y++;
    }
  }
  if (open) out.push(open);
  return out;
}
