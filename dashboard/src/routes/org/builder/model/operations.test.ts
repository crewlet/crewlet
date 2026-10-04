// @vitest-environment node
/**
 * Operations: recorded with their preconditions, evaluated, applied.
 *
 * The invariants: recording captures what each change replaced; a new node
 * carries an identity of the engine's grammar that no other node holds;
 * applying changes only what the operation names, clears the references that
 * named what it removed and moves those that named a new node whose identity
 * was chosen again, reporting each, while a rename moves nothing; an operation
 * whose preconditions no longer hold is refused with every value a person
 * needs; and a field another operation owns cannot be written around its
 * rules.
 */

import { describe, expect, test } from "vitest";
import type { CompanyDocument } from "~/protocol/index.ts";
import { cloneJson, getPath } from "./json.ts";
import { COMPANY_KEY, mintKey, type NodeKey } from "./keys.ts";
import { locate, type Draft } from "./draft.ts";
import { fromDocument, toDocument } from "./document.ts";
import {
  ApplyError,
  apply,
  describeOperation,
  evaluate,
  intentOf,
  isCredentialField,
  malformedReason,
  record,
  touchedKeys,
  type Intent,
  type Operation,
} from "./operations.ts";
import { countingKeys, fixtureCompany } from "./testkit.ts";

function fixture(doc: CompanyDocument = fixtureCompany()): Draft {
  return fromDocument(doc);
}

function recordOk(draft: Draft, intent: Intent): Operation {
  const result = record(draft, intent);
  if (!result.ok)
    throw new Error(`expected ${intent.type} to record: ${result.refusal}: ${result.message}`);
  return result.op;
}

function run(draft: Draft, intent: Intent) {
  const op = recordOk(draft, intent);
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
      placement: { parent: "unit:engineering", after: "seat:vp-engineering" },
      data: { name: "QA", handle: "qa" },
    });
    expect(doc(next).units![0]!.roles!.map((r) => r.name)).toEqual(["VP Engineering", "QA", "Dev"]);
    expect(locate(next, key)?.parent).toBe("unit:engineering");
  });

  test("a key that was not minted, or already names a node, is refused", () => {
    const draft = fixture();
    const placement = { parent: COMPANY_KEY, after: null };
    expect(
      record(draft, {
        type: "addSeat",
        key: "seat:x",
        placement,
        data: { name: "X", handle: "x" },
      }),
    ).toMatchObject({ refusal: "not_minted" });
    const key = mintKey(countingKeys());
    const added = run(draft, {
      type: "addSeat",
      key,
      placement,
      data: { name: "X", handle: "x" },
    }).draft;
    expect(
      record(added, { type: "addSeat", key, placement, data: { name: "Y", handle: "y" } }),
    ).toMatchObject({ refusal: "key_in_use" });
  });

  // A NEW NODE IS ADDRESSED FROM THE MOMENT IT EXISTS. A lead or a `manages`
  // entry may name it before any check answers, so it is given its handle or
  // key as it is added — the engine's grammar, and no other node's — and the
  // engine never derives one from a name the person may still change.
  test("a new node carries an identity of the engine's grammar that no other node holds", () => {
    const draft = fixture();
    const seat = (data: Record<string, unknown>) =>
      record(draft, {
        type: "addSeat",
        key: "new:s",
        placement: { parent: COMPANY_KEY, after: null },
        data: { name: "S", ...data },
      });
    const unitWith = (data: Record<string, unknown>) =>
      record(draft, {
        type: "addUnit",
        key: "new:u",
        placement: { parent: COMPANY_KEY, after: null },
        data: { name: "U", ...data },
      });
    expect(seat({})).toMatchObject({ refusal: "bad_identity" });
    expect(seat({ handle: "Not A Handle" })).toMatchObject({ refusal: "bad_identity" });
    // Held by a seat, and by a unit: one namespace, since a manages entry may
    // name either.
    expect(seat({ handle: "dev" })).toMatchObject({ refusal: "bad_identity" });
    expect(seat({ handle: "sales" })).toMatchObject({ refusal: "bad_identity" });
    expect(seat({ handle: "s-1" }).ok).toBe(true);
    expect(unitWith({})).toMatchObject({ refusal: "bad_identity" });
    expect(unitWith({ id: "1st" })).toMatchObject({ refusal: "bad_identity" });
    expect(unitWith({ id: "ceo" })).toMatchObject({ refusal: "bad_identity" });
    expect(unitWith({ id: "legal" }).ok).toBe(true);
  });

  test("a unit never carries lists into its own data", () => {
    const op = recordOk(fixture(), {
      type: "addUnit",
      key: "new:u",
      placement: { parent: COMPANY_KEY, after: null },
      data: { name: "Legal", id: "legal", roles: [{ name: "Counsel" }] },
    });
    expect(op.type === "addUnit" && op.data).toEqual({ name: "Legal", id: "legal" });
  });

  test("a neighbour that is not beside the slot is refused, or with leniency lands at the end", () => {
    const draft = fixture();
    const intent: Intent = {
      type: "addSeat",
      key: "new:s",
      placement: { parent: "unit:sales", after: "seat:dev" },
      data: { name: "S", handle: "s" },
    };
    expect(record(draft, intent)).toMatchObject({ refusal: "missing_neighbour" });
    const lenient = record(draft, intent, { lenientPlacement: true });
    expect(lenient.ok && lenient.op.type === "addSeat" && lenient.op.placement).toEqual({
      parent: "unit:sales",
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
    expect(unit(next, "unit:engineering").lead).toBeUndefined();
    expect(report.cleared).toEqual([
      { kind: "lead", holder: "unit:engineering", from: "vp-engineering" },
    ]);
    expect(op.type === "remove" && op.snapshot.json).toEqual(fixtureCompany().units![0]!.roles![0]);

    const dev = run(draft, { type: "remove", target: "seat:dev" });
    expect(seat(dev.draft, "seat:vp-engineering").manages).toBeUndefined();
    expect(
      getPath(doc(dev.draft), ["integrations", "gitlab", "provisioning", "access_levels", "dev"]),
    ).toBeUndefined();
    expect(dev.report.cleared).toEqual([
      { kind: "manages", holder: "seat:vp-engineering", from: "dev" },
      { kind: "gitlab_access_level", holder: COMPANY_KEY, from: "dev" },
    ]);
  });

  test("a unit's removal takes its subtree, clears manages entries naming it, and keeps or removes seats placed in it", () => {
    const draft = fixture();
    const kept = run(draft, { type: "remove", target: "unit:engineering", placedSeats: "keep" });
    expect(locate(kept.draft, "seat:sre")).toBeUndefined();
    expect(seat(kept.draft, "seat:ceo").manages).toEqual(["designer"]);
    expect(seat(kept.draft, "seat:designer").unit).toBeUndefined();
    expect(kept.report.cleared).toEqual(
      expect.arrayContaining([
        { kind: "manages", holder: "seat:ceo", from: "engineering" },
        { kind: "unit", holder: "seat:designer", from: "platform" },
        { kind: "gitlab_access_level", holder: COMPANY_KEY, from: "sre" },
      ]),
    );

    const removed = run(draft, {
      type: "remove",
      target: "unit:engineering",
      placedSeats: "remove",
    });
    expect(locate(removed.draft, "seat:designer")).toBeUndefined();
    expect(seat(removed.draft, "seat:ceo").manages).toBeUndefined();
  });

  // A MANAGES ENTRY NAMES A SEAT FIRST. The engine resolves an entry to the
  // seat holding that handle before the unit holding that key, so the two
  // removals below leave what the entry still names and clear what it no
  // longer can.
  test("an entry that named a removed SEAT is cleared even when a unit of that key remains", () => {
    const base: CompanyDocument = {
      name: "X",
      roles: [
        { name: "Boss", handle: "boss", manages: ["ops"] },
        { name: "Ops", handle: "ops", goal: "a seat keyed like a unit" },
      ],
      units: [{ name: "Ops", id: "ops", roles: [{ name: "Worker", handle: "worker" }] }],
    };
    const { draft, report } = run(fixture(base), { type: "remove", target: "seat:ops" });
    expect(seat(draft, "seat:boss").manages).toBeUndefined();
    expect(report.cleared).toEqual([{ kind: "manages", holder: "seat:boss", from: "ops" }]);
  });

  test("a removed unit's key still held by a seat is the seat's entry and stays", () => {
    const base: CompanyDocument = {
      name: "X",
      roles: [
        { name: "Boss", handle: "boss", manages: ["ops"] },
        { name: "Ops", handle: "ops" },
      ],
      units: [{ name: "Ops", id: "ops" }],
    };
    const { draft } = run(fixture(base), { type: "remove", target: "unit:ops" });
    expect(seat(draft, "seat:boss").manages).toEqual(["ops"]);
  });

  test("replacing the Datadog fallback travels with the removal", () => {
    const { draft } = run(fixture(), { type: "remove", target: "seat:sre", routeTo: "dev" });
    expect(getPath(doc(draft), ["integrations", "datadog", "route_to"])).toBe("dev");
  });
});

describe("renaming", () => {
  // A NAME IS PROSE. Every reference names a seat by its handle and a unit by
  // its key, so a rename touches the name and nothing else: no reference
  // follows, because none named the name.
  test("a seat's rename changes its name and nothing else: its handle and every reference stay", () => {
    const { op, draft, report } = run(fixture(), {
      type: "renameSeat",
      target: "seat:vp-engineering",
      name: " Head of Engineering ",
    });
    expect(op).toEqual({
      type: "renameSeat",
      target: "seat:vp-engineering",
      before: "VP Engineering",
      after: "Head of Engineering",
    });
    expect(seat(draft, "seat:vp-engineering")).toMatchObject({
      name: "Head of Engineering",
      handle: "vp-engineering",
    });
    expect(unit(draft, "unit:engineering").lead).toBe("vp-engineering");
    expect(report).toEqual({ cleared: [], followed: [], stripped: [] });
  });

  test("a unit's rename changes its name and nothing else", () => {
    const { draft, report } = run(fixture(), {
      type: "renameUnit",
      target: "unit:platform",
      name: "Infrastructure",
    });
    expect(unit(draft, "unit:platform")).toMatchObject({ name: "Infrastructure", id: "platform" });
    expect(seat(draft, "seat:designer").unit).toBe("platform");
    expect(seat(draft, "seat:ceo").manages).toEqual(["engineering", "designer"]);
    expect(report.followed).toEqual([]);
  });

  test("a created seat's rename leaves its handle and its access level where they are", () => {
    const added = run(fixture(), {
      type: "addSeat",
      key: "new:q",
      placement: { parent: COMPANY_KEY, after: null },
      data: { name: "QA", handle: "qa" },
    }).draft;
    const withLevel = run(added, {
      type: "updateSeat",
      target: "new:q",
      set: [],
      accessLevel: "developer",
    }).draft;
    const renamed = run(withLevel, { type: "renameSeat", target: "new:q", name: "Quality" }).draft;
    expect(seat(renamed, "new:q")).toMatchObject({ name: "Quality", handle: "qa" });
    expect(
      getPath(doc(renamed), ["integrations", "gitlab", "provisioning", "access_levels", "qa"]),
    ).toBe("developer");
  });

  test("an unchanged name records nothing", () => {
    expect(
      record(fixture(), { type: "renameUnit", target: "unit:sales", name: "Sales " }),
    ).toMatchObject({ refusal: "no_change" });
  });
});

// A NEW NODE'S IDENTITY MAY STILL BE CHOSEN, and nothing outside the draft
// names it yet, so choosing it again moves every reference the draft holds
// to it. A node the saved company holds keeps its identity for good.
describe("choosing a new node's identity again", () => {
  test("a created seat's new handle moves every lead, manages entry and fallback naming it", () => {
    let draft = run(fixture(), {
      type: "addSeat",
      key: "new:q",
      placement: { parent: "unit:sales", after: null },
      data: { name: "QA", handle: "qa" },
    }).draft;
    draft = run(draft, { type: "setLead", target: "unit:sales", lead: "qa" }).draft;
    draft = run(draft, {
      type: "setManages",
      target: "seat:ceo",
      manages: ["engineering", "designer", "qa"],
    }).draft;
    draft = run(draft, { type: "setDatadogRouteTo", routeTo: "qa" }).draft;
    const { draft: next, report } = run(draft, {
      type: "updateSeat",
      target: "new:q",
      set: [{ path: ["handle"], value: "quality" }],
    });
    expect(seat(next, "new:q").handle).toBe("quality");
    expect(unit(next, "unit:sales").lead).toBe("quality");
    expect(seat(next, "seat:ceo").manages).toEqual(["engineering", "designer", "quality"]);
    expect(getPath(doc(next), ["integrations", "datadog", "route_to"])).toBe("quality");
    expect(report.followed).toEqual([
      { kind: "lead", holder: "unit:sales", from: "qa", to: "quality" },
      { kind: "manages", holder: "seat:ceo", from: "qa", to: "quality" },
    ]);
  });

  test("a created unit's new key moves every manages entry naming it", () => {
    let draft = run(fixture(), {
      type: "addUnit",
      key: "new:l",
      placement: { parent: COMPANY_KEY, after: null },
      data: { name: "Legal", id: "legal" },
    }).draft;
    draft = run(draft, {
      type: "setManages",
      target: "seat:ceo",
      manages: ["engineering", "legal"],
    }).draft;
    const { draft: next, report } = run(draft, {
      type: "updateUnit",
      target: "new:l",
      set: [{ path: ["id"], value: "law" }],
    });
    expect(seat(next, "seat:ceo").manages).toEqual(["engineering", "law"]);
    expect(report.followed).toEqual([
      { kind: "manages", holder: "seat:ceo", from: "legal", to: "law" },
    ]);
  });

  test("a saved node's identity is not written, and a new one must be free and well formed", () => {
    const draft = fixture();
    expect(
      record(draft, {
        type: "updateUnit",
        target: "unit:sales",
        set: [{ path: ["id"], value: "x" }],
      }),
    ).toMatchObject({ refusal: "forbidden_field" });
    const added = run(draft, {
      type: "addSeat",
      key: "new:q",
      placement: { parent: COMPANY_KEY, after: null },
      data: { name: "QA", handle: "qa" },
    }).draft;
    for (const value of [undefined, "dev", "Not Valid"]) {
      expect(
        record(added, {
          type: "updateSeat",
          target: "new:q",
          set: [{ path: ["handle"], ...(value === undefined ? {} : { value }) }],
        }),
        String(value),
      ).toMatchObject({ refusal: "bad_identity" });
    }
  });
});

// A SAVED SEAT'S HANDLE NEVER CHANGES: another handle is another seat. So
// the role is moved to a new one, in one step that one undo takes back.
describe("replacing a saved seat", () => {
  test("the new seat takes the old one's place and fields under its own handle, and every reference follows", () => {
    const draft = fixture();
    const {
      op,
      draft: next,
      report,
    } = run(draft, {
      type: "replaceSeat",
      target: "seat:dev",
      key: "new:d",
      handle: "developer",
    });
    expect(locate(next, "seat:dev")).toBeUndefined();
    expect(locate(next, "new:d")).toMatchObject({ parent: "unit:engineering", index: 1 });
    expect(seat(next, "new:d")).toEqual({ ...seat(draft, "seat:dev"), handle: "developer" });
    expect(seat(next, "seat:vp-engineering").manages).toEqual(["developer"]);
    expect(getPath(doc(next), ["integrations", "gitlab", "provisioning", "access_levels"])).toEqual(
      { developer: "developer", sre: "maintainer" },
    );
    expect(report).toEqual({
      cleared: [],
      followed: [{ kind: "manages", holder: "seat:vp-engineering", from: "dev", to: "developer" }],
      stripped: [],
    });
    expect(describeOperation(op, draft)).toBe(
      "Replaced seat Dev (@dev) with a new seat, @developer.",
    );
    expect(touchedKeys(op)).toEqual(["new:d", "seat:dev"]);
    expect(intentOf(op)).toEqual({
      type: "replaceSeat",
      target: "seat:dev",
      key: "new:d",
      handle: "developer",
    });

    // The Datadog fallback follows too: an alert naming nobody wakes the
    // seat that took the role, rather than a handle no seat holds.
    const sre = run(draft, {
      type: "replaceSeat",
      target: "seat:sre",
      key: "new:s",
      handle: "reliability",
    }).draft;
    expect(getPath(doc(sre), ["integrations", "datadog", "route_to"])).toBe("reliability");
  });

  test("only a saved seat is replaced, under a handle that is new, well formed and free", () => {
    const draft = fixture();
    const replace = (target: string, handle: string, key = "new:r") =>
      record(draft, { type: "replaceSeat", target, key, handle });
    expect(replace("seat:dev", "dev")).toMatchObject({ refusal: "no_change" });
    expect(replace("seat:dev", "ceo")).toMatchObject({ refusal: "bad_identity" });
    expect(replace("seat:dev", "engineering")).toMatchObject({ refusal: "bad_identity" });
    expect(replace("seat:dev", "Not Valid")).toMatchObject({ refusal: "bad_identity" });
    expect(replace("seat:dev", "developer", "seat:developer")).toMatchObject({
      refusal: "not_minted",
    });
    expect(replace("unit:sales", "sales-2")).toMatchObject({ refusal: "wrong_kind" });
    const added = run(draft, {
      type: "addSeat",
      key: "new:q",
      placement: { parent: COMPANY_KEY, after: null },
      data: { name: "QA", handle: "qa" },
    }).draft;
    expect(
      record(added, { type: "replaceSeat", target: "new:q", key: "new:r", handle: "quality" }),
    ).toMatchObject({ refusal: "not_editable" });
  });

  test("a seat changed or removed upstream is held as a conflict or gone", () => {
    const draft = fixture();
    const op = recordOk(draft, {
      type: "replaceSeat",
      target: "seat:dev",
      key: "new:d",
      handle: "developer",
    });
    const theirs = cloneJson(fixtureCompany());
    theirs.units![0]!.roles![1]!.goal = "Ship";
    expect(evaluate(fixture(theirs), op)).toMatchObject({
      kind: "conflict",
      conflicts: [{ subject: "the whole seat", shape: "snapshot" }],
    });
    theirs.units![0]!.roles!.splice(1, 1);
    expect(evaluate(fixture(theirs), op).kind).toBe("gone");
    expect(malformedReason({ ...op, key: "seat:developer" } as Operation)).not.toBeNull();
  });
});

describe("moving", () => {
  test("a move places the node physically, records where it came from, and never by index", () => {
    const { op, draft } = run(fixture(), {
      type: "move",
      target: "seat:dev",
      to: { parent: "unit:sales", after: null },
    });
    expect(op).toMatchObject({
      from: { parent: "unit:engineering", after: "seat:vp-engineering" },
      to: { parent: "unit:sales", after: null },
    });
    expect(doc(draft).units![1]!.roles!.map((r) => r.name)).toEqual(["Dev", "Account Executive"]);
  });

  test("a seat's unit reference is removed by any move, including a nested seat's ignored one", () => {
    const placed = run(fixture(), {
      type: "move",
      target: "seat:designer",
      to: { parent: "unit:platform", after: null },
    });
    expect(seat(placed.draft, "seat:designer").unit).toBeUndefined();
    expect(placed.report.cleared).toEqual([
      { kind: "unit", holder: "seat:designer", from: "platform" },
    ]);

    const base = fixtureCompany();
    base.units![1]!.roles![0]!.unit = "engineering";
    const nested = run(fixture(base), {
      type: "move",
      target: "seat:account-executive",
      to: { parent: COMPANY_KEY, after: null },
    });
    // Left in place, the stale reference would place the seat in Engineering the moment it reached the root.
    expect(seat(nested.draft, "seat:account-executive").unit).toBeUndefined();
  });

  test("a move recorded against a unit reference conflicts once that reference changed upstream", () => {
    const op = recordOk(fixture(), {
      type: "move",
      target: "seat:designer",
      to: { parent: "unit:sales", after: null },
    });
    const theirs = fixtureCompany();
    theirs.roles![1]!.unit = "sales";
    expect(evaluate(fixture(theirs), op)).toMatchObject({
      kind: "conflict",
      conflicts: [{ subject: "unit reference", base: "platform", theirs: "sales" }],
    });
  });

  test("a unit cannot move into itself, and a reorder cannot change parents", () => {
    const draft = fixture();
    expect(
      record(draft, {
        type: "move",
        target: "unit:engineering",
        to: { parent: "unit:platform", after: null },
      }),
    ).toMatchObject({
      refusal: "into_itself",
    });
    expect(
      record(draft, {
        type: "reorder",
        target: "seat:dev",
        to: { parent: "unit:sales", after: null },
      }),
    ).toMatchObject({
      refusal: "across_parents",
    });
  });

  test("clearing the leads a moving seat holds is part of the move", () => {
    const { draft, report } = run(fixture(), {
      type: "move",
      target: "seat:vp-engineering",
      to: { parent: "unit:sales", after: null },
      clearLeads: ["unit:engineering"],
    });
    expect(unit(draft, "unit:engineering").lead).toBeUndefined();
    expect(report.cleared).toContainEqual({
      kind: "lead",
      holder: "unit:engineering",
      from: "vp-engineering",
    });
  });

  test("a reorder moves within the list", () => {
    const { draft } = run(fixture(), {
      type: "reorder",
      target: "seat:vp-engineering",
      to: { parent: "unit:engineering", after: "seat:dev" },
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
    for (const field of ["name", "lead", "roles", "children", "schedules", "id"]) {
      expect(
        record(draft, {
          type: "updateUnit",
          target: "unit:sales",
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
      data: { name: "QA", handle: "qa" },
    }).draft;
    const leveled = run(added, {
      type: "updateSeat",
      target: "new:q",
      set: [],
      accessLevel: "developer",
    }).draft;
    const { draft } = run(leveled, {
      type: "updateSeat",
      target: "new:q",
      set: [{ path: ["handle"], value: "quality" }],
      accessLevel: "maintainer",
    });
    const levels = getPath(doc(draft), ["integrations", "gitlab", "provisioning", "access_levels"]);
    expect(levels).toMatchObject({ quality: "maintainer" });
    expect(levels).not.toHaveProperty("qa");
  });

  test("a created seat that only chooses a new handle keeps its access level under it", () => {
    const added = run(fixture(), {
      type: "addSeat",
      key: "new:q",
      placement: { parent: COMPANY_KEY, after: null },
      data: { name: "QA", handle: "qa" },
    }).draft;
    const leveled = run(added, {
      type: "updateSeat",
      target: "new:q",
      set: [],
      accessLevel: "developer",
    }).draft;
    const intent: Intent = {
      type: "updateSeat",
      target: "new:q",
      set: [{ path: ["handle"], value: "quality" }],
    };
    const { op, draft } = run(leveled, intent);
    const levels = getPath(doc(draft), ["integrations", "gitlab", "provisioning", "access_levels"]);
    expect(levels).toMatchObject({ quality: "developer" });
    expect(levels).not.toHaveProperty("qa");
    // Recording it again from its intent says the same thing.
    expect(recordOk(leveled, intentOf(op))).toEqual(op);
  });

  test("the charter, the lead and the reports are edited through their own operations", () => {
    let draft = fixture();
    draft = run(draft, {
      type: "updateCompany",
      set: [{ path: ["vision"], value: "Everywhere" }],
    }).draft;
    draft = run(draft, { type: "setLead", target: "unit:sales", lead: "account-executive" }).draft;
    draft = run(draft, { type: "setManages", target: "seat:ceo", manages: ["designer"] }).draft;
    draft = run(draft, { type: "setManages", target: "seat:vp-engineering", manages: [] }).draft;
    const out = doc(draft);
    expect(out.vision).toBe("Everywhere");
    expect(out.units![1]!.lead).toBe("account-executive");
    expect(out.roles![0]!.manages).toEqual(["designer"]);
    expect(out.units![0]!.roles![0]).not.toHaveProperty("manages");
  });

  test("a schedule toggle writes only a change the engine would act on", () => {
    const draft = fixture();
    expect(
      record(draft, {
        type: "setScheduleEnabled",
        target: "unit:engineering",
        schedule: "standup",
        enabled: true,
      }),
    ).toMatchObject({
      refusal: "no_change",
    });
    const { draft: off } = run(draft, {
      type: "setScheduleEnabled",
      target: "unit:engineering",
      schedule: "standup",
      enabled: false,
    });
    expect(unit(off, "unit:engineering").schedules).toEqual([
      { name: "standup", cron: "0 9 * * 1-5", task: "Run standup", target: "lead", enabled: false },
    ]);
    expect(
      record(draft, {
        type: "setScheduleEnabled",
        target: "unit:engineering",
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
      token_budget: { day: 10, month: 200 },
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
      "integrations.github",
    ]);
    expect(seat(draft, "seat:dev")).toEqual({
      name: "Dev",
      kind: "human",
      contact: { github_login: "dev" },
      goal: "Build",
    });
    expect(
      op.type === "changeKind" && op.stripped.find((f) => f.path.join(".") === "mcp_env")?.before,
    ).toEqual({ git: { TOKEN: "__redacted__" } });
  });

  // The engine refuses a seat's own GitHub App on a human seat on admission
  // (`config.Company.validateHumanSeatApps`): a person acts on GitHub as
  // `contact.github_login`, never as an app. A kind change that kept the block
  // would produce a draft the next check refuses over a field nobody touched,
  // and the app's key is a credential the builder can never re-enter.
  test("becoming human strips the seat's own GitHub App, marked as a credential", () => {
    const base = fixtureCompany();
    base.units![0]!.roles![1] = {
      name: "Dev",
      integrations: {
        github: { tier: "review", app_slug: "acme-dev", private_key: "${DEV_GITHUB_KEY}" },
      },
    };
    const { op, draft } = run(fixture(base), {
      type: "changeKind",
      target: "seat:dev",
      kind: "human",
      contact: { github_login: "dev" },
    });
    expect(getPath(seat(draft, "seat:dev"), ["integrations", "github"])).toBeUndefined();
    expect(isCredentialField(["integrations", "github"])).toBe(true);
    expect(op.type === "changeKind" && op.stripped.map((f) => f.path.join("."))).toEqual([
      "integrations.github",
    ]);
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

  // A CONFLICT'S VALUES ARE NOT ALWAYS A FIELD'S. Node keys, placements, a
  // removal's whole node and a kind change's stripped fields are the
  // builder's own structures, so each says which it is and a view can name
  // what it is about rather than print keys and masked credentials.
  test("a conflict over the builder's own structures says which structure it holds", () => {
    const draft = fixture();
    const shapes = (theirs: Draft, op: Operation) => {
      const outcome = evaluate(theirs, op);
      if (outcome.kind !== "conflict") throw new Error(`expected a conflict, got ${outcome.kind}`);
      return outcome.conflicts.map((c) => [c.subject, c.shape]);
    };
    const editDev = {
      type: "updateSeat",
      target: "seat:dev",
      set: [{ path: ["goal"], value: "Ship" }],
    } as const;

    const removal = recordOk(draft, { type: "remove", target: "seat:dev" });
    expect(shapes(run(draft, editDev).draft, removal)).toEqual([["the whole seat", "snapshot"]]);

    const moveDev = recordOk(draft, {
      type: "move",
      target: "seat:dev",
      to: { parent: "unit:sales", after: null },
    });
    const movedUp = run(draft, {
      type: "move",
      target: "seat:dev",
      to: { parent: "unit:platform", after: null },
    }).draft;
    expect(shapes(movedUp, moveDev)).toContainEqual(["where it sits", "parent"]);

    const reorderDev = recordOk(draft, {
      type: "reorder",
      target: "seat:dev",
      to: { parent: "unit:engineering", after: null },
    });
    const addedAhead = run(draft, {
      type: "addSeat",
      key: mintKey(countingKeys("a")),
      placement: { parent: "unit:engineering", after: "seat:vp-engineering" },
      data: { name: "QA", handle: "qa" },
    }).draft;
    expect(shapes(addedAhead, reorderDev)).toEqual([["position", "placement"]]);

    const addAfterDev = recordOk(draft, {
      type: "addSeat",
      key: mintKey(countingKeys("b")),
      placement: { parent: "unit:engineering", after: "seat:dev" },
      data: { name: "QA", handle: "qa" },
    });
    const devGone = run(draft, { type: "remove", target: "seat:dev" }).draft;
    expect(shapes(devGone, addAfterDev)).toEqual([["position", "sibling"]]);

    const toHuman = recordOk(draft, { type: "changeKind", target: "seat:sre", kind: "human" });
    const trackerMoved = run(draft, {
      type: "updateSeat",
      target: "seat:sre",
      set: [{ path: ["integrations", "jira", "project"], value: "SUP" }],
    }).draft;
    expect(shapes(trackerMoved, toHuman)).toContainEqual(["fields the new kind removes", "fields"]);

    // A field's own values carry no shape: they are what the document writes.
    const editOp = recordOk(draft, editDev);
    const refactored = run(draft, {
      ...editDev,
      set: [{ path: ["goal"], value: "Refactor" }],
    }).draft;
    expect(shapes(refactored, editOp)).toEqual([["goal", undefined]]);
  });

  test("a missing target is gone", () => {
    const draft = fixture();
    const op = recordOk(draft, {
      type: "setLead",
      target: "unit:sales",
      lead: "account-executive",
    });
    const removed = run(draft, { type: "remove", target: "unit:sales" }).draft;
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
        target: "unit:engineering",
        placedSeats: "remove",
        routeTo: "account-executive",
      },
      { type: "renameSeat", target: "seat:dev", name: "Developer" },
      {
        type: "move",
        target: "seat:vp-engineering",
        to: { parent: "unit:sales", after: null },
        clearLeads: ["unit:engineering"],
      },
      { type: "updateSeat", target: "seat:dev", set: [{ path: ["goal"] }], accessLevel: null },
      { type: "changeKind", target: "seat:dev", kind: "human", contact: { slack_user_id: "U1" } },
      {
        type: "setScheduleEnabled",
        target: "unit:engineering",
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
        type: "updateUnit",
        target: "unit:sales",
        changes: [{ path: ["id"], after: "x" }],
      }),
    ).not.toBeNull();
    expect(
      malformedReason({
        type: "updateUnit",
        target: "new:u",
        changes: [{ path: ["id"], after: "x" }],
      }),
    ).toBeNull();
    expect(
      malformedReason({
        type: "addSeat",
        key: "seat:x",
        placement: { parent: COMPANY_KEY, after: null },
        data: { name: "X", handle: "x" },
      }),
    ).not.toBeNull();
    // A stored log whose created node carries no identity could never have
    // been recorded by this build, which always writes one.
    expect(
      malformedReason({
        type: "addSeat",
        key: "new:x",
        placement: { parent: COMPANY_KEY, after: null },
        data: { name: "X" },
      }),
    ).not.toBeNull();
    expect(
      malformedReason({
        type: "addUnit",
        key: "new:x",
        placement: { parent: COMPANY_KEY, after: null },
        data: { name: "X" },
      }),
    ).not.toBeNull();
  });

  test("describes itself in one sentence and names what to focus", () => {
    const draft = fixture();
    const op = recordOk(draft, { type: "remove", target: "unit:engineering" });
    expect(describeOperation(op, draft)).toBe("Removed unit Engineering and 3 seats in it.");
    expect(touchedKeys(op)).toEqual(["unit:engineering"]);
    const move = recordOk(draft, {
      type: "move",
      target: "seat:dev",
      to: { parent: "unit:sales", after: null },
    });
    expect(describeOperation(move, draft)).toBe("Moved Dev to Sales.");

    const withPlaced = recordOk(draft, {
      type: "remove",
      target: "unit:engineering",
      placedSeats: "remove",
    });
    expect(describeOperation(withPlaced, draft)).toBe(
      "Removed unit Engineering and 4 seats in it.",
    );
    const added = run(draft, {
      type: "addUnit",
      key: "new:legal",
      placement: { parent: COMPANY_KEY, after: null },
      data: { name: "Legal", id: "legal" },
    }).draft;
    expect(describeOperation(recordOk(added, { type: "remove", target: "new:legal" }), added)).toBe(
      "Removed unit Legal.",
    );

    const human = recordOk(draft, { type: "changeKind", target: "seat:dev", kind: "human" });
    expect(describeOperation(human, draft)).toBe("Changed Dev to a human seat.");
    const humanDraft = apply(draft, human).draft;
    const agent = recordOk(humanDraft, { type: "changeKind", target: "seat:dev", kind: "agent" });
    expect(describeOperation(agent, humanDraft)).toBe("Changed Dev to an agent seat.");
  });

  test("an operation that records always applies to the draft it was recorded on, whatever odd values that draft holds", () => {
    // Recording and evaluating read the same fields; where they read them
    // differently, an operation that just recorded fails its own
    // preconditions and applying it throws inside the reducer.
    const odd: CompanyDocument = {
      name: "Odd",
      roles: [
        { name: "Blank Handle", handle: "" },
        { name: "Null Kind", kind: null as unknown as string },
      ],
      units: [
        {
          name: "Spaced",
          lead: "  ",
          schedules: [null as never, { name: "weekly", cron: "0 9 * * 1", task: "Review" }],
          roles: [{ name: "Member" }],
        },
      ],
    };
    const draft = fixture(odd);
    const intents: Intent[] = [
      { type: "renameSeat", target: "seat:blank-handle", name: "Renamed" },
      {
        type: "changeKind",
        target: "seat:null-kind",
        kind: "human",
        contact: { github_login: "x" },
      },
      {
        type: "move",
        target: "seat:member",
        to: { parent: COMPANY_KEY, after: null },
        clearLeads: ["unit:spaced"],
      },
      { type: "setScheduleEnabled", target: "unit:spaced", schedule: "weekly", enabled: false },
    ];
    for (const intent of intents) {
      const op = recordOk(draft, intent);
      expect(evaluate(draft, op), intent.type).toEqual({ kind: "applies" });
      expect(() => apply(draft, op), intent.type).not.toThrow();
    }
  });

  test("applying never mutates the draft it was given", () => {
    const draft = fixture();
    const before = cloneJson(doc(draft));
    run(draft, { type: "remove", target: "unit:engineering", placedSeats: "remove" });
    run(draft, { type: "renameUnit", target: "unit:platform", name: "Infra" });
    run(draft, { type: "changeKind", target: "seat:dev", kind: "human" });
    expect(doc(draft)).toEqual(before);
  });
});

describe("editing a node in one operation", () => {
  const devEdit = (): Intent => ({
    type: "edit",
    target: "seat:dev",
    intents: [
      { type: "renameSeat", target: "seat:dev", name: "Developer" },
      { type: "updateSeat", target: "seat:dev", set: [{ path: ["goal"], value: "Ship" }] },
      { type: "setManages", target: "seat:dev", manages: ["sre"] },
    ],
  });

  test("a form's changes to one seat record as one edit that applies every part", () => {
    const draft = fixture();
    const { op, draft: next, report } = run(draft, devEdit());
    expect(op.type === "edit" && op.ops.map((part) => part.type)).toEqual([
      "renameSeat",
      "updateSeat",
      "setManages",
    ]);
    expect(seat(next, "seat:dev")).toMatchObject({
      name: "Developer",
      handle: "dev",
      goal: "Ship",
      manages: ["sre"],
    });
    // The rename moved no reference: the VP still manages the seat by its handle.
    expect(seat(next, "seat:vp-engineering").manages).toEqual(["dev"]);
    expect(report.followed).toEqual([]);
    expect(describeOperation(op, draft)).toBe("Edited Dev: renamed to Developer, goal, manages.");
    expect(touchedKeys(op)).toEqual(["seat:dev"]);
  });

  test("a unit's lead, fields and schedule toggle apply together, and the charter edits as the company", () => {
    const draft = fixture();
    const { op, draft: next } = run(draft, {
      type: "edit",
      target: "unit:engineering",
      intents: [
        {
          type: "updateUnit",
          target: "unit:engineering",
          set: [{ path: ["purpose"], value: "Build" }],
        },
        { type: "setLead", target: "unit:engineering", lead: "dev" },
        {
          type: "setScheduleEnabled",
          target: "unit:engineering",
          schedule: "standup",
          enabled: false,
        },
      ],
    });
    expect(unit(next, "unit:engineering")).toMatchObject({ purpose: "Build", lead: "dev" });
    expect(unit(next, "unit:engineering").schedules![0]!.enabled).toBe(false);
    expect(describeOperation(op, draft)).toBe(
      "Edited Engineering: purpose, lead, disabled schedule standup.",
    );

    const charter = recordOk(draft, {
      type: "edit",
      target: COMPANY_KEY,
      intents: [
        { type: "updateCompany", set: [{ path: ["mission"], value: "Make more" }] },
        { type: "updateCompany", set: [{ path: ["vision"], value: "Everywhere" }] },
      ],
    });
    expect(describeOperation(charter, draft)).toBe("Edited the charter: mission, vision.");
  });

  test("what every part moved is reported together", () => {
    const added = run(fixture(), {
      type: "addSeat",
      key: "new:qa",
      placement: { parent: "unit:engineering", after: null },
      data: { name: "QA", handle: "qa" },
    }).draft;
    const led = run(added, { type: "setLead", target: "unit:sales", lead: "qa" }).draft;
    const { report } = run(led, {
      type: "edit",
      target: "new:qa",
      intents: [
        { type: "renameSeat", target: "new:qa", name: "Quality" },
        { type: "updateSeat", target: "new:qa", set: [{ path: ["handle"], value: "quality" }] },
      ],
    });
    expect(report.followed).toEqual([
      { kind: "lead", holder: "unit:sales", from: "qa", to: "quality" },
    ]);
  });

  // ALL OR NOTHING: a refusal of a later part must not leave the earlier
  // parts recorded, or Apply would half save a form into the draft.
  test("a refused part refuses the whole edit", () => {
    const result = record(fixture(), {
      type: "edit",
      target: "seat:dev",
      intents: [
        { type: "renameSeat", target: "seat:dev", name: "Developer" },
        { type: "updateSeat", target: "seat:dev", set: [{ path: ["handle"], value: "developer" }] },
      ],
    });
    expect(result).toMatchObject({ ok: false, refusal: "forbidden_field" });
  });

  test("an unchanged part drops out, a single change records as itself, and no change is no change", () => {
    const draft = fixture();
    const single = recordOk(draft, {
      type: "edit",
      target: "seat:dev",
      intents: [
        { type: "renameSeat", target: "seat:dev", name: "Dev" },
        { type: "updateSeat", target: "seat:dev", set: [{ path: ["goal"], value: "Ship" }] },
      ],
    });
    expect(single.type).toBe("updateSeat");
    expect(
      record(draft, {
        type: "edit",
        target: "seat:dev",
        intents: [
          { type: "updateSeat", target: "seat:dev", set: [{ path: ["goal"], value: "Build" }] },
        ],
      }),
    ).toMatchObject({ ok: false, refusal: "no_change" });
  });

  test("a part for another node, or a change no editor makes, is refused", () => {
    const draft = fixture();
    expect(
      record(draft, {
        type: "edit",
        target: "seat:dev",
        intents: [
          { type: "updateSeat", target: "seat:sre", set: [{ path: ["goal"], value: "X" }] },
        ],
      }),
    ).toMatchObject({ ok: false, refusal: "not_editable" });
    expect(
      record(draft, {
        type: "edit",
        target: "seat:dev",
        intents: [{ type: "remove", target: "seat:dev" } as never],
      }),
    ).toMatchObject({ ok: false, refusal: "not_editable" });
  });

  test("an upstream change to one part's field is a conflict of the edit; a removed node is gone", () => {
    const draft = fixture();
    const op = recordOk(draft, devEdit());
    const theirs = run(draft, {
      type: "updateSeat",
      target: "seat:dev",
      set: [{ path: ["goal"], value: "Theirs" }],
    }).draft;
    expect(evaluate(theirs, op)).toEqual({
      kind: "conflict",
      conflicts: [{ subject: "goal", base: "Build", theirs: "Theirs", mine: "Ship" }],
    });
    const removed = run(draft, { type: "remove", target: "seat:dev" }).draft;
    expect(evaluate(removed, op).kind).toBe("gone");
    expect(() => apply(theirs, op)).toThrow(ApplyError);
  });

  // The person resolving a conflict chooses for the whole edit, so every part
  // that conflicts is on screen together, not only the first.
  test("every conflicting part of an edit is reported together", () => {
    const draft = fixture();
    const op = recordOk(draft, devEdit());
    const goal = run(draft, {
      type: "updateSeat",
      target: "seat:dev",
      set: [{ path: ["goal"], value: "Theirs" }],
    }).draft;
    const both = run(goal, { type: "setManages", target: "seat:dev", manages: ["ceo"] }).draft;
    const outcome = evaluate(both, op);
    expect(outcome.kind).toBe("conflict");
    expect(outcome.kind === "conflict" && outcome.conflicts.map((c) => c.subject)).toEqual([
      "goal",
      "manages",
    ]);
  });

  test("an edit is data: it round-trips, records again from its intent, and is malformed when no build could record it", () => {
    const draft = fixture();
    const op = recordOk(draft, devEdit());
    expect(JSON.parse(JSON.stringify(op))).toEqual(op);
    expect(recordOk(draft, intentOf(op))).toEqual(op);
    expect(malformedReason(op)).toBeNull();
    if (op.type !== "edit") throw new Error("expected an edit");
    const [rename, update] = op.ops;
    expect(malformedReason({ ...op, ops: [rename!] })).not.toBeNull();
    expect(malformedReason({ ...op, target: "seat:sre" })).not.toBeNull();
    expect(
      malformedReason({
        ...op,
        ops: [rename!, { type: "remove", target: "seat:dev" } as never],
      }),
    ).not.toBeNull();
    expect(
      malformedReason({
        ...op,
        ops: [
          rename!,
          {
            ...(update as Extract<Operation, { type: "updateSeat" }>),
            changes: [{ path: ["name"], after: "X" }],
          },
        ],
      }),
    ).not.toBeNull();
  });
});
