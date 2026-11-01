/**
 * A collected coding run says what its push record moved on the engine's own
 * code host: a chip per ref, and the change that ref heads by its reference,
 * opening the change's page. The invariants: the change is named `web!42` and
 * links to its page; a ref heading no change names none; what the record
 * elided is counted; and a record that could not be read says why rather than
 * reading as nothing pushed.
 */

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, expect, test } from "vitest";
import { fromPhaseEvent } from "~/lib/phases.ts";
import { PushedRefs } from "./Runs.tsx";

afterEach(cleanup);

const ZERO = "0000000000000000000000000000000000000000";
const A = "1111111111111111111111111111111111111111";
const B = "2222222222222222222222222222222222222222";

/** A sandbox phase record as the engine publishes it, with its push record. */
function record(payload: Record<string, unknown>) {
  return fromPhaseEvent({
    id: "e1",
    type: "agent_phase_completed",
    created_at: "2026-10-31T10:00:00Z",
    payload: { phase: "sandbox", turn_id: "t1", ...payload },
  } as never)!;
}

// THE CHANGE A PUSHED REF HEADS IS NAMED BY ITS REFERENCE AND OPENS ITS PAGE,
// read off the record's `pushed_refs[].change`; a ref heading none names none,
// and what the record elided is counted. Mutation: drop the change link, link
// it anywhere but the change's page, or drop the elided count, and this fails.
test("a pushed ref names the change it heads, linking its page, and the elided refs are counted", () => {
  const r = record({
    pushed_refs: [
      {
        repo: "web",
        ref: "refs/heads/feature/login",
        old: ZERO,
        new: A,
        pushes: 2,
        change: "web!42",
      },
      { repo: "api", ref: "refs/heads/spike", old: A, new: B, pushes: 1 },
    ],
    pushed_refs_elided: 3,
  });
  render(<PushedRefs record={r} />);
  expect(screen.getByText("web:feature/login")).toBeTruthy();
  expect(screen.getByText("api:spike")).toBeTruthy();
  const links = screen.getAllByRole("link");
  expect(links.map((a) => [a.textContent, a.getAttribute("href")])).toEqual([
    ["web!42", "#/code/repos/web/changes/42"],
  ]);
  expect(screen.getByText("+3 more its record names")).toBeTruthy();
});

// A RECORD THAT COULD NOT BE READ IS NOT "NOTHING PUSHED": it says why, and
// draws no chip. Mutation: ignore `pushed_refs_unread`, and this fails.
test("a push record that could not be read says why", () => {
  const r = record({ pushed_refs_unread: "the code log did not answer" });
  render(<PushedRefs record={r} />);
  expect(screen.getByText(/could not be read: the code log did not answer/)).toBeTruthy();
  expect(screen.queryByRole("link")).toBeNull();
});
