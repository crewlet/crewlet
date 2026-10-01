/**
 * WHAT A CASE WRITES TO THE CONSOLE IS A FAILURE OF THAT CASE. Imported by the
 * suite's setup file, which watches every case, and by this module's own
 * suite.
 *
 * # Why a warning cannot be left as a line in the log
 *
 * Every warning the suite printed was a defect somewhere, and every one of
 * them was printed by a case that passed: a duplicate list key in the search
 * grid's fixture, eight `flushSync` warnings from a stand-in observer that
 * reported mid-commit, an announcement made with no live region mounted —
 * which was the APPLICATION mounting none — and an update-depth warning that
 * was the audit asking the engine once a second. Vitest does not even show a
 * passing case's console output by default, so a warning that read as a
 * defect to anybody who saw it was seen by nobody, and the suite reported a
 * pass over all of them. The same reasoning as `internal/skipgate` on the Go
 * side: a pass that something is quietly wrong under is not a pass.
 *
 * # What counts
 *
 * `console.error` and `console.warn`, which is where React, the design system
 * and this application put what they say is wrong. A case that EXPECTS one —
 * it renders a component that throws, to hold the throw — silences it itself
 * with `vi.spyOn(console, "error").mockImplementation(...)`, which replaces
 * the watcher for that call and so records nothing; what it did not expect
 * still fails it. The method is put back as it was when the case ends, so one
 * case's silencing never reaches the next.
 */

import { format } from "node:util";

/** The two levels a case may not write to unasked. */
const LEVELS = ["error", "warn"] as const;

/** The part of a console this watches: what `console` is in production. */
type Watched = Pick<Console, (typeof LEVELS)[number]>;

/**
 * Records every call to `target.error` and `target.warn` from now, still
 * passing each one through, and returns how to stop: stopping puts both
 * methods back exactly as they were and hands back what was said, one line
 * per call, formatted as the console would print it.
 */
export function watchConsole(target: Watched = console): () => string[] {
  const said: string[] = [];
  const kept = LEVELS.map((level) => {
    const original = target[level];
    target[level] = (...args: unknown[]) => {
      said.push(`console.${level}: ${format(...args)}`);
      original.apply(target, args);
    };
    return { level, original };
  });
  return () => {
    for (const { level, original } of kept) target[level] = original;
    return said;
  };
}

/** One test file's watch, cut into the spans its cases ran in and the rest. */
export interface FileWatch {
  /** A case begins: what was said since the last case ended is the file's. */
  caseStarts(): void;
  /** A case ends: hands back what was said while it ran. */
  caseEnds(): string[];
  /** The file ends: stops watching and hands back everything said outside its cases. */
  fileEnds(): string[];
}

/**
 * Watches `target` from now until the file ends, and says which of what it
 * heard was said inside a case and which outside every case.
 *
 * OUTSIDE A CASE IS NOT OUTSIDE THE RULE. A file says things while its modules
 * load, in a `beforeAll` (a stylesheet jsdom cannot parse is a console error
 * there), in an `afterAll`, and after its last case, and a watch that began at
 * each case's start and ended at its end passed every one of those over in
 * silence. No case can be blamed for them, so the FILE is: each case's span is
 * cut out and judged on its own, and what is left is the file's.
 *
 * Every cut puts both methods back and watches them afresh, so a case that
 * silenced a call and never restored it leaves no silence behind it.
 */
export function watchFile(target: Watched = console): FileWatch {
  let watching = watchConsole(target);
  const outside: string[] = [];
  const cut = () => {
    const said = watching();
    watching = watchConsole(target);
    return said;
  };
  return {
    caseStarts: () => {
      outside.push(...cut());
    },
    caseEnds: cut,
    fileEnds: () => {
      outside.push(...watching());
      watching = () => [];
      return outside;
    },
  };
}
