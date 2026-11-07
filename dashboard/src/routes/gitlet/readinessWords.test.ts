/**
 * A readiness is worded for a person from what the engine judged, never from
 * the sentence it writes for the seats that call its tools: names as the
 * dashboard draws them, the target by its branch, one term for the count.
 */

import { describe, expect, test } from "vitest";

import type { CodePrecondition, CodeReadiness } from "~/protocol/index.ts";
import {
  preconditionSentence,
  readinessCount,
  readinessNext,
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
