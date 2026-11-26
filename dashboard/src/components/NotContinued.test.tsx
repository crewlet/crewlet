import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { NotContinuedCallout } from "./NotContinued.tsx";

afterEach(cleanup);

// EACH VERDICT IS SAID IN THE ENGINE'S SENTENCE, its detail behind a
// disclosure, and nothing is drawn where no session was lost: the callout a
// launch's card, a running executor and its turn all draw.
//
// Mutation: drop a verdict, put the detail in the sentence, or draw an empty
// callout, and a case goes red.
test("a session not continued is said in the engine's sentence, its detail behind a disclosure", () => {
  const { container } = render(<NotContinuedCallout verdicts={[]} />);
  expect(container.innerHTML).toBe("");
  render(
    <NotContinuedCallout
      verdicts={[
        { loss: "too_large", instead: "fresh", detail: "the archive is 50 MiB" },
        { loss: "capture_late", instead: "older", older: { launch_seq: 1, capture_seq: 2 } },
      ]}
    />,
  );
  expect(
    screen.getByText(
      "The run did not continue its session: its session was larger than one is kept at: 94 MiB in " +
        "all, at most 48 MiB of it text. It started a fresh session.",
    ),
  ).toBeDefined();
  expect(screen.getByText(/an older capture of its turn \(launch 1, capture 2\)/)).toBeDefined();
  expect(screen.getAllByRole("button", { name: /^Engine detail/ })).toHaveLength(1);
});
