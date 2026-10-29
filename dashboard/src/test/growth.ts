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
 * middle, 181: more than five times of headroom on each side. Sixteen times,
 * with a bound of 64, was not enough under a full parallel suite.
 *
 * EQUAL EXPOSURE. Noise only ever adds time, and it lands on a sample in
 * proportion to how long the sample runs. So each size is calibrated on its
 * own to about [SAMPLE_MS] a sample, the two compared as TIME PER CALL, and
 * each size's time is the fastest of several samples, interleaved, after a
 * warm-up. Timing both sizes with the same number of calls made a large
 * sample thirty-two times longer than a small one, and so thirty-two times as
 * likely to be preempted: the noise fell on the numerator.
 *
 * WHAT IT CATCHES IS EVERYTHING THE PASS DOES: its loops, the built-ins they
 * call and what a regular expression does inside one match alike, since the
 * clock sees all of it. Not a suite itself, and never imported by anything
 * that ships.
 */

/** How many times larger the second run is. */
const GROWTH = 32;

/** The geometric middle of a linear pass's growth (32) and a quadratic one's (1,024). */
export const LINEAR_BOUND = Math.round(Math.sqrt(GROWTH * GROWTH * GROWTH));

/** How many samples each size takes, the fastest of which is its time. */
const SAMPLES = 7;

/** How long one sample of either size runs, so both sizes are exposed to noise alike. */
const SAMPLE_MS = 20;

/** Milliseconds `calls` calls of `run` take. */
function time(run: () => void, calls: number): number {
  const start = performance.now();
  for (let i = 0; i < calls; i++) run();
  return performance.now() - start;
}

/** How many calls of `run` take about [SAMPLE_MS]: at least one. */
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
