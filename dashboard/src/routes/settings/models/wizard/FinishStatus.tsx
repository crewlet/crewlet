/**
 * Finish and save's progress and its failure, above the step it was pressed on:
 * the test call, then the engine's check, then the save, and what to fix when
 * one of them stops it. Nothing is stored on a failure.
 */

import { Callout, NoticeList } from "@crewlethq/ui";
import type { ConfigProblem } from "~/protocol/types.ts";
import type { WizardStep } from "../parts.tsx";
import { Waiting } from "../Waiting.tsx";
import { STEP_LABELS, fieldOf, messageOf, stepOf } from "./draft.ts";
import type { ReviewCheck, SaveStage } from "./steps/Review.tsx";
import type { TestState } from "./steps/TestStep.tsx";

/** Where Finish and save stands. */
export type Finish = { kind: "idle" } | { kind: "running" } | { kind: "failed" };

export function FinishStatus({
  finish,
  test,
  check,
  saving,
  problems,
  modelKey,
  secretNoun,
  pasted,
  editing,
  onTestStep,
  onGo,
}: {
  finish: Finish;
  test: TestState;
  check: ReviewCheck;
  saving: SaveStage | false;
  problems: readonly ConfigProblem[];
  modelKey: string;
  secretNoun: string;
  /** Whether a credential is pasted, which the test call tests. */
  pasted: boolean;
  editing: boolean;
  /** On the Test call step, whose own result already says what failed. */
  onTestStep: boolean;
  onGo: (step: WizardStep) => void;
}) {
  if (finish.kind === "running") {
    // The Test call step draws its own wait while the test runs.
    if (onTestStep && test.kind === "running" && !saving) return null;
    const words = saving
      ? saving === "credential"
        ? `Storing the ${secretNoun} and re-activating the company with it.`
        : `${editing ? "Saving" : "Creating"} the model as one revision.`
      : test.kind === "running"
        ? `Testing the ${pasted ? secretNoun : "model"} with one harness run. It usually answers in 5 to 20 seconds.`
        : "Checking the change with the engine.";
    return <Waiting key={saving || test.kind}>{words}</Waiting>;
  }
  if (finish.kind !== "failed") return null;
  if (test.kind === "failed" && onTestStep)
    return <Callout variant="danger" title="Not saved: the test call below failed" />;
  if (test.kind === "failed")
    return (
      <Callout
        variant="danger"
        title="Not saved: the test call failed"
        action={
          <button type="button" className="t-link" onClick={() => onGo("test")}>
            See the test call
          </button>
        }
      >
        {test.message}
      </Callout>
    );
  if (problems.length > 0)
    return (
      <Callout
        variant="danger"
        title={`Not saved: ${problems.length} ${problems.length === 1 ? "problem" : "problems"} to fix`}
      >
        <NoticeList
          notices={problems.map((p, i) => {
            const step = stepOf(p, modelKey);
            return {
              key: `${p.path}-${i}`,
              subject: fieldOf(p, modelKey),
              title: messageOf(p),
              action: (
                <button type="button" className="t-link" onClick={() => onGo(step)}>
                  Fix in {STEP_LABELS[step]}
                </button>
              ),
            };
          })}
        />
      </Callout>
    );
  if (check.kind === "blocked")
    return (
      <Callout variant="warning" title="Not saved">
        {check.message}
      </Callout>
    );
  return null;
}
