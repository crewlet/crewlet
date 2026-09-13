// @vitest-environment node
/**
 * Operations: recorded with their preconditions, evaluated, applied.
 *
 * The invariants: recording captures what each change replaced; applying
 * changes only what the operation names and follows or clears the references
 * that named what it renamed or removed, reporting each; an operation whose
 * preconditions no longer hold is refused with every value a person needs; and
 * a field another operation owns cannot be written around its rules.
 */

import { describe, expect, test } from "vitest";
import type { CompanyDocument } from "~/protocol/index.ts";
import { cloneJson, getPath } from "./json.ts";
import { COMPANY_KEY, mintKey, seatPathKey, type NodeKey } from "./keys.ts";
import { locate, type Draft } from "./draft.ts";
import { fromDocument, toDocument } from "./document.ts";
import {
  ApplyError,
  apply,
  describeOperation,
  evaluate,
  intentOf,
  malformedReason,
  record,
  touchedKeys,
  type Intent,
  type Operation,
  type RecordContext,
} from "./operations.ts";
import { countingKeys, fixtureCompany, fixtureDerived } from "./testkit.ts";

function fixture(doc: CompanyDocument = fixtureCompany()): Draft {
  return fromDocument(doc, fixtureDerived(doc));
}

function recordOk(draft: Draft, intent: Intent, ctx: RecordContext = {}): Operation {
  const result = record(draft, intent, ctx);
  if (!result.ok)
    throw new Error(`expected ${intent.type} to record: ${result.refusal}: ${result.message}`);
  return result.op;
}

function run(draft: Draft, intent: Intent, ctx: RecordContext = {}) {
  const op = recordOk(draft, intent, ctx);
  return { op, ...apply(draft, op) };
}

const doc = (draft: Draft) => toDocument(draft).document;
const seat = (draft: Draft, key: NodeKey) => {
  const found = locate(draft, key);
  if (found?.kind !== "seat") throw new Error(`no seat ${key}`);
  return found.node.data;
};
const unit = (draft: Draft, key: NodeKey) => {
  const found = locate(draft, key);
  if (found?.kind !== "unit") throw new Error(`no unit ${key}`);
  return found.node.data;
};

describe("adding", () => {
  test("a seat lands after the named neighbour, keyed by the key the handler minted", () => {
    const draft = fixture();
    const key = mintKey(countingKeys());
    const { draft: next } = run(draft, {
      type: "addSeat",
      key,
      placement: { parent: "unit:Engineering", after: "seat:vp-engineering" },
      data: { name: "QA" },
    });
    expect(doc(next).units![0]!.roles!.map((r) => r.name)).toEqual(["VP Engineering", "QA", "Dev"]);
    expect(locate(next, key)?.parent).toBe("unit:Engineering");
  });

  test("a key that was not minted, or already names a node, is refused", () => {
    const draft = fixture();
    const placement = { parent: COMPANY_KEY, after: null };
    expect(
      record(draft, { type: "addSeat", key: "seat:x", placement, data: { name: "X" } }),
    ).toMatchObject({ refusal: "not_minted" });
    const key = mintKey(countingKeys());
    const added = run(draft, { type: "addSeat", key, placement, data: { name: "X" } }).draft;
    expect(record(added, { type: "addSeat", key, placement, data: { name: "Y" } })).toMatchObject({
      refusal: "key_in_use",
    });
  });

  test("a unit never carries lists into its own data", () => {
    const op = recordOk(fixture(), {
      type: "addUnit",
      key: "new:u",
      placement: { parent: COMPANY_KEY, after: null },
      data: { name: "Legal", roles: [{ name: "Counsel" }] },
    });
    expect(op.type === "addUnit" && op.data).toEqual({ name: "Legal" });
  });

  test("a neighbour that is not beside the slot is refused, or with leniency lands at the end", () => {
    const draft = fixture();
    const intent: Intent = {
      type: "addSeat",
      key: "new:s",
      placement: { parent: "unit:Sales", after: "seat:dev" },
      data: { name: "S" },
    };
    expect(record(draft, intent)).toMatchObject({ refusal: "missing_neighbour" });
    const lenient = record(draft, intent, {}, { lenientPlacement: true });
    expect(lenient.ok && lenient.op.type === "addSeat" && lenient.op.placement).toEqual({
      parent: "unit:Sales",
      after: "seat:account-executive",
    });
  });
});

describe("removing", () => {
  test("a seat's removal clears the lead and manages entries that named it, and its access level, each reported", () => {
    const draft = fixture();
    const {
      op,
      draft: next,
      report,
    } = run(draft, { type: "remove", target: "seat:vp-engineering" });
    expect(unit(next, "unit:Engineering").lead).toBeUndefined();
    expect(report.cleared).toEqual([
      { kind: "lead", holder: "unit:Engineering", from: "VP Engineering" },
    ]);
    expect(op.type === "remove" && op.snapshot.json).toEqual(fixtureCompany().units![0]!.roles![0]);

    const dev = run(draft, { type: "remove", target: "seat:dev" });
    expect(seat(dev.draft, "seat:vp-engineering").manages).toBeUndefined();
    expect(
      getPath(doc(dev.draft), ["integrations", "gitlab", "provisioning", "access_levels", "dev"]),
    ).toBeUndefined();
    expect(dev.report.cleared).toEqual([
      { kind: "manages", holder: "seat:vp-engineering", from: "Dev" },
      { kind: "gitlab_access_level", holder: COMPANY_KEY, from: "dev" },
    ]);
  });

  test("a unit's removal takes its subtree, clears manages entries naming it, and keeps or removes seats placed in it", () => {
    const draft = fixture();
    const kept = run(draft, { type: "remove", target: "unit:Engineering", placedSeats: "keep" });
    expect(locate(kept.draft, "seat:sre")).toBeUndefined();
    expect(seat(kept.draft, "seat:ceo").manages).toEqual(["Designer"]);
    expect(seat(kept.draft, "seat:designer").unit).toBeUndefined();
    expect(kept.report.cleared).toEqual(
      expect.arrayContaining([
        { kind: "manages", holder: "seat:ceo", from: "Engineering" },
        { kind: "unit", holder: "seat:designer", from: "Platform" },
        { kind: "gitlab_access_level", holder: COMPANY_KEY, from: "sre" },
      ]),
    );

    const removed = run(draft, {
      type: "remove",
      target: "unit:Engineering",
      placedSeats: "remove",
    });
    expect(locate(removed.draft, "seat:designer")).toBeUndefined();
    expect(seat(removed.draft, "seat:ceo").manages).toBeUndefined();
  });

  test("an entry that named a removed SEAT is cleared even when a unit of that name remains", () => {
    const base: CompanyDocument = {
      name: "X",
      roles: [
        { name: "Boss", manages: ["Ops"] },
        { name: "Ops", goal: "a seat named like a unit" },
      ],
      units: [{ name: "Ops", roles: [{ name: "Worker" }] }],
    };
    const { draft, report } = run(fixture(base), { type: "remove", target: "seat:ops" });
    expect(seat(draft, "seat:boss").manages).toBeUndefined();
    expect(report.cleared).toEqual([{ kind: "manages", holder: "seat:boss", from: "Ops" }]);
  });

  test("a removed unit's name still carried by a seat is the seat's entry and stays", () => {
    const base: CompanyDocument = {
      name: "X",
      roles: [{ name: "Boss", manages: ["Ops"] }, { name: "Ops" }],
      units: [{ name: "Ops" }],
    };
    const { draft } = run(fixture(base), { type: "remove", target: "unit:Ops" });
    expect(seat(draft, "seat:boss").manages).toEqual(["Ops"]);
  });

  test("replacing the Datadog fallback travels with the removal", () => {
    const { draft } = run(fixture(), { type: "remove", target: "seat:sre", routeTo: "dev" });
    expect(getPath(doc(draft), ["integrations", "datadog", "route_to"])).toBe("dev");
  });
});

describe("renaming", () => {
  test("a seat of the base keeps its identity: the engine's handle is pinned and references follow", () => {
    const { op, draft, report } = run(fixture(), {
      type: "renameSeat",
      target: "seat:vp-engineering",
      name: " Head of Engineering ",
    });
    expect(op).toMatchObject({
      before: "VP Engineering",
      after: "Head of Engineering",
      pin: "vp-engineering",
    });
    expect(seat(draft, "seat:vp-engineering")).toMatchObject({
      name: "Head of Engineering",
      handle: "vp-engineering",
    });
    expect(unit(draft, "unit:Engineering").lead).toBe("Head of Engineering");
    expect(report.followed).toEqual([
      {
        kind: "lead",
        holder: "unit:Engineering",
        from: "VP Engineering",
        to: "Head of Engineering",
      },
    ]);
  });

  test("a seat keyed by path takes the handle the last check reported, and is refused without one", () => {
    const base: CompanyDocument = { name: "X", roles: [{ name: "A" }] };
    const draft = fromDocument(base, null);
    const target = seatPathKey("roles[0]");
    expect(record(draft, { type: "renameSeat", target, name: "B" })).toMatchObject({
      refusal: "unknown_handle",
    });
    const op = recordOk(
      draft,
      { type: "renameSeat", target, name: "B" },
      { handleOf: (k) => (k === target ? "a" : undefined) },
    );
    expect(op).toMatchObject({ pin: "a" });
  });

  test("a created seat is renamed without a pin, and the access level of its old handle is cleared", () => {
    const base = fixtureCompany();
    const draft = fixture(base);
    const added = run(draft, {
      type: "addSeat",
      key: "new:q",
      placement: { parent: COMPANY_KEY, after: null },
      data: { name: "QA" },
    }).draft;
    const withLevel = run(
      added,
      { type: "updateSeat", target: "new:q", set: [], accessLevel: "developer" },
      { handleOf: () => "qa" },
    ).draft;
    const { op, draft: renamed } = run(
      withLevel,
      { type: "renameSeat", target: "new:q", name: "Quality" },
      { handleOf: () => "qa" },
    );
    expect(op).toMatchObject({ accessLevels: [{ handle: "qa", before: "developer" }] });
    expect("pin" in op).toBe(false);
    expect(
      getPath(doc(renamed), ["integrations", "gitlab", "provisioning", "access_levels", "qa"]),
    ).toBeUndefined();
  });

  test("a unit rename follows root unit references and manages entries, but not an entry a seat of that name owns", () => {
    const { draft, report } = run(fixture(), {
      type: "renameUnit",
      target: "unit:Platform",
      name: "Infrastructure",
    });
    expect(seat(draft, "seat:designer").unit).toBe("Infrastructure");
    expect(report.followed).toEqual([
      { kind: "unit", holder: "seat:designer", from: "Platform", to: "Infrastructure" },
    ]);

    const shadowed: CompanyDocument = {
      name: "X",
      roles: [{ name: "Boss", manages: ["Ops"] }, { name: "Ops" }],
      units: [{ name: "Ops" }],
    };
    const renamed = run(fixture(shadowed), {
      type: "renameUnit",
      target: "unit:Ops",
      name: "Operations",
    }).draft;
    expect(seat(renamed, "seat:boss").manages).toEqual(["Ops"]);
  });

  test("an unchanged name records nothing", () => {
    expect(
      record(fixture(), { type: "renameUnit", target: "unit:Sales", name: "Sales " }),
    ).toMatchObject({ refusal: "no_change" });
  });
});

describe("moving", () => {
  test("a move places the node physically, records where it came from, and never by index", () => {
    const { op, draft } = run(fixture(), {
      type: "move",
      target: "seat:dev",
      to: { parent: "unit:Sales", after: null },
    });
    expect(op).toMatchObject({
      from: { parent: "unit:Engineering", after: "seat:vp-engineering" },
      to: { parent: "unit:Sales", after: null },
    });
    expect(doc(draft).units![1]!.roles!.map((r) => r.name)).toEqual(["Dev", "Account Executive"]);
  });

  test("a seat's unit reference is removed by any move, including a nested seat's ignored one", () => {
    const placed = run(fixture(), {
      type: "move",
      target: "seat:designer",
      to: { parent: "unit:Platform", after: null },
    });
    expect(seat(placed.draft, "seat:designer").unit).toBeUndefined();
    expect(placed.report.cleared).toEqual([
      { kind: "unit", holder: "seat:designer", from: "Platform" },
    ]);

    const base = fixtureCompany();
    base.units![1]!.roles![0]!.unit = "Engineering";
    const nested = run(fixture(base), {
      type: "move",
      target: "seat:account-executive",
      to: { parent: COMPANY_KEY, after: null },
    });
    // Left in place, the stale reference would place the seat in Engineering the moment it reached the root.
    expect(seat(nested.draft, "seat:account-executive").unit).toBeUndefined();
  });

  test("a unit cannot move into itself, and a reorder cannot change parents", () => {
    const draft = fixture();
    expect(
      record(draft, {
        type: "move",
        target: "unit:Engineering",
        to: { parent: "unit:Platform", after: null },
      }),
    ).toMatchObject({
      refusal: "into_itself",
    });
    expect(
      record(draft, {
        type: "reorder",
        target: "seat:dev",
        to: { parent: "unit:Sales", after: null },
      }),
    ).toMatchObject({
      refusal: "across_parents",
    });
  });

  test("clearing the leads a moving seat holds is part of the move", () => {
    const { draft, report } = run(fixture(), {
      type: "move",
      target: "seat:vp-engineering",
      to: { parent: "unit:Sales", after: null },
      clearLeads: ["unit:Engineering"],
    });
    expect(unit(draft, "unit:Engineering").lead).toBeUndefined();
    expect(report.cleared).toContainEqual({
      kind: "lead",
      holder: "unit:Engineering",
      from: "VP Engineering",
    });
  });

  test("a reorder moves within the list", () => {
    const { draft } = run(fixture(), {
      type: "reorder",
      target: "seat:vp-engineering",
      to: { parent: "unit:Engineering", after: "seat:dev" },
    });
    expect(doc(draft).units![0]!.roles!.map((r) => r.name)).toEqual(["Dev", "VP Engineering"]);
  });
});

describe("editing", () => {
  test("an edit records only the fields that differ, with what they held", () => {
    const op = recordOk(fixture(), {
      type: "updateSeat",
      target: "seat:dev",
      set: [
        { path: ["goal"], value: "Build" },
        { path: ["backstory"], value: "Joined early" },
        { path: ["integrations", "github", "tier"], value: "developer" },
      ],
    });
    expect(op).toEqual({
      type: "updateSeat",
      target: "seat:dev",
      changes: [
        { path: ["backstory"], after: "Joined early" },
        { path: ["integrations", "github", "tier"], after: "developer" },
      ],
      accessLevels: [],
    });
  });

  test("fields another operation owns cannot be written by an edit", () => {
    const draft = fixture();
    for (const field of ["name", "kind", "manages", "unit", "schedules"]) {
      expect(
        record(draft, {
          type: "updateSeat",
          target: "seat:dev",
          set: [{ path: [field], value: "x" }],
        }),
        field,
      ).toMatchObject({
        refusal: "forbidden_field",
      });
    }
    expect(
      record(draft, {
        type: "updateSeat",
        target: "seat:dev",
        set: [{ path: ["handle"], value: "d" }],
      }),
    ).toMatchObject({
      refusal: "forbidden_field",
    });
    for (const field of ["name", "lead", "roles", "children", "schedules"]) {
      expect(
        record(draft, {
          type: "updateUnit",
          target: "unit:Sales",
          set: [{ path: [field], value: "x" }],
        }),
        field,
      ).toMatchObject({
        refusal: "forbidden_field",
      });
    }
    expect(
      record(draft, { type: "updateCompany", set: [{ path: ["providers"], value: {} }] }),
    ).toMatchObject({ refusal: "forbidden_field" });
  });

  test("a created seat may choose its handle, moving its access level off the old one", () => {
    const added = run(fixture(), {
      type: "addSeat",
      key: "new:q",
      placement: { parent: COMPANY_KEY, after: null },
      data: { name: "QA" },
    }).draft;
    const leveled = run(
      added,
      { type: "updateSeat", target: "new:q", set: [], accessLevel: "developer" },
      { handleOf: () => "qa" },
    ).draft;
    const { draft } = run(
      leveled,
      {
        type: "updateSeat",
        target: "new:q",
        set: [{ path: ["handle"], value: "quality" }],
        accessLevel: "maintainer",
      },
      {
        handleOf: () => "qa",
      },
    );
    const levels = getPath(doc(draft), ["integrations", "gitlab", "provisioning", "access_levels"]);
    expect(levels).toMatchObject({ quality: "maintainer" });
    expect(levels).not.toHaveProperty("qa");
  });

  test("an access level needs a handle the engine reported", () => {
    const added = run(fixture(), {
      type: "addSeat",
      key: "new:q",
      placement: { parent: COMPANY_KEY, after: null },
      data: { name: "QA" },
    }).draft;
    expect(
      record(added, { type: "updateSeat", target: "new:q", set: [], accessLevel: "developer" }),
    ).toMatchObject({ refusal: "unknown_handle" });
  });

  test("the charter, the lead and the reports are edited through their own operations", () => {
    let draft = fixture();
    draft = run(draft, {
      type: "updateCompany",
      set: [{ path: ["vision"], value: "Everywhere" }],
    }).draft;
    draft = run(draft, { type: "setLead", target: "unit:Sales", lead: "Account Executive" }).draft;
    draft = run(draft, { type: "setManages", target: "seat:ceo", manages: ["Designer"] }).draft;
    draft = run(draft, { type: "setManages", target: "seat:vp-engineering", manages: [] }).draft;
    const out = doc(draft);
    expect(out.vision).toBe("Everywhere");
    expect(out.units![1]!.lead).toBe("Account Executive");
    expect(out.roles![0]!.manages).toEqual(["Designer"]);
    expect(out.units![0]!.roles![0]).not.toHaveProperty("manages");
  });

  test("a schedule toggle writes only a change the engine would act on", () => {
    const draft = fixture();
    expect(
      record(draft, {
        type: "setScheduleEnabled",
        target: "unit:Engineering",
        schedule: "standup",
        enabled: true,
      }),
    ).toMatchObject({
      refusal: "no_change",
    });
    const { draft: off } = run(draft, {
      type: "setScheduleEnabled",
      target: "unit:Engineering",
      schedule: "standup",
      enabled: false,
    });
    expect(unit(off, "unit:Engineering").schedules).toEqual([
      { name: "standup", cron: "0 9 * * 1-5", task: "Run standup", target: "lead", enabled: false },
    ]);
    expect(
      record(draft, {
        type: "setScheduleEnabled",
        target: "unit:Engineering",
        schedule: "nope",
        enabled: false,
      }),
    ).toMatchObject({
      refusal: "no_schedule",
    });
  });

  test("the Datadog fallback is set only while Datadog is connected", () => {
    const draft = fixture();
    expect(
      run(draft, { type: "setDatadogRouteTo", routeTo: "dev" }).draft.company.integrations,
    ).toMatchObject({ datadog: { route_to: "dev" } });
    const base = fixtureCompany();
    delete (base.integrations as Record<string, unknown>).datadog;
    expect(record(fixture(base), { type: "setDatadogRouteTo", routeTo: "dev" })).toMatchObject({
      refusal: "no_datadog",
    });
  });
});

describe("integration blocks", () => {
  const levels = (draft: Draft) =>
    getPath(doc(draft), ["integrations", "gitlab", "provisioning", "access_levels"]);

  test("an access level or a Datadog fallback is never written into a block that is not connected", () => {
    const base = fixtureCompany();
    delete (base.integrations as Record<string, unknown>).gitlab;
    delete (base.integrations as Record<string, unknown>).datadog;
    const draft = fixture(base);
    expect(
      record(draft, { type: "updateSeat", target: "seat:dev", set: [], accessLevel: "developer" }),
    ).toMatchObject({ refusal: "no_gitlab" });
    expect(record(draft, { type: "remove", target: "seat:sre", routeTo: "dev" })).toMatchObject({
      refusal: "no_datadog",
    });
    expect(
      record(draft, { type: "changeKind", target: "seat:sre", kind: "human", routeTo: "dev" }),
    ).toMatchObject({ refusal: "no_datadog" });
  });

  test("removing the last access level or the fallback leaves the connected block standing", () => {
    const base: CompanyDocument = {
      name: "X",
      integrations: {
        gitlab: { provisioning: { access_levels: { solo: "developer" } } },
        datadog: { route_to: "solo" },
      },
      roles: [{ name: "Solo" }],
    };
    const removed = run(fixture(base), { type: "remove", target: "seat:solo" }).draft;
    expect(doc(removed).integrations).toEqual({
      gitlab: { provisioning: {} },
      datadog: { route_to: "solo" },
    });
    const cleared = run(fixture(base), { type: "setDatadogRouteTo" }).draft;
    expect(doc(cleared).integrations).toMatchObject({ datadog: {} });
    expect(levels(cleared)).toEqual({ solo: "developer" });
  });

  test("an access level recorded before GitLab was disconnected upstream is gone, not written back", () => {
    const draft = fixture();
    const op = recordOk(draft, {
      type: "updateSeat",
      target: "seat:account-executive",
      set: [],
      accessLevel: "developer",
    });
    const base = fixtureCompany();
    delete (base.integrations as Record<string, unknown>).gitlab;
    expect(evaluate(fixture(base), op)).toEqual({
      kind: "gone",
      reason: "GitLab provisioning is no longer connected.",
    });
  });
});

describe("changing kind", () => {
  test("becoming human strips every field a human seat may not carry, each recorded, and sets the contact", () => {
    const base = fixtureCompany();
    base.units![0]!.roles![1] = {
      name: "Dev",
      llm: "default",
      token_budget: 10,
      mcp_env: { git: { TOKEN: "__redacted__" } },
      behavioral_guidelines: ["Be kind"],
      integrations: {
        github: { tier: "developer" },
        jira: { project: "ENG" },
        slack: { channel: "C1" },
      },
      goal: "Build",
    };
    const { op, draft, report } = run(fixture(base), {
      type: "changeKind",
      target: "seat:dev",
      kind: "human",
      contact: { github_login: "dev" },
    });
    expect(report.stripped).toEqual([
      "llm",
      "token_budget",
      "integrations.slack",
      "integrations.jira",
      "mcp_env",
      "behavioral_guidelines",
    ]);
    expect(seat(draft, "seat:dev")).toEqual({
      name: "Dev",
      kind: "human",
      contact: { github_login: "dev" },
      integrations: { github: { tier: "developer" } },
      goal: "Build",
    });
    expect(
      op.type === "changeKind" && op.stripped.find((f) => f.path.join(".") === "mcp_env")?.before,
    ).toEqual({ git: { TOKEN: "__redacted__" } });
  });

  test("becoming an agent strips contact and availability", () => {
    const base: CompanyDocument = {
      name: "X",
      roles: [
        { name: "Pat", kind: "human", contact: { slack_user_id: "U1" }, availability: "9-5" },
      ],
    };
    const { draft } = run(fixture(base), { type: "changeKind", target: "seat:pat", kind: "agent" });
    expect(seat(draft, "seat:pat")).toEqual({ name: "Pat" });
  });
});

describe("evaluating", () => {
  test("a changed precondition is a conflict carrying the base value, their value and this operation's", () => {
    const draft = fixture();
    const op = recordOk(draft, {
      type: "updateSeat",
      target: "seat:dev",
      set: [{ path: ["goal"], value: "Ship" }],
    });
    const theirs = run(draft, {
      type: "updateSeat",
      target: "seat:dev",
      set: [{ path: ["goal"], value: "Refactor" }],
    }).draft;
    expect(evaluate(theirs, op)).toEqual({
      kind: "conflict",
      conflicts: [{ subject: "goal", base: "Build", theirs: "Refactor", mine: "Ship" }],
    });
    expect(() => apply(theirs, op)).toThrow(ApplyError);
  });

  test("a missing target is gone", () => {
    const draft = fixture();
    const op = recordOk(draft, {
      type: "setLead",
      target: "unit:Sales",
      lead: "Account Executive",
    });
    const removed = run(draft, { type: "remove", target: "unit:Sales" }).draft;
    expect(evaluate(removed, op).kind).toBe("gone");
  });

  test("an edit to one field is untouched by an upstream edit of another", () => {
    const draft = fixture();
    const op = recordOk(draft, {
      type: "updateSeat",
      target: "seat:dev",
      set: [{ path: ["goal"], value: "Ship" }],
    });
    const theirs = run(draft, {
      type: "updateSeat",
      target: "seat:dev",
      set: [{ path: ["backstory"], value: "Theirs" }],
    }).draft;
    const mine = apply(theirs, op).draft;
    expect(seat(mine, "seat:dev")).toMatchObject({ goal: "Ship", backstory: "Theirs" });
  });
});

describe("an operation as data", () => {
  test("recording again from its intent reproduces it on the same draft", () => {
    const draft = fixture();
    const intents: Intent[] = [
      {
        type: "remove",
        target: "unit:Engineering",
        placedSeats: "remove",
        routeTo: "account-executive",
      },
      { type: "renameSeat", target: "seat:dev", name: "Developer" },
      {
        type: "move",
        target: "seat:vp-engineering",
        to: { parent: "unit:Sales", after: null },
        clearLeads: ["unit:Engineering"],
      },
      { type: "updateSeat", target: "seat:dev", set: [{ path: ["goal"] }], accessLevel: null },
      { type: "changeKind", target: "seat:dev", kind: "human", contact: { slack_user_id: "U1" } },
      {
        type: "setScheduleEnabled",
        target: "unit:Engineering",
        schedule: "standup",
        enabled: false,
      },
      { type: "updateCompany", set: [{ path: ["mission"] }] },
    ];
    for (const intent of intents) {
      const op = recordOk(draft, intent);
      expect(recordOk(draft, intentOf(op)), intent.type).toEqual(op);
      expect(JSON.parse(JSON.stringify(op)), intent.type).toEqual(op);
    }
  });

  test("an operation that could never have been recorded is malformed", () => {
    expect(
      malformedReason({
        type: "updateSeat",
        target: "seat:dev",
        changes: [{ path: ["name"], after: "X" }],
        accessLevels: [],
      }),
    ).not.toBeNull();
    expect(
      malformedReason({
        type: "updateSeat",
        target: "seat:dev",
        changes: [{ path: ["handle"], after: "x" }],
        accessLevels: [],
      }),
    ).not.toBeNull();
    expect(
      malformedReason({
        type: "updateSeat",
        target: "new:a",
        changes: [{ path: ["handle"], after: "x" }],
        accessLevels: [],
      }),
    ).toBeNull();
    expect(
      malformedReason({
        type: "updateUnit",
        target: "unit:A",
        changes: [{ path: ["lead"], after: "X" }],
      }),
    ).not.toBeNull();
    expect(
      malformedReason({ type: "updateCompany", changes: [{ path: ["providers"], after: {} }] }),
    ).not.toBeNull();
    expect(
      malformedReason({
        type: "addSeat",
        key: "seat:x",
        placement: { parent: COMPANY_KEY, after: null },
        data: { name: "X" },
      }),
    ).not.toBeNull();
  });

  test("describes itself in one sentence and names what to focus", () => {
    const draft = fixture();
    const op = recordOk(draft, { type: "remove", target: "unit:Engineering" });
    expect(describeOperation(op, draft)).toBe("Removed unit Engineering and 3 seats in it.");
    expect(touchedKeys(op)).toEqual(["unit:Engineering"]);
    const move = recordOk(draft, {
      type: "move",
      target: "seat:dev",
      to: { parent: "unit:Sales", after: null },
    });
    expect(describeOperation(move, draft)).toBe("Moved Dev to Sales.");
  });

  test("applying never mutates the draft it was given", () => {
    const draft = fixture();
    const before = cloneJson(doc(draft));
    run(draft, { type: "remove", target: "unit:Engineering", placedSeats: "remove" });
    run(draft, { type: "renameUnit", target: "unit:Platform", name: "Infra" });
    run(draft, { type: "changeKind", target: "seat:dev", kind: "human" });
    expect(doc(draft)).toEqual(before);
  });
});
