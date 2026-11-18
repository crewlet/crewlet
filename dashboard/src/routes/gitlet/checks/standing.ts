/** A run of a check in words: what it checked, its tests, and its output's lines and failing lines. */

import { shortOID } from "~/lib/code.ts";
import { fmtCount } from "~/lib/format.ts";
import type { CodeCheckDetail, CodeCheckFailure, CodeCheckRow } from "~/protocol/index.ts";

/** The pane's Checked fact, the target's chip between `before` and `after`; null for a purpose not known. */
export function checkedWords(d: CodeCheckRow): { before: string; after: string } | null {
  switch (d.purpose) {
    case "candidate":
      return {
        before: "merged onto ",
        after: d.base ? ` at ${shortOID(d.base)} in the queue` : " in the queue",
      };
    case "head":
      return { before: "its head, for ", after: "" };
    case "tip":
      return { before: "", after: "'s tip" };
    case "bisect":
      return { before: "a landing on ", after: ", bisected" };
  }
  return null;
}

/** The tests line: "1 of 214 tests failed · exit code 1"; null where neither is answered. */
export function testsWords(
  d: Pick<CodeCheckDetail, "tests" | "exit_code">,
): { words: string; failed: boolean | null } | null {
  const exit = d.exit_code === undefined ? "" : `exit code ${d.exit_code}`;
  const t = d.tests;
  if (!t) return exit ? { words: exit, failed: null } : null;
  const noun = t.total === 1 ? "test" : "tests";
  const skipped = t.skipped ? `, ${fmtCount(t.skipped)} skipped` : "";
  const failed = t.failed ?? 0;
  const counted =
    failed > 0
      ? `${fmtCount(failed)} of ${fmtCount(t.total)} ${noun} failed${skipped}`
      : t.skipped
        ? `${fmtCount(t.total - t.skipped)} of ${fmtCount(t.total)} ${noun} passed${skipped}`
        : `${fmtCount(t.total)} ${noun} passed`;
  return { words: exit ? `${counted} · ${exit}` : counted, failed: failed > 0 };
}

/** The tail's lines, a trailing newline not a line of its own. */
export function tailLines(tail: string): string[] {
  const lines = tail.split("\n");
  if (lines.at(-1) === "") lines.pop();
  return lines;
}

/** The words a runner prints at the start of a failing line. */
export const TAIL_FAIL_MARKS = ["--- FAIL", "FAILED", "FAIL", "panic:"] as const;

const escaped = (s: string) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");

/**
 * The indexes of the lines that say a failure: a failure mark at the line's start (after
 * spaces, as a word), or a listed failure's place as a runner prints it (`orders_test.go:88`).
 */
export function failingLines(
  lines: readonly string[],
  failures: readonly CodeCheckFailure[],
): Set<number> {
  const marks = TAIL_FAIL_MARKS.map(
    (m) => new RegExp(`^\\s*${escaped(m)}${m.endsWith(":") ? "" : "(?![\\w-])"}`),
  );
  const places = failures
    .filter((f) => f.path && f.line)
    .map((f) => {
      const base = f.path!.split("/").at(-1)!;
      return new RegExp(`(^|[\\s/(])${escaped(`${base}:${f.line}`)}(?!\\d)`);
    });
  const marked = new Set<number>();
  lines.forEach((line, i) => {
    if (marks.some((m) => m.test(line)) || places.some((p) => p.test(line))) marked.add(i);
  });
  return marked;
}

/** Where the tail sits in the whole output: "lines 1,184 to 1,201 of 1,201"; null where uncounted. */
export function tailRangeWords(
  d: Pick<CodeCheckDetail, "log_lines" | "tail_first_line">,
  count: number,
): string | null {
  if (!d.log_lines || !d.tail_first_line || count === 0) return null;
  const last = d.tail_first_line + count - 1;
  return `lines ${fmtCount(d.tail_first_line)} to ${fmtCount(last)} of ${fmtCount(d.log_lines)}`;
}
