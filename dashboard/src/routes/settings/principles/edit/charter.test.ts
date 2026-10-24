/**
 * The charter's rules as values: two readings of a part compare as every
 * agent reads them, a draft is refused where it breaks a rule and nowhere
 * else, and a revision's summary says which statement moved and how.
 */

import { describe, expect, test } from "vitest";
import {
  CHARTER_LIMITS,
  charterOf,
  hasProblems,
  lengthOf,
  normalized,
  policyChanges,
  problemsOf,
  samePart,
  summaryOf,
  trimLikeEngine,
  warningsOf,
} from "./charter.ts";

describe("samePart", () => {
  // THE SPACES AROUND A PART are not something anybody wrote: an agent reads
  // a mission with a trailing newline exactly as one without.
  test("compares statements trimmed", () => {
    expect(samePart("Ship it\n", "  Ship it")).toBe(true);
    expect(samePart("Ship it", "Ship it now")).toBe(false);
  });

  test("compares policies item by item, trimmed, in order", () => {
    expect(samePart(["a ", "b"], ["a", " b"])).toBe(true);
    // A REORDER IS A CHANGE: the order is the order every seat reads them in.
    expect(samePart(["a", "b"], ["b", "a"])).toBe(false);
    expect(samePart(["a"], ["a", "b"])).toBe(false);
  });
});

describe("charterOf", () => {
  test("reads what a source holds and leaves what it lacks empty", () => {
    expect(charterOf({ mission: "M", policies: ["p"] })).toEqual({
      mission: "M",
      vision: "",
      policies: ["p"],
    });
  });

  // A MALFORMED SOURCE is read as nothing written rather than thrown on:
  // the screen shows what it can and the engine's own validation refuses.
  test("drops what is not text", () => {
    expect(charterOf({ mission: 3, vision: null, policies: ["ok", 4, null] })).toEqual({
      mission: "",
      vision: "",
      policies: ["ok"],
    });
  });
});

describe("problemsOf", () => {
  test("a statement within its limit has none", () => {
    expect(problemsOf("mission", "x".repeat(CHARTER_LIMITS.mission))).toEqual({});
  });

  test("a statement past its limit is refused at the field, counted as it will be saved", () => {
    const text = `  ${"x".repeat(CHARTER_LIMITS.vision + 1)}  `;
    const problems = problemsOf("vision", text);
    expect(problems.field).toContain(`${CHARTER_LIMITS.vision + 1}`);
    expect(hasProblems(problems)).toBe(true);
  });

  // COUNTED AS THE ENGINE COUNTS (`utf8.RuneCountInString`): an astral
  // emoji is one character, where `.length` says two and UTF-8 says four. At
  // the boundary, so a count off by a unit fails one side or the other.
  test("counts characters at the boundary, an astral one as one", () => {
    expect(lengthOf("🙂🙂")).toBe(2);
    const full = "🙂".repeat(CHARTER_LIMITS.mission);
    expect(full.length).toBe(CHARTER_LIMITS.mission * 2);
    expect(problemsOf("mission", full)).toEqual({});
    expect(problemsOf("mission", `${full}é`).field).toContain(`${CHARTER_LIMITS.mission + 1}`);
    const policy = "漢".repeat(CHARTER_LIMITS.policy);
    expect(problemsOf("policies", [policy])).toEqual({});
    expect(hasProblems(problemsOf("policies", [`${policy}字`]))).toBe(true);
  });

  // ONE LINE: each is inlined in every seat's prompt, a policy as one item of
  // a list. The vision is paragraphs.
  test("the mission and a policy are one line, the vision may be paragraphs", () => {
    expect(problemsOf("mission", "Ship it.\nNow.").field).toBe("Keep the mission to one line.");
    expect(problemsOf("policies", ["Pay\non time."]).items).toEqual([
      "Keep each policy to one line, or split it into two.",
    ]);
    expect(problemsOf("vision", "One.\n\nTwo.")).toEqual({});
  });

  // A BLANK ROW IS NO RULE ANYBODY WROTE: dropped on save, never refused.
  test("a blank policy is no problem and is not saved", () => {
    expect(problemsOf("policies", ["Pay on time.", "  ", "Ask first."])).toEqual({});
    expect(normalized(["Pay on time.", "  "])).toEqual(["Pay on time."]);
    expect(samePart(["a", " "], ["a"])).toBe(true);
  });

  test("a repeated policy is refused on the later copy, naming the first", () => {
    const problems = problemsOf("policies", ["Ask first.", "Pay on time.", " Ask first. "]);
    expect(problems.items).toEqual([undefined, undefined, "This repeats policy 1."]);
  });

  // WORD FOR WORD, as the engine judges a repeat.
  test("two policies that differ by a capital are two policies", () => {
    expect(problemsOf("policies", ["Ask first.", "ask first."])).toEqual({});
  });

  test("a list past its length is refused at the field, blank rows not counted", () => {
    const full = Array.from({ length: CHARTER_LIMITS.policies }, (_, i) => `Rule ${i}.`);
    expect(problemsOf("policies", [...full, " "])).toEqual({});
    expect(problemsOf("policies", [...full, "One more."]).field).toContain(
      `${CHARTER_LIMITS.policies}`,
    );
  });

  // A FIELD WHOSE OWN COUNT SAYS IT RUNS LONG is not told twice, but the rule
  // still holds for the save, which asks with the default.
  test("lengths: false leaves a length to the field's count", () => {
    const long = "x".repeat(CHARTER_LIMITS.policy + 1);
    expect(problemsOf("policies", [long], { lengths: false })).toEqual({});
    expect(hasProblems(problemsOf("policies", [long]))).toBe(true);
    expect(
      problemsOf("mission", "x".repeat(CHARTER_LIMITS.mission + 1), { lengths: false }),
    ).toEqual({});
  });
});

describe("summaryOf", () => {
  test("says whether a statement was written, changed or cleared", () => {
    expect(summaryOf("mission", "", "Ship it")).toBe("Wrote the mission");
    expect(summaryOf("vision", "Old", "New")).toBe("Changed the vision");
    expect(summaryOf("mission", "Ship it", "  ")).toBe("Cleared the mission");
  });

  test("says what moved in the policies", () => {
    expect(summaryOf("policies", ["a", "b"], ["a", "b", "c"])).toBe("Standing policies: added 1");
  });
});

describe("policyChanges", () => {
  test.each([
    { before: ["a"], after: ["a", "b"], said: "added 1" },
    { before: ["a", "b", "c"], after: ["a"], said: "removed 2" },
    { before: ["a", "b"], after: ["a", "B"], said: "reworded 1" },
    { before: ["a", "b", "c"], after: ["c", "a", "b"], said: "reordered" },
    { before: ["a", "b"], after: ["b", "a", "c"], said: "added 1, reordered" },
    { before: ["a", "b"], after: ["a", "b"], said: "unchanged" },
  ])("$before to $after is $said", ({ before, after, said }) => {
    expect(policyChanges(before, after)).toBe(said);
  });

  // A POLICY KEPT TWICE is counted as two: removing one copy is a removal.
  test("counts a repeated policy by its copies", () => {
    expect(policyChanges(["a", "a"], ["a"])).toBe("removed 1");
  });
});

// TRIMMED AS THE ENGINE TRIMS (`strings.TrimSpace`), which is not JavaScript's
// `trim`: NEL is space to the engine and BOM is not. At the bound, so a value
// judged one way here and the other way there fails.
describe("trimLikeEngine", () => {
  test("trims NEL and keeps BOM, as the engine does", () => {
    expect(trimLikeEngine("\u0085 Ship it\u3000")).toBe("Ship it");
    expect(trimLikeEngine("\ufeffShip it")).toBe("\ufeffShip it");
    const full = "x".repeat(CHARTER_LIMITS.mission);
    expect(problemsOf("mission", `${full}\u0085`)).toEqual({});
    expect(hasProblems(problemsOf("mission", `\ufeff${full}`))).toBe(true);
  });

  test("a vertical tab, a form feed or NEL is a line break", () => {
    for (const brk of ["\v", "\f", "\u0085", "\u2028"]) {
      expect(problemsOf("policies", [`Pay${brk}on time.`]).items, JSON.stringify(brk)).toEqual([
        "Keep each policy to one line, or split it into two.",
      ]);
    }
  });
});

// JUDGED ON THE CHANGE, as `config.ValidateCharterChange` judges it: a fault
// the stored charter already had never holds a save that leaves it alone.
describe("problemsOf against what the person started from", () => {
  const long = "x".repeat(CHARTER_LIMITS.policy + 1);

  test("an untouched long policy is no problem, and is a warning", () => {
    const was = [long, "Ask first."];
    const now = [long, "Ask before deleting."];
    expect(problemsOf("policies", now, { was })).toEqual({});
    expect(warningsOf("policies", now, was)).toEqual([
      `Policy 1: Keep each policy to ${CHARTER_LIMITS.policy} characters. This one is ${CHARTER_LIMITS.policy + 1}.`,
    ]);
  });

  test("an edited policy is held to the rules", () => {
    expect(problemsOf("policies", [`${long}!`], { was: [long] }).items).toHaveLength(1);
  });

  // A REPEAT IS JUDGED AGAINST HOW MANY TIMES `was` SAID IT: a stored pair
  // stays a warning, a third copy is the person's.
  test("a repeat counts against the copies the person started with", () => {
    expect(problemsOf("policies", ["a", "a"], { was: ["a", "a"] })).toEqual({});
    expect(problemsOf("policies", ["a", "a", "a"], { was: ["a", "a"] }).items).toEqual([
      undefined,
      undefined,
      "This repeats policy 1.",
    ]);
  });

  test("a list over the bound may shrink but not grow", () => {
    const over = Array.from({ length: CHARTER_LIMITS.policies + 2 }, (_, i) => `Rule ${i}.`);
    expect(problemsOf("policies", over.slice(1), { was: over })).toEqual({});
    expect(problemsOf("policies", [...over, "More."], { was: over }).field).toBeDefined();
  });

  test("an untouched statement is not judged", () => {
    const long = "x".repeat(CHARTER_LIMITS.mission + 1);
    expect(problemsOf("mission", long, { was: long })).toEqual({});
    expect(problemsOf("mission", `${long}!`, { was: long }).field).toBeDefined();
  });
});
