import { firstSentence } from "./format.ts";
import { decisionTexts } from "./work.ts";
import type { SandboxRun, WorkDecision } from "~/protocol/index.ts";

/**
 * How often the durable coding-run record is asked again.
 *
 * ONE VALUE for every reader of `sandbox_runs` — the runs board, a run's own
 * page and rail, and the attention queue Home and the Inbox draw. It was two:
 * the board polled every 20 s and the queue every 30 s, so a run that parked
 * on a question appeared on the board and then, up to ten seconds later, as
 * the decision the Inbox asks a person to make — two screens disagreeing about
 * whether anybody was being waited on.
 *
 * TWENTY SECONDS, because nothing pushes this record: a run's status moves on
 * its own row in the coordination store and the live projection carries only
 * what a running-runs panel draws. A run lives for minutes and a parked one
 * waits for a person, who is not answering inside twenty seconds either; the
 * live projection reconciles its own panel against the same record every
 * 30 s, so a shorter poll here would be reading a record that has not moved.
 */
export const RUNS_POLL_MS = 20_000;

/**
 * What a parked run asks, as a reader reads it: a title of one sentence, the
 * Markdown context under it, and the options it offers where it offers any.
 *
 * A RUN ASKS THROUGH A STRUCTURED TOOL, which keeps the question to one
 * sentence and its context and options apart, so each is the run's own.
 *
 * A ROW FROM AN OLDER BUILD carries one free-text question, often several
 * paragraphs long. Its first sentence is the title and the rest its context,
 * read as Markdown like any other: drawn whole it was a wall of verbatim text
 * under a heading that repeated its start.
 */
export interface RunAsk {
  title: string;
  /** Markdown, or "" where the run said nothing beside its question. */
  context: string;
  decision: WorkDecision | undefined;
}

export function runAsk(
  run: Pick<SandboxRun, "question" | "question_context" | "decision">,
): RunAsk {
  const question = (run.question ?? "").trim();
  const decision = run.decision;
  if (run.question_context !== undefined || decision) {
    return {
      title: question || decision?.question || "",
      context: (run.question_context ?? "").trim(),
      decision,
    };
  }
  // THE TITLE IS A PREFIX of the trimmed question (`firstSentence` reads its
  // first line), so the rest is what follows it.
  const title = firstSentence(question);
  return { title, context: question.slice(title.length).trim(), decision: undefined };
}

/**
 * Every text of a run's question a reader reads as one message, as
 * `askTexts` is an ask's: a bare `#11` in any of them is placed by what the
 * others name (`TokenScope`).
 */
export function runAskTexts(ask: RunAsk): string[] {
  return [ask.title, ask.context, ...decisionTexts(ask.decision)].filter(
    (text) => text.trim() !== "",
  );
}
