// @vitest-environment node
/**
 * Changes and their consequences.
 *
 * What these protect: consequences come from comparing the two derived
 * organizations (so a reorder that changes a primary manager, a lead change
 * that moves unrouted work, or a unit rename that re-onboards a subtree are
 * all reported even though no operation says so), matched by key; what only
 * the log can say (stripped fields, cleared references) is read from it; and a
 * side without a derivation reports no derived consequence rather than a
 * guess.
 */

import { describe, expect, test } from "vitest";
import type { CompanyDocument, Derived } from "~/protocol/index.ts";
import { fromDocument, toDocument } from "./document.ts";
import { EMPTY_DRAFT, type Draft } from "./draft.ts";
import { apply, record, type ApplyReport, type Intent, type Operation } from "./operations.ts";
import { deriveChanges, type ChangeInputs } from "./changes.ts";
import { fixtureCompany, fixtureDerived, type DerivedOverrides } from "./testkit.ts";
import { templateIntent } from "./templates.ts";
import { countingKeys } from "./testkit.ts";

interface Scenario {
  base: Draft;
  baseDerived: Derived;
  draft: Draft;
  ops: Operation[];
  reports: ApplyReport[];
}

function scenario(
  intents: Intent[],
  doc: CompanyDocument = fixtureCompany(),
  overrides: DerivedOverrides = {},
): Scenario {
  const baseDerived = fixtureDerived(doc, overrides);
  const base = fromDocument(doc, baseDerived);
  let draft = base;
  const ops: Operation[] = [];
  const reports: ApplyReport[] = [];
  for (const intent of intents) {
    const result = record(draft, intent, { handleOf: () => "new-seat" });
    if (!result.ok) throw new Error(`${intent.type}: ${result.message}`);
    const applied = apply(draft, result.op);
    ops.push(result.op);
    reports.push(applied.report);
    draft = applied.draft;
  }
  return { base, baseDerived, draft, ops, reports };
}

function changesOf(
  s: Scenario,
  nextOverrides: DerivedOverrides = {},
  extra: Partial<ChangeInputs> = {},
) {
  return deriveChanges({
    base: { draft: s.base, derived: s.baseDerived },
    next: { draft: s.draft, derived: fixtureDerived(toDocument(s.draft).document, nextOverrides) },
    ops: s.ops,
    reports: s.reports,
    ...extra,
  });
}

const names = (refs: readonly { name: string }[]) => refs.map((r) => r.name);

describe("structure", () => {
  test("reports what was added, removed, renamed, moved and edited, matched by key", () => {
    const s = scenario([
      {
        type: "addSeat",
        key: "new:qa",
        placement: { parent: "unit:Engineering", after: null },
        data: { name: "QA" },
      },
      { type: "remove", target: "unit:Sales" },
      { type: "renameSeat", target: "seat:dev", name: "Developer" },
      { type: "move", target: "seat:sre", to: { parent: "unit:Engineering", after: null } },
      {
        type: "updateSeat",
        target: "seat:ceo",
        set: [
          { path: ["goal"], value: "Grow" },
          { path: ["integrations", "github", "tier"], value: "developer" },
        ],
      },
      { type: "updateCompany", set: [{ path: ["mission"], value: "Make better things." }] },
      {
        type: "updateSeat",
        target: "seat:sre",
        set: [{ path: ["integrations", "github", "tier"], value: "maintainer" }],
      },
    ]);
    const changes = changesOf(s);
    expect(names(changes.added)).toEqual(["QA"]);
    expect(names(changes.removed)).toEqual(["Account Executive", "Sales"]);
    expect(changes.renamed.map((r) => [r.before, r.after])).toEqual([["Dev", "Developer"]]);
    expect(changes.moved.map((m) => [m.ref.name, m.from.name, m.to.name])).toEqual([
      ["SRE", "Platform", "Engineering"],
    ]);
    expect(changes.edited).toEqual(
      expect.arrayContaining([
        {
          ref: { key: "seat:ceo", kind: "seat", name: "CEO" },
          fields: ["goal", "integrations.github"],
        },
        { ref: { key: "seat:dev", kind: "seat", name: "Developer" }, fields: ["handle"] },
        // Its Jira project is untouched, so only the tool that changed is named.
        { ref: { key: "seat:sre", kind: "seat", name: "SRE" }, fields: ["integrations.github"] },
      ]),
    );
    expect(changes.charter).toEqual(["mission"]);
    expect(changes.summary).toBe(
      "Added 1 seat, removed 1 seat and 1 unit, renamed 1 seat, moved 1 seat, edited 4 seats, edited the charter",
    );
  });

  test("a root seat whose unit reference became the same physical placement has not moved", () => {
    const placedBase = { seats: { "roles[1]": { unit_path: "units[0].children[0]" } } };
    const s = scenario(
      [{ type: "move", target: "seat:designer", to: { parent: "unit:Platform", after: null } }],
      fixtureCompany(),
      placedBase,
    );
    const changes = changesOf(s);
    expect(changes.moved).toEqual([]);
    expect(changes.clearedReferences).toEqual([
      {
        kind: "unit",
        holder: { key: "seat:designer", kind: "seat", name: "Designer" },
        from: "Platform",
      },
    ]);
  });

  test("more than half the base's seats removed asks for acknowledgement", () => {
    const changes = changesOf(
      scenario([
        { type: "remove", target: "unit:Engineering" },
        { type: "remove", target: "unit:Sales" },
      ]),
    );
    expect(changes.massRemoval).toEqual({ removed: 4, total: 6 });
    expect(changes.acknowledgements).toContain("mass_removal");
    expect(
      changesOf(scenario([{ type: "remove", target: "unit:Engineering" }])).massRemoval,
    ).toBeNull();
  });
});

describe("identity and onboarding", () => {
  test("a company rename re-onboards every agent seat and asks for acknowledgement", () => {
    const changes = changesOf(
      scenario([{ type: "updateCompany", set: [{ path: ["name"], value: "Acme Labs" }] }]),
    );
    expect(changes.companyRename).toEqual({ before: "Acme", after: "Acme Labs" });
    expect(changes.onboarding.map((g) => [g.cause, g.seats.length])).toEqual([
      ["company_rename", 6],
    ]);
    expect(changes.acknowledgements).toEqual(["company_rename"]);
  });

  test("a seat rename keeps its handle but re-onboards that seat", () => {
    const changes = changesOf(
      scenario([{ type: "renameSeat", target: "seat:dev", name: "Developer" }]),
    );
    expect(changes.handleChanges).toEqual([]);
    expect(changes.onboarding).toEqual([
      { cause: "seat_rename", seats: [{ key: "seat:dev", kind: "seat", name: "Developer" }] },
    ]);
  });

  test("a unit rename re-onboards its subtree, re-keys its schedules and reads onboarding pages under the new name", () => {
    const changes = changesOf(
      scenario([{ type: "renameUnit", target: "unit:Engineering", name: "Platform Engineering" }]),
    );
    expect(changes.onboarding).toEqual([{ cause: "unit_rename", seats: expect.any(Array) }]);
    expect(names(changes.onboarding[0]!.seats)).toEqual(["VP Engineering", "Dev", "SRE"]);
    expect(changes.unitRenames).toEqual([
      {
        ref: { key: "unit:Engineering", kind: "unit", name: "Platform Engineering" },
        before: "Engineering",
        after: "Platform Engineering",
        schedules: ["standup"],
      },
    ]);
  });

  test("a move re-onboards the seat and changes the tool credential servers it inherits", () => {
    const changes = changesOf(
      scenario([{ type: "move", target: "seat:dev", to: { parent: "unit:Sales", after: null } }]),
    );
    expect(changes.onboarding).toEqual([
      { cause: "move", seats: [{ key: "seat:dev", kind: "seat", name: "Dev" }] },
    ]);
    expect(changes.credentialServers).toEqual([
      { ref: { key: "seat:dev", kind: "seat", name: "Dev" }, gained: [], lost: ["tracker"] },
    ]);
    expect(changes.acknowledgements).toContain("credential_servers");
  });

  test("a handle the engine derives differently is a handle change", () => {
    const s = scenario([
      { type: "updateSeat", target: "seat:ceo", set: [{ path: ["goal"], value: "x" }] },
    ]);
    const changes = changesOf(s, { seats: { "roles[0]": { handle: "chief" } } });
    expect(changes.handleChanges).toEqual([
      { ref: { key: "seat:ceo", kind: "seat", name: "CEO" }, before: "ceo", after: "chief" },
    ]);
    expect(changes.acknowledgements).toContain("handle_change");
  });

  test("an added seat given a removed seat's handle reuses its memory, from this draft or recent history", () => {
    const s = scenario([
      { type: "remove", target: "seat:dev" },
      {
        type: "addSeat",
        key: "new:dev",
        placement: { parent: "unit:Sales", after: null },
        data: { name: "Dev" },
      },
      {
        type: "addSeat",
        key: "new:old",
        placement: { parent: "unit:Sales", after: null },
        data: { name: "Old Hand" },
      },
    ]);
    const changes = changesOf(s, {}, { recentlyRemoved: new Map([["old-hand", "Old Hand"]]) });
    expect(changes.memoryReuse.map((m) => [m.ref.name, m.handle, m.previous])).toEqual([
      ["Old Hand", "old-hand", "Old Hand"],
      ["Dev", "dev", "Dev"],
    ]);
  });
});

describe("reporting, leads, channels and routing", () => {
  const managed: DerivedOverrides = {
    seats: {
      "units[0].roles[1]": { manager: "vp-engineering", managers: ["vp-engineering", "ceo"] },
    },
  };

  test("a primary manager change is reported, and marked when only the order of the same managers changed", () => {
    const s = scenario(
      [{ type: "updateSeat", target: "seat:dev", set: [{ path: ["goal"], value: "x" }] }],
      fixtureCompany(),
      managed,
    );
    const orderOnly = changesOf(s, {
      seats: { "units[0].roles[1]": { manager: "ceo", managers: ["ceo", "vp-engineering"] } },
    });
    expect(orderOnly.reportsTo).toEqual([
      {
        ref: { key: "seat:dev", kind: "seat", name: "Dev" },
        before: { key: "seat:vp-engineering", kind: "seat", name: "VP Engineering" },
        after: { key: "seat:ceo", kind: "seat", name: "CEO" },
        orderOnly: true,
      },
    ]);
    const lost = changesOf(s, { seats: { "units[0].roles[1]": { manager: "", managers: null } } });
    expect(lost.reportsTo).toEqual([expect.objectContaining({ after: null, orderOnly: false })]);
  });

  test("effective lead and channel changes are reported with whether each was inherited", () => {
    const s = scenario([{ type: "setLead", target: "unit:Engineering" }], fixtureCompany(), {
      units: {
        "units[0].children[0]": {
          lead: "vp-engineering",
          lead_inherited: true,
          channel: "eng",
          channel_inherited: true,
        },
      },
    });
    const changes = changesOf(s, {
      units: {
        "units[0].children[0]": {
          lead: "",
          lead_inherited: false,
          channel: "",
          channel_inherited: false,
        },
      },
    });
    expect(
      changes.leads.map((l) => [
        l.ref.name,
        l.before?.name ?? null,
        l.beforeInherited,
        l.after?.name ?? null,
      ]),
    ).toEqual([
      ["Engineering", "VP Engineering", false, null],
      ["Platform", "VP Engineering", true, null],
    ]);
    // The same seat leading, but now by inheritance rather than by its own
    // declaration, is a change too: clearing the parent's lead would take it.
    const inherited = changesOf(s, {
      units: {
        "units[0]": { lead: "vp-engineering", lead_inherited: true },
        "units[0].children[0]": {
          lead: "vp-engineering",
          lead_inherited: true,
          channel: "eng",
          channel_inherited: true,
        },
      },
    });
    expect(
      inherited.leads.map((l) => [
        l.ref.name,
        l.before?.name,
        l.beforeInherited,
        l.after?.name,
        l.afterInherited,
      ]),
    ).toEqual([["Engineering", "VP Engineering", false, "VP Engineering", true]]);
    expect(changes.channels).toEqual([
      {
        ref: { key: "unit:Platform", kind: "unit", name: "Platform" },
        before: "eng",
        beforeInherited: true,
        after: "",
        afterInherited: false,
      },
    ]);
  });

  test("unrouted tracker work follows a declaring unit's lead and a root seat's own declaration", () => {
    const doc = fixtureCompany();
    doc.units![0]!.children![0]!.integrations = { jira: { project: "ops" } };
    doc.roles![0]!.integrations = { confluence: { space: "LEAD" } };
    const s = scenario([{ type: "remove", target: "seat:ceo" }], doc, {
      units: { "units[0].children[0]": { lead: "sre" } },
    });
    const changes = changesOf(s, {
      units: { "units[0].children[0]": { lead: "vp-engineering", lead_inherited: true } },
    });
    expect(changes.routing).toEqual([
      {
        tool: "confluence",
        scope: "LEAD",
        holder: { key: "seat:ceo", kind: "seat", name: "CEO" },
        before: { key: "seat:ceo", kind: "seat", name: "CEO" },
        after: null,
        shared: false,
      },
      {
        tool: "jira",
        scope: "OPS",
        holder: { key: "unit:Platform", kind: "unit", name: "Platform" },
        before: { key: "seat:sre", kind: "seat", name: "SRE" },
        after: { key: "seat:vp-engineering", kind: "seat", name: "VP Engineering" },
        shared: false,
      },
    ]);
  });

  test("a scope declared with different owners is marked shared, since the engine picks one and logs the ambiguity", () => {
    const doc = fixtureCompany();
    doc.units![0]!.children![0]!.integrations = { jira: { project: "OPS" } };
    doc.units![1]!.integrations = { jira: { project: "ops" } };
    doc.units![1]!.lead = "Account Executive";
    const s = scenario([{ type: "setLead", target: "unit:Sales" }], doc, {
      units: { "units[0].children[0]": { lead: "sre" } },
    });
    const changes = changesOf(s, {
      units: { "units[0].children[0]": { lead: "sre" }, "units[1]": { lead: "" } },
    });
    expect(changes.routing).toEqual([
      expect.objectContaining({
        tool: "jira",
        scope: "OPS",
        holder: { key: "unit:Sales", kind: "unit", name: "Sales" },
        before: { key: "seat:account-executive", kind: "seat", name: "Account Executive" },
        after: null,
        shared: true,
      }),
    ]);
  });
});

describe("what only the log says", () => {
  test("fields a kind change removed, credentials called out, and the kind change acknowledged", () => {
    const doc = fixtureCompany();
    doc.units![0]!.roles![1]!.mcp_env = { git: { TOKEN: "__redacted__" } };
    doc.units![0]!.roles![1]!.llm = "default";
    const s = scenario(
      [{ type: "changeKind", target: "seat:dev", kind: "human", contact: { github_login: "dev" } }],
      doc,
    );
    const changes = changesOf(s);
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
    const s = scenario([{ type: "remove", target: "seat:sre", routeTo: "dev" }]);
    const changes = changesOf(s);
    expect(changes.clearedReferences).toEqual([
      {
        kind: "gitlab_access_level",
        holder: { key: "company", kind: "company", name: "Acme" },
        from: "sre",
      },
    ]);
    expect(changes.datadogFallback).toEqual({ before: "sre", after: "dev" });
    expect(changes.gitlabAccessLevels).toEqual([
      { handle: "sre", before: "maintainer", after: null },
    ]);
  });
});

describe("without a derivation", () => {
  test("structure is still reported and every derived consequence is left out, not guessed", () => {
    const s = scenario([
      { type: "updateCompany", set: [{ path: ["name"], value: "Renamed" }] },
      { type: "move", target: "seat:dev", to: { parent: "unit:Sales", after: null } },
    ]);
    const changes = deriveChanges({
      base: { draft: s.base, derived: s.baseDerived },
      next: { draft: s.draft, derived: null },
      ops: s.ops,
      reports: s.reports,
    });
    expect(changes.derivedKnown).toBe(false);
    expect(changes.moved).toHaveLength(1);
    expect(changes.companyRename).not.toBeNull();
    expect(changes.onboarding).toEqual([]);
    expect(changes.credentialServers).toEqual([]);
    expect(changes.reportsTo).toEqual([]);
  });

  test("an empty base needs no derivation, and a template reads as the company it creates", () => {
    const built = templateIntent(
      { template: "new_company", charter: { name: "Acme" } },
      countingKeys(),
    );
    if (!built.ok) throw new Error(built.message);
    const recorded = record(EMPTY_DRAFT, built.intent);
    if (!recorded.ok) throw new Error(recorded.message);
    const { draft, report } = apply(EMPTY_DRAFT, recorded.op);
    const changes = deriveChanges({
      base: { draft: EMPTY_DRAFT, derived: null },
      next: { draft, derived: fixtureDerived(toDocument(draft).document) },
      ops: [recorded.op],
      reports: [report],
    });
    expect(changes.derivedKnown).toBe(true);
    expect(changes.companyRename).toBeNull();
    expect(changes.added).toHaveLength(7);
    expect(changes.summary).toBe("Created Acme in the organization builder");
  });
});
