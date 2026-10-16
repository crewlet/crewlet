/**
 * How long a suite waits before it calls something hung: one bound for a
 * wait (`findBy…`, `waitFor`), set in `setup.ts`, and one for a whole case,
 * set in `vitest.config.ts`. `environment.test.tsx` holds both to what they
 * produce.
 *
 * BOTH ARE HANG DETECTORS, NEVER ASSERTIONS ABOUT SPEED. What a case costs is
 * CPU work — jsdom rendering a screen, React committing it — so its duration
 * is that work over the share of a CPU its worker is given, and a gate runs
 * this suite's parallel workers on machines it shares, beside the engine's Go
 * suites or another checkout's gate. The tools' own defaults, a
 * second for a wait and five for a case, were chosen by nobody here and held
 * that work to a wall-clock figure anyway. Measured on the org builder's
 * first check, mount to "No problems": about 150 ms idle, and 460 to 1081 ms
 * with fourteen busy processes beside the suite on four CPUs — the same two
 * requests every time, no retry and no backoff, only slower renders. Under
 * that load the builder's suites lost six and seven of their 663 cases in two
 * runs, to the one-second wait and the five-second case in about equal
 * numbers, and none under these bounds.
 *
 * A wait that passes returns the moment its condition holds (Testing Library
 * polls every 50 ms and on every DOM mutation), so a longer bound costs a
 * passing run nothing; it lengthens only a run that is failing anyway. A test
 * about a delay drives the clock it is about rather than leaning on these.
 */

/**
 * A wait's bound: an order of magnitude over the slowest wait measured under
 * that load. Below {@link CASE_BOUND_MS}, so a wait that never comes fails
 * with Testing Library's own error and the DOM it searched, rather than with
 * "Test timed out", which is a sentence about the runner and not the page.
 */
export const WAIT_BOUND_MS = 10_000;

/**
 * A case's bound: three waits at their bound, and an order of magnitude over
 * the slowest single case in the suite — the stylesheet scan in
 * `styles/classes.test.ts`, which walks every source file a character at a
 * time and measures 2-3 s idle and 5.5 s on a machine with other work on it.
 */
export const CASE_BOUND_MS = 30_000;
