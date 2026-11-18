/**
 * One run of the harness with the draft's settings before anything is stored. A
 * pasted credential is tested by the credential route's dry run, alone in a
 * fresh home, and stored nowhere; a draft with nothing new to sign in with is
 * tested by the model's own check. Either costs a few tokens, and says so.
 *
 * A TEST BELONGS TO THE DRAFT IT RAN ON. A change to the entry or the
 * credential after it leaves the result on screen as history, and Review says
 * the test has not run on what will be saved.
 */

import { Button, Callout, Text } from "@crewlethq/ui";
import type { ModelSignInRoute } from "~/contract/models.ts";
import type { CheckAnswer, CredentialLimited } from "~/protocol/models.ts";
import { DoctorLines, type SignInFacts } from "../../flows/TestDialog.tsx";
import { Waiting } from "../../Waiting.tsx";
import { WINDOW } from "../../words.ts";
import type { Draft, Method } from "../draft.ts";

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

/** "Passed in 3.4 s", "Failed" or "Not run": the test as Review's row says it (design QA Minor 7). */
export function testWords(test: TestState, current: string): string {
  if (test.kind === "idle" || test.for !== current) return "Not run";
  if (test.kind === "running") return "Running";
  if (test.kind === "failed") return "Failed";
  const seconds = /in ([\d.]+) s/.exec(test.verified)?.[1];
  return seconds ? `Passed in ${seconds} s` : "Passed";
}

export function TestStep({
  draft,
  test,
  current,
  editing,
  signIn,
  pasted,
  onRun,
}: {
  draft: Draft;
  test: TestState;
  /** The draft as it stands, which a result belongs to or not. */
  current: string;
  editing: boolean;
  signIn: SignInFacts;
  /** Whether the draft carries a credential typed here, which its test signs in with alone. */
  pasted: boolean;
  onRun: () => void;
}) {
  const running = test.kind === "running";
  const stale = test.kind !== "idle" && test.kind !== "running" && test.for !== current;
  // A NEW MODEL ON A LOGIN made after it is saved cannot pass yet, and that is no fault.
  const signsInLater = !editing && (draft.method === "host-login" || draft.method === "device");
  return (
    <>
      <span>
        <Button variant="secondary" onClick={onRun} loading={running} disabled={!draft.key}>
          {test.kind === "idle" || running ? "Run a test call" : "Test again"}
        </Button>
      </span>
      {running ? (
        // WHAT THE WAIT IS, AND ITS REAL LENGTH: a pasted credential is one
        // harness run; a model with nothing new to sign in with gets its whole
        // check, which is three runs one after another.
        <Waiting>
          {pasted
            ? "Running the harness once to sign in with what you pasted. It usually answers in 5 to 20 seconds, and is given two minutes at most."
            : "Running the harness three times: one answer, then a check of its local tools and of its web access. Each usually takes 5 to 20 seconds, and the three are given three minutes at most."}
        </Waiting>
      ) : null}
      {stale ? (
        <Text as="p" variant="caption" tone="secondary">
          The settings changed since this test. Test again to try what will be saved.
        </Text>
      ) : null}
      {test.kind === "passed" ? (
        <>
          <Callout variant="success" title="It answers">
            {test.verified} Nothing is stored until you save.
            {test.limited
              ? ` The vendor is limiting it now (${WINDOW[test.limited.window]}), which shows the credential signs in.`
              : ""}
          </Callout>
          {test.check ? <DoctorLines lines={test.check.lines} signIn={signIn} /> : null}
        </>
      ) : null}
      {test.kind === "failed" ? (
        <>
          <Callout variant="warning" title="Not yet">
            {signsInLater
              ? "Expected: this model signs in after it is saved."
              : `${test.message} Fix what failed and test again: Finish and save needs a passing test.`}
          </Callout>
          {test.check ? <DoctorLines lines={test.check.lines} signIn={signIn} /> : null}
        </>
      ) : null}
    </>
  );
}
