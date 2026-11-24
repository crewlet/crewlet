// @vitest-environment node
/**
 * Placing problems and warnings on nodes.
 *
 * What these protect: a problem is placed through the path index of the
 * document that was SENT, never the draft as it stands when the answer lands;
 * the engine's segments are used as given, without parsing a path; and what
 * the builder cannot fix is kept at document level with a link to where it is
 * fixed.
 */

import { describe, expect, test } from "vitest";
import type { ConfigProblem, ConfigWarning } from "~/protocol/index.ts";
import { COMPANY_KEY } from "./keys.ts";
import { fromDocument, toDocument } from "./document.ts";
import { apply, record } from "./operations.ts";
import { placeProblems, problemCountOf } from "./problems.ts";
import { fixtureCompany, fixtureDerived } from "./testkit.ts";

const problem = (
  segments: (string | number)[] | null,
  extra: Partial<ConfigProblem> = {},
): ConfigProblem => ({
  path: "",
  segments,
  kind: "invalid",
  message: `problem at ${JSON.stringify(segments)}`,
  ...extra,
});

const warning = (segments: (string | number)[] | null): ConfigWarning => ({
  kind: "dangling_reference",
  ref: "lead",
  path: "",
  segments,
  seat: "",
  unit: "",
  from: "",
  to: "",
  message: "a warning",
});

function sentFixture() {
  const doc = fixtureCompany();
  const draft = fromDocument(doc);
  return { doc, draft, sent: toDocument(draft) };
}

describe("placeProblems", () => {
  test("places a problem on the node its longest indexed prefix names, with the field below it", () => {
    const { sent } = sentFixture();
    const index = placeProblems(sent, {
      problems: [
        problem(["units", 0, "roles", 1, "goal"]),
        problem(["units", 0, "children", 0, "roles", 0, "integrations", "mattermost", "channel"]),
        problem(["units", 1]),
      ],
    });
    expect(index.byNode.get("seat:dev")?.map((p) => p.field)).toEqual([["goal"]]);
    expect(index.byNode.get("seat:sre")?.[0]).toMatchObject({
      field: ["integrations", "mattermost", "channel"],
      link: null,
      severity: "problem",
    });
    expect(index.byNode.get("unit:sales")?.[0]?.field).toEqual([]);
    expect(index.document).toEqual([]);
    expect(index.problemCount).toBe(3);
  });

  test("uses the index of the document that was sent, not the draft as it stands now", () => {
    const { draft, sent } = sentFixture();
    const moved = record(draft, {
      type: "reorder",
      target: "seat:dev",
      to: { parent: "unit:engineering", after: null },
    });
    if (!moved.ok) throw new Error(moved.message);
    const now = toDocument(apply(draft, moved.op).draft);
    const answer = { problems: [problem(["units", 0, "roles", 1, "goal"])] };
    expect(placeProblems(sent, answer).byNode.has("seat:dev")).toBe(true);
    expect(placeProblems(now, answer).byNode.has("seat:vp-engineering")).toBe(true);
  });

  test("the company's name belongs to the company node", () => {
    const { sent } = sentFixture();
    const index = placeProblems(sent, { problems: [problem(["name"])] });
    expect(index.byNode.get(COMPANY_KEY)?.map((p) => p.field)).toEqual([["name"]]);
  });

  // THE PRINCIPLES HAVE NO FORM HERE: a problem with the mission, the vision
  // or a policy is fixed on Company › Principles, so it stays at document
  // level with that link rather than on a company node whose form cannot
  // show the field.
  test("a problem with the principles links to Company › Principles", () => {
    const { sent } = sentFixture();
    const index = placeProblems(sent, {
      problems: [problem(["mission"]), problem(["vision"]), problem(["policies", 0])],
    });
    expect(index.byNode.has(COMPANY_KEY)).toBe(false);
    expect(index.document.map((p) => [p.node, p.link])).toEqual([
      [null, "principles"],
      [null, "principles"],
      [null, "principles"],
    ]);
  });

  // WHERE AN ENTRY RUNS IS CHOSEN ON SETTINGS › MODELS, so an advisory at a
  // model entry or at the sandbox default links there.
  test("a model entry's or the sandbox default's warning links to Settings › Models & Fleets", () => {
    const { sent } = sentFixture();
    const index = placeProblems(sent, {
      warnings: [
        warning(["providers", "llm", "claude", "run_in"]),
        warning(["providers", "sandbox", "default_run_in"]),
        warning(["providers", "embeddings", "model"]),
      ],
    });
    expect(index.document.map((p) => [p.severity, p.link])).toEqual([
      ["warning", "models"],
      ["warning", "models"],
      ["warning", null],
    ]);
  });

  test("what the builder cannot fix stays at document level, linked to where it is fixed", () => {
    const { sent } = sentFixture();
    const index = placeProblems(sent, {
      problems: [
        problem(["integrations", "datadog", "route_to"]),
        problem(["scheduling", "tick_seconds"]),
        problem(["providers", "llm"]),
        problem(null, { message: "yaml: line 3: could not parse" }),
        problem(["roles", 9, "goal"]),
      ],
    });
    expect(index.document.map((p) => [p.node, p.link])).toEqual([
      [null, "integrations"],
      [null, "schedules"],
      // The model catalogue's entries are Settings › Models & Fleets'.
      [null, "models"],
      [null, null],
      [null, null],
    ]);
  });

  test("a node's own schedule links to the Schedules screen, and a tool server named schedules does not", () => {
    const { sent } = sentFixture();
    const index = placeProblems(sent, {
      problems: [
        problem(["units", 0, "schedules", 0, "cron"]),
        problem(["units", 0, "mcp_env", "schedules", "TOKEN"]),
      ],
    });
    expect(index.byNode.get("unit:engineering")?.map((p) => p.link)).toEqual(["schedules", null]);
  });

  // A refusal that names only a seat (a seat a person still holds, from a
  // write's 409) comes with no derivation at all, and is still placed on the
  // seat: a saved seat is keyed by its handle. A seat this draft created is
  // keyed by a minted token, so its handle is read through the derivation.
  test("a problem naming only a seat is placed on the seat with that handle", () => {
    const { doc, sent } = sentFixture();
    const answer = { problems: [problem([], { seat: "sre" }), problem(null, { seat: "nobody" })] };
    const bare = placeProblems(sent, answer);
    expect(bare.byNode.get("seat:sre")).toHaveLength(1);
    expect(bare.document).toHaveLength(1);
    const derived = fixtureDerived(doc, { seats: { "roles[0]": { handle: "chief" } } });
    const created = placeProblems(sent, { problems: [problem([], { seat: "chief" })], derived });
    expect(created.byNode.get("seat:ceo")).toHaveLength(1);
  });

  // A PART OF A LEAD'S WRITE THE ENGINE REFUSED names a unit by its KEY,
  // where a validation problem names one by its name and carries its path.
  test("a refused part naming a unit is placed on the unit with that key", () => {
    const { sent } = sentFixture();
    const index = placeProblems(sent, {
      problems: [
        problem(null, { kind: "refused", unit: "platform" }),
        problem(null, { unit: "platform" }),
      ],
    });
    expect(index.byNode.get("unit:platform")).toHaveLength(1);
    expect(index.document).toHaveLength(1);
  });

  test("warnings are placed the same way and counted apart from problems", () => {
    const { sent } = sentFixture();
    const index = placeProblems(sent, {
      problems: [problem(["units", 1, "roles", 0, "goal"])],
      warnings: [warning(["units", 1]), warning(["units", 1, "roles", 0])],
    });
    expect(index.problemCount).toBe(1);
    expect(index.warningCount).toBe(2);
    expect(index.byNode.get("seat:account-executive")?.map((p) => p.severity)).toEqual([
      "problem",
      "warning",
    ]);
    expect(problemCountOf(index, "seat:account-executive")).toBe(1);
    expect(problemCountOf(index, "unit:sales")).toBe(0);
  });

  // A WARNING IS SAID IN THE WORDS THE ENGINE WROTE FOR A PERSON, where it
  // wrote some, on every surface the builder draws it: the banner, the review
  // and the node's own editor. The operator's sentence named
  // `providers.llm.default` and `llm_auxiliary` to a founder. A problem has no
  // such words and keeps its own. Mutation: place `source.message` for a
  // warning, and the first expectation goes red.
  test("a warning reads in the engine's words for a person, beside the operator's on its source", () => {
    const { sent } = sentFixture();
    const index = placeProblems(sent, {
      problems: [problem(["units", 1, "roles", 0, "goal"])],
      warnings: [
        {
          ...warning(["units", 1, "roles", 0, "llm_auxiliary"]),
          message: "account-executive's reflection runs on providers.llm.default",
          class: "learning.auxiliary_model",
          person: "Reflection and summaries run on the same model as the agent's own work.",
        },
        warning(["units", 1]),
      ],
    });
    const seat = index.byNode.get("seat:account-executive")!;
    expect(seat.map((p) => p.message)).toEqual([
      'problem at ["units",1,"roles",0,"goal"]',
      "Reflection and summaries run on the same model as the agent's own work.",
    ]);
    expect(seat[1]!.source.message).toBe(
      "account-executive's reflection runs on providers.llm.default",
    );
    expect(index.byNode.get("unit:sales")!.map((p) => p.message)).toEqual(["a warning"]);
  });

  // A SEAT'S MODEL FOR ITS ROUTINES, REVIEWS, DELEGATED WORK AND JUDGE is
  // chosen on Settings › Models & Fleets, by what each model is for; the builder edits
  // the seat's own chain only. So a warning there stays on the seat and
  // links to where it is changed. Mutation: link only a node's schedules.
  test("a seat's model for a job beside its own work links to Settings › Models & Fleets", () => {
    const { sent } = sentFixture();
    const index = placeProblems(sent, {
      warnings: [
        warning(["units", 1, "roles", 0, "llm_auxiliary"]),
        warning(["units", 1, "roles", 0, "llm_review"]),
        warning(["units", 1, "roles", 0, "llm"]),
      ],
    });
    expect(index.byNode.get("seat:account-executive")?.map((p) => p.link)).toEqual([
      "models",
      "models",
      null,
    ]);
  });
});
