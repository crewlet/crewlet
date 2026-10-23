/**
 * What a save that did not land says to the person who made it.
 *
 * ONE SENTENCE PER CAUSE, and a cause is named by what the person can do
 * about it: ask an administrator, change it where the company is managed,
 * wait for a node, or save again. The configuration surface's refusals are
 * read once (`protocol/configAnswer.ts`); this says them about one part of
 * the charter.
 *
 * SAVING AGAIN IS ALWAYS SAFE, which is why no sentence here sends anybody to
 * reload the page: every save reads what the company holds before it writes
 * (`save.ts`), so a second press after an unanswered one finds the first
 * landed and writes nothing, finds somebody else's words and says so, or
 * writes what never arrived.
 */

import { managedSentence, type ConfigRefusal } from "~/protocol/configAnswer.ts";
import { configGuardedReason } from "~/lib/useWriteAccess.ts";
import { partName, type CharterPart, type PartProblems } from "./charter.ts";
import type { SaveOutcome } from "./save.ts";

/** The outcomes that leave the draft where it was. */
export type Failure = Extract<SaveOutcome, { kind: "none" | "refused" | "lost" | "busy" }>;

/** What a failed save says. */
export function failureWords(outcome: Failure, part: CharterPart): string {
  const name = partName(part);
  switch (outcome.kind) {
    case "none":
      return "No company is configured, so there is nothing to save it to.";
    case "lost":
      return `The engine did not answer, and ${name} still reads as it did, so nothing was saved. Save again when the engine is back.`;
    case "busy":
      return "The company's configuration kept changing while this was saved, so nothing was stored. Save again in a moment.";
    case "refused":
      return refusalWords(outcome.refusal, name);
  }
}

function refusalWords(refusal: ConfigRefusal, name: string): string {
  switch (refusal.kind) {
    case "guarded":
      return configGuardedReason(refusal.code);
    case "managed":
      return managedSentence(refusal.managedBy);
    case "conflict":
      return "The company's configuration changed in a way this save cannot follow, so nothing was stored.";
    case "draining":
      return `This node is shutting down and stored nothing${refusal.detail ? `: ${sentence(refusal.detail)}` : "."} Save again once another node answers.`;
    case "starting":
      return "This node is still starting and stored nothing. Save again in a moment.";
    case "unavailable":
      return refusal.detail;
    case "unreachable":
      return `The engine did not answer, so ${name} may or may not have changed. Save again: the page checks what the company holds before it writes.`;
    case "problems":
      return (
        refusal.problems.map((p) => p.message).join(" ") || `The engine refused ${name} as written.`
      );
  }
}

/**
 * What the engine refused a save over, placed where it applies: a problem at
 * `["policies", 2]` under the third policy, one at the part itself under the
 * field, and null when the refusal is about nothing on this part (it is then
 * said as the failure's sentence, which names it whole).
 */
export function refusedProblems(outcome: Failure, part: CharterPart): PartProblems | null {
  if (outcome.kind !== "refused" || outcome.refusal.kind !== "problems") return null;
  const items: (string | undefined)[] = [];
  const field: string[] = [];
  for (const problem of outcome.refusal.problems) {
    const [head, index] = problem.segments ?? [];
    if (head !== part) continue;
    if (typeof index === "number" && part === "policies") {
      items[index] = items[index] ? `${items[index]} ${problem.message}` : problem.message;
    } else {
      field.push(problem.message);
    }
  }
  if (field.length === 0 && items.length === 0) return null;
  return {
    ...(field.length > 0 ? { field: field.join(" ") } : {}),
    ...(items.length > 0 ? { items: Array.from(items) } : {}),
  };
}

/** A detail the engine wrote, ended with a full stop when it has none. */
function sentence(detail: string): string {
  const text = detail.trim();
  return /[.!?]$/.test(text) ? text : `${text}.`;
}
