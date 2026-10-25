// @vitest-environment node

/**
 * Every wire state the assistant draws, mapped to exactly one state of the
 * design system's component that draws it.
 *
 * THE MAPS ARE RECORDS OVER THE WIRE'S UNIONS, so a member the engine adds is
 * a compile error until it is drawn; this holds each value to the
 * component's OWN list of states, so a component state that is renamed or
 * removed fails here rather than drawing nothing. And it pins the two rules
 * the mapping keeps: an `unknown` proposal is the one failed proposal that
 * offers Retry, and a step-up is the dashboard's dialog, never a failure.
 */

import { describe, expect, test } from "vitest";
import { PROPOSAL_STATES, RUN_STATES, STEP_STATES } from "@crewlethq/ui";
import {
  CHANGE_KIND,
  PROPOSAL_STATE,
  RUN_STATE,
  STEP_STATE,
  WORD_OP,
  changeRowsOf,
  runChipOf,
} from "./blocks.tsx";
import type { TurnView } from "./turn.ts";

describe("each wire state is exactly one component state", () => {
  test("a step's state", () => {
    for (const [wire, kit] of Object.entries(STEP_STATE)) {
      expect(STEP_STATES, wire).toContain(kit);
    }
    expect(Object.keys(STEP_STATE).sort()).toEqual(
      ["done", "failed", "refused", "running", "stopped", "waiting"].sort(),
    );
  });

  test("a turn's phase", () => {
    for (const [phase, kit] of Object.entries(RUN_STATE)) expect(RUN_STATES, phase).toContain(kit);
  });

  test("a proposal's state, with unknown the one failed state that offers Retry", () => {
    for (const [wire, kit] of Object.entries(PROPOSAL_STATE)) {
      expect(PROPOSAL_STATES, wire).toContain(kit);
    }
    expect(PROPOSAL_STATE.proposed).toBe("pending");
    expect(PROPOSAL_STATE.unknown).toBe("failed");
    // NO WIRE STATE IS A STEP-UP: the engine answers an approval that needs
    // a fresh proof with 403 step_up_required, which the dashboard's own
    // dialog confirms before the approval is sent again.
    expect(Object.values(PROPOSAL_STATE)).not.toContain("stepup");
  });

  test("a change's kind and a word's op", () => {
    expect(CHANGE_KIND).toEqual({
      fact: "fact",
      changed: "changed",
      added: "added",
      removed: "removed",
      text: "text",
    });
    expect(WORD_OP).toEqual({ same: "=", added: "+", removed: "-" });
  });
});

describe("the run chip says the turn's state in words", () => {
  const base: TurnView = {
    key: "k",
    id: "t",
    phase: "working",
    seq: 0,
    question: "q",
    page: null,
    askedAt: "2026-10-24T09:41:00Z",
    stepsDone: 0,
    steps: [],
    replies: [],
    cards: [],
    proposals: {},
    actions: [],
    actionStates: {},
    done: null,
    error: null,
    refusal: null,
    stopping: false,
  };
  const at = Date.parse("2026-10-24T09:41:03Z");

  test("working, with the time it has run", () => {
    expect(runChipOf(base, at)).toEqual({ state: "working", title: "Working", meta: "3s" });
  });

  test("done, with its duration and its steps", () => {
    const done = {
      ...base,
      phase: "completed" as const,
      done: { state: "completed" as const, duration_ms: 5000, steps: 2 },
    };
    expect(runChipOf(done, at)).toEqual({ state: "done", title: "Done in 5s", meta: "2 steps" });
  });

  test("done, with no step count when it took no step", () => {
    const done = {
      ...base,
      phase: "completed" as const,
      done: { state: "completed" as const, duration_ms: 1000, steps: 0 },
    };
    expect(runChipOf(done, at)).toEqual({ state: "done", title: "Done in 1s", meta: undefined });
  });

  test("done, read back from a transcript, says no duration it does not know", () => {
    const reloaded = { ...base, phase: "completed" as const, askedAt: "2026-10-24T09:00:00Z" };
    expect(runChipOf(reloaded, at).title).toBe("Done");
  });

  test("waiting for the person's approval", () => {
    expect(runChipOf({ ...base, phase: "awaiting_approval" }, at).title).toBe(
      "Waiting for your approval",
    );
  });

  test("a turn that failed on a refused step says Not permitted rather than failed", () => {
    const refused = {
      ...base,
      phase: "failed" as const,
      steps: [
        {
          step_id: "s",
          index: 2,
          tool: "create_work_item",
          label: "File a task",
          state: "refused" as const,
          started_at: "2026-10-24T09:41:01Z",
        },
      ],
    };
    expect(runChipOf(refused, at)).toMatchObject({ state: "refused", title: "Not permitted" });
  });
});

describe("a proposal's change rows", () => {
  const base = {
    proposal_id: "p",
    scope: "server" as const,
    tool: "share_artifact",
    title: "Share artifact",
    digest: `sha256:${"a".repeat(64)}`,
    expires_at: "2026-10-24T10:41:00Z",
    state: "proposed" as const,
    approve_label: "Share",
    changes: [
      { kind: "fact" as const, field: "artifact", label: "Artifact", to: "Q4 pricing brief" },
      {
        kind: "changed" as const,
        field: "readers",
        label: "Readers",
        from: "Product",
        to: "Everyone",
      },
    ],
  };

  test("say who will see the result as a fact, after the facts and before the changes (D09)", () => {
    const rows = changeRowsOf({ ...base, audience: ["Product Management", "Everyone at Nimbus"] });
    expect(rows.map((r) => r.label)).toEqual(["Artifact", "Who will see it", "Readers"]);
    expect(rows[1]).toEqual({
      kind: "fact",
      label: "Who will see it",
      value: "Product Management, Everyone at Nimbus",
    });
  });

  test("carry no audience row where the visibility does not change", () => {
    expect(changeRowsOf(base).map((r) => r.label)).toEqual(["Artifact", "Readers"]);
  });
});
