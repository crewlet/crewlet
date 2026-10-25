/**
 * The console is part of a case's result.
 *
 * React reports a broken contract to `console.error` and carries on — a list
 * child with no key, two children under one key, a paragraph inside a
 * paragraph, an `act` that was not awaited, a component that suspended inside
 * one — and the dashboard reports a sentence nobody heard to `console.warn`
 * (`announce()` with no announcer mounted). Every one of those is a bug the
 * case drew straight past, and Vitest's default reporter prints nothing a
 * PASSING case wrote: a run read with a second reporter found them in dozens
 * of green cases, beside the reports the boundaries' own cases provoke on
 * purpose.
 *
 * So whatever a case writes to `console.error` or `console.warn` fails it,
 * after the case, quoting what was said ([verdict], run by `setup.ts` after
 * every case). A case that provokes one on purpose says so with
 * [expectConsole] — a boundary's case expects React's report of the error the
 * boundary caught — and the declaration is held to happening: one that nothing
 * meets fails the case too, so a declaration cannot outlive the behaviour it
 * excused. `console.log` and the rest are not judged; a case may print what it
 * likes.
 *
 * A PLAIN WRAPPER, NOT A SPY, installed once per file: a case that spies the
 * console for itself (`vi.spyOn(console, "error")`) wraps this one, sees every
 * call first and decides what reaches it, and its `mockRestore` puts this one
 * back — where a spy here would be the same spy as the case's, and the case's
 * restore would take the gate away for the rest of the file.
 */

/** The two streams a case is judged by. */
export type ConsoleLevel = "error" | "warn";

interface Said {
  level: ConsoleLevel;
  text: string;
  /** The case running when it was said, or none between cases. */
  during: string | undefined;
}

interface Expected {
  level: ConsoleLevel;
  pattern: RegExp;
  met: number;
}

const said: Said[] = [];
let expected: Expected[] = [];
let given = 0;

/**
 * Declare that the running case makes the console say something matching
 * `pattern` on `level`, at least once: every such line is taken as the case's
 * own and not printed, and if nothing matches by the case's end the case
 * fails.
 */
export function expectConsole(level: ConsoleLevel, pattern: RegExp): void {
  expected.push({ level, pattern, met: 0 });
}

/** One argument as the console would print it, an error as its name and message. */
function argText(arg: unknown): string {
  if (arg instanceof Error) return `${arg.name}: ${arg.message}`;
  if (typeof arg === "string") return arg;
  try {
    return JSON.stringify(arg) ?? String(arg);
  } catch {
    return String(arg);
  }
}

/**
 * One console call's arguments as the line a reader would see — WITH ITS
 * FORMAT STRING FILLED IN, the way the console prints it: React writes every
 * report as one (`"%o\n\n%s\n\n%s\n"`, the error, then its sentences), and
 * quoted unfilled, a failure said only "%o / %s" about a case that had drawn
 * a refused child.
 */
export function consoleText(args: readonly unknown[]): string {
  const [first, ...rest] = args;
  if (typeof first !== "string") return args.map(argText).join(" ");
  const left = [...rest];
  const head = first.replace(/%([sdifoOc%])/g, (whole, directive: string) => {
    if (directive === "%") return "%";
    if (left.length === 0) return whole;
    const arg = left.shift();
    // `%c` is a style for the text after it, which a line of text has no use for.
    return directive === "c" ? "" : argText(arg);
  });
  return [head, ...left.map(argText)].join(" ");
}

/**
 * A console call, judged: true when a declaration of the running case covers
 * it (the caller prints nothing), false when it is the case's failure (the
 * caller prints it, so the run shows it beside the failure).
 */
export function record(level: ConsoleLevel, args: readonly unknown[], during?: string): boolean {
  const text = consoleText(args);
  const declared = expected.find((e) => e.level === level && e.pattern.test(text));
  if (declared) {
    declared.met++;
    return true;
  }
  said.push({ level, text, during });
  return false;
}

/**
 * What the case just ended left unanswered — everything said that no
 * declaration covered, and every declaration nothing met — as sentences, and
 * a fresh start for the next case.
 */
export function verdict(): string[] {
  const problems = [
    ...said.map(
      (s) =>
        `console.${s.level} said ${s.during ? `during "${s.during}"` : "between cases"}, ` +
        `and nothing declared it: ${s.text
          .split("\n")
          .filter((line) => line.trim() !== "")
          .slice(0, 3)
          .join(" / ")}`,
    ),
    ...expected
      .filter((e) => e.met === 0)
      .map((e) => `console.${e.level} was expected to say ${String(e.pattern)}, and did not`),
  ];
  said.length = 0;
  expected = [];
  given++;
  return problems;
}

/**
 * How many verdicts have been given in this file — what lets a case see that
 * `setup.ts` judged the one before it, which no case could see from inside
 * itself.
 */
export function verdictsGiven(): number {
  return given;
}

/**
 * Put the gate in front of the console's two judged streams. `during` names
 * the case running at the moment of a call.
 */
export function installConsoleGate(during: () => string | undefined): void {
  for (const level of ["error", "warn"] as const) {
    const original = console[level].bind(console);
    console[level] = (...args: unknown[]) => {
      if (!record(level, args, during())) original(...args);
    };
  }
}
