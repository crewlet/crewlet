// @vitest-environment node
/**
 * What colouring a file costs: each grammar's walk at [SYNTAX_MAX_BYTES], which
 * decides that bound, and one page's lines. No gate runs it, as a time is the
 * machine's: `npm run bench -- src/lib/syntax/syntax.bench.ts`.
 */
import { expect, test } from "vitest";
import { SAMPLES } from "~/test/syntax.ts";
import { SYNTAX_MAX_BYTES, load, statesOf, type Lang } from "./index.ts";

/** Each case is timed this many times, after [WARMUP] untimed runs. */
const RUNS = 20;
const WARMUP = 3;
const bounded = { time: 0, iterations: RUNS, warmupTime: 0, warmupIterations: WARMUP };
/** The lines a file view draws a page of. */
const PAGE = 1500;

/** A language's sample repeated to just under the bound, as lines. */
function atTheBound(lang: Lang): string[] {
  const lines = SAMPLES[lang].split("\n");
  const out: string[] = [];
  let size = 0;
  for (let k = 0; ; k++) {
    const line = lines[k % lines.length]!;
    if (size + line.length + 1 >= SYNTAX_MAX_BYTES) return out;
    out.push(line);
    size += line.length + 1;
  }
}

for (const lang of Object.keys(SAMPLES) as Lang[]) {
  test(`${lang}: the walk over a file at the bound, and a page of lines`, async ({ bench }) => {
    const g = await load(lang);
    const file = atTheBound(lang);
    expect(file.join("\n").length).toBeGreaterThan(SYNTAX_MAX_BYTES - 4096);
    const states = statesOf(g, file, "");
    const page = file.slice(0, PAGE);
    await bench(`${lang} walk`, () => statesOf(g, file, "")).run(bounded);
    await bench(`${lang} page`, () => {
      for (let k = 0; k < page.length; k++) g.line(page[k]!, states[k]!);
    }).run(bounded);
  });
}
