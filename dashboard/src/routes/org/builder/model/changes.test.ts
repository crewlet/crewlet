// @vitest-environment node
/**
 * Changes and their consequences.
 *
 * What these protect: changes are read by comparing the base chart with the
 * draft node by node, matched by key, so "the same seat" is the seat the chart
 * holds under that address and never a seat that shares its name; a name and
 * an address are reported apart; what follows from the tree itself (the agent
 * seats that onboard again, the tool credential servers a seat's unit hands
 * it) is reported even though no operation says so, and ONLY what the engine's
 * own marker keys on re-onboards anybody; and what only the log can say
 * (stripped fields, cleared references) is read from it.
 */

import { describe, expect, test } from "vitest";
import { fromChart } from "./document.ts";
import { EMPTY_DRAFT, type Draft } from "./draft.ts";
import { apply, record, type ApplyReport, type Intent, type Operation } from "./operations.ts";
import { deriveChanges } from "./changes.ts";
import { countingKeys, fixtureChart, fixtureSettings } from "./testkit.ts";
import { templateIntent } from "./templates.ts";

interface Scenario {
  base: Draft;
  draft: Draft;
  ops: Operation[];
  reports: ApplyReport[];
}

function scenario(
  intents: Intent[],
  base: Draft = fromChart(fixtureSettings(), fixtureChart()),
): Scenario {
  let draft = base;
  const ops: Operation[] = [];
  const reports: ApplyReport[] = [];
  for (const intent of intents) {
    const result = record(draft, intent);
    if (!result.ok) throw new Error(`${intent.type}: ${result.message}`);
    const applied = apply(draft, result.op);
    ops.push(result.op);
    reports.push(applied.report);
    draft = applied.draft;
  }
  return { base, draft, ops, reports };
}

const changesOf = (s: Scenario) =>
  deriveChanges({ base: s.base, next: s.draft, ops: s.ops, reports: s.reports });

const names = (refs: readonly { name: string }[]) => refs.map((r) => r.name);

describe("structure", () => {
  test("reports what was added, removed, renamed, moved and edited, matched by key", () => {
    const changes = changesOf(
      scenario([
        {
          type: "addSeat",
          key: "new:qa",
          placement: { parent: "unit:sales" },
          data: { handle: "qa", name: "QA" },
        },
        { type: "remove", target: "seat:designer" },
        { type: "renameUnit", target: "unit:sales", name: "Revenue" },
        { type: "move", target: "seat:dev", to: { parent: "unit:platform" } },
        {
          type: "updateSeat",
          target: "seat:sre",
          set: [
            { path: ["goal"], value: "Stay up" },
            { path: ["runtime", "llm"], value: "fast" },
          ],
        },
      ]),
    );
    expect(names(changes.added)).toEqual(["QA"]);
    expect(names(changes.removed)).toEqual(["Designer"]);
    expect(changes.renamed).toEqual([
      {
        ref: { key: "unit:sales", kind: "unit", name: "Revenue" },
        before: "Sales",
        after: "Revenue",
      },
    ]);
    expect(changes.moved.map((m) => [m.ref.name, m.from.name, m.to.name])).toEqual([
      ["Dev", "Engineering", "Platform"],
    ]);
    // One level into the runtime half, by the name a person reads.
    expect(changes.edited).toContainEqual({
      ref: { key: "seat:sre", kind: "seat", name: "SRE" },
      fields: ["goal", "llm"],
    });
    expect(changes.summary).toBe(
      "Added 1 seat, removed 1 seat, renamed 1 unit, moved 1 seat, edited 2 seats",
    );
  });

  test("a name and an address are reported apart, and neither is an edited field", () => {
    const changes = changesOf(
      scenario([
        { type: "renameSeat", target: "seat:dev", name: "Developer" },
        { type: "updateSeat", target: "seat:dev", set: [{ path: ["handle"], value: "developer" }] },
        { type: "updateUnit", target: "unit:sales", set: [{ path: ["key"], value: "revenue" }] },
      ]),
    );
    expect(changes.renamed.map((r) => [r.before, r.after])).toEqual([["Dev", "Developer"]]);
    expect(changes.addressChanges.map((r) => [r.ref.key, r.before, r.after])).toEqual([
      ["seat:dev", "dev", "developer"],
      ["unit:sales", "sales", "revenue"],
    ]);
    // The follow moved the reference that named the seat, which IS an edit of its holder.
    expect(changes.edited).toEqual([
      {
        ref: { key: "seat:vp-engineering", kind: "seat", name: "VP Engineering" },
        fields: ["manages"],
      },
    ]);
  });

  test("more than half the base's seats removed asks for acknowledgement", () => {
    const most = changesOf(scenario([{ type: "remove", target: "unit:engineering" }]));
    expect(most.massRemoval).toEqual({ removed: 4, total: 6 });
    expect(most.acknowledgements).toContain("mass_removal");
    // Control: half is not more than half.
    const half = changesOf(
      scenario([
        { type: "remove", target: "unit:platform" },
        { type: "remove", target: "seat:dev" },
      ]),
    );
    expect(half.massRemoval).toBeNull();
  });
});

describe("onboarding and credentials", () => {
  test("a company rename re-onboards every agent seat and asks for acknowledgement", () => {
    const changes = changesOf(
      scenario([{ type: "updateCompany", set: [{ path: ["name"], value: "Acme Corp" }] }]),
    );
    expect(changes.companyRename).toEqual({ before: "Acme", after: "Acme Corp" });
    expect(changes.onboarding).toEqual([
      {
        cause: "company_rename",
        seats: expect.arrayContaining([expect.objectContaining({ key: "seat:dev" })]),
      },
    ]);
    expect(changes.onboarding[0]!.seats).toHaveLength(6);
    expect(changes.acknowledgements).toContain("company_rename");
  });

  // THE ENGINE'S MARKER HASHES IDENTITIES, never names or addresses: a seat's
  // own identity is the address it was created under, and each unit's the key
  // it was created under. A rename of either keeps both.
  test("renaming a seat, a unit or either's address re-onboards nobody", () => {
    const changes = changesOf(
      scenario([
        { type: "renameSeat", target: "seat:dev", name: "Developer" },
        { type: "updateSeat", target: "seat:dev", set: [{ path: ["handle"], value: "developer" }] },
        { type: "renameUnit", target: "unit:engineering", name: "R&D" },
        { type: "updateUnit", target: "unit:platform", set: [{ path: ["key"], value: "infra" }] },
      ]),
    );
    expect(changes.onboarding).toEqual([]);
    // A unit rename is still said, because its onboarding pages are found by name.
    expect(changes.unitRenames.map((r) => [r.before, r.after])).toEqual([["Engineering", "R&D"]]);
  });

  test("a move re-onboards the seats it puts under other units, and changes the servers a unit hands its members", () => {
    const moved = changesOf(
      scenario([{ type: "move", target: "seat:dev", to: { parent: "unit:sales" } }]),
    );
    expect(moved.onboarding).toEqual([
      { cause: "move", seats: [{ key: "seat:dev", kind: "seat", name: "Dev" }] },
    ]);
    // Engineering's `mcp_env` reaches its DIRECT agent members only.
    expect(moved.credentialServers).toEqual([
      { ref: { key: "seat:dev", kind: "seat", name: "Dev" }, gained: [], lost: ["tracker"] },
    ]);
    expect(moved.acknowledgements).toContain("credential_servers");

    // A unit's move takes every agent seat below it under other units too.
    const unit = changesOf(
      scenario([{ type: "move", target: "unit:platform", to: { parent: "unit:sales" } }]),
    );
    expect(unit.onboarding[0]!.seats.map((s) => s.key).sort()).toEqual([
      "seat:designer",
      "seat:sre",
    ]);
    // Control: a seat in a unit that did not move stays.
    expect(unit.onboarding[0]!.seats.map((s) => s.key)).not.toContain("seat:dev");
    // Platform's members were never handed Engineering's servers.
    expect(unit.credentialServers).toEqual([]);
  });
});

describe("what only the log says", () => {
  test("fields a kind change removed, credentials called out, and the kind change acknowledged", () => {
    let base = fromChart(fixtureSettings(), fixtureChart());
    base = scenario(
      [
        {
          type: "updateSeat",
          target: "seat:dev",
          set: [{ path: ["runtime", "mcp_env"], value: { github: { TOKEN: "${GH}" } } }],
        },
      ],
      base,
    ).draft;
    const changes = changesOf(
      scenario([{ type: "changeKind", target: "seat:dev", kind: "human" }], base),
    );
    expect(changes.strippedFields).toEqual([
      {
        ref: { key: "seat:dev", kind: "seat", name: "Dev" },
        fields: [
          { name: "llm", credential: false },
          { name: "mcp_env", credential: true },
        ],
      },
    ]);
    expect(changes.acknowledgements).toEqual(["kind_change", "credential_servers"]);
  });

  test("references a removal cleared, and the integration entries keyed by handle", () => {
    const changes = changesOf(
      scenario([
        { type: "remove", target: "seat:sre", routeTo: "ceo" },
        { type: "remove", target: "seat:dev" },
      ]),
    );
    expect(changes.clearedReferences.map((c) => [c.kind, c.holder.name, c.from])).toEqual([
      ["gitlab_access_level", "Acme", "sre"],
      ["manages", "VP Engineering", "dev"],
      ["gitlab_access_level", "Acme", "dev"],
    ]);
    expect(changes.datadogFallback).toEqual({ before: "sre", after: "ceo" });
    expect(changes.gitlabAccessLevels).toEqual([
      { handle: "dev", before: "developer", after: null },
      { handle: "sre", before: "maintainer", after: null },
    ]);
  });
});

describe("a create draft", () => {
  test("reads as the company it creates", () => {
    const built = templateIntent(
      { template: "new_company", charter: { name: "Acme" } },
      countingKeys(),
    );
    if (!built.ok) throw new Error(built.message);
    const changes = changesOf(scenario([built.intent], EMPTY_DRAFT));
    expect(changes.added.length).toBeGreaterThan(0);
    expect(changes.removed).toEqual([]);
    // Nothing had onboarded, so nothing re-onboards.
    expect(changes.onboarding).toEqual([]);
    expect(changes.companyRename).toBeNull();
    expect(changes.summary).toBe("Created Acme in the organization builder");
  });
});
