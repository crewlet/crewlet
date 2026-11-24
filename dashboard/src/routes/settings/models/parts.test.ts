/**
 * The wizard's address: what an action opens it at reads back from the URL
 * whole, and anything the page does not know is dropped rather than trusted.
 */

import { expect, test } from "vitest";
import { wizardAtOf, wizardQuery, type WizardAt } from "./parts.tsx";

const back = (at: WizardAt) => {
  const q = wizardQuery(at);
  return wizardAtOf({ step: q.step ?? "", focus: q.focus ?? "", add: q.add ?? "" });
};

test("every use an action adds reads back from the address, in its order", () => {
  const at: WizardAt = {
    step: "review",
    add: [
      { job: "coding", seat: "backend-engineer" },
      { job: "routines", seat: "qa-2" },
      { job: "assistant" },
    ],
  };
  expect(wizardQuery(at).add).toBe("coding:backend-engineer,routines:qa-2,assistant");
  expect(back(at)).toEqual(at);
  expect(back({ step: "signin", focus: "credential" })).toEqual({
    step: "signin",
    focus: "credential",
  });
});

test("a hand-edited address keeps only what the page knows", () => {
  expect(wizardAtOf({ step: "nowhere", focus: "", add: "coding:pm" })).toBeUndefined();
  // USE IT FOR IS NO STEP ANY MORE: an old link to it opens nothing it cannot draw.
  expect(wizardAtOf({ step: "assign", focus: "", add: "coding:pm" })).toBeUndefined();
  expect(
    wizardAtOf({
      step: "review",
      focus: "everything",
      add: "coding:pm,deep_work:pm,coding:Not A Handle,reviews:,assistant,coding:qa:x",
    }),
  ).toEqual({ step: "review", add: [{ job: "coding", seat: "pm" }, { job: "assistant" }] });
  expect(wizardAtOf({ step: "review", focus: "", add: "nothing" })).toEqual({ step: "review" });
});
