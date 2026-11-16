/**
 * What colouring adds to the windowing threshold's worst case: a 1,000-row split
 * diff of Go rendered plain and with a blob's spans, and the walk over its two
 * blobs. jsdom lays nothing out, so this is render work, not a first paint.
 */
import { cleanup, render } from "@testing-library/react";
import { expect, test } from "vitest";
import { SYNTAX_MAX_BYTES, load, statesOf } from "~/lib/syntax/index.ts";
import type { DiffSpan } from "~/lib/wordDiff.ts";
import { SAMPLES } from "~/test/syntax.ts";
import { DiffView, WINDOW_FROM_ROWS, type DiffHunk, type DiffLine } from "./DiffView.tsx";

/** Each case is timed this many times, after [WARMUP] untimed runs. */
const RUNS = 20;
const WARMUP = 3;
const bounded = { time: 0, iterations: RUNS, warmupTime: 0, warmupIterations: WARMUP };

/** The Go sample repeated to just under the bound, as lines. */
function blob(): string[] {
  const lines = SAMPLES.go.split("\n");
  const out: string[] = [];
  for (let k = 0, size = 0; ; k++) {
    const line = lines[k % lines.length]!;
    if (size + line.length + 1 >= SYNTAX_MAX_BYTES) return out;
    out.push(line);
    size += line.length + 1;
  }
}

/** One hunk of [WINDOW_FROM_ROWS] split rows: three unchanged lines, then a line edited. */
function hunkOf(older: readonly string[], newer: readonly string[]): DiffHunk[] {
  const lines: DiffLine[] = [];
  for (let row = 0; row < WINDOW_FROM_ROWS; row++) {
    const n = row + 1;
    if (row % 4 === 3) {
      lines.push({ kind: "remove", text: older[row]!, before: n, after: null });
      lines.push({ kind: "add", text: newer[row]!, before: null, after: n });
    } else {
      lines.push({ kind: "same", text: newer[row]!, before: n, after: n });
    }
  }
  return [{ skipped: 0, lines }];
}

test("a coloured split diff at the threshold, and its blobs' walk", async ({ bench }) => {
  const g = await load("go");
  const older = blob();
  // Every fourth line edited: a word renamed, so each pair has a changed run.
  const newer = older.map((line, i) => (i % 4 === 3 ? line.replace(/\w+/, "renamed") : line));
  const hunks = hunkOf(older, newer);
  const states = { before: statesOf(g, older, ""), after: statesOf(g, newer, "") };
  const texts = { before: older, after: newer };
  const tokens = (side: "before" | "after", n: number): DiffSpan[] =>
    g
      .line(texts[side][n - 1]!, states[side][n - 1]!)
      .tokens.map((t) => ({ end: t.end, className: t.kind ? `syn-${t.kind}` : null }));
  const drawn = render(<DiffView hunks={hunks} hue="code" layout="split" tokens={tokens} />);
  expect(drawn.container.querySelectorAll(".diff-view__pair")).toHaveLength(WINDOW_FROM_ROWS);
  expect(drawn.container.querySelector(".syn-keyword")).not.toBeNull();
  cleanup();

  await bench("plain", () => {
    render(<DiffView hunks={hunks} hue="code" layout="split" />);
    cleanup();
  }).run(bounded);
  await bench("coloured", () => {
    render(<DiffView hunks={hunks} hue="code" layout="split" tokens={tokens} />);
    cleanup();
  }).run(bounded);
  await bench("both blobs walked", () => {
    statesOf(g, older, "");
    statesOf(g, newer, "");
  }).run(bounded);
});
