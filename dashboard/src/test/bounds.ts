/**
 * How long a suite waits before it calls something hung: one bound for a
 * wait (`findBy…`, `waitFor`, and Vitest's own wait through `eventually`),
 * set in `setup.ts` and `settle.ts`, and one each for a whole case and a hook,
 * set in `vitest.config.ts`. `environment.test.tsx` holds all three to what
 * they produce, and `budgets.test.ts` holds every suite to them: no case, hook
 * or wait names a budget of its own.
 *
 * ALL THREE ARE HANG DETECTORS, NEVER ASSERTIONS ABOUT SPEED. What a case costs
 * is CPU work — jsdom rendering a screen, React committing it — so its
 * duration is that work over the share of a CPU its worker is given, and a
 * gate runs this suite's parallel workers on machines it shares, beside the
 * engine's Go suites or another checkout's gate. The tools' own defaults, a
 * second for a wait, five for a case and ten for a hook, were chosen by nobody
 * here and held that work to a wall-clock figure anyway.
 *
 * EACH IS SIZED FROM MEASUREMENT, at no less than twice the worst it measured,
 * rounded up to a whole second for the wait and a whole five for the case and
 * the hook (M3-O78). The measurement is one run of the whole suite under the
 * load a contributor's `make check` puts beside it — `go build ./...` and
 * `go vet ./...` looping on a fresh GOCACHE on the same four cores (a runner
 * slot of four E-cores) — with every bound lifted and every case, hook and
 * wait timed on the real clock. An earlier measurement under a heavier load
 * set the wait's ten seconds, and stands where it asks for more: the org
 * builder's first check, mount to "No problems", took about 150 ms idle and
 * 460 to 1081 ms with fourteen busy processes beside the suite on four CPUs,
 * the same two requests every time, only slower renders.
 *
 * A wait that passes returns the moment its condition holds (Testing Library
 * polls every 50 ms and on every DOM mutation), so a longer bound costs a
 * passing run nothing; it lengthens only a run that is failing anyway. A test
 * about a delay drives the clock it is about rather than leaning on these.
 */

/**
 * A wait's bound. The worst wait measured took 3.55 s: the Audit log drawing a
 * source whose page of 200 events filled, every row's date written by the
 * design system's own cell. Six of the 2458 waits took over a second, and
 * none of Vitest's own over a tenth of one. Twice the worst is 8 s; ten keeps
 * the heavier machine's margin. Below {@link CASE_BOUND_MS}, so a wait that
 * never comes fails with Testing Library's own error and the DOM it searched,
 * rather than with "Test timed out", which is a sentence about the runner and
 * not the page.
 */
export const WAIT_BOUND_MS = 10_000;

/**
 * A case's bound: three waits at their bound, since a case holds several. The
 * worst case measured took 7.38 s, App.test.tsx drawing every destination of
 * the navigation in turn; 15 of the 6512 cases took over three seconds, and
 * the stylesheet scan in `styles/classes.test.ts`, once the slowest at 5.5 s,
 * 1.6 s. Twice the worst is 15 s, inside the three waits.
 */
export const CASE_BOUND_MS = 30_000;

/**
 * A hook's bound. The worst hook measured took 2.51 s: App.session.test.tsx's
 * `beforeAll` importing every screen's chunk, which the other suites that load
 * them all do in 2.2 s and less. Twice it, rounded up to a whole five, is
 * 10 s. Set rather than left to Vitest's default of the same figure, which
 * nobody here measured and a release may move.
 */
export const HOOK_BOUND_MS = 10_000;
