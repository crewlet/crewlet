import { expect, test } from "vitest";
import { askStateWords } from "./askState.ts";

const when = (at: string) => `[${at}]`;

// AN ASK THE FACTS SETTLED SAYS WHY it no longer waits, before "still open".
test("a question's state is said, an overtaken one with why", () => {
  const ask = { comment: "c-1", asked_of: "jane", open: true };
  expect(askStateWords(ask, when)).toBe("The question it is about is still open.");
  expect(askStateWords({ ...ask, overtaken: "superseded" }, when)).toBe(
    "The question it is about no longer needs an answer: it was asked again since.",
  );
  expect(askStateWords({ ...ask, overtaken: "moot" }, when)).toBe(
    "The question it is about no longer needs an answer: its item is finished.",
  );
  expect(askStateWords({ ...ask, open: false, answered_at: "t1" }, when)).toBe(
    "The question it is about was answered at [t1].",
  );
  expect(askStateWords({ ...ask, open: false, resolved: true }, when)).toBe(
    "The question it is about was withdrawn by its asker.",
  );
});

// A WITHDRAWN QUESTION SAYS WHY, in its asker's words, rather than only closing.
test("a withdrawn question carries its asker's reason", () => {
  const ask = { comment: "c-1", asked_of: "jane", open: false, resolved: true };
  expect(
    askStateWords(
      {
        ...ask,
        answered_at: "t2",
        withdrawn: { reason: "Covered by the pull request.", reason_comment: "c-2" },
      },
      when,
    ),
  ).toBe(
    "The question it is about was withdrawn by its asker at [t2]: “Covered by the pull request.”",
  );
});
