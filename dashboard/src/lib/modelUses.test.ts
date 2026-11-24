import { describe, expect, it } from "vitest";
import type { CompanyDocument } from "~/protocol/types.ts";
import type { ModelUse } from "~/protocol/models.ts";
import {
  CATEGORY_JOBS,
  CATEGORY_ORDER,
  SEAT_JOBS,
  agentRoles,
  applyFleetWhereUnset,
  applySeatTarget,
  applyUses,
  categoryFields,
  categoryState,
  intentOf,
  noUses,
  providerOrder,
  usesForIntent,
  usesOf,
  withCategory,
  type Uses,
} from "./modelUses.ts";

/** A company with a seat at the root and two in a nested unit, each written a different way. */
function company(): CompanyDocument {
  return {
    name: "Acme",
    providers: { llm: { big: { model: "sonnet" }, fast: { model: "haiku" }, old: {} } },
    roles: [{ name: "Lead", handle: "lead", llm: "old" }],
    units: [
      {
        name: "Eng",
        id: "eng",
        roles: [{ name: "Human", handle: "boss", kind: "human" }],
        children: [
          {
            name: "Platform",
            id: "platform",
            roles: [
              {
                name: "Mapped",
                handle: "mapped",
                llm: { default: "old", review: ["old", "fast"] },
              },
              { name: "Flat", handle: "flat", llm: "old", llm_judge: ["fast", "old"] },
            ],
          },
        ],
      },
    ],
    assistant: { model: "fast" },
  } as unknown as CompanyDocument;
}

const role = (doc: CompanyDocument, handle: string) =>
  agentRoles(doc as unknown as Record<string, unknown>).get(handle)!;

describe("the categories", () => {
  // GROUPED BY THE MODEL EACH JOB WANTS: the review with the delegated work (a
  // strong model), the routines with the onboarding judge and a busy seat's
  // wake triage (a fast one, each a bounded call under a fixed deadline), so
  // one model chosen serves every job it writes.
  it("group the jobs that want the same kind of model", () => {
    expect(CATEGORY_JOBS.reviews).toEqual(["review", "subagent"]);
    expect(CATEGORY_JOBS.routines).toEqual(["auxiliary", "judge", "triage"]);
  });

  it("cover every seat job exactly once", () => {
    const covered = CATEGORY_ORDER.flatMap((c) => CATEGORY_JOBS[c]);
    expect([...covered].sort()).toEqual([...SEAT_JOBS].sort());
  });

  it("name the fields they write", () => {
    expect(categoryFields("coding")).toEqual(["llm"]);
    expect(categoryFields("reviews")).toEqual(["llm_review", "llm_subagent"]);
    expect(categoryFields("routines")).toEqual(["llm_auxiliary", "llm_judge", "llm_triage"]);
  });
});

describe("usesOf", () => {
  it("reads what runs on the model, not what falls back to it", () => {
    const used: ModelUse[] = [
      { seat: "lead", job: "executor", position: 0 },
      { seat: "lead", job: "review", position: 1 },
      { assistant: "chat", position: 0 },
    ];
    const uses = usesOf(used);
    expect(uses.jobs.executor).toEqual(["lead"]);
    expect(uses.jobs.review).toEqual([]);
    expect(uses.assistant).toBe(true);
    expect(uses.advisor).toBe(false);
    expect(usesOf([{ advisor: "findings", position: 0 }]).advisor).toBe(true);
  });
  it("reads a seat on a fleet as no use of the fleet's member: the page edits pinned uses", () => {
    const uses = usesOf([{ seat: "lead", job: "executor", position: 0, fleet: "claude-opus" }]);
    expect(uses.jobs.executor).toEqual([]);
  });
});

describe("applyFleetWhereUnset", () => {
  const ref = "fleet:default-fleet";
  it("a seat on no fleet runs every job there, its fallbacks gone", () => {
    const out = applyFleetWhereUnset(company(), ["lead", "flat"], ref);
    expect(role(out, "lead")).toEqual({ name: "Lead", handle: "lead", llm: ref });
    expect(role(out, "flat")).toEqual({ name: "Flat", handle: "flat", llm: ref });
  });

  it("a job already on a fleet keeps it; a satellite with no field follows the executor", () => {
    const doc = company() as unknown as Record<string, any>;
    doc.roles[0].llm_review = "fleet:claude-opus";
    const out = applyFleetWhereUnset(doc as unknown as CompanyDocument, ["lead"], ref);
    expect(role(out, "lead")).toEqual({
      name: "Lead",
      handle: "lead",
      llm: ref,
      llm_review: "fleet:claude-opus",
    });
  });

  it("a seat not named, or one the document does not hold, is left as written", () => {
    const out = applyFleetWhereUnset(company(), ["nobody"], ref);
    expect(out).toEqual(company());
  });
});

describe("applySeatTarget", () => {
  it("every job writes llm alone, clears the satellites and names the fallbacks it drops", () => {
    const { doc, dropped } = applySeatTarget(company(), "mapped", "all", "fleet:opus");
    expect(role(doc, "mapped")).toEqual({ name: "Mapped", handle: "mapped", llm: "fleet:opus" });
    expect(dropped).toEqual([{ seat: "mapped", job: "review", keys: ["fast"] }]);
  });

  it("one job writes its own field, a mapping's executor its default, and leaves the rest", () => {
    const flat = applySeatTarget(company(), "flat", ["judge"], "fleet:opus").doc;
    expect(role(flat, "flat")).toEqual({
      name: "Flat",
      handle: "flat",
      llm: "old",
      llm_judge: "fleet:opus",
    });
    const mapped = applySeatTarget(company(), "mapped", ["executor"], "big").doc;
    expect(role(mapped, "mapped").llm).toEqual({ default: "big", review: ["old", "fast"] });
  });

  it("a satellite written flat leaves the mapping's own key, which it would shadow, out", () => {
    const { doc } = applySeatTarget(company(), "mapped", ["review"], "fleet:opus");
    expect(role(doc, "mapped")).toEqual({
      name: "Mapped",
      handle: "mapped",
      llm: "old",
      llm_review: "fleet:opus",
    });
  });

  it("a seat the document does not hold changes nothing", () => {
    expect(applySeatTarget(company(), "nobody", "all", "big").doc).toEqual(company());
  });
});

describe("intentOf", () => {
  it("reads one choice where one describes the uses", () => {
    expect(intentOf(noUses())).toBe("none");
    expect(intentOf({ ...noUses(), assistant: true })).toBe("assistant");
    expect(intentOf(usesForIntent("all", ["a", "b"], false))).toBe("all");
    expect(intentOf(usesForIntent("reviews", ["a"], false))).toBe("reviews");
    // THE ASSISTANT BESIDE SEAT WORK keeps the seat choice.
    expect(intentOf(usesForIntent("routines", ["a"], true))).toBe("routines");
    // THE ADVISOR ALONE is a choice of its own; beside seat work it keeps the seat choice.
    expect(intentOf(usesForIntent("advisor", [], false))).toBe("advisor");
    expect(usesForIntent("advisor", ["a", "b"], false).jobs.executor).toEqual([]);
    expect(intentOf(usesForIntent("coding", ["a"], false, true))).toBe("coding");
  });

  it("reads a category split by hand as custom", () => {
    const uses: Uses = { ...noUses(), jobs: { ...noUses().jobs, review: ["a"] } };
    expect(intentOf(uses)).toBe("custom");
    expect(categoryState(uses, "a", "reviews")).toBe("some");
  });

  it("switches a whole category for one seat", () => {
    const on = withCategory(noUses(), "a", "routines", true);
    expect(on.jobs.auxiliary).toEqual(["a"]);
    expect(on.jobs.judge).toEqual(["a"]);
    expect(on.jobs.subagent).toEqual([]);
    expect(categoryState(on, "a", "routines")).toBe("all");
    expect(categoryState(withCategory(on, "a", "routines", false), "a", "routines")).toBe("none");
  });
});

describe("applyUses", () => {
  it("writes both fields of a category in one document, one model each", () => {
    const { doc, dropped } = applyUses(
      company(),
      "big",
      noUses(),
      usesForIntent("routines", ["flat"], false),
    );
    const flat = role(doc, "flat");
    expect(flat.llm_auxiliary).toBe("big");
    expect(flat.llm_judge).toBe("big");
    expect(flat.llm_review).toBeUndefined();
    expect(flat.llm).toBe("old");
    // THE FALLBACKS IT DROPPED are named for the review; `fast`, the model the
    // job ran on first, is the change itself, which the review lists apart.
    expect(dropped).toEqual([{ seat: "flat", job: "judge", keys: ["old"] }]);
  });

  it("clears the mapping's own key, which the flat field would shadow", () => {
    const { doc, dropped } = applyUses(
      company(),
      "big",
      noUses(),
      usesForIntent("reviews", ["mapped"], false),
    );
    const mapped = role(doc, "mapped");
    expect(mapped.llm_review).toBe("big");
    expect(mapped.llm_subagent).toBe("big");
    // The mapping held only its default once review left it, so it is stored as the scalar.
    expect(mapped.llm).toBe("old");
    expect(dropped).toContainEqual({ seat: "mapped", job: "review", keys: ["fast"] });
  });

  it("writes Everything as one scalar llm and clears every satellite", () => {
    const { doc } = applyUses(
      company(),
      "big",
      noUses(),
      usesForIntent("all", ["flat", "mapped"], false),
    );
    for (const handle of ["flat", "mapped"]) {
      const r = role(doc, handle);
      expect(r.llm).toBe("big");
      for (const field of [
        "llm_review",
        "llm_subagent",
        "llm_auxiliary",
        "llm_judge",
        "llm_triage",
      ])
        expect(r[field]).toBeUndefined();
    }
    expect(role(doc, "lead").llm).toBe("old");
  });

  it("takes a model off a job by removing it from the field, keeping the rest", () => {
    const start = company();
    const was: Uses = { ...noUses(), jobs: { ...noUses().jobs, judge: ["flat"] } };
    const { doc } = applyUses(start, "fast", was, noUses());
    expect(role(doc, "flat").llm_judge).toBe("old");
  });

  it("deletes a field the model was alone in, so the job resolves as an unset one", () => {
    const first = applyUses(company(), "big", noUses(), usesForIntent("routines", ["lead"], false));
    const { doc } = applyUses(
      first.doc,
      "big",
      usesForIntent("routines", ["lead"], false),
      noUses(),
    );
    expect(role(doc, "lead").llm_auxiliary).toBeUndefined();
    expect(role(doc, "lead").llm_judge).toBeUndefined();
  });

  it("writes the Advisor's one model, and clears it only where it is taken off the model it runs on", () => {
    const before = company();
    const advisorOf = (doc: unknown) => (doc as { advisor?: { model?: string } }).advisor;
    const { doc, dropped } = applyUses(
      before,
      "big",
      noUses(),
      usesForIntent("advisor", [], false, true),
    );
    expect(advisorOf(doc)).toEqual({ model: "big" });
    expect(role(doc, "lead")).toEqual(role(before, "lead"));
    expect(dropped).toEqual([]);
    // TAKEN OFF the model it runs on, its checks run with no model.
    const off = applyUses(doc, "big", { ...noUses(), advisor: true }, noUses()).doc;
    expect(advisorOf(off)?.model).toBeUndefined();
    // ANOTHER MODEL'S edit leaves the Advisor where it is.
    const kept = applyUses(doc, "other", noUses(), noUses()).doc;
    expect(advisorOf(kept)).toEqual({ model: "big" });
  });

  it("writes Ask Crewlet's one model, company-wide, and leaves seats alone", () => {
    const before = company();
    const { doc, dropped } = applyUses(
      before,
      "big",
      noUses(),
      usesForIntent("assistant", [], true),
    );
    expect((doc as unknown as { assistant: { model: string } }).assistant.model).toBe("big");
    expect(role(doc, "lead")).toEqual(role(before, "lead"));
    expect(dropped).toEqual([]);
    const bare = company();
    delete (bare as unknown as { assistant?: unknown }).assistant;
    const { doc: made } = applyUses(bare, "big", noUses(), usesForIntent("assistant", [], true));
    expect((made as unknown as { assistant: unknown }).assistant).toEqual({ model: "big" });
  });

  it("never edits a human seat or the document it was given", () => {
    const before = company();
    const snapshot = structuredClone(before);
    applyUses(before, "big", noUses(), usesForIntent("all", ["boss", "lead"], false));
    expect(before).toEqual(snapshot);
    expect(agentRoles(before as unknown as Record<string, unknown>).has("boss")).toBe(false);
  });
});

describe("providerOrder", () => {
  // THE ORDER A SEAT THAT NAMES NO MODEL FALLS BACK THROUGH: the document's
  // own order, reconciled against the map rather than trusted.
  it("comes in the document's declared order, reconciled against the map", () => {
    const llm = { alpha: {}, bravo: {}, charlie: {} };
    expect(providerOrder(llm, ["charlie", "gone", "charlie", "alpha"])).toEqual([
      "charlie",
      "alpha",
      "bravo",
    ]);
    // No order recorded: sorted, arbitrary and stable.
    expect(providerOrder({ zulu: {}, alpha: {} }, undefined)).toEqual(["alpha", "zulu"]);
  });
});
