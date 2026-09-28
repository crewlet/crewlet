/**
 * The one reading of where a running turn is — shared by Home's Live now and a
 * seat's Current turn card, so two rows can never disagree about one seat.
 */

import { expect, test } from "vitest";

import { roundOf } from "./seats.ts";
import { callWords, lastCallLine, turnSteps } from "./turnsteps.ts";
import type { AgentRow, LiveCall } from "~/protocol/index.ts";

const row = (call: Partial<LiveCall> | null, over: Partial<AgentRow> = {}): AgentRow =>
  ({
    role: "SWE",
    activity: "working",
    live_call: call
      ? { turn_id: "t", phase: "execute", round_num: 0, rounds_used: 0, ...call }
      : null,
    ...over,
  }) as unknown as AgentRow;

// `round_num` IS ZERO-BASED and `rounds_used` is not: read raw, the round in
// flight was named one lower than it is.
test("the round is one-based, whichever counter is ahead", () => {
  expect(roundOf({ round_num: 6, rounds_used: 6 } as LiveCall)).toBe(7);
  expect(roundOf({ round_num: -1, rounds_used: 3 } as LiveCall)).toBe(3);
  expect(roundOf(null)).toBe(0);
});

test("execute carries its round against the cap the phase was granted", () => {
  const { steps, current } = turnSteps(row({ round_num: 6, rounds_used: 6, max_rounds: 25 }));
  expect(current).toBe("execute");
  expect(steps.map((s) => s.label)).toEqual(["Context", "Execute · round 7 of 25", "Review"]);
});

// SAID ONCE, WHERE THERE IS ROOM FOR IT: a phone's card takes the round out
// of the step and says it beside the stepper, and the step is plain.
test("the round can leave the step's label and is still said once", () => {
  const compact = turnSteps(row({ round_num: 6, rounds_used: 6, max_rounds: 25 }), {
    inLabel: false,
  });
  expect(compact.steps.map((s) => s.label)).toEqual(["Context", "Execute", "Review"]);
  expect(compact.detail).toBe("round 7 of 25");
  expect(turnSteps(row({ phase: "review" })).detail).toBe("");
});

// THERE IS NO DELIVERY STEP: whether a turn reached anybody is decided as it
// closes, and a step that could never be current is a promise nothing keeps.
test("a turn walks three steps, and a parked one is in its coding run", () => {
  const parked = turnSteps(row(null, { turn: { turn_id: "t", started_at: "", stage: "parked" } }));
  expect(parked.current).toBe("execute");
  expect(parked.steps.map((s) => s.label)).toEqual(["Context", "Execute · coding run", "Review"]);
  const onboarding = turnSteps(row({ phase: "onboarding" }));
  expect(onboarding.current).toBe("context");
  expect(onboarding.steps[0]?.label).toBe("Onboarding");
  expect(turnSteps(row({ phase: "review" })).current).toBe("review");
});

test("a call is read by its arguments' values, the one running first", () => {
  expect(callWords('{"cmd":"go test ./..."}')).toBe("go test ./...");
  expect(callWords("not json")).toBe("not json");
  // WORDS, NOT JSON AND NOT BARE VALUES: "me true" placed nothing. The
  // subject leads, bare; every other argument carries its name; a set flag is
  // its name and an unset one is nothing.
  expect(callWords({ assignee: "me", open: true, include_done: false })).toBe("assignee me · open");
  expect(callWords({ body: "Picked this up.\n\nFirst pass", key: "ENG-38" })).toBe(
    "ENG-38 · Picked this up. First pass",
  );
  expect(callWords({ query: "GPU scheduling", limit: 5 })).toBe("GPU scheduling · limit 5");
  expect(callWords({ labels: ["infra", "p1"], filter: { a: 1 } })).toBe(
    "labels infra, p1 · filter",
  );
  expect(callWords({})).toBe("");
  expect(
    lastCallLine(
      row({
        tool_executions: [{ name: "gitlab.get_file", arguments: { path: "a.go" } }],
        running_call: {
          round: 2,
          name: "sandbox.run",
          arguments: '{"cmd":"make"}',
          started_at: "",
        },
      }),
    ),
  ).toBe("sandbox.run make");
});
