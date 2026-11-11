import { expect, test } from "vitest";
import { holdingWaiting } from "./workGrid.tsx";
import type { WorkHoldingRow } from "~/protocol/types.ts";

const row = (over: Partial<WorkHoldingRow>): WorkHoldingRow =>
  ({
    id: "t-1",
    key: "ACME-12",
    title: "Ship the release",
    blocks: 2,
    holders: ["pm", "qa"],
    held_since: "2026-11-01T00:00:00Z",
    agent_waiting: 2,
    ...over,
  }) as WorkHoldingRow;

// A HELD ROW SAYS WHAT WAITS ON IT (ADR-0124): the person's answer where a
// question holds it, then what waits behind it, how many agents hold, and who.
//
// Mutation: drop `asked` from the words and a row held by a question reads as
// one assigned to the person.
test("a held row's waiting words name the answer, the agent items and who", () => {
  const name = (h: string) => h.toUpperCase();
  expect(holdingWaiting(row({}), name)).toBe("2 items (2 agent items), held by PM, QA");
  expect(holdingWaiting(row({ asked: "c-1", agent_waiting: 3 }), name)).toBe(
    "Your answer, then 2 items (3 agent items), held by PM, QA",
  );
  expect(
    holdingWaiting(row({ asked: "c-1", blocks: 0, agent_waiting: 1, holders: ["pm"] }), name),
  ).toBe("Your answer (1 agent item), held by PM");
  expect(holdingWaiting(row({ agent_waiting: 0, blocks_capped: true }), name)).toBe(
    "2 items or more, held by PM, QA",
  );
});
