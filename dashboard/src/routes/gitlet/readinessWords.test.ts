/**
 * A readiness is worded for a person from what the engine judged, never from
 * the sentence it writes for the seats that call its tools: names as the
 * dashboard draws them, the target by its branch, one term for the count.
 */

import { describe, expect, test } from "vitest";

import type { CodePrecondition, CodeReadiness } from "~/protocol/index.ts";
import type { CodeCheckRow } from "~/protocol/index.ts";
import {
  checksWords,
  findingsWords,
  preconditionSentence,
  readinessCount,
  readinessNext,
  readinessWaits,
  reviewsWords,
  type ReadinessSubject,
} from "./readinessWords.ts";

const NAMES: Record<string, string> = { ada: "Ada Lovelace", rev: "Release Reviewer" };

const subject = (state = "open"): ReadinessSubject => ({
  change: {
    state,
    owner: "ada",
    owner_kind: "person",
    revision: 2,
    target_ref: "refs/heads/main",
  },
  nameOf: (h) => NAMES[h] ?? h,
});

const pre = (p: Partial<CodePrecondition> & Pick<CodePrecondition, "kind">): CodePrecondition => ({
  state: "unmet",
  detail: 'the engine\'s words: person "ada" merges into refs/heads/main',
  ...p,
});

describe("a readiness in a person's words", () => {
  // ONE TERM FOR THE COUNT on every surface. Mutation: call it
  // "preconditions" here and "conditions" in the merge panel, and this fails.
  test("the count has one name", () => {
    const r: CodeReadiness = { met: 5, total: 6, first: "x", preconditions: [] };
    expect(readinessCount(r)).toBe("5 of 6 conditions to merge met");
  });

  // NEVER THE ENGINE'S SENTENCE: no quoted handle, no full ref, a capital at
  // the start, the owner by name. Mutation: return `p.detail`, and this fails.
  test("an unasked approval names the owner and the step, in the dashboard's words", () => {
    const said = preconditionSentence(
      pre({ kind: "approval", waits_on: [{ handle: "ada", kind: "person" }] }),
      subject(),
    )!;
    expect(said).toBe(
      "No approval counts on revision 2, and nobody who may approve it has been asked. Ada Lovelace asks a reviewer.",
    );
    expect(said).not.toMatch(/"|refs\/heads|person /);
  });

  test("an asked approval waits on the reviewers by name", () => {
    expect(
      preconditionSentence(
        pre({ kind: "approval", waits_on: [{ handle: "rev", kind: "agent" }] }),
        subject(),
      ),
    ).toBe("No approval counts on revision 2 yet. Waiting on Release Reviewer.");
  });

  test("a conflict names the target by its branch", () => {
    expect(preconditionSentence(pre({ kind: "mergeable" }), subject())).toBe(
      "Revision 2 conflicts with main. Ada Lovelace merges main into it.",
    );
  });

  test("a draft says who marks it ready", () => {
    expect(preconditionSentence(pre({ kind: "draft" }), subject("draft"))).toMatch(
      /^It is a draft\. Ada Lovelace, a maintainer of the repository or any person marks it ready for review\.$/,
    );
  });

  test("a met precondition has no sentence: its tag says it", () => {
    expect(preconditionSentence(pre({ kind: "check", state: "met" }), subject())).toBeNull();
  });

  test("the next sentence is the first unmet one's, else that every condition is met", () => {
    const r: CodeReadiness = {
      met: 1,
      total: 2,
      first: "engine words",
      preconditions: [pre({ kind: "draft", state: "met" }), pre({ kind: "mergeable" })],
    };
    expect(readinessNext(r, subject())).toBe(
      "Revision 2 conflicts with main. Ada Lovelace merges main into it.",
    );
    expect(
      readinessNext(
        { ...r, met: 2, preconditions: [pre({ kind: "draft", state: "met" })] },
        subject(),
      ),
    ).toBe("Every condition to merge is met.");
  });
});

const row = (name: string, state: string, required = true) =>
  ({ name, state, required, attempt: 1, target: "refs/heads/main" }) as CodeCheckRow;

describe("what a change waits on, and its merge box's sections", () => {
  // ONE PHRASE PER KIND, the approval kinds as one, the reader as "you", a
  // waiting check by its name. Mutation: name only the first, and this fails.
  test("what it waits on is one phrase a kind, in one sentence", () => {
    const one: CodeReadiness = {
      met: 0,
      total: 1,
      first: "",
      preconditions: [pre({ kind: "approval", waits_on: [{ handle: "rev", kind: "agent" }] })],
    };
    const me = { ...subject(), me: "bo" };
    expect(readinessWaits(one, me, null)).toBe("It waits on an approval from Release Reviewer.");
    const three: CodeReadiness = {
      ...one,
      total: 4,
      preconditions: [
        ...one.preconditions,
        pre({ kind: "owners_approval", waits_on: [{ handle: "bo", kind: "person" }] }),
        pre({ kind: "check", state: "computing" }),
        pre({ kind: "mergeable" }),
      ],
    };
    expect(readinessWaits(three, me, [row("unit", "passed"), row("e2e", "running")])).toBe(
      "It waits on approvals from Release Reviewer and you, on e2e and on a conflict with main being resolved.",
    );
    expect(
      readinessWaits({ ...three, preconditions: three.preconditions.slice(0, 3) }, me, null),
    ).toBe("It waits on approvals from Release Reviewer and you, and on its required checks.");
    const met = { ...one, met: 1, preconditions: [pre({ kind: "approval", state: "met" })] };
    expect(readinessWaits(met, me, null)).toBe("It is ready to merge.");
    expect(readinessWaits(met, me, null, "Waits on")).toBe("Ready to merge");
  });

  // ONE APPROVAL MEETS A CONDITION: its candidates are joined with "or", and an
  // unasked one names its asker. Mutation: list the owner as a candidate, and this fails.
  test("an approval condition waits on any one of its candidates, or on its owner's ask", () => {
    const either = pre({
      kind: "approval",
      waits_on: [
        { handle: "rev", kind: "agent" },
        { handle: "bo", kind: "person" },
      ],
    });
    const r = (...p: CodePrecondition[]): CodeReadiness => ({
      met: 0,
      total: p.length,
      first: "",
      preconditions: p,
    });
    expect(readinessWaits(r(either), subject(), null)).toBe(
      "It waits on an approval from Release Reviewer or bo.",
    );
    const unasked = pre({ kind: "approval", waits_on: [{ handle: "ada", kind: "person" }] });
    expect(readinessWaits(r(unasked), subject(), null)).toBe(
      "It waits on a reviewer asked by Ada Lovelace.",
    );
    expect(reviewsWords([unasked], subject(), [])).toEqual({
      title: "Waits on an approval",
      sentence:
        "No approval counts on revision 2, and nobody who may approve it has been asked. Ada Lovelace asks a reviewer.",
    });
    expect(reviewsWords([either], subject(), [])).toEqual({
      title: "Waits on an approval",
      sentence: "Release Reviewer and bo have not reviewed yet.",
    });
  });

  // REVIEWS SAY EACH STANDING VERDICT THAT IS NOT AN APPROVAL OF THE HEAD and
  // who has not reviewed. Mutation: count an approval of an older revision as one.
  test("the reviews section says each standing that is not an approval of the head", () => {
    const waiting = [
      pre({
        kind: "approval",
        waits_on: [
          { handle: "rev", kind: "agent" },
          { handle: "bo", kind: "person" },
          { handle: "ada", kind: "person" },
        ],
      }),
    ];
    const standings = [{ handle: "rev", kind: "agent", verdict: "approve", revision: 1 }];
    expect(reviewsWords(waiting, { ...subject(), me: "bo" }, standings)).toEqual({
      title: "Waits on an approval",
      sentence: "Release Reviewer approved revision 1; you have not reviewed yet.",
    });
    const met = [pre({ kind: "approval", state: "met" })];
    expect(
      reviewsWords(met, subject(), [{ handle: "rev", verdict: "approve", revision: 2 }]),
    ).toEqual({ title: "Approved", sentence: "Release Reviewer approved revision 2." });
    expect(reviewsWords([], subject(), []).title).toBe("No approval is required");
  });

  // CHECKS ARE COUNTED BY STATE with what is required. Mutation: call an
  // unrequired failure a failed gate, and this fails.
  test("the checks section counts the latest attempts and what is required", () => {
    expect(checksWords([row("unit", "passed"), row("e2e", "running")], "main", true)).toEqual({
      title: "1 of 2 checks is running",
      sentence: "1 passed and 1 is running. Both are required to merge.",
    });
    expect(checksWords([row("unit", "passed"), row("e2e", "failed")], "main", true).title).toBe(
      "1 of 2 checks failed",
    );
    expect(
      checksWords([row("unit", "passed"), row("lint", "failed", false)], "main", true),
    ).toEqual({
      title: "2 checks ran, 1 failed",
      sentence: "lint failed, and is not required.",
    });
    expect(checksWords([row("unit", "passed")], "main", true)).toEqual({
      title: "Its check passed",
      sentence: "It is required to merge.",
    });
    expect(checksWords([row("unit", "passed"), row("e2e", "passed")], "main", true).title).toBe(
      "Both checks passed",
    );
    expect(checksWords([], "main", false).title).toBe("No check of its head is kept");
    expect(checksWords([], "main", true).title).toBe("No check has run on its head");
  });

  // A CANCELLED CHECK, or a state a newer engine names, has not passed: the gate
  // counts it failing. Mutation: count only failed and errored, and this fails.
  test("a check that finished without passing is never said to have passed", () => {
    expect(checksWords([row("unit", "passed"), row("e2e", "cancelled")], "main", true)).toEqual({
      title: "1 of 2 checks did not pass",
      sentence: "unit passed and e2e cancelled. Both are required to merge.",
    });
    expect(checksWords([row("unit", "passed"), row("e2e", "timed_out")], "main", true)).toEqual({
      title: "1 of 2 checks did not pass",
      sentence: "unit passed and e2e timed_out. Both are required to merge.",
    });
    expect(
      checksWords([row("unit", "passed"), row("lint", "cancelled", false)], "main", true),
    ).toEqual({
      title: "2 checks ran, 1 did not pass",
      sentence: "lint cancelled, and is not required.",
    });
  });

  test("the findings section weighs the open findings, or says only how many", () => {
    const met = [pre({ kind: "blocker", state: "met" })];
    const two = new Map([
      ["major", 2],
      ["minor", 1],
    ]);
    expect(findingsWords(met, subject(), 3, two).sentence).toBe(
      "Its 3 open findings are 2 majors and a minor. Only a blocker holds the merge.",
    );
    expect(findingsWords(met, subject(), 3, null).sentence).toBe(
      "None of its 3 open findings is a blocker.",
    );
    expect(findingsWords(met, subject(), 0, null).sentence).toBe("No finding is open.");
  });
});
