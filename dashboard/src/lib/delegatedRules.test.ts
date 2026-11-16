/**
 * The delegated-decision rules a page writes: a form read as a rule, and each
 * change a merge patch of the WHOLE rules list as the document holds it at
 * the save, refused as a conflict where the rule it was made to is gone.
 */

import { describe, expect, test } from "vitest";
import type { CompanyDocument } from "~/protocol/types.ts";
import {
  addRulePlan,
  documentEnabled,
  documentRules,
  draftOf,
  editRulePlan,
  enabledPlan,
  placeProblems,
  readDraft,
  removeRulePlan,
  sampleWords,
  type DocumentRule,
} from "./delegatedRules.ts";

const SSH: DocumentRule = {
  kinds: ["ssh_operate"],
  delegates: ["security-engineer"],
  escalate_to: ["ops-lead"],
};
const RELEASE: DocumentRule = {
  kinds: ["pr_merge", "deploy"],
  delegates: ["release-reviewer"],
  escalate_to: ["ops-lead"],
  sample_one_in: 5,
  always_escalate_paths: ["billing/**"],
  required_checks: [{ repo: "acme/api", branch: "main", checks: ["test (ubuntu, 1.22)"] }],
};

const doc = (block: unknown): CompanyDocument =>
  ({ name: "Acme", delegated_decisions: block }) as unknown as CompanyDocument;

describe("the document's rules", () => {
  test("are read as written, and nothing where the block is absent", () => {
    expect(documentRules(doc({ enabled: true, rules: [SSH, RELEASE] }))).toEqual([SSH, RELEASE]);
    expect(documentRules({ name: "Acme" } as unknown as CompanyDocument)).toEqual([]);
    expect(documentEnabled(doc({ enabled: true }))).toBe(true);
    expect(documentEnabled(doc({}))).toBe(false);
  });
});

describe("a form read as a rule", () => {
  test("round-trips a rule it was filled from", () => {
    expect(readDraft(draftOf(RELEASE))).toEqual({ ok: true, rule: RELEASE });
    expect(readDraft(draftOf(SSH))).toEqual({ ok: true, rule: SSH });
  });

  // AN EMPTY RATE IS THE ENGINE'S DEFAULT, written as no key at all.
  test("an empty spot-check rate is left out; a rate that is not a whole number is refused", () => {
    expect(readDraft({ ...draftOf(SSH), sample: " " })).toEqual({ ok: true, rule: SSH });
    expect(readDraft({ ...draftOf(SSH), sample: "2.5" })).toEqual({
      ok: false,
      errors: { sample: "Enter a whole number." },
    });
  });

  // THE ENGINE'S RANGE, where the read carried it, said before anything is sent.
  test("a rate outside the engine's range is refused in its words", () => {
    const range = { default: 10, min: 1, max: 1000 };
    expect(readDraft({ ...draftOf(SSH), sample: "1001" }, range)).toEqual({
      ok: false,
      errors: { sample: "From 1 to 1000." },
    });
    expect(readDraft({ ...draftOf(SSH), sample: "1000" }, range).ok).toBe(true);
  });

  // WHAT EVERY RULE NEEDS is asked for before the engine is: a kind, the agent
  // that decides (a panel at most two) and somebody to escalate to.
  test("a rule without a kind, an agent or a person is refused field by field", () => {
    expect(readDraft(draftOf())).toEqual({
      ok: false,
      errors: {
        kinds: "Pick at least one kind of request.",
        delegates: "Pick the agent that decides.",
        escalateTo: "Pick at least one person.",
      },
    });
    expect(readDraft({ ...draftOf(SSH), delegates: ["a", "b", "c"] })).toEqual({
      ok: false,
      errors: { delegates: "Pick one agent, or two for a panel." },
    });
  });

  // ONLY A MERGE OR DEPLOY RULE TAKES PATHS AND CHECKS: the engine refuses
  // either on a command rule, so a form that dropped those kinds drops them.
  test("paths and checks are left out of a rule with no merge or deploy", () => {
    const draft = { ...draftOf(RELEASE), kinds: ["ssh_operate"] };
    const read = readDraft(draft);
    expect(read.ok && read.rule.always_escalate_paths).toBeUndefined();
    expect(read.ok && read.rule.required_checks).toBeUndefined();
  });

  test("a checks entry wholly empty is dropped, one half filled is refused", () => {
    const blank = { repo: " ", branch: "", checks: [] };
    expect(readDraft({ ...draftOf(RELEASE), checks: [...draftOf(RELEASE).checks, blank] })).toEqual(
      {
        ok: true,
        rule: RELEASE,
      },
    );
    const half = { repo: "acme/web", branch: "", checks: ["test"] };
    expect(readDraft({ ...draftOf(RELEASE), checks: [half] })).toEqual({
      ok: false,
      errors: { checks: "Pick a repository and a branch for each entry." },
    });
    const none = { repo: "acme/web", branch: "main", checks: [] };
    expect(readDraft({ ...draftOf(RELEASE), checks: [none] })).toEqual({
      ok: false,
      errors: { checks: "Add at least one check to each entry." },
    });
  });
});

describe("the engine's refusal of a rule", () => {
  const problem = (path: string, message: string, said?: string) => ({
    path,
    message: `${path}: ${message}`,
    ...(said ? { said } : {}),
  });

  // EACH UNDER ITS FIELD, in the engine's person-facing words where it wrote
  // them, else its line without the document path.
  test("is placed under the fields of the rule it names, the rest said whole", () => {
    const placed = placeProblems(
      [
        problem("delegated_decisions.rules[1].escalate_to", "missing", "Pick at least one person."),
        problem("delegated_decisions.rules[1].delegates[0]", "ops-lead is a person's seat"),
        problem("delegated_decisions.rules[0].kinds[0]", "taken"),
        problem("roles[2].llm", "unknown entry"),
      ],
      1,
    );
    expect(placed.fields).toEqual({
      escalateTo: "Pick at least one person.",
      delegates: "ops-lead is a person's seat",
    });
    expect(placed.rest).toEqual(["taken", "unknown entry"]);
  });

  test("a spot-check rate's words say how often the people review", () => {
    expect(sampleWords(1)).toBe("These people review every decision afterwards.");
    expect(sampleWords(10)).toBe(
      "These people review about 1 in 10 of the agent's decisions afterwards.",
    );
  });
});

describe("each change", () => {
  test("turning delegation on writes true, and off removes the key", () => {
    expect(enabledPlan(true)(doc({ rules: [SSH] }))).toEqual({
      patch: { delegated_decisions: { enabled: true } },
      summary: "Delegated decisions: turned on",
    });
    expect(enabledPlan(false)(doc({ enabled: true }))).toEqual({
      patch: { delegated_decisions: { enabled: null } },
      summary: "Delegated decisions: turned off",
    });
    expect(enabledPlan(true)(doc({ enabled: true }))).toBeNull();
  });

  // THE WHOLE LIST, as the document holds it AT THE SAVE: a rule a colleague
  // added since the page was read is kept.
  test("an added rule goes after every rule the document holds", () => {
    expect(addRulePlan(RELEASE)(doc({ rules: [SSH] }))).toEqual({
      patch: { delegated_decisions: { rules: [SSH, RELEASE] } },
      summary: "Delegated decisions: add rule 2 (Merges and Deploys)",
    });
  });

  test("a changed rule replaces the one it was filled from, in its place", () => {
    const changed = { ...SSH, escalate_to: ["ops-lead", "cto"] };
    expect(editRulePlan(1, SSH, changed)(doc({ rules: [SSH, RELEASE] }))).toEqual({
      patch: { delegated_decisions: { rules: [changed, RELEASE] } },
      summary: "Delegated decisions: change rule 1 (Server commands)",
    });
    expect(editRulePlan(1, SSH, SSH)(doc({ rules: [SSH, RELEASE] }))).toBeNull();
  });

  // A RULE CHANGED OR GONE since the form was filled is never written over.
  test("a change to a rule the document no longer holds as it was is a conflict", () => {
    const moved = { ...SSH, delegates: ["sre-bot"] };
    expect(editRulePlan(1, SSH, { ...SSH, sample_one_in: 3 })(doc({ rules: [moved] }))).toBe(
      "conflict",
    );
    expect(editRulePlan(2, RELEASE, SSH)(doc({ rules: [SSH] }))).toBe("conflict");
  });

  test("a removed rule moves every later one up, and the last removes the key", () => {
    expect(removeRulePlan(1, SSH.kinds)(doc({ rules: [SSH, RELEASE] }))).toEqual({
      patch: { delegated_decisions: { rules: [RELEASE] } },
      summary: "Delegated decisions: remove rule 1 (Server commands)",
    });
    expect(removeRulePlan(1, SSH.kinds)(doc({ rules: [SSH] }))).toEqual({
      patch: { delegated_decisions: { rules: null } },
      summary: "Delegated decisions: remove rule 1 (Server commands)",
    });
    // The rule at that place delegates other kinds now: not the one drawn.
    expect(removeRulePlan(1, SSH.kinds)(doc({ rules: [RELEASE] }))).toBe("conflict");
  });
});
