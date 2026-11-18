// @vitest-environment node
/**
 * The merge box's sections: every condition the meter counts lands in one
 * section, in the box's order, and each disc wears the meter's tone.
 */
import { describe, expect, test } from "vitest";

import type { CodePrecondition, CodeReadiness } from "~/protocol/index.ts";
import {
  boxTone,
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
