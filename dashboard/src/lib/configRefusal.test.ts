/**
 * A refused write of the company document, as a person is told it: the
 * engine's words for the refusal, and never its advice to a program calling
 * the API.
 */

import { expect, test } from "vitest";
import type { ConfigRefusal } from "~/protocol/configAnswer.ts";
import { configRefusalWords } from "./configRefusal.ts";

const REREAD = "Reload to see the configuration as it is now.";

// THE API'S HINT IS NEVER A PERSON'S. A revert refused for a seat an open
// invitation holds told the person who pressed it to send `PATCH
// /iam/people/{person}` and `DELETE /iam/invitations/{id}`, and a refused
// limit shouted "the WHOLE document a write produces is validated" at them:
// both were the answer's `hint`, written for a program, drawn after the
// message. The problems say what is wrong; the screen says what to do.
test("a problems refusal whose answer carries a hint naming a route draws no route", () => {
  const refusal: ConfigRefusal = {
    kind: "problems",
    problems: [
      {
        path: "roles[2].handle",
        segments: ["roles", 2, "handle"],
        kind: "conflict",
        message: "The seat @new-hire is held by an open invitation.",
      },
    ],
    derived: null,
    code: "seat_held",
    hint:
      "move each person to another human seat (PATCH /iam/people/{person} with seat) or " +
      "remove them, cancel an open invitation (DELETE /iam/invitations/{id}), then send the write again",
  };
  const words = configRefusalWords(refusal, REREAD);
  expect(words).toEqual({
    message: "The seat @new-hire is held by an open invitation.",
    reload: false,
  });
  expect(JSON.stringify(words)).not.toMatch(/PATCH|DELETE|\/iam\//);
});

// A PERSON'S WORDS WHERE THE RULE WROTE THEM, the operator's line otherwise.
test("a problem's own words for a person are said in place of its line for an operator", () => {
  const refusal: ConfigRefusal = {
    kind: "problems",
    problems: [
      {
        path: "turn_engine.delegation.task_timeout_seconds",
        segments: ["turn_engine", "delegation", "task_timeout_seconds"],
        kind: "conflict",
        message:
          "turn_engine.delegation.task_timeout_seconds: conflict: is 1000, above call_timeout_seconds (900)",
        said: "One task’s time limit (1000 seconds) can’t be longer than one call’s (15 minutes): a worker would outlive the call waiting for it.",
      },
    ],
    derived: null,
    code: "validation_error",
    hint: "The whole company is checked on every change.",
  };
  expect(configRefusalWords(refusal, REREAD).message).toBe(refusal.problems[0]!.said);
});
