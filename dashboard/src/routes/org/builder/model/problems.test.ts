// @vitest-environment node
/**
 * What is wrong with a draft, placed on the nodes it is about.
 *
 * What these protect: the draft's own shape is judged by the chart's own
 * rules before anything is written (an address's grammar, a reserved word, a
 * field past its cap, what a seat's kind forbids); the addresses the chart
 * would refuse given the chart the draft was made on are named on the node
 * that took one, with a node's IDENTITY read off its key rather than guessed;
 * a reference naming nothing is a warning, resolved the way the chart resolves
 * one; and a settings dry run's findings go to the company node or to the
 * screen that fixes them.
 */

import { describe, expect, test } from "vitest";
import type { ConfigProblem, ConfigWarning } from "~/protocol/index.ts";
import { COMPANY_KEY } from "./keys.ts";
import { fromChart } from "./document.ts";
import type { Draft } from "./draft.ts";
import { apply, record, type Intent } from "./operations.ts";
import {
  CHART_LIMITS,
  foldKey,
  handleProblem,
  indexProblems,
  placeSettingsFindings,
  preflight,
  problemCountOf,
  referenceWarnings,
  reservedAddresses,
  unitKeyProblem,
} from "./problems.ts";
import { chartOf, fixtureChart, fixtureSettings } from "./testkit.ts";

const base = () => fromChart(fixtureSettings(), fixtureChart());

function run(draft: Draft, ...intents: Intent[]): Draft {
  for (const intent of intents) {
    const result = record(draft, intent);
    if (!result.ok) throw new Error(`${intent.type}: ${result.message}`);
    draft = apply(draft, result.op).draft;
  }
  return draft;
}

/** A draft's problems as [node, field, kind], warnings left out. */
const problemsOf = (draft: Draft, against: Draft | null = null) =>
  preflight(draft, against)
    .filter((p) => p.severity === "problem")
    .map((p) => [p.node, p.field.join("."), p.kind]);

describe("an address", () => {
  test("a handle is one run of lowercase letters, digits and hyphens, and never a reserved word", () => {
    expect(handleProblem("dev-2")).toBeNull();
    for (const bad of ["", "Dev", "jane.doe", "ci:release", "-x", "none", "root", "x".repeat(65)]) {
      expect(handleProblem(bad), bad).not.toBeNull();
    }
  });

  test("a unit key carries nothing that splits a subject, and is written folded", () => {
    expect(unitKeyProblem("go-to-market")).toBeNull();
    expect(foldKey("  Go  To Market ")).toBe("go-to-market");
    for (const bad of ["", "a b", "a/b", "a*", "a>", "Sales", "tree"]) {
      expect(unitKeyProblem(bad), bad).not.toBeNull();
    }
    // A unit may take `none`, which only a seat reserves.
    expect(unitKeyProblem("none")).toBeNull();
  });
});

describe("the draft's own shape", () => {
  test("a clean draft has no problems", () => {
    expect(problemsOf(base())).toEqual([]);
  });

  test("a bad or repeated address, an unknown kind and a field past its cap are problems on their node", () => {
    const draft = run(
      base(),
      {
        type: "addSeat",
        key: "new:a",
        placement: { parent: "unit:sales" },
        data: { handle: "Bad.Handle", name: "A", kind: "robot" },
      },
      {
        type: "updateSeat",
        target: "seat:dev",
        set: [{ path: ["goal"], value: "x".repeat(CHART_LIMITS.prose + 1) }],
      },
    );
    // In walk order: the root's seats, then each unit's, depth first.
    expect(problemsOf(draft)).toEqual([
      ["seat:dev", "goal", "out_of_range"],
      ["new:a", "handle", "invalid"],
      ["new:a", "kind", "unknown_value"],
    ]);
  });

  test("what a seat's kind forbids is a problem on the field that carries it", () => {
    // A kind change strips these; a draft that set the kind another way still holds them.
    const draft: Draft = {
      ...base(),
      roles: [
        {
          key: "seat:ceo",
          data: { handle: "ceo", name: "CEO", kind: "human", runtime: { llm: "fast" } },
        },
      ],
    };
    expect(problemsOf(draft)).toEqual([["seat:ceo", "runtime.llm", "conflict"]]);
  });

  test("a reference naming nothing is a warning, and a retired address still names what used to answer to it", () => {
    const draft = fromChart(
      null,
      chartOf({
        units: [{ key: "ops", name: "Ops", lead: "boss" }],
        seats: [
          { handle: "lead", name: "Lead", former_handles: ["boss"] },
          { handle: "ceo", name: "CEO" },
        ],
        manages: { ceo: ["lead", "ghost"] },
      }),
    );
    expect(referenceWarnings(draft).map((w) => [w.node, w.field.join(".")])).toEqual([
      ["seat:ceo", "manages.1"],
    ]);
    // Control: without the seat the alias answered to, the lead names nobody.
    const unanswered: Draft = {
      ...draft,
      roles: draft.roles.filter((s) => s.key !== "seat:lead"),
    };
    expect(referenceWarnings(unanswered).map((w) => [w.node, w.field.join(".")])).toEqual([
      ["seat:ceo", "manages.0"],
      ["seat:ceo", "manages.1"],
      ["unit:ops", "lead"],
    ]);
  });
});

describe("the addresses the chart would refuse", () => {
  const messages = (draft: Draft, against: Draft) =>
    reservedAddresses(draft, against).map((r) => [r.node, r.message.split(",")[0]]);

  test("a removed node's address is never given to anything else", () => {
    const b = base();
    const draft = run(
      b,
      { type: "remove", target: "seat:designer" },
      {
        type: "addSeat",
        key: "new:d",
        placement: { parent: COMPANY_KEY },
        data: { handle: "designer", name: "New designer" },
      },
    );
    expect(messages(draft, b)).toEqual([
      ["new:d", "The handle designer belongs to a seat this draft removes"],
    ]);
  });

  test("a renamed node's identity is never given to anything else, but it may take it back", () => {
    // `chief-tech` was created as `cto`: its key says so.
    const b = fromChart(
      null,
      chartOf({
        seats: [
          { handle: "chief-tech", name: "CTO", origin_handle: "cto" },
          { handle: "ceo", name: "CEO" },
        ],
      }),
    );
    const taken = run(b, {
      type: "addSeat",
      key: "new:c",
      placement: { parent: COMPANY_KEY },
      data: { handle: "cto", name: "Another" },
    });
    expect(messages(taken, b)).toEqual([
      ["new:c", "The handle cto is the address @chief-tech was created under"],
    ]);
    const renamed = run(b, {
      type: "updateSeat",
      target: "seat:ceo",
      set: [{ path: ["handle"], value: "cto" }],
    });
    expect(messages(renamed, b)).toHaveLength(1);
    // Control: the seat renaming back to its own identity collides with nothing.
    const back = run(b, {
      type: "updateSeat",
      target: "seat:cto",
      set: [{ path: ["handle"], value: "cto" }],
    });
    expect(messages(back, b)).toEqual([]);
  });

  test("a retired alias may be taken by a creation, and not by a rename", () => {
    const b = fromChart(
      null,
      chartOf({
        seats: [
          { handle: "lead", name: "Lead", former_handles: ["boss"] },
          { handle: "ceo", name: "CEO" },
        ],
      }),
    );
    const created = run(b, {
      type: "addSeat",
      key: "new:b",
      placement: { parent: COMPANY_KEY },
      data: { handle: "boss", name: "Boss" },
    });
    expect(messages(created, b)).toEqual([]);
    const renamed = run(b, {
      type: "updateSeat",
      target: "seat:ceo",
      set: [{ path: ["handle"], value: "boss" }],
    });
    expect(messages(renamed, b)).toEqual([["seat:ceo", "The handle boss still reaches @lead"]]);
  });

  test("preflight names them on the node's own address field", () => {
    const b = base();
    const draft = run(
      b,
      { type: "remove", target: "unit:sales" },
      {
        type: "addUnit",
        key: "new:s",
        placement: { parent: COMPANY_KEY },
        data: { key: "sales", name: "Sales again" },
      },
    );
    expect(problemsOf(draft, b)).toEqual([["new:s", "key", "conflict"]]);
    // Control: without the chart it was made on, nothing is reserved.
    expect(problemsOf(draft)).toEqual([]);
  });
});

describe("a settings dry run's findings", () => {
  const problem = (segments: (string | number)[] | null): ConfigProblem => ({
    path: "",
    segments,
    kind: "invalid",
    message: `problem at ${JSON.stringify(segments)}`,
  });
  const warning = (segments: (string | number)[] | null): ConfigWarning => ({
    kind: "unused",
    ref: "",
    path: "",
    segments,
    seat: "",
    unit: "",
    from: "",
    to: "",
    message: "a warning",
  });

  test("a charter field's go to the company node, and what the builder cannot fix is linked to where it is fixed", () => {
    const placed = placeSettingsFindings({
      problems: [
        problem(["name"]),
        problem(["policies", 0]),
        problem(["integrations", "datadog", "route_to"]),
        problem(["scheduling", "timezone"]),
        problem(["providers", "llm"]),
        problem(null),
      ],
      warnings: [warning(["mission"])],
    });
    expect(placed.map((p) => [p.severity, p.node, p.link])).toEqual([
      ["problem", COMPANY_KEY, null],
      ["problem", COMPANY_KEY, null],
      ["problem", null, "integrations"],
      ["problem", null, "schedules"],
      ["problem", null, null],
      ["problem", null, null],
      ["warning", COMPANY_KEY, null],
    ]);
    const index = indexProblems(placed);
    expect(index.problemCount).toBe(6);
    expect(index.warningCount).toBe(1);
    expect(problemCountOf(index, COMPANY_KEY)).toBe(2);
    expect(index.document).toHaveLength(4);
  });
});
