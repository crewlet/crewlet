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
        problem(["units", 0, "children", 0, "roles", 0, "integrations", "jira", "project"]),
        problem(["units", 1]),
      ],
    });
    expect(index.byNode.get("seat:dev")?.map((p) => p.field)).toEqual([["goal"]]);
    expect(index.byNode.get("seat:sre")?.[0]).toMatchObject({
      field: ["integrations", "jira", "project"],
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

  test("charter fields belong to the company node", () => {
    const { sent } = sentFixture();
    const index = placeProblems(sent, { problems: [problem(["name"]), problem(["policies", 0])] });
    expect(index.byNode.get(COMPANY_KEY)?.map((p) => p.field)).toEqual([["name"], ["policies", 0]]);
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
      [null, null],
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
});
