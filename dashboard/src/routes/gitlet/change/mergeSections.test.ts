// @vitest-environment node
/**
 * The merge box's sections: every condition the meter counts lands in one
 * section, in the box's order, and each disc wears the meter's tone.
 */
import { describe, expect, test } from "vitest";

import type { CodePrecondition, CodeReadiness } from "~/protocol/index.ts";
import {
  boxTone,
  fastForwardHeld,
  measuredBeforeMove,
  mergeSections,
  reRequestable,
  reviewerStandings,
  sectionTone,
} from "./mergeSections.ts";

const pre = (
  kind: string,
  state = "met",
  extra: Partial<CodePrecondition> = {},
): CodePrecondition => ({
  kind,
  state,
  detail: "",
  ...extra,
});
const readiness = (...preconditions: CodePrecondition[]): CodeReadiness => ({
  met: preconditions.filter((p) => p.state === "met").length,
  total: preconditions.length,
  first: "",
  preconditions,
});

describe("the merge box's sections", () => {
  // EVERY CONDITION IN ONE SECTION, the five in order and the rest after them.
  // Mutation: drop the other sections, or put a kind in two, and this fails.
  test("each kind lands in its section, and a kind none holds gets its own after them", () => {
    const sections = mergeSections(
      readiness(
        pre("mergeable"),
        pre("queue", "unmet"),
        pre("approval", "unmet"),
        pre("owners_approval", "computing"),
        pre("draft"),
        pre("newer_kind", "computing"),
      ),
    );
    expect(sections.map((s) => [s.key, s.state, s.preconditions.map((p) => p.kind)])).toEqual([
      ["reviews", "unmet", ["approval", "owners_approval"]],
      ["checks", "none", []],
      ["findings", "none", []],
      ["facts", "none", []],
      ["mergeable", "met", ["mergeable"]],
      ["other", "unmet", ["queue"]],
      ["other", "met", ["draft"]],
      ["other", "computing", ["newer_kind"]],
    ]);
  });

  // THE METER'S TONES, most urgent first, neutral where no condition backs the
  // section. Mutation: tone by the first condition, and this fails.
  test("a section's disc wears its most urgent condition's tone, neutral for none", () => {
    const person = { waits_on: [{ handle: "ada", kind: "person" as const }] };
    const seat = { waits_on: [{ handle: "rev", kind: "agent" as const }] };
    expect(sectionTone({ preconditions: [] })).toBe("neutral");
    expect(sectionTone({ preconditions: [pre("approval")] })).toBe("success");
    expect(sectionTone({ preconditions: [pre("approval", "unmet", seat)] })).toBe("info");
    expect(
      sectionTone({
        preconditions: [pre("approval", "unmet", seat), pre("owners_approval", "unmet", person)],
      }),
    ).toBe("warning");
    expect(sectionTone({ preconditions: [pre("check", "unmet")] })).toBe("danger");
    expect(boxTone(readiness(pre("approval"), pre("check", "computing")))).toBe("info");
    // THE BOX'S DISC IS ITS FIRST WAITING SECTION'S, not its first condition's.
    expect(
      boxTone(readiness(pre("approval", "unmet", seat), pre("owners_approval", "unmet", person))),
    ).toBe("warning");
    expect(boxTone(readiness(pre("approval"), pre("facts")))).toBe("success");
    expect(boxTone(undefined)).toBe("neutral");
  });

  // THE PARTICIPANTS' VERDICT STANDS; the loaded reviews only past them. Mutation:
  // read the loaded reviews first, and the participant's verdict is overruled.
  test("a reviewer's standing is the participants' verdict, the reviews only past them", () => {
    const review = (reviewer: string, verdict: string, revision: number) =>
      ({ reviewer, reviewer_kind: "agent", verdict, revision }) as never;
    const change = {
      participants: {
        rows: [
          {
            handle: "rev",
            kind: "agent" as const,
            verdict: "approve" as const,
            verdict_revision: 2,
          },
        ],
      },
    };
    expect(reviewerStandings(change, [review("rev", "comment", 1)])).toEqual([
      { handle: "rev", kind: "agent", verdict: "approve", revision: 2 },
    ]);
    // A COMMENT JUDGES NOTHING: the approval before it stands. Mutation: keep the latest review.
    expect(
      reviewerStandings({ participants: undefined }, [
        review("rev", "approve", 2),
        review("rev", "comment", 3),
        review("sre", "comment", 3),
      ]),
    ).toEqual([{ handle: "rev", kind: "agent", verdict: "approve", revision: 2 }]);
    const standings = [
      { handle: "rev", kind: "agent", verdict: "approve", revision: 2 },
      { handle: "cto", kind: "agent", verdict: "approve", revision: 1 },
      { handle: "sre", kind: "agent", verdict: "comment", revision: 2 },
      { handle: "dev", kind: "agent", verdict: "comment", revision: 2 },
    ];
    const asked = [{ reviewer: "sre", state: "requested" }] as never[];
    const head = { revision: 2, authors: [{ author: "dev" }] as never[] };
    expect(reRequestable(head, standings, asked, "jane")).toEqual(["cto"]);
    // NEVER THE READER: a reviewer is not offered to ask their own review again.
    // Mutation: drop the reader's filter, and cto re-requests cto.
    expect(reRequestable(head, standings, asked, "cto")).toEqual([]);
  });
});

describe("a merge measured against its target", () => {
  const TIP = "c".repeat(40);
  const OLD = "a".repeat(40);
  const BASE = "b".repeat(40);
  const HEAD = "d".repeat(40);
  const merge = (target_oid: string) => ({
    revision: 2,
    target_ref: "refs/heads/main",
    target_oid,
    state: "clean",
    node: "git-1",
    at: "2026-11-18T10:00:00Z",
  });
  const change = { target_ref: "refs/heads/main", head: HEAD, revision: 2, target_tip: TIP };

  // Mutation: compare the measured tip with the head, and a moved target reads current.
  test("a measurement of a tip the target has moved past says so", () => {
    expect(measuredBeforeMove({ ...change, mergeability: merge(OLD) })).toBe(true);
    expect(measuredBeforeMove({ ...change, mergeability: merge(TIP) })).toBe(false);
    expect(measuredBeforeMove({ target_ref: "refs/heads/main", mergeability: merge(OLD) })).toBe(
      false,
    );
  });

  const facts = (target_oid: string, base_oid: string) => [
    {
      n: 2,
      facts: {
        target_ref: "refs/heads/main",
        target_oid,
        target_position: { stream: "CREWLET_CODE_LOG", generation: 1, seq: 1 },
        base_oid,
        base_tree: "",
        head_tree: "",
        touched: [],
        node: "git-1",
        owners_digest: "",
        position: { stream: "CREWLET_CODE_LOG", generation: 1, seq: 2 },
      },
    },
  ];

  // Mutation: offer Fast-forward wherever the facts name the tip, and a branch behind it is offered.
  test("Fast-forward is held where the target moved past the branch, and only there", () => {
    expect(fastForwardHeld(change, facts(TIP, BASE))).toBe(
      "main has moved past this branch, so main cannot move to its head. Rebase it first, or merge it another way.",
    );
    expect(fastForwardHeld(change, facts(TIP, TIP))).toBeUndefined();
    // FACTS OF AN EARLIER TIP say nothing of this one: the build answers.
    expect(fastForwardHeld(change, facts(OLD, BASE))).toBeUndefined();
    expect(fastForwardHeld(change, [])).toBeUndefined();
    expect(fastForwardHeld({ ...change, target_tip: undefined }, facts(TIP, BASE))).toBeUndefined();
  });
});
