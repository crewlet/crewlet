/**
 * The test call: one run of the harness with the draft's settings before
 * anything is stored, run from Review (or by Finish and save) and said there
 * in one line. A pasted credential is tested by the credential route's dry
 * run, alone in a fresh home, and stored nowhere; a draft with nothing new to
 * sign in with is tested by the model's own check.
 *
 * A TEST BELONGS TO THE DRAFT IT RAN ON. A change to the entry or the
 * credential after it reads "Not run" again, since the test has not run on
 * what will be saved.
 */

import type { ModelSignInRoute } from "~/contract/models.ts";
import type { CheckAnswer, CredentialLimited } from "~/protocol/models.ts";
import type { Method } from "../draft.ts";

/** Where the draft's test is: not run, running, or its answer for the draft it ran on. */
export type TestState =
  | { kind: "idle" }
  | { kind: "running"; for: string }
  | {
      kind: "passed";
      for: string;
      /** The engine's sentence: "Answered a one-turn Claude Code run in 3.4 s with only this value." */
      verified: string;
      check: CheckAnswer | null;
      limited: CredentialLimited | null;
    }
  | { kind: "failed"; for: string; message: string; check: CheckAnswer | null };

/** How a draft's method signs in, as the engine names the route a test reports on. */
export function routeOf(method: Method, fallback: ModelSignInRoute): ModelSignInRoute {
  switch (method) {
    case "api-key":
      return "api-key";
    case "token":
      return "token";
    case "host-login":
    case "device":
      return "credentials";
    case "cloud":
      return "env";
    case "unchanged":
      return fallback;
  }
}

/**
 * The test in one line, as Review's row says it (design QA Minor 7): "Passed
 * in 3.4 s", "Failed: <why>", "Running" or "Not run".
 */
export function testWords(test: TestState, current: string): string {
  if (test.kind === "idle" || test.for !== current) return "Not run";
  if (test.kind === "running") return "Running";
  if (test.kind === "failed") return `Failed: ${personWords(test.message)}`;
  const seconds = /in ([\d.]+) s/.exec(test.verified)?.[1];
  return seconds ? `Passed in ${seconds} s` : "Passed";
}

/**
 * A failure's sentences in a person's words, up to the first that reads as
 * the engine's own chain ("failed: llm harness/claude-code/haiku: auth: ..."),
 * which the row keeps for its hover. A remedy written for the person stays.
 */
export function personWords(message: string): string {
  const sentences = message.trim().match(/[^.!?]+[.!?]*(\s+|$)/g) ?? [message];
  const chain = /\bfailed:|\w+\/[\w-]+\/|\w: \w+:|·/;
  const kept: string[] = [];
  for (const sentence of sentences) {
    if (chain.test(sentence)) break;
    kept.push(sentence.trim());
  }
  return (kept.length > 0 ? kept : [sentences[0]!.trim()]).join(" ");
}
