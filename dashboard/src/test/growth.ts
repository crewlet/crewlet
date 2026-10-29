/**
 * How a cost case tells a linear pass from a quadratic one by the clock,
 * with room enough that a loaded machine cannot decide it.
 *
 * A pass over prose an agent wrote runs on every render of every reader, so
 * one that turns quadratic freezes everybody, and its case has to catch that.
 * The cases this replaces grew the input four times and refused a ratio over
 * eight, where a linear pass reads four and a quadratic one sixteen: two
 * times of headroom, and a shared runner's noise (another job's load, a
 * collection, the regular expression engine compiling the longer input
 * differently) read a linear pass as 8.8.
 *
 * WIDE SEPARATION. The input grows THIRTY-TWO times, so a linear pass reads
 * about 32 and a quadratic one about 1,024, and the bound is their geometric
 * middle, 181. Measured, the margins are narrower than that arithmetic: over
 * twenty full parallel runs the linear cases read 9.8 to 43.6, four times
 * under the bound at worst, and the historical regressions read 471 to 1,073,
 * 2.6 times over it at worst. A regression whose linear term is large reads
 * well under 1,024 at a small run, so a case whose regression read under
 * twice the bound takes a larger base run (`inlineTokens.test.ts`). Sixteen
 * times, with a bound of 64, was not enough under a full parallel suite.
 *
 * THE PROCESS'S CPU TIME, NOT THE WALL CLOCK. Under a full parallel suite
 * every core is oversubscribed, and the wall clock counts the time this worker
 * waits for a core: a measuring window can hold no unloaded moment at all, so
 * a size's fastest sample was the CPU share it happened to get. Timed by the
 * wall, one linear case read anywhere from 9 to 80 beside a full suite, and
 * one run of twenty read 138; timed by CPU, the same case read 24 to 39.
 * `process.cpuUsage()` counts only the time the process runs, and still counts
 * everything the pass does, the regular expression engine and the garbage
 * collector included, so a quadratic pass reads as quadratic as ever.
 *
 * EQUAL EXPOSURE. What noise is left only ever adds time, and lands on a
 * sample in proportion to how long it runs. So each size is calibrated on its
 * own to about [SAMPLE_MS] of CPU a sample (a single call, where one call of
 * a slow input takes longer than that), the two compared as TIME PER CALL,
 * and each size's time is the fastest of several samples, interleaved, after
 * a warm-up. Timing both sizes with the same number of calls made a large
 * sample thirty-two times longer than a small one, and so thirty-two times as
 * likely to be disturbed: the noise fell on the numerator.
 *
 * ONE PROCESS PER WORKER, OR NOTHING. The process's CPU counts every thread
 * in it, so in a worker thread (Vitest's `threads` pool) it would count the
 * other test files running beside this one. The suite runs in the `forks`
 * pool, pinned in `vitest.config.ts`, and [growth] refuses to time anything
 * off a process's main thread rather than answer with a number that is
 * partly somebody else's.
 *
 * WHAT IT CATCHES IS EVERYTHING THE PASS DOES: its loops, the built-ins they
 * call and what a regular expression does inside one match alike, since the
 * clock sees all of it. Not a suite itself, and never imported by anything
 * that ships.
 */

import { isMainThread } from "node:worker_threads";

/** How many times larger the second run is. */
const GROWTH = 32;

/** The geometric middle of a linear pass's growth (32) and a quadratic one's (1,024). */
export const LINEAR_BOUND = Math.round(Math.sqrt(GROWTH * GROWTH * GROWTH));

/** How many samples each size takes, the fastest of which is its time. */
const SAMPLES = 7;

/** How much CPU one sample of either size takes, so both sizes are exposed to noise alike. */
const SAMPLE_MS = 20;

/** The CPU this process has used, user and system, in milliseconds. */
function cpuMs(): number {
  const { user, system } = process.cpuUsage();
  return (user + system) / 1000;
}

/** Milliseconds of CPU `calls` calls of `run` take. */
function time(run: () => void, calls: number): number {
  const start = cpuMs();
  for (let i = 0; i < calls; i++) run();
  return cpuMs() - start;
}

/** How many calls of `run` take about [SAMPLE_MS] of CPU: at least one. */
function calibrate(run: () => void): number {
  let calls = 1;
  for (;;) {
    const took = time(run, calls);
    if (took >= SAMPLE_MS) return calls;
    // Straight to the estimate once a sample is long enough to measure, so a
    // fast pass does not double its way up through a hundred short samples.
    calls = took > SAMPLE_MS / 16 ? Math.ceil((calls * SAMPLE_MS) / took) : calls * 2;
  }
}

/**
 * How many times longer a call over `GROWTH * n` takes than one over `n`,
 * where `make` builds the input once and returns the pass over it.
 */
export function growth(make: (n: number) => () => void, n: number): number {
  if (!isMainThread) {
    throw new Error(
      "growth times a pass by its process's CPU, which a worker thread shares with every " +
        "other test file in it: run this suite in Vitest's forks pool (vitest.config.ts)",
    );
  }
  const small = make(n);
  const large = make(GROWTH * n);
  small();
  large();
  const smallCalls = calibrate(small);
  const largeCalls = calibrate(large);
  let fastestSmall = Infinity;
  let fastestLarge = Infinity;
  for (let i = 0; i < SAMPLES; i++) {
    fastestSmall = Math.min(fastestSmall, time(small, smallCalls) / smallCalls);
    fastestLarge = Math.min(fastestLarge, time(large, largeCalls) / largeCalls);
  }
  return fastestLarge / fastestSmall;
}
