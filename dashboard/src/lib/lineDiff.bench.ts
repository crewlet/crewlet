// @vitest-environment node
/**
 * What comparing two texts line by line costs: the measurement that decides
 * [MAX_COMPARE_LINES] and [MAX_EDITS] in `lineDiff.ts`, kept so the next change
 * to the comparison (its middle snake, the interning, the sections it builds)
 * is timed against the same four pairs those bounds were set by.
 *
 * `npm run bench -- src/lib/lineDiff.bench.ts` runs it, through `vitest bench`.
 * No gate does: a time is the machine's, and a gate on one is a gate on
 * whoever else is using the machine. The bounds' comments quote what it read,
 * on which machine.
 *
 * EACH PAIR IS CHECKED BEFORE IT IS TIMED, so a case times what its name says:
 * a pair at both bounds that a change of either bound pushed past one would
 * otherwise time a quick refusal and read as a faster comparison.
 *
 * NODE, NOT JSDOM: a comparison touches no DOM, and the environment's setup is
 * not what a reader's browser pays for.
 *
 * A FIXED NUMBER OF RUNS, not tinybench's one second a case: a run at both
 * bounds takes about a tenth of a second, so twenty of them after three to
 * warm up give an average and a worst to quote in a few seconds a case.
 */

import { expect, test } from "vitest";
import { MAX_COMPARE_LINES, MAX_EDITS, compareLines, type LineComparison } from "./lineDiff.ts";

/** Every pair is timed this many times, after [WARMUP] untimed runs. */
const RUNS = 20;
/** Untimed runs first, so the time is the optimised code's, as a reader's second diff is. */
const WARMUP = 3;
const bounded = { time: 0, iterations: RUNS, warmupTime: 0, warmupIterations: WARMUP };

/**
 * Line `i` of a text of code: one in five a blank or a closing brace, the lines
 * a source file repeats and that give a comparison false matches to rule out,
 * and the rest each its own statement.
 */
const codeLine = (i: number) =>
  i % 10 === 3 ? "" : i % 10 === 7 ? "}" : `  const value${i} = compute(value${i - 1}, ${i});`;

/** `n` lines of code, as a text ending each with a newline. */
const code = (n: number, line: (i: number) => string = codeLine) =>
  Array.from({ length: n }, (_, i) => line(i));

const text = (lines: readonly string[]) => lines.join("\n") + "\n";

/** A deterministic pseudo-random source, so every run times the same shuffle. */
function random(seed: number) {
  return () => {
    seed = (seed * 1103515245 + 12345) % 2147483648;
    return seed / 2147483648;
  };
}

/** The lines a comparison marks removed or added. */
function marked(got: LineComparison): number {
  if (got.kind !== "compared") throw new Error(`not compared: ${got.kind}`);
  return got.hunks.reduce(
    (sum, h) => sum + h.lines.filter((l) => l.kind === "add" || l.kind === "remove").length,
    0,
  );
}

const lines = code(MAX_COMPARE_LINES);
const older = text(lines);

// AT BOTH BOUNDS: half the edit bound's lines are replaced, spread evenly from
// the first line to the last, so every line is in what changed and the
// shortest edit removes and adds exactly [MAX_EDITS]. At 20,000 and 4,000,
// about every tenth line.
test("a comparison at both bounds", async ({ bench }) => {
  const replaced = MAX_EDITS / 2;
  const last = lines.length - 1;
  const edited = new Set(
    Array.from({ length: replaced }, (_, k) => Math.round((k * last) / (replaced - 1))),
  );
  const newer = text(lines.map((l, i) => (edited.has(i) ? `${l} // edited` : l)));
  expect(edited.size).toBe(replaced);
  expect(marked(compareLines(older, newer))).toBe(MAX_EDITS);
  await bench("at both bounds", () => compareLines(older, newer)).run(bounded);
});

// ONE LINE EDITED in the same text: the shared start and end are walked once,
// and only the edited line is compared.
test("a comparison of one line edited", async ({ bench }) => {
  const newer = text(lines.map((l, i) => (i === lines.length / 2 ? `${l} // edited` : l)));
  expect(marked(compareLines(older, newer))).toBe(2);
  await bench("one line edited", () => compareLines(older, newer)).run(bounded);
});

// NO LINE SHARED: the floor read off how often each line occurs refuses the
// pair before a round of the comparison runs.
test("a refusal of two texts that share no line", async ({ bench }) => {
  const other = text(code(MAX_COMPARE_LINES, (i) => `  let other${i} = ${i};`));
  expect(compareLines(older, other)).toEqual({ kind: "too-different" });
  await bench("no line shared", () => compareLines(older, other)).run(bounded);
});

// THE SAME LINES SHUFFLED: every line occurs as often in both, so the floor
// is nothing, and only the rounds, run to the edit bound, can refuse it.
test("a refusal of the same lines shuffled", async ({ bench }) => {
  const rand = random(1);
  const shuffled = [...lines];
  for (let i = shuffled.length - 1; i > 0; i--) {
    const j = Math.floor(rand() * (i + 1));
    [shuffled[i], shuffled[j]] = [shuffled[j]!, shuffled[i]!];
  }
  const newer = text(shuffled);
  expect(compareLines(older, newer)).toEqual({ kind: "too-different" });
  await bench("same lines shuffled", () => compareLines(older, newer)).run(bounded);
});
