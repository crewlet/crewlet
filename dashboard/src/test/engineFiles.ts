/**
 * Files the ENGINE writes that this dashboard's suites read as fixtures.
 *
 * A fixture typed here agrees with the screen whatever the engine sends, and
 * that is how the evict dialog came to read a top-level `outcome` for as long
 * as the route had stopped writing one: its suite passed on answers it had
 * written itself. So where the engine commits its own rendering — a golden its
 * Go test regenerates and compares — the suites read THAT file, and a change
 * on either side is a failing test until the other follows it.
 *
 * Found from the module root rather than by counting `../..`: the root is the
 * nearest directory above this checkout's dashboard that holds `go.mod`, which
 * is right from any depth and inside a nested worktree too.
 */

import { existsSync, readFileSync } from "node:fs";
import { dirname, join } from "node:path";

/** The module root: the nearest directory at or above the working directory holding go.mod. */
export function repoRoot(): string {
  let dir = process.cwd();
  for (;;) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    const up = dirname(dir);
    if (up === dir) throw new Error(`no go.mod at or above ${process.cwd()}`);
    dir = up;
  }
}

/** A JSON file the engine commits, parsed, by its path from the module root. */
export function engineFile<T>(path: string): T {
  return JSON.parse(readFileSync(join(repoRoot(), path), "utf8")) as T;
}

/**
 * GitHub's anchor for a heading, the same rule `internal/docsgate` pins: the
 * rendered text lower-cased, keeping letters, marks, digits, connectors,
 * spaces and hyphens, each space a hyphen.
 */
export function slug(heading: string): string {
  return heading
    .replace(/!?\[([^\]]*)\]\([^)]*\)/g, "$1")
    .replace(/<[^>]+>/g, "")
    .toLowerCase()
    .replace(/[^\p{L}\p{M}\p{N}\p{Pc} -]/gu, "")
    .replace(/ /g, "-");
}

/** Every anchor a markdown file renders, numbered on repetition, outside fences. */
export function headingAnchors(file: string): Set<string> {
  const out = new Set<string>();
  const seen = new Map<string, number>();
  let fenced = false;
  for (const line of readFileSync(file, "utf8").split("\n")) {
    if (/^\s{0,3}(```|~~~)/.test(line)) {
      fenced = !fenced;
      continue;
    }
    const m = fenced ? null : /^\s{0,3}#{1,6}\s+(.*?)\s*#*\s*$/.exec(line);
    if (!m) continue;
    const s = slug(m[1]!);
    const n = seen.get(s) ?? 0;
    out.add(n > 0 ? `${s}-${n}` : s);
    seen.set(s, n + 1);
  }
  return out;
}
